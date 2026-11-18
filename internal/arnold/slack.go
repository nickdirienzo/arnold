package arnold

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/gorilla/websocket"
)

type Slack struct {
	BotToken, AppToken, BaseURL string
	HTTP                        *http.Client
	Log                         *slog.Logger
}
type SlackError string

type slackHTTPError struct {
	Method     string
	Status     int
	RetryAfter time.Duration
}

func (e *slackHTTPError) Error() string { return fmt.Sprintf("Slack %s HTTP %d", e.Method, e.Status) }

func (e SlackError) Error() string { return "Slack: " + string(e) }
func slackCode(e error) string {
	var se SlackError
	if errors.As(e, &se) {
		return string(se)
	}
	return "request_failed"
}
func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func (s *Slack) API(ctx context.Context, method string, args any, app bool, out any) error {
	return s.api(ctx, method, args, app, out, 3)
}

// api permits non-idempotent callers to use a single attempt. A transport or
// server error after posting a message must be reconciled before another post.
func (s *Slack) api(ctx context.Context, method string, args any, app bool, out any, attempts int) error {
	b, e := json.Marshal(args)
	if e != nil {
		return e
	}
	base := s.BaseURL
	if base == "" {
		base = "https://slack.com/api/"
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	token := s.BotToken
	if app {
		token = s.AppToken
	}
	httpMethod, endpoint := http.MethodPost, base+method
	if method == "conversations.replies" {
		// This read endpoint accepts query parameters, not a JSON body.
		var params map[string]any
		if e = json.Unmarshal(b, &params); e != nil {
			return e
		}
		query := url.Values{}
		for key, value := range params {
			query.Set(key, fmt.Sprint(value))
		}
		httpMethod, endpoint = http.MethodGet, endpoint+"?"+query.Encode()
		b = nil
	}
	for attempt := 0; attempt < attempts; attempt++ {
		req, e := http.NewRequestWithContext(ctx, httpMethod, endpoint, bytes.NewReader(b))
		if e != nil {
			return fmt.Errorf("invalid Slack endpoint")
		}
		if httpMethod == http.MethodPost {
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, e := client.Do(req)
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("Slack %s transport failed", method)
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("Slack %s response read failed", method)
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			if attempt == attempts-1 {
				retryAfter := time.Second
				if seconds, _ := strconv.Atoi(resp.Header.Get("Retry-After")); seconds > 0 {
					retryAfter = time.Duration(min(seconds, 3600)) * time.Second
				}
				return &slackHTTPError{Method: method, Status: resp.StatusCode, RetryAfter: retryAfter}
			}
			delay := time.Duration(attempt+1) * time.Second
			if seconds, _ := strconv.Atoi(resp.Header.Get("Retry-After")); seconds > 0 {
				delay = time.Duration(seconds) * time.Second
			}
			if e = pause(ctx, delay); e != nil {
				return e
			}
			continue
		}
		if resp.StatusCode != 200 {
			return &slackHTTPError{Method: method, Status: resp.StatusCode}
		}
		var result struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &result) != nil {
			return fmt.Errorf("invalid Slack %s response", method)
		}
		if !result.OK {
			return fmt.Errorf("%s: %w", method, SlackError(result.Error))
		}
		if out != nil {
			return json.Unmarshal(raw, out)
		}
		return nil
	}
	return fmt.Errorf("Slack request failed")
}

type Auth struct {
	TeamID string `json:"team_id"`
	UserID string `json:"user_id"`
	BotID  string `json:"bot_id"`
}

