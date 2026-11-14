package arnold

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestIdleTTLConfiguration(t *testing.T) {
	for _, value := range []string{"", "30m", "24h", "90m", "1h30m", "1ns"} {
		c := configFixture()
		c.IdleTTL = value
		data, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadConfig(path)
		if err != nil || loaded.IdleTTL != value {
			t.Fatalf("load TTL %q: %+v, %v", value, loaded, err)
		}
	}
	for _, value := range []string{"0", "0s", "-1h", "24", "1d", "forever", " 24h", "999999999h"} {
		c := configFixture()
		c.IdleTTL = value
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "idleTtl") {
			t.Fatalf("invalid TTL %q: %v", value, err)
		}
	}
}

func TestPaddockIdleTTLCLIContract(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		for value, want := range map[string]string{"": "", "30m": "30m0s", "24h": "24h0m0s", "90m": "1h30m0s"} {
			t.Run(agent+"/"+value, func(t *testing.T) {
				p, path := helperPaddock(t)
				task, err := p.Run(testContext, RunOptions{Prompt: "Literal $(echo nope) and `pwd`", Repo: "project", Actor: "U1", Identity: "engineer", Profile: "implementation", Agent: agent, IdleTTL: value, ExternalID: "stable", Context: map[string]string{"channel": "C1"}})
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var args []string
				if err := json.Unmarshal(data, &args); err != nil {
					t.Fatal(err)
				}
				index := slices.Index(args, "--idle-ttl")
				if want == "" {
					if index != -1 {
						t.Fatal("omitted TTL must not add a CLI option")
					}
				} else if index == -1 || index+1 >= len(args) || args[index+1] != want {
					t.Fatalf("expected canonical TTL %q in %v", want, args)
				}
				if task.IdleTTL != want {
					t.Fatalf("TTL response %q, want %q", task.IdleTTL, want)
				}
			})
		}
	}
	p, path := helperPaddock(t)
	if _, err := p.Run(testContext, RunOptions{IdleTTL: "-1h"}); err == nil {
		t.Fatal("invalid TTL reached Paddock")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid TTL invoked subprocess: %v", err)
	}
}

func TestIdleTTLDispatchRetryKeepsFrozenPolicy(t *testing.T) {
	for _, initial := range []string{"", "30m", "24h"} {
		for _, changed := range []string{"", "1h"} {
			t.Run(initial+"/"+changed, func(t *testing.T) {
				f := newFixture(t)
				f.a.Config.IdleTTL = initial
				f.p.runErr = errors.New("dispatch outcome unavailable")
				id := f.receive(incoming("1"))
				f.drain()
				rows, err := f.a.Store.Pending()
				if err != nil || len(rows) != 1 || rows[0].Payload.Prepared == nil {
					t.Fatalf("missing frozen dispatch: %+v, %v", rows, err)
				}
				prepared := *rows[0].Payload.Prepared
				want := "30m0s"
				if initial == "24h" {
					want = "24h0m0s"
				}
				if prepared.IdleTTL != want {
					t.Fatalf("saved TTL %q, want %q", prepared.IdleTTL, want)
				}
				f.restart()
				f.a.Config.IdleTTL = changed
				f.p.runErr = nil
				f.chat.contextErr = errors.New("must not refetch history")
				f.drain()
				if len(f.p.runs) != 1 || f.p.runs[0].IdleTTL != want || f.p.runs[0].ExternalID != id || f.p.runs[0].Prompt != prepared.Prompt {
					t.Fatalf("retry changed dispatch: %+v", f.p.runs)
				}
				// A separate task uses the latest policy, while the old task keeps its own.
				f.chat.contextErr = nil
				f.receive(incoming("3"))
				f.drain()
				next := "30m0s"
				if changed != "" {
					next = "1h0m0s"
				}
				if len(f.p.runs) != 2 || f.p.runs[1].IdleTTL != next {
					t.Fatalf("new task did not use current policy: %+v", f.p.runs)
				}
			})
		}
	}
}

