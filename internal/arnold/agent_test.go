package arnold

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var testContext = context.Background()

func configFixture() Config {
	return Config{TeamID: "T1", Channels: map[string]Route{"C1": {Repo: "project"}}, Users: map[string]Actor{"U1": {Identity: "engineer-a", Profile: "implementation"}, "U2": {Identity: "engineer-b", Profile: "implementation"}}, Automation: &Actor{Identity: "automation", Profile: "investigation"}, Triggers: []Trigger{{ID: "monitor", Channel: "C1", BotID: "B2", Contains: "500"}}}
}

type sentMessage struct{ Task, Text, ID string }
type fakePaddock struct {
	runs          []RunOptions
	messages      []sentMessage
	tasks         map[string]Task
	runErr        error
	messageStatus string
	output        func(string, string) (OutputPage, error)
}

func (p *fakePaddock) Check(context.Context, Config) error { return nil }
func (p *fakePaddock) Run(_ context.Context, o RunOptions) (Task, error) {
	if p.runErr != nil {
		return Task{}, p.runErr
	}
	p.runs = append(p.runs, o)
	var task Task
	task.ID = fmt.Sprintf("task%d", len(p.runs))
	task.Runtime = "openshell"
	task.Status = "running"
	task.IdleTTL = o.IdleTTL
	task.Session.Agent = o.Agent
	task.Session.ConversationID = "conversation"
	task.Provenance.Identity = o.Identity
	task.Provenance.Profile = o.Profile
	task.Provenance.Requester = o.Actor
	task.Terminal.State = "running"
	p.tasks[task.ID] = task
	return task, nil
}
func (p *fakePaddock) Show(_ context.Context, id string) (Task, error) {
	task, ok := p.tasks[id]
	if !ok {
		return task, fmt.Errorf("unknown task %s", id)
	}
	return task, nil
}
func (p *fakePaddock) Message(_ context.Context, id, text, external string) (MessageReceipt, error) {
	p.messages = append(p.messages, sentMessage{id, text, external})
	status := p.messageStatus
	if status == "" {
		status = "delivered"
	}
	return MessageReceipt{Status: status}, nil
}
func (p *fakePaddock) Output(_ context.Context, id, after string) (OutputPage, error) {
	if p.output != nil {
		return p.output(id, after)
	}
	return OutputPage{TaskID: id, Events: []OutputEvent{}, NextCursor: after}, nil
}

type fakeChat struct {
	posts     []string
	events    []StreamEvent
	reactions []struct {
		channel, ts string
		add         bool
	}
	contextCalls                                int
	contextErr, postErr, reactionErr, streamErr error
	beforeStream                                func(StreamEvent)
	beforeReaction                              func()
}

func (c *fakeChat) Context(context.Context, string, string, string) (string, error) {
	c.contextCalls++
	return "parent alert", c.contextErr
}
func (c *fakeChat) Post(_ context.Context, channel, ts, text, id string) error {
	if c.postErr != nil {
		return c.postErr
	}
	c.posts = append(c.posts, text)
	return nil
}
func (c *fakeChat) Reaction(_ context.Context, channel, ts string, add bool) error {
	if c.beforeReaction != nil {
		c.beforeReaction()
	}
	if c.reactionErr != nil {
		return c.reactionErr
	}
	c.reactions = append(c.reactions, struct {
		channel, ts string
		add         bool
	}{channel, ts, add})
	return nil
}
func (c *fakeChat) StreamEvent(_ context.Context, o StreamEvent, s *Store) error {
	if c.beforeStream != nil {
		c.beforeStream(o)
	}
	if c.streamErr != nil {
		return c.streamErr
	}
	c.events = append(c.events, o)
	return s.RecordOutput(o.Task, o.Event.ID, o.Chunk)
}
func (c *fakeChat) StreamStatus(context.Context, string, string, string, *Store) error { return nil }

type agentFixture struct {
	a    *Agent
	p    *fakePaddock
	chat *fakeChat
	path string
	t    *testing.T
}

