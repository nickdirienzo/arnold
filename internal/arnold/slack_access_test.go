package arnold

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func accessViewFixture() AccessCardView {
	return AccessCardView{
		CardID: "card-123", TaskID: "task-1", RequestID: "request-1", Kind: "network",
		Summary: "Download dependencies <@U123> & <!channel>",
		Details: []string{"registry.npmjs.org:443 from /usr/local/bin/node", "GET only"},
		Status:  "Awaiting operator review", Actionable: true, CanApprove: true, CanReject: true, Token: "opaque-token",
	}
}

func TestAccessCardRateLimitPreservesDefiniteRejectionAndCooldown(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
	}))
	defer server.Close()
	s := &Slack{BaseURL: server.URL + "/", HTTP: server.Client()}
	err := s.api(testContext, "chat.postMessage", map[string]any{}, false, nil, 1)
	var limited *slackHTTPError
	if !errors.As(err, &limited) || limited.Status != 429 || limited.RetryAfter != 30*time.Second || calls.Load() != 1 {
		t.Fatalf("rate limit became ambiguous or retried: %+v calls=%d err=%v", limited, calls.Load(), err)
	}
}

func TestAccessCardRenderingPostingAndUpdating(t *testing.T) {
	f := newSlackFixture(t)
	f.hook = func(method string, args map[string]any) (any, int) {
		if method != "chat.postMessage" && method != "chat.update" {
			t.Fatalf("unexpected API %s", method)
		}
		return map[string]any{"ok": true, "channel": "C1", "ts": "2.001"}, 200
	}
	v := accessViewFixture()
	for i := 0; i < 2; i++ {
		ts, err := f.s.PostAccessCard(testContext, "C1", "1.001", "stable-delivery", v)
		if err != nil || ts != "2.001" {
			t.Fatal(ts, err)
		}
	}
	if f.calls[0].args["client_msg_id"] != f.calls[1].args["client_msg_id"] {
		t.Fatal("delivery identity changed")
	}
	args := f.calls[0].args
	if args["thread_ts"] != "1.001" || args["mrkdwn"] != false || args["parse"] != "none" || args["link_names"] != false {
		t.Fatal("unsafe message options", args)
	}
	blocks := args["blocks"].([]any)
	if blocks[0].(map[string]any)["block_id"] != accessCardBlockPrefix+v.CardID {
		t.Fatal("missing stable card identity")
	}
	for _, raw := range blocks {
		block := raw.(map[string]any)
		if text, ok := block["text"].(map[string]any); ok && text["type"] != "plain_text" {
			t.Fatal("untrusted text uses markup")
		}
	}
	if blocks[1].(map[string]any)["text"].(map[string]any)["text"] != v.Summary {
		t.Fatal("plain text should preserve request content literally")
	}
	actions := blocks[len(blocks)-1].(map[string]any)["elements"].([]any)
	approve := actions[0].(map[string]any)
	if approve["value"] != v.Token || approve["action_id"] != "arnold_access_approve" || approve["text"].(map[string]any)["text"] != "Approve for this sandbox" {
		t.Fatal("incorrect network controls", approve)
	}
	v.Summary = "Network access still pending"
	if err := f.s.UpdateAccessCard(testContext, "C1", "2.001", v); err != nil {
		t.Fatal(err)
	}
	updated := f.calls[2].args
	if updated["ts"] != "2.001" || updated["thread_ts"] != nil {
		t.Fatal("update changed message association", updated)
	}
	blocks = updated["blocks"].([]any)
	actions = blocks[len(blocks)-1].(map[string]any)["elements"].([]any)
	if actions[0].(map[string]any)["text"].(map[string]any)["text"] != "Approve for this sandbox" {
		t.Fatal("network lifetime misrepresented")
	}
	v.Actionable, v.Status = false, "Approved; agent notification pending"
	if err := f.s.UpdateAccessCard(testContext, "C1", "2.001", v); err != nil {
		t.Fatal(err)
	}
	for _, block := range f.calls[3].args["blocks"].([]any) {
		if block.(map[string]any)["type"] == "actions" {
			t.Fatal("resolved card retained controls")
		}
	}
}

func TestAccessCardFailsRatherThanHidingScope(t *testing.T) {
	for name, mutate := range map[string]func(*AccessCardView){
		"long detail":      func(v *AccessCardView) { v.Details = []string{strings.Repeat("x", 3001)} },
		"too many details": func(v *AccessCardView) { v.Details = make([]string, 17) },
		"long summary":     func(v *AccessCardView) { v.Summary = strings.Repeat("x", 3001) },
		"unknown kind":     func(v *AccessCardView) { v.Kind = "new-kind" },
		"missing token":    func(v *AccessCardView) { v.Token = "" },
		"invalid marker":   func(v *AccessCardView) { v.CardID = "<@U123>" },
	} {
		t.Run(name, func(t *testing.T) {
			v := accessViewFixture()
			mutate(&v)
			if _, err := accessCardBlocks(v); err == nil {
				t.Fatal("invalid scope accepted")
			}
		})
	}
}