func TestDefaultIdleTTLDispatch(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			f := newFixture(t)
			f.a.Config.Agent = agent
			f.receive(incoming("1"))
			f.drain()
			e := incoming("2")
			e.Event.BotID, e.Event.Subtype, e.Event.Text = "B2", "bot_message", "Production 500"
			f.receive(e)
			f.drain()
			if len(f.p.runs) != 2 {
				t.Fatalf("expected human and automation dispatches: %+v", f.p.runs)
			}
			for _, run := range f.p.runs {
				if run.Agent != agent || run.IdleTTL != "30m0s" {
					t.Fatalf("dispatch lost default TTL: %+v", run)
				}
			}
		})
	}
}

func TestLegacyDispatchRetryPreservesOmittedTTL(t *testing.T) {
	f := newFixture(t)
	f.p.runErr = errors.New("dispatch outcome unavailable")
	id := f.receive(incoming("1"))
	f.drain()
	rows, err := f.a.Store.Pending()
	if err != nil || len(rows) != 1 {
		t.Fatalf("missing pending dispatch: %+v, %v", rows, err)
	}
	// Simulate a dispatch prepared before Arnold defaulted new tasks to 30m.
	payload := *rows[0].Payload
	payload.Prepared.IdleTTL = ""
	if err := f.a.Store.Prepare(id, payload); err != nil {
		t.Fatal(err)
	}
	f.restart()
	f.p.runErr = nil
	f.drain()
	if len(f.p.runs) != 1 || f.p.runs[0].IdleTTL != "" || f.p.runs[0].ExternalID != id || f.p.runs[0].Prompt != payload.Prepared.Prompt {
		t.Fatalf("legacy retry changed dispatch: %+v", f.p.runs)
	}
}

func TestIdleTTLAutomationDispatch(t *testing.T) {
	f := newFixture(t)
	f.a.Config.IdleTTL = "90m"
	e := incoming("1")
	e.Event.BotID, e.Event.Subtype, e.Event.Text = "B2", "bot_message", "Production 500"
	f.receive(e)
	f.drain()
	if len(f.p.runs) != 1 || f.p.runs[0].IdleTTL != "1h30m0s" || f.p.runs[0].Identity != "automation" {
		t.Fatalf("automation lost TTL or identity: %+v", f.p.runs)
	}
}

func TestStoppedTTLTaskContinuesWithSameMessageReceipt(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			testStoppedTTLTaskContinuesWithSameMessageReceipt(t, agent)
		})
	}
}

func testStoppedTTLTaskContinuesWithSameMessageReceipt(t *testing.T, agent string) {
	t.Helper()
	f := newFixture(t)
	f.a.Config.Agent = agent
	f.a.Config.IdleTTL = "30m"
	f.receive(incoming("1"))
	f.drain()
	task := f.p.tasks["task1"]
	task.Status, task.Terminal.State = "stopped", "missing"
	task.LastTurn.ID, task.LastTurn.State = "turn1", "completed"
	f.p.tasks[task.ID] = task
	f.restart()
	f.a.Config.IdleTTL = "1h"
	f.chat.contextErr = errors.New("continuation must not refetch history")
	f.p.messageStatus = "pending"
	id := f.receive(followup("2", "U1", "continue after expiry"))
	f.drain()
	f.restart()
	f.p.messageStatus = "delivered"
	f.drain()
	if len(f.p.runs) != 1 || len(f.p.messages) != 2 || f.chat.contextCalls != 1 {
		t.Fatal("stopped task was replaced or continuation duplicated context")
	}
	for _, message := range f.p.messages {
		if message.Task != task.ID || message.ID != id || message.Text != "Current message from U1:\ncontinue after expiry" {
			t.Fatalf("continuation changed: %+v", message)
		}
	}
}