func newFixture(t *testing.T) *agentFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	s, e := OpenStore(path)
	if e != nil {
		t.Fatal(e)
	}
	p := &fakePaddock{tasks: map[string]Task{}}
	chat := &fakeChat{}
	f := &agentFixture{a: &Agent{Config: configFixture(), Store: s, Paddock: p, Slack: chat, BotUserID: "UA", BotID: "BA"}, p: p, chat: chat, path: path, t: t}
	t.Cleanup(func() { f.a.Store.Close() })
	return f
}
func (f *agentFixture) restart() {
	f.t.Helper()
	f.a.Store.Close()
	s, e := OpenStore(f.path)
	if e != nil {
		f.t.Fatal(e)
	}
	f.a = &Agent{Config: f.a.Config, Store: s, Paddock: f.p, Slack: f.chat, BotUserID: "UA", BotID: "BA"}
}
func incoming(ts string) Envelope {
	return Envelope{TeamID: "T1", Event: Event{Type: "message", Channel: "C1", TS: ts, User: "U1", Text: "<@UA> fix it"}}
}
func (f *agentFixture) receive(e Envelope) string {
	f.t.Helper()
	id, err := f.a.Receive(e)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}
func (f *agentFixture) drain() {
	f.t.Helper()
	if e := f.a.Drain(testContext); e != nil {
		f.t.Fatal(e)
	}
}
func (f *agentFixture) poll() {
	f.t.Helper()
	if e := f.a.Poll(testContext); e != nil {
		f.t.Fatal(e)
	}
}
func followup(ts, who, text string) Envelope {
	e := incoming(ts)
	e.Event.ThreadTS = "1"
	e.Event.User = who
	e.Event.Text = text
	return e
}
func TestThreadContextFailureRetainsRequestAndRecovers(t *testing.T) {
	f := newFixture(t)
	f.chat.contextErr = SlackError("invalid_arguments")
	f.receive(incoming("1"))
	f.drain()
	f.drain()
	if len(f.p.runs) != 0 || len(f.chat.posts) != 1 || !strings.Contains(f.chat.posts[0], "could not read this Slack thread") || !strings.Contains(f.chat.posts[0], "Nothing has been sent to Paddock") {
		t.Fatal("thread context failure was misreported or dispatched")
	}
	rows, err := f.a.Store.Pending()
	if err != nil || len(rows) != 1 || rows[0].Payload.Prepared != nil {
		t.Fatal("request was lost or frozen before context was available")
	}
	f.restart()
	f.chat.contextErr = nil
	f.drain()
	if len(f.p.runs) != 1 || !strings.Contains(f.p.runs[0].Prompt, "Current message from U1:\n<@UA> fix it") {
		t.Fatal("retained request did not recover")
	}
	f.drain()
	if len(f.p.runs) != 1 {
		t.Fatal("recovered request was dispatched twice")
	}
}
func TestRoutingContinuationAndNewMessagesOnly(t *testing.T) {
	f := newFixture(t)
	first := incoming("1")
	id := f.receive(first)
	first.Event.Type = "app_mention"
	if f.receive(first) != "" {
		t.Fatal("duplicate event accepted")
	}
	f.a.SyncReaction(testContext, id)
	if len(f.p.runs) != 0 || len(f.chat.reactions) != 1 {
		t.Fatal("acknowledgement must precede dispatch")
	}
	f.drain()
	if len(f.p.runs) != 1 || f.p.runs[0].Identity != "engineer-a" || !strings.Contains(f.p.runs[0].Prompt, "parent alert") {
		t.Fatal("initial dispatch/context")
	}
	f.restart()
	f.chat.contextErr = errors.New("history must not be fetched on resume")
	f.receive(followup("2", "U1", "what did you find?"))
	f.drain()
	if len(f.p.messages) != 1 || f.p.messages[0].Task != "task1" || f.p.messages[0].Text != "Current message from U1:\nwhat did you find?" || f.chat.contextCalls != 1 {
		t.Fatalf("follow-up duplicated history: %+v", f.p.messages)
	}
}
func TestRapidContinuation(t *testing.T) {
	f := newFixture(t)
	f.receive(incoming("1"))
	f.receive(followup("2", "U1", "more detail"))
	f.drain()
	if len(f.p.runs) != 1 || len(f.p.messages) != 1 {
		t.Fatal("rapid follow-up dropped")
	}
}
func TestAuthorizedAutomationAndHumanHandoffs(t *testing.T) {
	f := newFixture(t)
	e := incoming("1")
	e.Event.BotID = "B2"
	e.Event.Subtype = "bot_message"
	e.Event.Text = "Production 500"
	f.receive(e)
	f.drain()
	if f.p.runs[0].Identity != "automation" || f.p.runs[0].Profile != "investigation" {
		t.Fatal("automation identity")
	}
	f.receive(followup("2", "U1", "fix that"))
	f.drain()
	f.receive(followup("3", "U2", "test that"))
	f.drain()
	if f.p.runs[1].Identity != "engineer-a" || f.p.runs[1].Context["predecessor"] != "task1" || f.p.runs[2].Identity != "engineer-b" {
		t.Fatal("handoff provenance")
	}
	f.restart()
	f.receive(followup("4", "U1", "continue my fix"))
	f.drain()
	if len(f.p.runs) != 3 || f.p.messages[0].Task != "task2" {
		t.Fatal("participant's native session was lost")
	}
	if f.chat.contextCalls != 3 {
		t.Fatal("only task creation should fetch context")
	}
}
func TestUntrustedEventsCannotStartWork(t *testing.T) {
	for _, kind := range []string{"own-user", "own-bot", "other-bot", "edited", "wrong-team", "wrong-channel", "chatter", "app", "hidden"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			e := incoming("1")
			switch kind {
			case "own-user":
				e.Event.User = "UA"
			case "own-bot":
				e.Event.BotID = "BA"
			case "other-bot":
				e.Event.BotID = "B3"
				e.Event.Text = "500 <@UA>"
			case "edited":
				e.Event.Subtype = "message_changed"
			case "wrong-team":
				e.TeamID = "T2"
			case "wrong-channel":
				e.Event.Channel = "C2"
			case "chatter":
				e.Event.Text = "hello"
			case "app":
				e.Event.AppID = "A2"
			case "hidden":
				e.Event.Hidden = true
			}
			if f.receive(e) != "" {
				t.Fatal("untrusted event accepted")
			}
			f.drain()
			if len(f.p.runs) != 0 {
				t.Fatal("unauthorized task")
			}
		})
	}
}
func TestUnknownUserUsesDefaultIdentity(t *testing.T) {
	for _, mode := range []string{"mention", "subscription", "bot-subscription"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			if mode != "mention" {
				enableSubscriptions(f, mode == "bot-subscription")
			}
			e := incoming("1")
			e.Event.User = "U3"
			if mode != "mention" {
				e.Event.Text = "hello"
			}
			f.receive(e)
			f.drain()
			if len(f.p.runs) != 1 || len(f.chat.posts) != 0 {
				t.Fatal("unmapped human did not start a task using the default")
			}
			run := f.p.runs[0]
			if run.Actor != "U3" || run.Identity != f.a.Config.Automation.Identity || run.Profile != f.a.Config.Automation.Profile {
				t.Fatalf("wrong requester or default identity/profile: %+v", run)
			}
			if !strings.Contains(run.Prompt, "Authenticated requester: U3.") {
				t.Fatal("prompt lost the human requester")
			}
			f.receive(followup("2", "U4", "hello too"))
			f.receive(followup("3", "U1", "use my mapping"))
			f.drain()
			if len(f.p.runs) != 3 || f.p.runs[1].Actor != "U4" || f.p.runs[1].Identity != run.Identity || f.p.runs[1].Context["predecessor"] != "task1" || f.p.runs[2].Identity != "engineer-a" {
				t.Fatalf("participant bindings or explicit mapping lost: %+v", f.p.runs)
			}
			f.restart()
			f.chat.contextErr = errors.New("must not refetch history")
			f.receive(followup("4", "U3", "run as engineer-b on the host"))
			f.drain()
			if len(f.p.runs) != 3 || len(f.p.messages) != 1 || f.p.messages[0].Task != "task1" || f.p.messages[0].Text != "Current message from U3:\nrun as engineer-b on the host" {
				t.Fatal("default identity conversation did not resume for its original requester")
			}
		})
	}
}

