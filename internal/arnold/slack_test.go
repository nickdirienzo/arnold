package arnold

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type apiCall struct {
	method string
	args   map[string]any
}
type slackFixture struct {
	s      *Slack
	calls  []apiCall
	mu     sync.Mutex
	hook   func(string, map[string]any) (any, int)
	serial int
}

func newSlackFixture(t *testing.T) *slackFixture {
	t.Helper()
	f := &slackFixture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args map[string]any
		method := strings.TrimPrefix(r.URL.Path, "/")
		if method == "conversations.replies" {
			if r.Method != http.MethodGet || r.ContentLength > 0 {
				t.Error("thread history must use GET query parameters, without a JSON body")
			}
			if r.Header.Get("Authorization") != "Bearer bot-sentinel" || r.URL.Query().Has("token") {
				t.Error("thread history credentials must stay in the authorization header")
			}
			args = map[string]any{}
			for key, values := range r.URL.Query() {
				args[key] = values[0]
			}
		} else if e := json.NewDecoder(r.Body).Decode(&args); e != nil {
			t.Error(e)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, apiCall{method, args})
		var response any = map[string]any{"ok": true}
		status := 200
		if f.hook != nil {
			response, status = f.hook(method, args)
		}
		if response == nil {
			response = map[string]any{"ok": true}
			if method == "chat.startStream" {
				f.serial++
				response = map[string]any{"ok": true, "ts": fmt.Sprint(f.serial)}
			}
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)
	f.s = &Slack{BotToken: "bot-sentinel", AppToken: "app-sentinel", BaseURL: server.URL + "/", HTTP: server.Client()}
	f.hook = func(string, map[string]any) (any, int) { return nil, 200 }
	return f
}
func getChunks(c apiCall) []any { return c.args["chunks"].([]any) }
func TestAssistantStreamGroupingStatusAndRestart(t *testing.T) {
	f := newFixture(t)
	api := newSlackFixture(t)
	send := func(e OutputEvent) error {
		return api.s.StreamEvent(testContext, StreamEvent{Task: "task1", Channel: "C1", ThreadTS: "1", Text: e.Text, Event: e, Recipient: Recipient{TeamID: "T1", UserID: "U1"}}, f.a.Store)
	}
	f.a.Store.Observed("task1", "running; access=clear")
	if e := send(public("progress", "Checking logs", "commentary")); e != nil {
		t.Fatal(e)
	}
	meta, e := f.a.Store.Stream("task1", "turn1")
	if e != nil || meta.TS == "" {
		t.Fatal("missing stream metadata")
	}
	f.restart()
	if e = api.s.StreamStatus(testContext, "task1", "C1", "access=pending review", f.a.Store); e != nil {
		t.Fatal(e)
	}
	if e = send(public("answer", "Found it", "final_answer")); e != nil {
		t.Fatal(e)
	}
	if e = send(finished("completed")); e != nil {
		t.Fatal(e)
	}
	if len(api.calls) != 5 {
		t.Fatal("unexpected calls", api.calls)
	}
	start := api.calls[0]
	if start.args["task_display_mode"] != "plan" || start.args["recipient_user_id"] != "U1" {
		t.Fatal("assistant presentation")
	}
	if getChunks(api.calls[1])[0].(map[string]any)["type"] != "task_update" {
		t.Fatal("progress outside thinking block")
	}
	if getChunks(api.calls[3])[0].(map[string]any)["type"] != "markdown_text" {
		t.Fatal("answer not markdown")
	}
	for _, c := range api.calls {
		if c.method == "chat.postMessage" {
			t.Fatal("standalone debug noise")
		}
		if c.args["ts"] != nil && c.args["ts"] != meta.TS {
			t.Fatal("stream was not resumed")
		}
	}
	end, _ := f.a.Store.Stream("task1", "turn1")
	if !end.Finished {
		t.Fatal("stream not finalized")
	}
}
func TestStreamRetryRotationAndExpiry(t *testing.T) {
	f := newFixture(t)
	api := newSlackFixture(t)
	send := func(e OutputEvent, chunk int) error {
		return api.s.StreamEvent(testContext, StreamEvent{Task: "task1", Channel: "C1", ThreadTS: "1", Text: e.Text, Event: e, Chunk: chunk, Recipient: Recipient{TeamID: "T1", UserID: "U1"}}, f.a.Store)
	}
	fail := true
	api.hook = func(method string, args map[string]any) (any, int) {
		if method == "chat.appendStream" && fail {
			fail = false
			return map[string]any{"ok": false, "error": "temporary_error"}, 200
		}
		return nil, 200
	}
	e := send(public("answer", "Result", "final_answer"), 0)
	if e == nil {
		t.Fatal("append should fail")
	}
	f.restart()
	if e = send(public("answer", "Result", "final_answer"), 0); e != nil {
		t.Fatal(e)
	}
	starts := 0
	for _, c := range api.calls {
		if c.method == "chat.startStream" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatal("retry created another stream")
	}
	m, _ := f.a.Store.Stream("task1", "turn1")
	m.Started = time.Now().Add(-10 * time.Minute).UnixMilli()
	f.a.Store.SaveStream("task1", "turn1", *m)
	if e = send(public("progress", "Continuing", "commentary"), 0); e != nil {
		t.Fatal(e)
	}
	expired := true
	api.hook = func(method string, args map[string]any) (any, int) {
		if method == "chat.appendStream" && expired {
			expired = false
			return map[string]any{"ok": false, "error": "message_not_in_streaming_state"}, 200
		}
		return nil, 200
	}
	if e = send(public("more", "More", "commentary"), 0); e != nil {
		t.Fatal(e)
	}
	if e = send(finished("failed"), 0); e != nil {
		t.Fatal(e)
	}
	last := api.calls[len(api.calls)-1]
	if last.method != "chat.stopStream" || getChunks(last)[0].(map[string]any)["status"] != "error" {
		t.Fatal("failed turn not visible")
	}
}
func TestStreamLongAnswersAndAutomaticFallback(t *testing.T) {
	f := newFixture(t)
	api := newSlackFixture(t)
	text := strings.Repeat("🦖", 3500)
	for i := 0; i < 4; i++ {
		if e := api.s.StreamEvent(testContext, StreamEvent{Task: "task1", Channel: "C1", ThreadTS: "1", Text: text, Event: public("answer", text, "final_answer"), Chunk: i, Recipient: Recipient{TeamID: "T1", UserID: "U1"}}, f.a.Store); e != nil {
			t.Fatal(e)
		}
	}
	starts := 0
	var delivered strings.Builder
	for _, c := range api.calls {
		if c.method == "chat.startStream" {
			starts++
		}
		if chunks, ok := c.args["chunks"].([]any); ok {
			for _, ch := range chunks {
				m := ch.(map[string]any)
				if m["type"] == "markdown_text" {
					delivered.WriteString(m["text"].(string))
				}
			}
		}
	}
	if starts != 4 || delivered.String() != strings.Repeat(text, 4) {
		t.Fatal("stream rotation lost answer")
	}
	before := len(api.calls)
	for _, e := range []OutputEvent{public("c", "progress", "commentary"), public("a", "answer", "final_answer"), finished("completed")} {
		if err := api.s.StreamEvent(testContext, StreamEvent{Task: "automatic", Channel: "C1", ThreadTS: "1", Text: e.Text, Event: e}, f.a.Store); err != nil {
			t.Fatal(err)
		}
	}
	if len(api.calls) != before+1 || api.calls[before].method != "chat.postMessage" {
		t.Fatal("automation diagnostics leaked")
	}
}
func TestSlackContextPaginationAndPermissionErrors(t *testing.T) {
	f := newSlackFixture(t)
	count := 0
	f.hook = func(method string, args map[string]any) (any, int) {
		count++
		if method != "conversations.replies" || args["channel"] != "C1" || args["ts"] != "1" || args["limit"] != "100" || (count > 1 && args["cursor"] != "more") {
			t.Error("thread history query or pagination changed")
		}
		return map[string]any{"ok": true, "messages": []any{map[string]any{"ts": fmt.Sprint(count), "user": "U1", "text": strings.Repeat("x", 7000)}, map[string]any{"ts": "100", "text": "future"}}, "response_metadata": map[string]any{"next_cursor": "more"}}, 200
	}
	text, e := f.s.Context(testContext, "C1", "1", "10")
	if e != nil {
		t.Fatal(e)
	}
	if count != 4 || !strings.HasPrefix(text, "[Context truncated]") || strings.Contains(text, "future") || len(text) > 24100 {
		t.Fatal("context bounds")
	}
	f.hook = func(string, map[string]any) (any, int) {
		return map[string]any{"ok": false, "error": "missing_scope"}, 200
	}
	if _, e = f.s.Context(testContext, "C1", "1", "10"); slackCode(e) != "missing_scope" {
		t.Fatal("context error discarded")
	}
}
func TestReactionsAndRateLimitCancellation(t *testing.T) {
	f := newSlackFixture(t)
	f.hook = func(method string, args map[string]any) (any, int) {
		if args["timestamp"] != "123.456" || args["name"] != "eyes" {
			t.Error("wrong reaction target")
		}
		code := "no_reaction"
		if method == "reactions.add" {
			code = "already_reacted"
		}
		return map[string]any{"ok": false, "error": code}, 200
	}
	if e := f.s.Reaction(testContext, "C1", "123.456", true); e != nil {
		t.Fatal(e)
	}
	if e := f.s.Reaction(testContext, "C1", "123.456", false); e != nil {
		t.Fatal(e)
	}
	f.hook = func(string, map[string]any) (any, int) { return map[string]any{"ok": false}, 429 }
	ctx, cancel := context.WithTimeout(testContext, 25*time.Millisecond)
	defer cancel()
	if e := f.s.API(ctx, "auth.test", map[string]any{}, false, nil); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("rate-limit retry ignored cancellation", e)
	}
}
func TestSocketModeDurableAcknowledgementAndReconnect(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(testContext)
	defer cancel()
	upgrader := websocket.Upgrader{}
	connections, acks, reactions := 0, 0, 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apps.connections.open" {
			if r.Header.Get("Authorization") != "Bearer app-sentinel" {
				t.Error("wrong socket token")
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(server.URL, "http") + "/socket"})
			return
		}
		conn, e := upgrader.Upgrade(w, r, nil)
		if e != nil {
			return
		}
		defer conn.Close()
		connections++
		conn.WriteJSON(map[string]any{"type": "hello"})
		conn.WriteJSON(map[string]any{"type": "events_api", "envelope_id": "envelope", "payload": incoming("1")})
		var ack map[string]any
		if e = conn.ReadJSON(&ack); e != nil {
			return
		}
		pending, e := f.a.Store.Pending()
		if e != nil || len(pending) != 1 {
			t.Error("acknowledged before durable intake")
		}
		if ack["envelope_id"] != "envelope" {
			t.Error("wrong envelope ack")
		}
		acks++
		if connections == 2 {
			cancel()
			return
		}
		conn.WriteJSON(map[string]any{"type": "disconnect"})
	}))
	defer server.Close()
	s := &Slack{AppToken: "app-sentinel", BaseURL: server.URL + "/", HTTP: server.Client()}
	e := s.Socket(ctx, f.a.Receive, func(string) { reactions++ })
	if !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if connections != 2 || acks != 2 || reactions != 1 {
		t.Fatalf("socket dedup/reconnect: %d %d %d", connections, acks, reactions)
	}
}
