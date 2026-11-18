package arnold

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRequireMentionIgnoresUntaggedMessages(t *testing.T) {
	for _, state := range []string{"inactive", "pending", "active", "restarted"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t)
			enableSubscriptions(f, true)
			f.a.Config.RequireMention = true
			// Even an explicitly matching human trigger must require a tag.
			f.a.Config.Triggers = []Trigger{{ID: "human", Channel: "C1", UserID: "U1", Contains: "detail"}}
			if state != "inactive" {
				if f.receive(incoming("1")) == "" {
					t.Fatal("tagged root was ignored")
				}
			}
			if state == "active" || state == "restarted" {
				f.drain()
			}
			if state == "restarted" {
				f.restart()
			}
			for _, thread := range []string{"", "1"} {
				e := incoming("2")
				e.Event.ThreadTS, e.Event.Text = thread, "more detail"
				if f.receive(e) != "" {
					t.Fatal("untagged human message was accepted")
				}
				e.Event.Text = "<@U2> more detail"
				if f.receive(e) != "" {
					t.Fatal("tagging another user activated Arnold")
				}
			}
			e := followup("3", "U1", "<@UA> more detail")
			if f.receive(e) == "" {
				t.Fatal("tagged follow-up was ignored")
			}
			f.drain()
			wantMessages := 1
			if state == "inactive" {
				wantMessages = 0
			}
			if len(f.p.runs) != 1 || len(f.p.messages) != wantMessages {
				t.Fatal("tagged messages did not start or resume the conversation")
			}
		})
	}
}

func TestRequireMentionWithSelectedBotSources(t *testing.T) {
	for _, source := range []string{"webhook", "pagerduty", "other", "own", "human", "app-only", "wrong-channel", "wrong-team", "edited", "hidden", "filter-mismatch"} {
		for _, thread := range []string{"", "1"} {
			t.Run(source+"/thread="+thread, func(t *testing.T) {
				f := newFixture(t)
				f.a.Config.RequireMention = true
				f.a.Config.Triggers = []Trigger{
					{ID: "webhook", Channel: "C1", BotID: "B-webhook"},
					{ID: "pagerduty", Channel: "C1", BotID: "B-pagerduty"},
				}
				// An active thread must not admit unrelated sources.
				f.receive(incoming("1"))
				f.drain()
				e := incoming("2")
				e.Event.User, e.Event.BotID, e.Event.Subtype = "", "B-"+source, "bot_message"
				e.Event.ThreadTS, e.Event.Text = thread, "alert without a tag"
				switch source {
				case "own":
					e.Event.BotID = "BA"
				case "human":
					e.Event.User, e.Event.BotID, e.Event.Subtype = "U1", "", ""
				case "app-only":
					e.Event.BotID, e.Event.AppID = "", "A-webhook"
				case "wrong-channel":
					f.a.Config.DefaultRepo = "project"
					e.Event.Channel, e.Event.BotID = "C2", "B-webhook"
				case "wrong-team":
					e.TeamID, e.Event.BotID = "T2", "B-webhook"
				case "edited":
					e.Event.BotID, e.Event.Subtype = "B-webhook", "message_changed"
				case "hidden":
					e.Event.BotID, e.Event.Hidden = "B-webhook", true
				case "filter-mismatch":
					e.Event.BotID = "B-webhook"
					f.a.Config.Triggers[0].Contains = "500"
				}
				id := f.receive(e)
				accepted := source == "webhook" || source == "pagerduty"
				if (id != "") != accepted {
					t.Fatal("source allowlist changed")
				}
				if accepted {
					if f.receive(e) != "" {
						t.Fatal("duplicate alert was accepted")
					}
					f.drain()
					if len(f.p.runs) != 2 || f.p.runs[1].Actor != "automation:"+source || f.p.runs[1].Identity != "automation" {
						t.Fatal("alert did not use its automation identity")
					}
				}
			})
		}
	}
}

func TestSourceOnlyTriggerConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"teamId":"T1","requireMention":true,"channels":{"C1":{"repo":"project"}},"users":{},"automation":{"identity":"automation","profile":"investigation"},"triggers":[{"id":"webhook","channel":"C1","botId":"B1"}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil || !c.RequireMention {
		t.Fatal(c, err)
	}
	c.Triggers[0].BotID, c.Triggers[0].UserID = "", "U1"
	if c.Validate() == nil {
		t.Fatal("human trigger accepted without a text filter")
	}
}
