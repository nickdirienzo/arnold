package arnold

import "testing"

func enableSubscriptions(f *agentFixture, bots bool) {
	f.a.Config.Channels["C1"] = Route{Repo: "project", AutoSubscribe: true, AutoSubscribeBots: bots}
	f.a.Config.Automation.Profile = "implementation"
	f.a.Config.Triggers = nil
}

func TestChannelSubscriptionStartsThreadsAndResumesNewMessages(t *testing.T) {
	for _, bot := range []bool{false, true} {
		name := "human"
		if bot {
			name = "bot"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			enableSubscriptions(f, bot)
			root := incoming("1")
			root.Event.Text = "new incident"
			if bot {
				root.Event.User, root.Event.BotID, root.Event.Subtype = "", "B2", "bot_message"
			}
			id := f.receive(root)
			if id == "" || f.receive(root) != "" {
				t.Fatal("root was not accepted exactly once")
			}
			reply := root
			reply.Event.TS, reply.Event.ThreadTS, reply.Event.Text = "2", "1", "new detail"
			if f.receive(reply) == "" {
				t.Fatal("reply while root was pending was lost")
			}
			f.drain()
			if len(f.p.runs) != 1 || len(f.p.messages) != 1 || f.p.runs[0].Profile != "implementation" {
				t.Fatal("thread did not use one implementation conversation")
			}
			actor, identity := "U1", "engineer-a"
			if bot {
				actor, identity = "automation:channel:C1", "automation"
			}
			if f.p.runs[0].Actor != actor || f.p.runs[0].Identity != identity || f.p.messages[0].Text != "Current message from "+actor+":\nnew detail" {
				t.Fatal("authenticated routing or new-message-only continuation changed")
			}
			f.restart()
			f.chat.contextErr = SlackError("must_not_fetch")
			reply.Event.TS, reply.Event.Text = "3", "after restart"
			f.receive(reply)
			f.drain()
			if len(f.p.runs) != 1 || len(f.p.messages) != 2 || f.p.messages[1].Task != "task1" {
				t.Fatal("subscription did not resume after restart")
			}
			f.chat.contextErr = nil
			root.Event.TS = "4"
			f.receive(root)
			f.drain()
			if len(f.p.runs) != 2 {
				t.Fatal("independent root did not start its own conversation")
			}
		})
	}
}

func TestChannelSubscriptionKeepsBoundaries(t *testing.T) {
	for _, kind := range []string{"disabled", "other-channel", "inactive-reply", "bot-disabled", "own-user", "own-bot", "edited", "deleted", "hidden", "app-bot", "root-thread-ts"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			enableSubscriptions(f, true)
			e := incoming("1")
			e.Event.Text = "new incident"
			accepted := false
			switch kind {
			case "disabled":
				f.a.Config.Channels["C1"] = Route{Repo: "project"}
			case "other-channel":
				f.a.Config.Channels["C2"] = Route{Repo: "project"}
				e.Event.Channel = "C2"
			case "inactive-reply":
				e.Event.ThreadTS = "0"
			case "bot-disabled":
				enableSubscriptions(f, false)
				e.Event.BotID = "B2"
			case "own-user":
				e.Event.User = "UA"
			case "own-bot":
				e.Event.BotID = "BA"
			case "edited":
				e.Event.Subtype = "message_changed"
			case "deleted":
				e.Event.Subtype = "message_deleted"
			case "hidden":
				e.Event.Hidden = true
			case "app-bot":
				e.Event.AppID = "A2"
				accepted = true
			case "root-thread-ts":
				e.Event.ThreadTS = e.Event.TS
				accepted = true
			}
			if (f.receive(e) != "") != accepted {
				t.Fatal("channel subscription boundary changed")
			}
		})
	}
}

func TestBotSubscriptionConfiguration(t *testing.T) {
	for _, kind := range []string{"valid", "missing-automation", "missing-subscription", "invalid-profile"} {
		t.Run(kind, func(t *testing.T) {
			c := configFixture()
			c.Triggers = nil
			c.Channels["C1"] = Route{Repo: "project", AutoSubscribe: true, AutoSubscribeBots: true}
			c.Automation.Profile = "implementation"
			switch kind {
			case "missing-automation":
				c.Automation = nil
			case "missing-subscription":
				c.Channels["C1"] = Route{Repo: "project", AutoSubscribeBots: true}
			case "invalid-profile":
				c.Automation.Profile = "root"
			}
			if (c.Validate() == nil) != (kind == "valid") {
				t.Fatal("invalid bot subscription authorization")
			}
		})
	}
}