func (s *Slack) Auth(ctx context.Context) (Auth, error) {
	var a Auth
	e := s.API(ctx, "auth.test", map[string]any{}, false, &a)
	return a, e
}
func (s *Slack) Socket(ctx context.Context, receive func(Envelope) (string, error), react func(string), interaction ...func(AccessInteraction) error) error {
	delay := time.Second
	for ctx.Err() == nil {
		if e := s.socketOnce(ctx, receive, react, interaction...); e != nil && ctx.Err() == nil && s.Log != nil {
			s.Log.Warn("Socket Mode disconnected; reconnecting", "error", e)
		}
		if e := pause(ctx, delay); e != nil {
			return e
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
	return ctx.Err()
}
func (s *Slack) socketOnce(ctx context.Context, receive func(Envelope) (string, error), react func(string), interaction ...func(AccessInteraction) error) error {
	var response struct {
		URL string `json:"url"`
	}
	if e := s.API(ctx, "apps.connections.open", map[string]any{}, true, &response); e != nil {
		return e
	}
	if response.URL == "" {
		return fmt.Errorf("Slack did not return a Socket Mode endpoint")
	}
	conn, _, e := websocket.DefaultDialer.DialContext(ctx, response.URL, nil)
	if e != nil {
		return fmt.Errorf("Socket Mode connection failed")
	}
	defer conn.Close()
	conn.SetReadLimit(4 << 20)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	for {
		var message struct {
			Type       string          `json:"type"`
			EnvelopeID string          `json:"envelope_id"`
			Payload    json.RawMessage `json:"payload"`
		}
		if e = conn.ReadJSON(&message); e != nil {
			return fmt.Errorf("Socket Mode connection closed")
		}
		if message.Type == "hello" {
			if s.Log != nil {
				s.Log.Info("Arnold listening in Socket Mode")
			}
			continue
		}
		if message.Type == "disconnect" {
			return nil
		}
		receipt := ""
		if message.Type == "events_api" {
			var envelope Envelope
			if e = json.Unmarshal(message.Payload, &envelope); e != nil {
				return fmt.Errorf("invalid Slack event payload")
			}
			receipt, e = receive(envelope)
			if e != nil {
				return fmt.Errorf("durable Slack intake failed: %w", e)
			}
		}
		if message.Type == "interactive" && len(interaction) > 0 && interaction[0] != nil {
			var payload AccessInteraction
			if e = json.Unmarshal(message.Payload, &payload); e != nil {
				return fmt.Errorf("invalid Slack interactive payload")
			}
			if payload.Type == "block_actions" {
				if e = interaction[0](payload); e != nil {
					return fmt.Errorf("durable Slack interaction intake failed: %w", e)
				}
			}
		}
		if message.EnvelopeID != "" {
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if e = conn.WriteJSON(map[string]string{"envelope_id": message.EnvelopeID}); e != nil {
				return fmt.Errorf("Socket Mode acknowledgement failed")
			}
		}
		if receipt != "" {
			react(receipt)
		}
	}
}
func (s *Slack) Context(ctx context.Context, channel, ts, exclude string) (string, error) {
	parts := []string{}
	cursor := ""
	more := false
	excludeN, _ := strconv.ParseFloat(exclude, 64)
	for page := 0; page < 4; page++ {
		var r struct {
			Messages []struct {
				TS    string `json:"ts"`
				User  string `json:"user"`
				BotID string `json:"bot_id"`
				Text  string `json:"text"`
			} `json:"messages"`
			Metadata struct {
				Cursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		args := map[string]any{"channel": channel, "ts": ts, "limit": 100}
		if cursor != "" {
			args["cursor"] = cursor
		}
		if e := s.API(ctx, "conversations.replies", args, false, &r); e != nil {
			return "", e
		}
		for _, m := range r.Messages {
			n, _ := strconv.ParseFloat(m.TS, 64)
			if m.TS == "" || m.TS == exclude || n >= excludeN {
				continue
			}
			who := m.User
			if who == "" {
				who = m.BotID
			}
			if who == "" {
				who = "unknown"
			}
			parts = append(parts, fmt.Sprintf("%s [%s]: %s", m.TS, who, m.Text))
		}
		cursor = r.Metadata.Cursor
		more = cursor != ""
		if !more {
			break
		}
	}
	text := strings.Join(parts, "\n")
	units := utf16.Encode([]rune(text))
	if len(units) > 24000 {
		units = units[len(units)-24000:]
		more = true
	}
	text = string(utf16.Decode(units))
	if more {
		text = "[Context truncated]\n" + text
	}
	return text, nil
}
func (s *Slack) Post(ctx context.Context, channel, ts, text, id string) error {
	if len(id) < 32 {
		return fmt.Errorf("invalid delivery ID")
	}
	uuid := id[:8] + "-" + id[8:12] + "-4" + id[13:16] + "-8" + id[17:20] + "-" + id[20:32]
	return s.API(ctx, "chat.postMessage", map[string]any{"channel": channel, "thread_ts": ts, "text": text, "client_msg_id": uuid, "unfurl_links": false, "unfurl_media": false, "parse": "none"}, false, nil)
}
func (s *Slack) Reaction(ctx context.Context, channel, ts string, add bool) error {
	method := "remove"
	if add {
		method = "add"
	}
	e := s.API(ctx, "reactions."+method, map[string]any{"channel": channel, "timestamp": ts, "name": "eyes"}, false, nil)
	if (add && slackCode(e) == "already_reacted") || (!add && slackCode(e) == "no_reaction") {
		return nil
	}
	return e
}

type chunk map[string]any

func work(task, status, details string) chunk {
	if details != "" {
		details = "\n" + details
	}
	return chunk{"type": "task_update", "id": "paddock", "title": "Engineering task", "status": status, "details": "Paddock task " + task + details}
}
func (s *Slack) stop(ctx context.Context, channel, ts string, chunks []chunk, continuing bool) error {
	status := "active"
	if continuing {
		status = "processing"
	}
	e := s.API(ctx, "chat.stopStream", map[string]any{"channel": channel, "ts": ts, "chunks": chunks, "session_status": status}, false, nil)
	if slackCode(e) == "message_not_in_streaming_state" {
		return nil
	}
	return e
}
func (s *Slack) StreamEvent(ctx context.Context, o StreamEvent, store *Store) error {
	event := o.Event
	m, e := store.Stream(o.Task, event.TurnID)
	if e != nil {
		return e
	}
	if o.Recipient.UserID == "" {
		answer := event.Type == "message" && event.Phase != "commentary"
		failure := event.Type == "turn_finished" && event.State != "completed"
		if answer || failure {
			text := o.Text
			if failure {
				text = "The request " + event.State + ". Please try again."
			}
			if e = s.Post(ctx, o.Channel, o.ThreadTS, text, jsonHash([]any{"output", o.Task, event.ID, o.Chunk})); e != nil {
				return e
			}
		}
		return store.RecordOutput(o.Task, event.ID, o.Chunk)
	}
	if m != nil && m.Finished {
		return fmt.Errorf("cannot append output to a finished Slack turn")
	}
	if event.Type == "turn_finished" && (m == nil || m.TS == "") && event.State == "completed" {
		return store.CommitStream(o.Task, event.TurnID, Stream{Finished: true}, event.ID, o.Chunk)
	}
	markdown := event.Type == "message" && event.Phase != "commentary"
	length := len(utf16.Encode([]rune(o.Text)))
	if m != nil && m.TS != "" && (time.Now().UnixMilli()-m.Started >= 9*60_000 || (markdown && m.Characters+length > 10000) || m.Tasks >= 48) {
		if e = s.stop(ctx, o.Channel, m.TS, []chunk{work(o.Task, "complete", "Continuing in the next message")}, true); e != nil {
			return e
		}
		m = &Stream{}
		if e = store.SaveStream(o.Task, event.TurnID, *m); e != nil {
			return e
		}
	}
	if m == nil || m.TS == "" {
		status, e := store.Status(o.Task)
		if e != nil {
			return e
		}
		var started struct {
			TS string `json:"ts"`
		}
		e = s.API(ctx, "chat.startStream", map[string]any{"channel": o.Channel, "thread_ts": o.ThreadTS, "recipient_team_id": o.Recipient.TeamID, "recipient_user_id": o.Recipient.UserID, "task_display_mode": "plan", "chunks": []chunk{{"type": "plan_update", "title": "Working"}, work(o.Task, "in_progress", "Request received; execution isolated in OpenShell.\n"+status)}}, false, &started)
		if e != nil {
			return e
		}
		if started.TS == "" {
			return fmt.Errorf("Slack did not return a stream timestamp")
		}
		m = &Stream{TS: started.TS, Started: time.Now().UnixMilli(), Tasks: 1}
		if e = store.SaveStream(o.Task, event.TurnID, *m); e != nil {
			return e
		}
	}
	if event.Type == "turn_finished" {
		status := "complete"
		if event.State != "completed" {
			status = "error"
		}
		chunks := []chunk{work(o.Task, status, "Turn "+event.TurnID+": "+event.State)}
		if event.State != "completed" {
			chunks = append(chunks, chunk{"type": "markdown_text", "text": "The request " + event.State + ". Please try again."})
		}
		if e = s.stop(ctx, o.Channel, m.TS, chunks, false); e != nil {
			return e
		}
		m.Finished = true
	} else {
		var update chunk
		if markdown {
			update = chunk{"type": "markdown_text", "text": o.Text}
		} else {
			title := []rune(o.Text)
			if len(title) > 160 {
				title = title[:160]
			}
			update = chunk{"type": "task_update", "id": Hash(fmt.Sprintf("%s:%d", event.ID, o.Chunk))[:32], "title": string(title), "details": o.Text, "status": "complete"}
		}
		e = s.API(ctx, "chat.appendStream", map[string]any{"channel": o.Channel, "ts": m.TS, "chunks": []chunk{update}}, false, nil)
		if e != nil {
			if slackCode(e) == "message_not_in_streaming_state" {
				if e = store.SaveStream(o.Task, event.TurnID, Stream{}); e != nil {
					return e
				}
				return s.StreamEvent(ctx, o, store)
			}
			return e
		}
		if markdown {
			m.Characters += length
		} else {
			m.Tasks++
		}
	}
	return store.CommitStream(o.Task, event.TurnID, *m, event.ID, o.Chunk)
}
func (s *Slack) StreamStatus(ctx context.Context, task, channel, status string, store *Store) error {
	streams, e := store.ActiveStreams(task)
	if e != nil {
		return e
	}
	for _, m := range streams {
		e = s.API(ctx, "chat.appendStream", map[string]any{"channel": channel, "ts": m.TS, "chunks": []chunk{work(task, "in_progress", status)}}, false, nil)
		if e != nil {
			if slackCode(e) != "message_not_in_streaming_state" {
				return e
			}
			if e = store.SaveStream(task, m.Turn, Stream{}); e != nil {
				return e
			}
		}
	}
	return nil
}
