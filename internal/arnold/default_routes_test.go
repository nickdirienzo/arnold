package arnold

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultRepoMentionInAlertThread(t *testing.T) {
	for _, channel := range []string{"C2", "G2"} {
		t.Run(channel, func(t *testing.T) {
			f := newFixture(t)
			f.a.Config.DefaultRepo = "default-project"
			mention := incoming("2")
			mention.Event.Channel, mention.Event.ThreadTS = channel, "1"
			mention.Event.Type, mention.Event.Text = "app_mention", "<@UA> can you investigate"
			if f.receive(mention) == "" {
				t.Fatal("mention in unlisted channel was ignored")
			}
			mention.Event.Type = "message"
			if f.receive(mention) != "" {
				t.Fatal("duplicate mention was accepted")
			}
			reply := mention
			reply.Event.TS, reply.Event.Text = "3", "more detail"
			if f.receive(reply) == "" {
				t.Fatal("reply while mention was pending was ignored")
			}
			f.drain()
			if len(f.p.runs) != 1 || len(f.p.messages) != 1 {
				t.Fatalf("expected one conversation: runs=%d messages=%d", len(f.p.runs), len(f.p.messages))
			}
			run := f.p.runs[0]
			if run.Repo != "default-project" || run.Actor != "U1" || run.Identity != "engineer-a" || !strings.Contains(run.Prompt, "parent alert") || !strings.Contains(run.Prompt, mention.Event.Text) {
				t.Fatalf("incorrect route, identity or alert context: %+v", run)
			}
			botReply := reply
			botReply.Event.TS, botReply.Event.BotID, botReply.Event.Subtype = "3.5", "B2", "bot_message"
			botReply.Event.Text = "<@UA> 500 alert"
			if f.receive(botReply) != "" {
				t.Fatal("bot continued a thread without explicit channel authorization")
			}
			f.restart()
			f.chat.contextErr = errors.New("must not refetch history")
			reply.Event.TS, reply.Event.Text = "4", "what did you find?"
			if f.receive(reply) == "" {
				t.Fatal("reply after restart was ignored")
			}
			f.drain()
			if len(f.p.runs) != 1 || len(f.p.messages) != 2 || f.p.messages[1].Task != "task1" || f.p.messages[1].Text != "Current message from U1:\nwhat did you find?" || f.chat.contextCalls != 1 {
				t.Fatal("reply did not resume the saved conversation with only the new message")
			}
		})
	}
}

func TestDefaultRepoRetryKeepsPreparedRepository(t *testing.T) {
	f := newFixture(t)
	f.a.Config.DefaultRepo = "original-project"
	f.p.runErr = errors.New("offline")
	e := incoming("1")
	e.Event.Channel = "C2"
	f.receive(e)
	f.drain()
	f.restart()
	f.a.Config.DefaultRepo = "changed-project"
	f.p.runErr = nil
	f.drain()
	if len(f.p.runs) != 1 || f.p.runs[0].Repo != "original-project" {
		t.Fatal("retry changed the prepared repository")
	}
}

func TestDefaultRepoKeepsRoutingBoundaries(t *testing.T) {
	for _, kind := range []string{"disabled", "other-workspace", "direct-message", "invalid-channel", "chatter", "inactive-reply", "bot-mention", "app-mention", "own-user", "own-bot", "edited", "hidden", "channel-override"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			enableSubscriptions(f, true)
			f.a.Config.DefaultRepo = "default-project"
			e := incoming("1")
			e.Event.Channel = "C2"
			switch kind {
			case "disabled":
				f.a.Config.DefaultRepo = ""
			case "other-workspace":
				e.TeamID = "T2"
			case "direct-message":
				e.Event.Channel = "D2"
			case "invalid-channel":
				e.Event.Channel = "invalid"
			case "chatter":
				e.Event.Text = "new incident"
			case "inactive-reply":
				e.Event.ThreadTS, e.Event.Text = "0", "more detail"
			case "bot-mention":
				e.Event.BotID, e.Event.Subtype = "B2", "bot_message"
			case "app-mention":
				e.Event.AppID = "A2"
			case "own-user":
				e.Event.User = "UA"
			case "own-bot":
				e.Event.BotID = "BA"
			case "edited":
				e.Event.Subtype = "message_changed"
			case "hidden":
				e.Event.Hidden = true
			case "channel-override":
				e.Event.Channel = "C1"
			}
			accepted := f.receive(e) != ""
			if accepted != (kind == "channel-override") {
				t.Fatalf("unexpected acceptance: %v", accepted)
			}
			f.drain()
			if kind == "channel-override" && (len(f.p.runs) != 1 || f.p.runs[0].Repo != "project") {
				t.Fatal("default repository overrode the channel repository")
			}
		})
	}
}

func TestDefaultRepoConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"teamId":"T1","defaultRepo":"project","channels":{},"users":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil || c.DefaultRepo != "project" {
		t.Fatal(c, err)
	}
	c.Automation = &Actor{Identity: "automation", Profile: "investigation"}
	c.Triggers = []Trigger{{ID: "monitor", Channel: "C2", BotID: "B2", Contains: "alert"}}
	if c.Validate() == nil {
		t.Fatal("default repository allowed an automatic trigger without an explicit channel")
	}
}