func TestAccessCardReviewLeadsWithStatusAndOmitsOnlyDuplicateMetadata(t *testing.T) {
	v := accessViewFixture()
	v.Summary = "Network access requested"
	rule := "Requested access:\n{\"host\":\"api.github.com\",\"port\":443,\"methods\":[\"GET\"]}"
	v.Details = []string{
		"Task: " + v.TaskID,
		"Request: " + v.RequestID,
		"Sandbox: pd-example (sandbox-123)",
		"Purpose: Download dependencies <@U123> & <!channel>",
		"Scope: this sandbox. This rule may permit later matching requests; it is not a single download.",
		rule,
		"Scope: GET requests only; executable /usr/local/bin/gh",
		"Validation: no new findings",
		"Evaluated candidate policy: abc123",
	}
	blocks, err := accessCardBlocks(v)
	if err != nil {
		t.Fatal(err)
	}
	if blocks[1]["text"].(map[string]any)["text"] != "Status: "+v.Status {
		t.Fatal("status was buried below the review")
	}
	var texts []string
	for _, block := range blocks {
		objects := []map[string]any{}
		if object, ok := block["text"].(map[string]any); ok {
			objects = append(objects, object)
		}
		if block["type"] == "context" {
			objects = append(objects, block["elements"].([]map[string]any)...)
		}
		for _, object := range objects {
			if object["type"] != "plain_text" {
				t.Fatal("review metadata enabled markup")
			}
			texts = append(texts, object["text"].(string))
		}
	}
	joined := strings.Join(texts, "\n")
	for _, once := range []string{v.Summary, "Task: " + v.TaskID, "Request: " + v.RequestID, "Matching requests remain allowed until the rule is removed."} {
		if strings.Count(joined, once) != 1 {
			t.Fatalf("expected one copy of %q: %s", once, joined)
		}
	}
	for _, detail := range v.Details[2:] {
		if strings.HasPrefix(detail, "Scope: this sandbox.") {
			continue // Replaced by the equivalent sandbox scope statement.
		}
		if !strings.Contains(joined, detail) {
			t.Fatalf("review content was lost: %q", detail)
		}
	}
	footer := blocks[len(blocks)-2] // The action buttons follow the footer.
	if footer["type"] != "context" || footer["elements"].([]map[string]any)[0]["text"] != "Task: "+v.TaskID+"\nRequest: "+v.RequestID {
		t.Fatal("task and request identity were not retained in the footer")
	}
	args, err := accessCardArgs("C1", v)
	if err != nil || args["text"] != v.Summary+"\nStatus: "+v.Status {
		t.Fatal("notification fallback did not describe the request and status", args, err)
	}
}

func TestAccessCardControlsRequireQualifiedActions(t *testing.T) {
	for _, test := range []struct {
		approve, reject bool
		want            string
	}{
		{true, false, "arnold_access_approve"},
		{false, true, "arnold_access_reject"},
		{false, false, ""},
	} {
		v := accessViewFixture()
		v.CanApprove, v.CanReject = test.approve, test.reject
		blocks, err := accessCardBlocks(v)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, block := range blocks {
			if block["type"] != "actions" {
				continue
			}
			for _, button := range block["elements"].([]map[string]any) {
				count++
				if button["action_id"] != test.want {
					t.Fatal("unqualified decision control", button)
				}
			}
		}
		if (test.want == "" && count != 0) || (test.want != "" && count != 1) {
			t.Fatal("wrong qualified control count", count)
		}
	}
}

func TestAccessCardPostDoesNotReplayUncertainResult(t *testing.T) {
	f := newSlackFixture(t)
	f.hook = func(string, map[string]any) (any, int) { return map[string]any{"ok": false}, 500 }
	if _, err := f.s.PostAccessCard(testContext, "C1", "1.001", "stable", accessViewFixture()); err == nil {
		t.Fatal("uncertain post accepted")
	}
	if len(f.calls) != 1 {
		t.Fatal("uncertain post was retried", len(f.calls))
	}
	f.hook = func(string, map[string]any) (any, int) {
		return map[string]any{"ok": true, "channel": "C-other", "ts": "2.001"}, 200
	}
	if _, err := f.s.PostAccessCard(testContext, "C1", "1.001", "stable", accessViewFixture()); err == nil {
		t.Fatal("mismatched message identity accepted")
	}
}

func cardMessage(ts, thread, user, marker string) map[string]any {
	return map[string]any{"ts": ts, "thread_ts": thread, "user": user, "bot_id": "B1", "blocks": []any{map[string]any{"type": "header", "block_id": marker}}}
}

