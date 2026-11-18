package arnold

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const accessCardBlockPrefix = "arnold_access_card:"

func accessCardBlockID(cardID string) (string, error) {
	if cardID == "" || len(cardID) > 100 {
		return "", fmt.Errorf("invalid access card ID")
	}
	for _, r := range cardID {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return "", fmt.Errorf("invalid access card ID")
		}
	}
	return accessCardBlockPrefix + cardID, nil
}

// accessCardBlocks fails rather than clipping permission scope. Every field
// derived from the request is a plain_text object, never Slack markup.
func accessCardBlocks(v AccessCardView) ([]map[string]any, error) {
	marker, err := accessCardBlockID(v.CardID)
	if err != nil {
		return nil, err
	}
	if v.TaskID == "" || v.RequestID == "" || v.Summary == "" || v.Status == "" || len(v.Details) > 16 {
		return nil, fmt.Errorf("incomplete or oversized access card")
	}
	plain := func(text string) map[string]any {
		return map[string]any{"type": "plain_text", "text": text, "emoji": false}
	}
	title := "Access requested"
	scope := "Review the request's scope before approving."
	if v.Kind == "network" {
		title = "Network access requested"
		scope = "Scope: this task's OpenShell sandbox. Matching requests remain allowed until the rule is removed."
	} else if v.Actionable {
		return nil, fmt.Errorf("unsupported actionable access kind")
	}
	identity := "Task: " + v.TaskID + "\nRequest: " + v.RequestID
	texts := []string{v.Summary, identity, scope, "Status: " + v.Status}
	texts = append(texts, v.Details...)
	for _, text := range texts {
		if !utf8.ValidString(text) || strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > 3000 {
			return nil, fmt.Errorf("access card text exceeds Slack limits")
		}
	}
	blocks := []map[string]any{{"type": "header", "block_id": marker, "text": plain(title)}}
	section := func(text string) {
		blocks = append(blocks, map[string]any{"type": "section", "text": plain(text)})
	}
	if v.Summary != title {
		section(v.Summary)
	}
	section("Status: " + v.Status)
	section(scope)
	for _, detail := range v.Details {
		// Remove only exact duplicates of information already shown. Preserve
		// every other review field, including the complete requested rule.
		if detail == "Task: "+v.TaskID || detail == "Request: "+v.RequestID || detail == scope ||
			(v.Kind == "network" && detail == "Scope: this sandbox. This rule may permit later matching requests; it is not a single download.") {
			continue
		}
		section(detail)
	}
	blocks = append(blocks, map[string]any{"type": "context", "elements": []map[string]any{plain(identity)}})
	if v.Actionable && (v.CanApprove || v.CanReject) {
		if v.Token == "" || len(v.Token) > 256 || !utf8.ValidString(v.Token) {
			return nil, fmt.Errorf("invalid access card action token")
		}
		label := "Approve for this sandbox"
		buttons := []map[string]any{}
		if v.CanApprove {
			buttons = append(buttons, map[string]any{"type": "button", "text": plain(label), "style": "primary", "action_id": "arnold_access_approve", "value": v.Token})
		}
		if v.CanReject {
			buttons = append(buttons, map[string]any{"type": "button", "text": plain("Reject"), "style": "danger", "action_id": "arnold_access_reject", "value": v.Token})
		}
		blocks = append(blocks, map[string]any{"type": "actions", "elements": buttons})
	}
	return blocks, nil
}

func accessCardArgs(channel string, v AccessCardView) (map[string]any, error) {
	if channel == "" {
		return nil, fmt.Errorf("access card channel is required")
	}
	blocks, err := accessCardBlocks(v)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"channel": channel, "text": v.Summary + "\nStatus: " + v.Status,
		"blocks": blocks, "mrkdwn": false, "parse": "none", "link_names": false,
		"unfurl_links": false, "unfurl_media": false,
	}, nil
}

