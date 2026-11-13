package arnold

import (
	"context"
	"fmt"
)

func Chunks(text string, size int) []string {
	if size < 1 {
		panic("chunk size must be positive")
	}
	r := []rune(text)
	var out []string
	for len(r) > 0 {
		n := min(size, len(r))
		out = append(out, string(r[:n]))
		r = r[n:]
	}
	return out
}
func RelayOutput(ctx context.Context, task, channel, thread string, p PaddockClient, chat Chat, store *Store, recipient Recipient) error {
	after, e := store.Cursor(task)
	if e != nil {
		return e
	}
	seen := map[string]bool{after: true}
	var partial *OutputEvent
	var text string
	parts, nextPart := 0, 0
	send := func(event OutputEvent, text string) error {
		for i, c := range Chunks(text, 3500) {
			delivered, e := store.Delivered(task, event.ID, i)
			if e != nil {
				return e
			}
			if delivered {
				continue
			}
			if e = chat.StreamEvent(ctx, StreamEvent{Task: task, Channel: channel, ThreadTS: thread, Text: c, Event: event, Chunk: i, Recipient: recipient}, store); e != nil {
				return e
			}
		}
		return nil
	}
	for {
		page, e := p.Output(ctx, task, after)
		if e != nil {
			return e
		}
		if page.TaskID != task || page.Events == nil {
			return fmt.Errorf("invalid Paddock output page")
		}
		if len(page.Events) > 0 && (page.NextCursor == "" || seen[page.NextCursor]) {
			return fmt.Errorf("Paddock output cursor did not advance")
		}
		if len(page.Events) == 0 && (page.HasMore || page.NextCursor != after) {
			return fmt.Errorf("invalid empty Paddock output page")
		}
		for _, event := range page.Events {
			if event.ID == "" || event.TurnID == "" {
				return fmt.Errorf("Paddock output event has no stable ID or turn")
			}
			switch event.Type {
			case "message":
				if (event.Phase != "commentary" && event.Phase != "partial_answer" && event.Phase != "final_answer") || event.Text == "" {
					return fmt.Errorf("invalid public Paddock message")
				}
				part, total := event.Part, event.Parts
				if part == 0 {
					part = 1
				}
				if total == 0 {
					total = 1
				}
				if part < 1 || total < part {
					return fmt.Errorf("invalid Paddock message parts")
				}
				if partial == nil {
					if part != 1 {
						return fmt.Errorf("missing first Paddock message part")
					}
					copy := event
					partial = &copy
					parts = total
					nextPart = 1
					text = ""
				}
				if part != nextPart || total != parts || event.TurnID != partial.TurnID || event.Phase != partial.Phase {
					return fmt.Errorf("Paddock message parts are not contiguous")
				}
				text += event.Text
				nextPart++
				if part == total {
					if e = send(*partial, text); e != nil {
						return e
					}
					partial = nil
					text = ""
				}
			case "turn_finished":
				if partial != nil || (event.State != "completed" && event.State != "failed" && event.State != "interrupted") {
					return fmt.Errorf("invalid Paddock turn completion")
				}
				delivered, e := store.Delivered(task, event.ID, 0)
				if e != nil {
					return e
				}
				if !delivered {
					if e = chat.StreamEvent(ctx, StreamEvent{Task: task, Channel: channel, ThreadTS: thread, Event: event, Recipient: recipient}, store); e != nil {
						return e
					}
				}
				if e = store.TurnFinished(task, event.TurnID); e != nil {
					return e
				}
			default:
				return fmt.Errorf("unknown Paddock output event type")
			}
		}
		if partial == nil {
			if e = store.SaveCursor(task, page.NextCursor); e != nil {
				return e
			}
		}
		after = page.NextCursor
		seen[after] = true
		if !page.HasMore {
			if partial != nil {
				return fmt.Errorf("Paddock output ended before the message was complete")
			}
			return nil
		}
	}
}