func TestAccessCardLookupBindsBotThreadAndMarker(t *testing.T) {
	f := newSlackFixture(t)
	marker := accessCardBlockPrefix + "card-123"
	f.hook = func(method string, args map[string]any) (any, int) {
		if method != "conversations.replies" || args["channel"] != "C1" || args["ts"] != "1.001" {
			t.Fatal("wrong lookup scope", method, args)
		}
		if args["cursor"] == nil {
			return map[string]any{"ok": true, "messages": []any{
				cardMessage("2.001", "1.001", "U-human", marker),
				cardMessage("2.002", "other-thread", "U-bot", marker),
				cardMessage("2.003", "1.001", "U-bot", "different-card"),
			}, "has_more": true, "response_metadata": map[string]any{"next_cursor": "next"}}, 200
		}
		return map[string]any{"ok": true, "messages": []any{cardMessage("3.001", "1.001", "U-bot", marker)}}, 200
	}
	ts, err := f.s.FindAccessCard(testContext, "C1", "1.001", "card-123", "U-bot")
	if err != nil || ts != "3.001" || len(f.calls) != 2 {
		t.Fatal(ts, err, f.calls)
	}
}

func TestAccessCardLookupRejectsAmbiguousOrTruncatedHistory(t *testing.T) {
	for _, scenario := range []string{"duplicate", "has_more_without_cursor", "bounded", "repeated_cursor", "missing_messages"} {
		t.Run(scenario, func(t *testing.T) {
			f := newSlackFixture(t)
			f.hook = func(string, map[string]any) (any, int) {
				r := map[string]any{"ok": true, "messages": []any{cardMessage("2.001", "1.001", "U-bot", accessCardBlockPrefix+"card-123")}}
				switch scenario {
				case "duplicate":
					r["messages"] = append(r["messages"].([]any), cardMessage("2.002", "1.001", "U-bot", accessCardBlockPrefix+"card-123"))
				case "has_more_without_cursor":
					r["has_more"] = true
				case "bounded":
					r["response_metadata"] = map[string]any{"next_cursor": fmt.Sprint(len(f.calls))}
				case "repeated_cursor":
					r["response_metadata"] = map[string]any{"next_cursor": "same"}
				case "missing_messages":
					delete(r, "messages")
				}
				return r, 200
			}
			if _, err := f.s.FindAccessCard(testContext, "C1", "1.001", "card-123", "U-bot"); err == nil {
				t.Fatal("ambiguous lookup accepted")
			}
		})
	}
}

func TestSocketModeAccessInteractionDurableAck(t *testing.T) {
	for _, intakeFails := range []bool{false, true} {
		t.Run(fmt.Sprint(intakeFails), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(testContext, 5*time.Second)
			defer cancel()
			var accepted atomic.Bool
			var acknowledgements atomic.Int32
			upgrader := websocket.Upgrader{}
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/apps.connections.open" {
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(server.URL, "http") + "/socket"})
					return
				}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				conn.WriteJSON(map[string]any{"type": "interactive", "envelope_id": "interaction-1", "payload": map[string]any{
					"type": "block_actions", "team": map[string]string{"id": "T1"}, "user": map[string]string{"id": "U1"},
					"container": map[string]string{"channel_id": "C1", "message_ts": "2.001"},
					"actions":   []any{map[string]string{"action_id": "arnold_access_approve", "value": "opaque-token"}},
				}})
				var ack map[string]any
				if err = conn.ReadJSON(&ack); err == nil {
					acknowledgements.Add(1)
					if !accepted.Load() || ack["envelope_id"] != "interaction-1" {
						t.Error("acknowledged before durable interaction intake")
					}
					conn.WriteJSON(map[string]string{"type": "disconnect"})
				}
			}))
			defer server.Close()
			s := &Slack{AppToken: "app-sentinel", BaseURL: server.URL + "/", HTTP: server.Client()}
			err := s.socketOnce(ctx, func(Envelope) (string, error) {
				t.Error("interaction reached conversational handler")
				return "", nil
			}, func(string) { t.Error("interaction caused a message reaction") }, func(v AccessInteraction) error {
				if v.Team.ID != "T1" || v.User.ID != "U1" || v.Container.ChannelID != "C1" || v.Container.MessageTS != "2.001" || len(v.Actions) != 1 || v.Actions[0].Value != "opaque-token" {
					t.Error("lost interaction identity", v)
				}
				if intakeFails {
					return errors.New("durable write failed")
				}
				accepted.Store(true)
				return nil
			})
			if intakeFails {
				if err == nil || !strings.Contains(err.Error(), "durable Slack interaction intake failed") || acknowledgements.Load() != 0 {
					t.Fatal("failed intake was acknowledged", err)
				}
			} else if err != nil || acknowledgements.Load() != 1 {
				t.Fatal("successful interaction was not acknowledged", err)
			}
		})
	}
}