// PostAccessCard makes exactly one attempt. An uncertain result must be
// reconciled with FindAccessCard rather than blindly posting another card.
func (s *Slack) PostAccessCard(ctx context.Context, channel, thread, clientID string, v AccessCardView) (string, error) {
	args, err := accessCardArgs(channel, v)
	if err != nil {
		return "", err
	}
	if thread == "" || clientID == "" {
		return "", fmt.Errorf("access card thread and delivery ID are required")
	}
	hash := sha256.Sum256([]byte(clientID))
	hash[6] = (hash[6] & 0x0f) | 0x40
	hash[8] = (hash[8] & 0x3f) | 0x80
	uuid := hex.EncodeToString(hash[:16])
	args["client_msg_id"] = uuid[:8] + "-" + uuid[8:12] + "-" + uuid[12:16] + "-" + uuid[16:20] + "-" + uuid[20:]
	args["thread_ts"] = thread
	var response struct {
		TS      string `json:"ts"`
		Channel string `json:"channel"`
	}
	if err = s.api(ctx, "chat.postMessage", args, false, &response, 1); err != nil {
		var status *slackHTTPError
		if errors.As(err, &status) && status.Status == 429 {
			// There was no accepted message. Honor Slack's cooldown without
			// retrying a post whose identity has not been durably recorded.
			_ = pause(ctx, status.RetryAfter)
		}
		return "", fmt.Errorf("access card post outcome requires reconciliation: %w", err)
	}
	if response.TS == "" || response.Channel != channel {
		return "", fmt.Errorf("access card post returned an unconfirmed message identity")
	}
	return response.TS, nil
}

func (s *Slack) UpdateAccessCard(ctx context.Context, channel, messageTS string, v AccessCardView) error {
	args, err := accessCardArgs(channel, v)
	if err != nil {
		return err
	}
	if messageTS == "" {
		return fmt.Errorf("access card message timestamp is required")
	}
	args["ts"] = messageTS
	var response struct {
		TS      string `json:"ts"`
		Channel string `json:"channel"`
	}
	if err = s.API(ctx, "chat.update", args, false, &response); err != nil {
		return err
	}
	if response.TS != messageTS || response.Channel != channel {
		return fmt.Errorf("access card update returned an unconfirmed message identity")
	}
	return nil
}

// FindAccessCard searches the complete bounded thread view for one card posted
// by Arnold. A partial view cannot establish that its match is unique.
func (s *Slack) FindAccessCard(ctx context.Context, channel, thread, cardID, botUserID string) (string, error) {
	marker, err := accessCardBlockID(cardID)
	if err != nil {
		return "", err
	}
	if channel == "" || thread == "" || botUserID == "" {
		return "", fmt.Errorf("access card lookup requires channel, thread, and bot identity")
	}
	cursor, found := "", ""
	seen := map[string]bool{}
	for page := 0; page < 4; page++ {
		var response struct {
			Messages []struct {
				TS       string `json:"ts"`
				ThreadTS string `json:"thread_ts"`
				User     string `json:"user"`
				BotID    string `json:"bot_id"`
				Blocks   []struct {
					BlockID string `json:"block_id"`
				} `json:"blocks"`
			} `json:"messages"`
			HasMore  bool `json:"has_more"`
			Metadata struct {
				Cursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		args := map[string]any{"channel": channel, "ts": thread, "limit": 100}
		if cursor != "" {
			args["cursor"] = cursor
		}
		if err = s.API(ctx, "conversations.replies", args, false, &response); err != nil {
			return "", err
		}
		if response.Messages == nil {
			return "", fmt.Errorf("access card lookup returned an invalid message list")
		}
		for _, message := range response.Messages {
			if message.User != botUserID || message.BotID == "" || message.ThreadTS != thread || message.TS == "" || message.TS == thread {
				continue
			}
			match := false
			for _, block := range message.Blocks {
				match = match || block.BlockID == marker
			}
			if match {
				if found != "" && found != message.TS {
					return "", fmt.Errorf("multiple access cards match the delivery identity")
				}
				found = message.TS
			}
		}
		next := strings.TrimSpace(response.Metadata.Cursor)
		if next == "" {
			if response.HasMore {
				return "", fmt.Errorf("access card lookup has incomplete pagination")
			}
			return found, nil
		}
		if seen[next] {
			return "", fmt.Errorf("access card lookup cursor did not advance")
		}
		seen[next], cursor = true, next
	}
	return "", fmt.Errorf("access card lookup exceeds the bounded thread history")
}