func TestDefaultIdentityChangeDuringRetryRefusesDispatch(t *testing.T) {
	f := newFixture(t)
	f.p.runErr = errors.New("timeout")
	e := incoming("1")
	e.Event.User = "U3"
	f.receive(e)
	f.drain()
	f.restart()
	f.p.runErr = nil
	f.a.Config.Automation.Identity = "other"
	f.drain()
	if len(f.p.runs) != 0 || len(f.chat.posts) != 2 || !strings.Contains(f.chat.posts[1], "Execution configuration changed") {
		t.Fatal("retry dispatched with a different default identity")
	}
}

func TestUnknownUserWithoutDefaultAndPromptCannotChooseIdentity(t *testing.T) {
	f := newFixture(t)
	f.a.Config.Automation = nil
	f.a.Config.Triggers = nil
	e := incoming("1")
	e.Event.User = "U3"
	f.receive(e)
	f.drain()
	if len(f.p.runs) != 0 || len(f.chat.posts) != 1 {
		t.Fatal("unknown user accepted")
	}
	e = incoming("2")
	e.Event.Text = "<@UA> run as engineer-b on the host"
	f.receive(e)
	f.drain()
	if f.p.runs[0].Identity != "engineer-a" {
		t.Fatal("prompt selected identity")
	}
}
func TestFrozenDispatchRetryAcrossRestart(t *testing.T) {
	f := newFixture(t)
	f.p.runErr = errors.New("timeout")
	id := f.receive(incoming("1"))
	f.drain()
	rows, e := f.a.Store.Pending()
	if e != nil || len(rows) != 1 {
		t.Fatal(e)
	}
	prepared := *rows[0].Payload.Prepared
	f.restart()
	f.chat.contextErr = errors.New("must not refetch history")
	f.p.runErr = nil
	f.drain()
	if len(f.p.runs) != 1 || f.p.runs[0].Prompt != prepared.Prompt || f.p.runs[0].ExternalID != id {
		t.Fatal("changed retry")
	}
}
func TestPendingAndUncertainDelivery(t *testing.T) {
	for _, status := range []string{"uncertain", "claimed"} {
		t.Run(status, func(t *testing.T) { testPendingAndUncertainDelivery(t, status) })
	}
}

