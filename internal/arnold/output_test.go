package arnold

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func public(id, text string, phase string) OutputEvent {
	return OutputEvent{ID: id, Type: "message", TurnID: "turn1", Phase: phase, Text: text}
}
func finished(state string) OutputEvent {
	return OutputEvent{ID: "finish", Type: "turn_finished", TurnID: "turn1", State: state}
}
func page(events []OutputEvent, next string, more bool) OutputPage {
	return OutputPage{TaskID: "task1", Events: events, NextCursor: next, HasMore: more}
}
func outputFixture(t *testing.T, pages map[string]OutputPage) (*agentFixture, func() error) {
	f := newFixture(t)
	f.p.output = func(id, after string) (OutputPage, error) {
		p, ok := pages[after]
		if !ok {
			return p, fmt.Errorf("unexpected cursor %s", after)
		}
		return p, nil
	}
	return f, func() error {
		return RelayOutput(testContext, "task1", "C1", "1", f.p, f.chat, f.a.Store, Recipient{TeamID: "T1", UserID: "U1"})
	}
}
func TestOutputMultipartPagesRestart(t *testing.T) {
	first := public("a", "first", "final_answer")
	first.Part = 1
	first.Parts = 2
	last := public("b", " second", "final_answer")
	last.Part = 2
	last.Parts = 2
	pages := map[string]OutputPage{"": page([]OutputEvent{public("early", "Earlier update", "commentary"), first}, "one", true)}
	f, run := outputFixture(t, pages)
	if e := run(); e == nil {
		t.Fatal("missing page should fail")
	}
	if len(f.chat.events) != 1 {
		t.Fatal("fragment should not be posted")
	}
	if c, _ := f.a.Store.Cursor("task1"); c != "" {
		t.Fatal("partial message skipped")
	}
	f.restart()
	pages["one"] = page([]OutputEvent{last, finished("completed")}, "end", false)
	if e := run(); e != nil {
		t.Fatal(e)
	}
	if len(f.chat.events) != 3 || f.chat.events[1].Text != "first second" {
		t.Fatal("lost or duplicated multipart message")
	}
	if c, _ := f.a.Store.Cursor("task1"); c != "end" {
		t.Fatal("cursor not committed")
	}
}
func TestOutputFailureMidPageAndLongAnswer(t *testing.T) {
	text := strings.Repeat("ø🦖", 20000)
	pages := map[string]OutputPage{"": page([]OutputEvent{public("answer", text, "final_answer"), finished("completed")}, "end", false)}
	f, run := outputFixture(t, pages)
	attempts := 0
	f.chat.beforeStream = func(o StreamEvent) {
		attempts++
		if attempts == 3 {
			f.chat.streamErr = errors.New("offline")
		}
	}
	if e := run(); e == nil {
		t.Fatal("expected offline")
	}
	if c, _ := f.a.Store.Cursor("task1"); c != "" {
		t.Fatal("advanced cursor on failed delivery")
	}
	f.restart()
	f.chat.beforeStream = nil
	f.chat.streamErr = nil
	if e := run(); e != nil {
		t.Fatal(e)
	}
	var answer strings.Builder
	seen := map[int]bool{}
	for _, o := range f.chat.events {
		if o.Event.Type != "message" {
			continue
		}
		if seen[o.Chunk] {
			t.Fatal("duplicate chunk")
		}
		seen[o.Chunk] = true
		answer.WriteString(o.Text)
	}
	if answer.String() != text {
		t.Fatal("lost Unicode text")
	}
}
func TestOutputInvalidPagesAndCursorRetention(t *testing.T) {
	broken := []OutputPage{page([]OutputEvent{public("x", "x", "commentary")}, "", false), page([]OutputEvent{}, "", true), {TaskID: "other", Events: []OutputEvent{}}, page([]OutputEvent{{ID: "private", TurnID: "turn1", Type: "reasoning", Text: "private"}}, "end", false), page([]OutputEvent{{ID: "a", TurnID: "turn1", Type: "message", Phase: "final_answer", Text: "x", Part: 2, Parts: 2}}, "end", false), page([]OutputEvent{{ID: "a", TurnID: "turn1", Type: "message", Phase: "final_answer", Text: "x", Part: 1, Parts: 2}}, "end", false)}
	for i, p := range broken {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			f, run := outputFixture(t, map[string]OutputPage{"": p})
			if e := run(); e == nil {
				t.Fatal("invalid page accepted")
			}
			if c, _ := f.a.Store.Cursor("task1"); c != "" {
				t.Fatal("cursor advanced")
			}
			if len(f.chat.events) != 0 {
				t.Fatal("invalid data delivered")
			}
		})
	}
	f, run := outputFixture(t, map[string]OutputPage{})
	f.a.Store.SaveCursor("task1", "expired")
	if e := run(); e == nil {
		t.Fatal("expired cursor replayed")
	}
	if c, _ := f.a.Store.Cursor("task1"); c != "expired" {
		t.Fatal("cursor reset")
	}
}
func TestOutputEmptyPageAndTurnStates(t *testing.T) {
	f, run := outputFixture(t, map[string]OutputPage{"": page([]OutputEvent{}, "", false)})
	if e := run(); e != nil {
		t.Fatal(e)
	}
	if len(f.chat.events) != 0 {
		t.Fatal("empty page implied output")
	}
	for _, state := range []string{"completed", "failed", "interrupted"} {
		t.Run(state, func(t *testing.T) {
			f, run := outputFixture(t, map[string]OutputPage{"": page([]OutputEvent{finished(state)}, "end", false)})
			if e := run(); e != nil {
				t.Fatal(e)
			}
			if len(f.chat.events) != 1 || f.chat.events[0].Event.State != state {
				t.Fatal("turn state not delivered")
			}
		})
	}
}
