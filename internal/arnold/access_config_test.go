package arnold

import (
	"strings"
	"testing"
)

func TestAccessApproversDoNotInjectRuntimeInstructionsOrReplayHistory(t *testing.T) {
	f := newFixture(t)
	f.a.Config.AccessApprovers = []string{"U1"}
	f.receive(incoming("1"))
	if err := f.a.Drain(testContext); err != nil {
		t.Fatal(err)
	}
	if len(f.p.runs) != 1 || strings.Contains(f.p.runs[0].Prompt, "/sandbox/") {
		t.Fatal("Slack adapter injected runtime-specific instructions")
	}
	f.receive(followup("2", "U1", "try the configured workflow"))
	if err := f.a.Drain(testContext); err != nil {
		t.Fatal(err)
	}
	if len(f.p.messages) != 1 {
		t.Fatal("follow-up did not continue existing task")
	}
	prompt := f.p.messages[0].Text
	if strings.Contains(prompt, "/sandbox/") || !strings.Contains(prompt, "try the configured workflow") || strings.Contains(prompt, "parent alert") || strings.Contains(prompt, "Earlier Slack messages") {
		t.Fatalf("follow-up injected runtime instructions, replayed history, or omitted new work: %s", prompt)
	}
}

func TestAccessApproversConfigurationRequiresExplicitUniqueMemberIDs(t *testing.T) {
	for name, ids := range map[string][]string{
		"duplicate":    {"U1", "U1"},
		"empty member": {""},
		"bot ID":       {"B123"},
		"channel ID":   {"C123"},
		"display name": {"nick"},
		"whitespace":   {" U1"},
		"lowercase":    {"u123"},
	} {
		t.Run(name, func(t *testing.T) {
			c := configFixture()
			c.AccessApprovers = ids
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "accessApprovers") {
				t.Fatalf("invalid approvers accepted: %v", err)
			}
		})
	}
	for _, ids := range [][]string{nil, {}, {"U1"}, {"U1", "W2"}} {
		c := configFixture()
		c.AccessApprovers = ids
		if err := c.Validate(); err != nil {
			t.Fatalf("valid approvers %v rejected: %v", ids, err)
		}
	}
}

func TestAccessApproverAuthorityDoesNotInheritConversationRoles(t *testing.T) {
	for name, setup := range map[string]func(*Config, *AccessInteraction){
		"requester and mapped engineer with approvals disabled": func(c *Config, _ *AccessInteraction) { c.AccessApprovers = nil },
		"requester and mapped engineer outside allowlist":       func(c *Config, _ *AccessInteraction) { c.AccessApprovers = []string{"U9"} },
		"another mapped participant":                            func(c *Config, i *AccessInteraction) { c.AccessApprovers, i.User.ID = []string{"U9"}, "U2" },
		"automation recipient": func(c *Config, i *AccessInteraction) {
			c.AccessApprovers, c.Automation.RecipientUserID, i.User.ID = []string{"U9"}, "U8", "U8"
		},
		"allowed user in another workspace": func(c *Config, i *AccessInteraction) {
			c.AccessApprovers, i.User.ID, i.Team.ID = []string{"U9"}, "U9", "T2"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, ref, p := boundAccessStore(t)
			_, token := postedAccessCard(t, f.a.Store, ref, p)
			i := accessClick(token, "approve")
			setup(&f.a.Config, &i)
			if err := f.a.ReceiveAccessInteraction(i); err != nil {
				t.Fatal(err)
			}
			if actions, err := f.a.Store.pendingAccessActions(); err != nil || len(actions) != 0 {
				t.Fatalf("conversation role conferred approval: %+v %v", actions, err)
			}
			if len(f.p.runs) != 0 || len(f.p.messages) != 0 {
				t.Fatal("interaction intake invoked agent work")
			}
		})
	}
	t.Run("explicit unmapped approver", func(t *testing.T) {
		f, ref, p := boundAccessStore(t)
		_, token := postedAccessCard(t, f.a.Store, ref, p)
		f.a.Config.AccessApprovers = []string{"U9"}
		i := accessClick(token, "approve")
		i.User.ID = "U9"
		if err := f.a.ReceiveAccessInteraction(i); err != nil {
			t.Fatal(err)
		}
		actions, err := f.a.Store.pendingAccessActions()
		if err != nil || len(actions) != 1 || actions[0].Input.Actor.UserID != "U9" {
			t.Fatalf("explicit approver rejected: %+v %v", actions, err)
		}
		if len(f.p.runs) != 0 || len(f.p.messages) != 0 {
			t.Fatal("durable admission executed agent work synchronously")
		}
	})
}