func testPendingAndUncertainDelivery(t *testing.T, status string) {
	t.Helper()
	f := newFixture(t)
	f.receive(incoming("1"))
	f.drain()
	f.p.messageStatus = "pending"
	f.receive(followup("2", "U1", "first"))
	f.receive(followup("3", "U1", "second"))
	f.drain()
	if len(f.p.messages) != 1 {
		t.Fatal("pending message did not hold thread")
	}
	f.p.messageStatus = status
	f.drain()
	pending, e := f.a.Store.Pending()
	if e != nil || len(pending) != 0 {
		t.Fatal("uncertain message should not replay")
	}
	count := len(f.chat.posts)
	f.restart()
	f.drain()
	if len(f.chat.posts) != count || !strings.Contains(f.chat.posts[count-1], "uncertain") {
		t.Fatal("uncertain outcome not preserved")
	}
}
func TestAgentSwitchAndFrozenHandoffRetry(t *testing.T) {
	f := newFixture(t)
	f.receive(incoming("1"))
	f.drain()
	f.a.Config.Agent = "claude"
	f.p.runErr = errors.New("timeout")
	f.receive(followup("2", "U1", "switch"))
	f.drain()
	f.restart()
	f.p.runErr = nil
	f.drain()
	if len(f.p.runs) != 2 || f.p.runs[1].Agent != "claude" || f.p.runs[1].Context["predecessor"] != "task1" || len(f.p.messages) != 0 {
		t.Fatal("agent handoff")
	}
	f.receive(followup("3", "U1", "continue"))
	f.drain()
	if f.p.messages[0].Task != "task2" {
		t.Fatal("Claude session was not resumed")
	}
}
func TestRuntimeAndIdentityMismatchRefusesContinuation(t *testing.T) {
	for _, field := range []string{"runtime", "identity", "profile"} {
		t.Run(field, func(t *testing.T) {
			f := newFixture(t)
			f.receive(incoming("1"))
			f.drain()
			task := f.p.tasks["task1"]
			switch field {
			case "runtime":
				task.Runtime = "host"
			case "identity":
				task.Provenance.Identity = "other"
			case "profile":
				task.Provenance.Profile = "other"
			}
			f.p.tasks[task.ID] = task
			f.receive(followup("2", "U1", "continue"))
			f.drain()
			if len(f.p.messages) > 0 {
				t.Fatal("mismatch permitted")
			}
		})
	}
}
func TestReactionCleanupOlderTurnReplayAndRestart(t *testing.T) {
	f := newFixture(t)
	f.receive(incoming("1"))
	f.drain()
	task := f.p.tasks["task1"]
	task.LastTurn.ID = "turn1"
	task.LastTurn.State = "inProgress"
	f.p.tasks[task.ID] = task
	id := f.receive(followup("2", "U1", "follow up"))
	f.drain()
	if e := f.a.Store.TurnFinished("task1", "turn1"); e != nil {
		t.Fatal(e)
	}
	if e := f.a.Store.TurnFinished("task1", "turn1"); e != nil {
		t.Fatal(e)
	}
	f.restart()
	f.drain()
	r, e := f.a.Store.Reaction(id)
	if e != nil || r.State != "active" {
		t.Fatal("old turn cleared follow-up", r, e)
	}
	if e = f.a.Store.TurnFinished("task1", "turn2"); e != nil {
		t.Fatal(e)
	}
	f.restart()
	f.drain()
	r, e = f.a.Store.Reaction(id)
	if e != nil || r.State != "done" {
		t.Fatal("reaction cleanup not durable", r, e)
	}
}
func TestReactionPermissionFailureDoesNotBlockWork(t *testing.T) {
	f := newFixture(t)
	f.chat.reactionErr = SlackError("missing_scope")
	id := f.receive(incoming("1"))
	f.drain()
	r, e := f.a.Store.Reaction(id)
	if e != nil || r.State != "add" || len(f.p.runs) != 1 || len(f.chat.posts) != 0 {
		t.Fatal("reaction failed the request")
	}
}
func TestOutputPollUnchangedProcessAndRestart(t *testing.T) {
	f := newFixture(t)
	f.receive(incoming("1"))
	f.drain()
	ready := false
	f.p.output = func(id, after string) (OutputPage, error) {
		events := []OutputEvent{}
		next := after
		if after == "" {
			events = append(events, OutputEvent{ID: "progress", Type: "message", TurnID: "turn1", Phase: "commentary", Text: "Checking logs"})
			next = "progress"
		} else if ready && after == "progress" {
			events = append(events, OutputEvent{ID: "answer", Type: "message", TurnID: "turn1", Phase: "final_answer", Text: "Found it"}, OutputEvent{ID: "finish", Type: "turn_finished", TurnID: "turn1", State: "completed"})
			next = "end"
		}
		return OutputPage{TaskID: id, Events: events, NextCursor: next}, nil
	}
	f.poll()
	f.restart()
	ready = true
	f.poll()
	if len(f.chat.events) != 3 || f.chat.events[1].Text != "Found it" {
		t.Fatal("missing output while process remained running")
	}
	count := len(f.chat.events)
	f.poll()
	if count != len(f.chat.events) {
		t.Fatal("output replay")
	}
	if got, _ := f.a.Store.Cursor("task1"); got != "end" {
		t.Fatal("cursor")
	}
	if len(f.chat.posts) != 0 {
		t.Fatal("debug noise")
	}
}
func TestConfiguration(t *testing.T) {
	c := configFixture()
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{"agent", "automation", "channel", "profile", "source", "duplicate", "empty"} {
		t.Run(bad, func(t *testing.T) {
			c := configFixture()
			switch bad {
			case "agent":
				c.Agent = "other"
			case "automation":
				c.Automation = nil
			case "channel":
				c.Channels["bad"] = Route{Repo: "x"}
			case "profile":
				c.Users["U1"] = Actor{Identity: "x", Profile: "root"}
			case "source":
				c.Triggers[0].UserID = "U1"
			case "duplicate":
				c.Triggers = append(c.Triggers, c.Triggers[0])
			case "empty":
				c.Triggers[0].Contains = " "
			}
			if c.Validate() == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	c.Triggers = nil
	c.Automation = nil
	if e := c.Validate(); e != nil {
		t.Fatal("human-only config", e)
	}
}
func TestStoreExclusionAndPublicTaskPointers(t *testing.T) {
	f := newFixture(t)
	if s, e := OpenStore(f.path); e == nil {
		s.Close()
		t.Fatal("second service acquired state")
	}
	f.receive(incoming("1"))
	f.drain()
	b, e := f.a.Store.Thread("T1:C1:1")
	if e != nil || b.Task != "task1" {
		t.Fatal(b, e)
	}
	rows, _ := f.a.Store.Pending()
	if len(rows) != 0 {
		t.Fatal("pending payload retained")
	}
	var payload any
	if e = f.a.Store.db.QueryRow("SELECT payload FROM receipts LIMIT 1").Scan(&payload); e != nil || payload != nil {
		t.Fatal("successful payload not deleted")
	}
}
func TestActionableNoticeRetryAndReactionRemoval(t *testing.T) {
	f := newFixture(t)
	f.a.Config.Automation = nil
	f.a.Config.Triggers = nil
	f.chat.postErr = errors.New("offline")
	e := incoming("1")
	e.Event.User = "U3"
	id := f.receive(e)
	f.drain()
	f.restart()
	f.chat.postErr = nil
	f.drain()
	if len(f.p.runs) != 0 || len(f.chat.posts) != 1 {
		t.Fatal("notice recovery")
	}
	r, _ := f.a.Store.Reaction(id)
	if r.State != "done" {
		t.Fatal("eyes not removed")
	}
}
func TestUnicodeChunks(t *testing.T) {
	if !reflect.DeepEqual(Chunks("a🦖b", 2), []string{"a🦖", "b"}) {
		t.Fatal("unicode chunks")
	}
}
