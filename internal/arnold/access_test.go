package arnold

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type orchestrationAccessPaddock struct {
	*fakePaddock
	mu         sync.Mutex
	proposals  []AccessProposal
	listErr    error
	listHook   func()
	decide     func(AccessDecisionInput) (AccessResult, error)
	reconcile  func() (AccessResult, error)
	decisions  []AccessDecisionInput
	reconciles int
}

func (p *orchestrationAccessPaddock) AccessList(_ context.Context, _ string) ([]AccessProposal, error) {
	p.mu.Lock()
	proposals, err, hook := append([]AccessProposal(nil), p.proposals...), p.listErr, p.listHook
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	return proposals, err
}

func (p *orchestrationAccessPaddock) AccessDecide(_ context.Context, _ string, input AccessDecisionInput) (AccessResult, error) {
	p.mu.Lock()
	p.decisions = append(p.decisions, input)
	fn := p.decide
	p.mu.Unlock()
	if fn != nil {
		return fn(input)
	}
	return AccessResult{Terminal: true, Summary: "Access approved; agent notification delivered"}, nil
}

func (p *orchestrationAccessPaddock) AccessReconcile(_ context.Context, _, _ string) (AccessResult, error) {
	p.mu.Lock()
	p.reconciles++
	fn := p.reconcile
	p.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return AccessResult{Terminal: true, Summary: "Access approved; agent notification delivered"}, nil
}

type orchestrationCardCall struct {
	Channel, TS, ID string
	View            AccessCardView
}

type orchestrationAccessChat struct {
	*fakeChat
	mu           sync.Mutex
	posted       []orchestrationCardCall
	updated      []orchestrationCardCall
	finds        int
	postErr      error
	findTS       string
	findErr      error
	beforeUpdate func(AccessCardView)
}

func (c *orchestrationAccessChat) PostAccessCard(_ context.Context, channel, thread, id string, v AccessCardView) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.posted = append(c.posted, orchestrationCardCall{Channel: channel, TS: thread, ID: id, View: v})
	if c.postErr != nil {
		return "", c.postErr
	}
	return "101.001", nil
}

func (c *orchestrationAccessChat) UpdateAccessCard(_ context.Context, channel, ts string, v AccessCardView) error {
	c.mu.Lock()
	fn := c.beforeUpdate
	c.mu.Unlock()
	if fn != nil {
		fn(v)
	}
	c.mu.Lock()
	c.updated = append(c.updated, orchestrationCardCall{Channel: channel, TS: ts, View: v})
	c.mu.Unlock()
	return nil
}

func (c *orchestrationAccessChat) FindAccessCard(_ context.Context, _, _, _, _ string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finds++
	return c.findTS, c.findErr
}

func accessOrchestrationFixture(t *testing.T) (*agentFixture, *orchestrationAccessPaddock, *orchestrationAccessChat, TaskRef, AccessProposal) {
	t.Helper()
	f, ref, p := boundAccessStore(t)
	backend := &orchestrationAccessPaddock{fakePaddock: f.p, proposals: []AccessProposal{p}}
	chat := &orchestrationAccessChat{fakeChat: f.chat}
	f.a.Paddock, f.a.Slack = backend, chat
	f.a.Config.AccessApprovers = []string{"U1"}
	f.a.accessWake = make(chan struct{}, 1)
	return f, backend, chat, ref, p
}

func latestAccessView(t *testing.T, c *orchestrationAccessChat) AccessCardView {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.updated) == 0 {
		t.Fatal("no access card update")
	}
	return c.updated[len(c.updated)-1].View
}

func admitOrchestrationClick(t *testing.T, f *agentFixture, c *orchestrationAccessChat) accessAction {
	t.Helper()
	v := latestAccessView(t, c)
	if !v.Actionable || v.Token == "" {
		t.Fatalf("card is not actionable: %+v", v)
	}
	if err := f.a.ReceiveAccessInteraction(accessClick(v.Token, "approve")); err != nil {
		t.Fatal(err)
	}
	items, err := f.a.Store.pendingAccessActions()
	if err != nil || len(items) != 1 {
		t.Fatalf("decision intake: %+v %v", items, err)
	}
	return items[0]
}

func restartAccessOrchestration(f *agentFixture, p *orchestrationAccessPaddock, c *orchestrationAccessChat) {
	f.restart()
	f.a.Paddock, f.a.Slack = p, c
	f.a.accessWake = make(chan struct{}, 1)
}

func TestAccessOrchestrationPostsBeforeControlsAndDefersDecision(t *testing.T) {
	f, backend, chat, ref, _ := accessOrchestrationFixture(t)
	chat.beforeUpdate = func(v AccessCardView) {
		card, err := f.a.Store.accessCard(v.CardID)
		if err != nil || card.Phase != "posted" || card.MessageTS != "101.001" {
			t.Errorf("controls appeared before durable message binding: %+v %v", card, err)
		}
	}
	if err := f.a.PollAccess(testContext); err != nil {
		t.Fatal(err)
	}
	if len(chat.posted) != 1 || chat.posted[0].View.Actionable || chat.posted[0].View.Token != "" || chat.posted[0].Channel != "C1" || chat.posted[0].TS != "100" {
		t.Fatalf("initial access card exposed controls or wrong thread: %+v", chat.posted)
	}
	v := latestAccessView(t, chat)
	if v.TaskID != ref.Task || !v.Actionable || !v.CanApprove || !v.CanReject || v.Token == "" {
		t.Fatalf("qualified controls missing: %+v", v)
	}
	action := admitOrchestrationClick(t, f, chat)
	if len(backend.decisions) != 0 || backend.reconciles != 0 || len(f.p.messages) != 0 {
		t.Fatal("socket intake invoked Paddock instead of durably queuing")
	}
	if err := f.a.DrainAccess(testContext); err != nil {
		t.Fatal(err)
	}
	if len(backend.decisions) != 1 || !reflect.DeepEqual(backend.decisions[0], action.Input) || len(f.p.messages) != 0 {
		t.Fatalf("decision or continuation duplicated: %+v %+v", backend.decisions, f.p.messages)
	}
	saved, err := f.a.Store.accessAction(action.CardID)
	if err != nil || saved.Phase != "done" || len(saved.Result) == 0 {
		t.Fatalf("terminal receipt not retained: %+v %v", saved, err)
	}
	if err := f.a.PollAccess(testContext); err != nil {
		t.Fatal(err)
	}
	v = latestAccessView(t, chat)
	if v.Actionable || v.Status != "Access approved; agent notification delivered" {
		t.Fatalf("decision outcome not reflected in card: %+v", v)
	}
	if err := f.a.DrainAccess(testContext); err != nil || len(backend.decisions) != 1 {
		t.Fatalf("terminal decision was resubmitted: %d %v", len(backend.decisions), err)
	}
}

func TestAccessOrchestrationTerminalHistoryDoesNotBackfillOrReplaceReviewedScope(t *testing.T) {
	f, backend, chat, _, proposal := accessOrchestrationFixture(t)
	if _, err := f.a.Store.db.Exec("DELETE FROM access_cards"); err != nil {
		t.Fatal(err)
	}
	historical := proposal
	historical.Status = "approved"
	historical.Actionable = false
	backend.proposals = []AccessProposal{historical}
	if err := f.a.PollAccess(testContext); err != nil {
		t.Fatal(err)
	}
	if len(chat.posted) != 0 {
		t.Fatal("historical settled request was posted into the thread")
	}
	backend.proposals = []AccessProposal{proposal}
	if err := f.a.PollAccess(testContext); err != nil {
		t.Fatal(err)
	}
	reviewed := latestAccessView(t, chat)
	historical.Revision = "different-post-approval-revision"
	historical.Details = []string{"A changed effective scope must not replace the reviewed scope"}
	historical.Outcome = "Access approved; agent notification delivered"
	backend.proposals = []AccessProposal{historical}
	if err := f.a.PollAccess(testContext); err != nil {
		t.Fatal(err)
	}
	settled := latestAccessView(t, chat)
	if len(chat.posted) != 1 || settled.Actionable || !reflect.DeepEqual(settled.Details, reviewed.Details) || settled.Status != historical.Outcome {
		t.Fatalf("terminal outcome replaced the reviewed scope or posted another card: %+v", settled)
	}
	if err := f.a.ReceiveAccessInteraction(accessClick(reviewed.Token, "approve")); err != nil {
		t.Fatal(err)
	}
	if actions, err := f.a.Store.pendingAccessActions(); err != nil || len(actions) != 0 {
		t.Fatalf("settled card retained approval authority: %+v %v", actions, err)
	}
}

func TestAccessOrchestrationRestartReconcilesClaimedDecision(t *testing.T) {
	f, backend, chat, _, _ := accessOrchestrationFixture(t)
	if err := f.a.PollAccess(testContext); err != nil {
		t.Fatal(err)
	}
	action := admitOrchestrationClick(t, f, chat)
	if ok, err := f.a.Store.claimAccessAction(action.ID); err != nil || !ok {
		t.Fatalf("simulate durable claim: %v %v", ok, err)
	}
	restartAccessOrchestration(f, backend, chat)
	if err := f.a.DrainAccess(testContext); err != nil {
		t.Fatal(err)
	}
	if len(backend.decisions) != 0 || backend.reconciles != 1 || len(f.p.messages) != 0 {
		t.Fatalf("claimed decision replayed after restart: decide=%d reconcile=%d messages=%d", len(backend.decisions), backend.reconciles, len(f.p.messages))
	}
	saved, err := f.a.Store.accessAction(action.CardID)
	if err != nil || saved.Phase != "done" || !reflect.DeepEqual(saved.Input, action.Input) {
		t.Fatalf("reconciliation lost frozen payload: %+v %v", saved, err)
	}
}

func TestAccessOrchestrationEarlierAttemptDoesNotConsumeNewDecision(t *testing.T) {
	f, backend, chat, _, _ := accessOrchestrationFixture(t)
	if err := f.a.PollAccess(testContext); err != nil {
		t.Fatal(err)
	}
	action := admitOrchestrationClick(t, f, chat)
	if ok, err := f.a.Store.claimAccessAction(action.ID); err != nil || !ok {
		t.Fatal(err)
	}
	r := accessWireReceipt{Version: 1, TaskID: action.TaskID, RequestID: action.RequestID, ExternalID: "earlier-review", Revision: "earlier-revision"}
	r.Decision.State = "stale"
	raw, _ := json.Marshal(accessWireResponse{Version: 1, TaskID: action.TaskID, RequestID: action.RequestID, Receipt: &r})
	backend.reconcile = func() (AccessResult, error) {
		return AccessResult{Terminal: true, ReviewEnded: true, Summary: "An old request was stale", Receipt: raw}, nil
	}
	if err := f.a.DrainAccess(testContext); err != nil {
		t.Fatal(err)
	}
	saved, _ := f.a.Store.accessAction(action.CardID)
	if saved.Phase != "pending" || len(backend.decisions) != 0 {
		t.Fatalf("earlier attempt consumed or executed a new approval: %+v", saved)
	}
	if err := f.a.DrainAccess(testContext); err != nil {
		t.Fatal(err)
	}
	if len(backend.decisions) != 1 || !reflect.DeepEqual(backend.decisions[0], action.Input) {
		t.Fatal("recovery changed the frozen decision or idempotency key")
	}
}

func TestAccessOrchestrationReconciliationPreservesOtherReviewAttribution(t *testing.T) {
	a := accessAction{TaskID: "task1", RequestID: "request1", Input: AccessDecisionInput{ExternalID: "ours", Revision: "ours", Decision: "approve", Actor: AccessActor{Source: "arnold-slack", WorkspaceID: "T1", UserID: "U1"}}}
	r := accessWireReceipt{TaskID: a.TaskID, RequestID: a.RequestID, ExternalID: "another-review", Revision: "another-revision"}
	r.Decision.State = "confirmed"
	raw, _ := json.Marshal(accessWireResponse{Receipt: &r})
	result, err := accessActionResult(a, AccessResult{Terminal: true, Summary: "Access approved", Receipt: raw})
	if err != nil || !result.Terminal || result.RetryDecision || !strings.Contains(result.Summary, "Another recorded review") {
		t.Fatal("another review was attributed to this approval", result, err)
	}
	r.ExternalID = a.Input.ExternalID
	raw, _ = json.Marshal(accessWireResponse{Receipt: &r})
	result, err = accessActionResult(a, AccessResult{Terminal: true, Receipt: raw})
	if err == nil || result.Terminal || result.RetryDecision {
		t.Fatal("same-key changed decision content was accepted", result, err)
	}
}

func TestAccessOrchestrationNoDecisionRearmsOnlyFrozenInput(t *testing.T) {
	f, backend, chat, _, _ := accessOrchestrationFixture(t)
	if err := f.a.PollAccess(testContext); err != nil {
		t.Fatal(err)
	}
	action := admitOrchestrationClick(t, f, chat)
	if ok, err := f.a.Store.claimAccessAction(action.ID); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	backend.reconcile = func() (AccessResult, error) {
		return AccessResult{RetryDecision: true, Summary: "No decision was admitted"}, errors.New("no_decision")
	}
	restartAccessOrchestration(f, backend, chat)
	if err := f.a.DrainAccess(testContext); err == nil {
		t.Fatal("no-decision diagnostic was swallowed")
	}
	saved, err := f.a.Store.accessAction(action.CardID)
	if err != nil || saved.Phase != "pending" || !reflect.DeepEqual(saved.Input, action.Input) || len(backend.decisions) != 0 {
		t.Fatalf("no-decision recovery changed input or applied early: %+v %v", saved, err)
	}
	if err := f.a.DrainAccess(testContext); err != nil {
		t.Fatal(err)
	}
	if len(backend.decisions) != 1 || !reflect.DeepEqual(backend.decisions[0], action.Input) {
		t.Fatalf("retry did not use exact original decision: %+v", backend.decisions)
	}
}

func TestAccessOrchestrationPendingNotificationDoesNotDuplicateDecision(t *testing.T) {
	f, backend, chat, _, _ := accessOrchestrationFixture(t)
	backend.decide = func(AccessDecisionInput) (AccessResult, error) {
		return AccessResult{Summary: "Access approved; agent notification pending"}, nil
	}
	backend.reconcile = func() (AccessResult, error) {
		return AccessResult{Terminal: true, Summary: "Access approved; agent notification delivered"}, nil
	}
	if err := f.a.PollAccess(testContext); err != nil {
		t.Fatal(err)
	}
	action := admitOrchestrationClick(t, f, chat)
	if err := f.a.DrainAccess(testContext); err != nil {
		t.Fatal(err)
	}
	saved, err := f.a.Store.accessAction(action.CardID)
	if err != nil || saved.Phase != "reconcile" {
		t.Fatalf("pending notification ended recovery: %+v %v", saved, err)
	}
	restartAccessOrchestration(f, backend, chat)
	if err := f.a.DrainAccess(testContext); err != nil {
		t.Fatal(err)
	}
	if len(backend.decisions) != 1 || backend.reconciles != 1 || len(f.p.messages) != 0 {
		t.Fatalf("pending notification caused duplicate continuation: decide=%d reconcile=%d messages=%d", len(backend.decisions), backend.reconciles, len(f.p.messages))
	}
}

func TestAccessOrchestrationInspectionFailureRemovesControlsAndGuidesOnce(t *testing.T) {
	f, backend, chat, _, _ := accessOrchestrationFixture(t)
	f.p.tasks["task1"] = Task{ID: "task1", Blocked: true}
	if err := f.a.PollAccess(testContext); err != nil {
		t.Fatal(err)
	}
	token := latestAccessView(t, chat).Token
	backend.listErr = errors.New("unsupported Paddock access protocol")
	for n := 0; n < 2; n++ {
		if err := f.a.PollAccess(testContext); err == nil {
			t.Fatal("inspection failure was swallowed")
		}
	}
	v := latestAccessView(t, chat)
	if v.Actionable || v.CanApprove || v.CanReject {
		t.Fatalf("failed inspection left qualified controls: %+v", v)
	}
	if len(f.chat.posts) != 1 {
		t.Fatalf("operator guidance repeated %d times", len(f.chat.posts))
	}
	if err := f.a.ReceiveAccessInteraction(accessClick(token, "approve")); err != nil {
		t.Fatal(err)
	}
	if items, err := f.a.Store.pendingAccessActions(); err != nil || len(items) != 0 {
		t.Fatalf("stale controls admitted a decision: %+v %v", items, err)
	}
	backend.listErr, backend.proposals = nil, nil
	if err := f.a.PollAccess(testContext); err != nil {
		t.Fatal(err)
	}
	if v := latestAccessView(t, chat); v.Actionable {
		t.Fatalf("withdrawn request became actionable: %+v", v)
	}
}

func TestAccessOrchestrationAmbiguousPostRecoversWithoutReposting(t *testing.T) {
	f, backend, chat, _, _ := accessOrchestrationFixture(t)
	chat.postErr = errors.New("reply lost after Slack accepted the post")
	if err := f.a.PollAccess(testContext); err == nil {
		t.Fatal("ambiguous post was reported confirmed")
	}
	cards, err := f.a.Store.accessCards()
	if err != nil || len(cards) != 1 || cards[0].Phase != "posting" || cards[0].MessageTS != "" || cards[0].TokenHash != "" {
		t.Fatalf("unconfirmed post exposed an actionable binding: %+v %v", cards, err)
	}
	if len(chat.updated) != 0 || chat.posted[0].View.Actionable {
		t.Fatal("controls exposed before post reconciliation")
	}
	if err := f.a.PollAccess(testContext); err == nil || len(chat.posted) != 1 || len(chat.updated) != 0 {
		t.Fatalf("unresolved post retried or exposed controls: posts=%d updates=%d err=%v", len(chat.posted), len(chat.updated), err)
	}
	chat.findTS = "101.001"
	if err := f.a.PollAccess(testContext); err != nil {
		t.Fatal(err)
	}
	if len(chat.posted) != 1 || chat.finds != 2 || !latestAccessView(t, chat).Actionable {
		t.Fatalf("post lookup recovery failed: posts=%d finds=%d", len(chat.posted), chat.finds)
	}
	admitOrchestrationClick(t, f, chat)
	if len(backend.decisions) != 0 {
		t.Fatal("recovering a post submitted a decision")
	}
}

func TestAccessOrchestrationDefinitePostRejectionCanRetry(t *testing.T) {
	for _, code := range []string{"missing_scope", "not_in_channel", "invalid_blocks", "rate_limit"} {
		t.Run(code, func(t *testing.T) {
			f, _, chat, _, _ := accessOrchestrationFixture(t)
			chat.postErr = fmt.Errorf("access card post rejected: %w", SlackError(code))
			if code == "rate_limit" {
				chat.postErr = &slackHTTPError{Method: "chat.postMessage", Status: 429}
			}
			if err := f.a.PollAccess(testContext); err == nil {
				t.Fatal("definite Slack rejection was swallowed")
			}
			cards, err := f.a.Store.accessCards()
			if err != nil || len(cards) != 1 || cards[0].Phase != "new" || cards[0].MessageTS != "" || cards[0].TokenHash != "" {
				t.Fatalf("definitely unsent post cannot retry: %+v %v", cards, err)
			}
			if len(chat.posted) != 1 || chat.finds != 0 || len(chat.updated) != 0 || chat.posted[0].View.Actionable {
				t.Fatalf("rejected post exposed controls or searched ambiguous history: %+v", chat)
			}
			chat.postErr = nil
			if err := f.a.PollAccess(testContext); err != nil {
				t.Fatal(err)
			}
			if len(chat.posted) != 2 || chat.finds != 0 || !latestAccessView(t, chat).Actionable || chat.posted[0].ID != chat.posted[1].ID || chat.posted[1].View.Actionable {
				t.Fatalf("fixed configuration did not retry the same safe card: posts=%+v finds=%d", chat.posted, chat.finds)
			}
		})
	}
}

func TestAccessOrchestrationAmbiguousPostErrorsRemainLookupOnly(t *testing.T) {
	for name, postErr := range map[string]error{
		"transport":       errors.New("connection reset after write"),
		"internal server": fmt.Errorf("Slack rejected ambiguously: %w", SlackError("internal_error")),
		"unknown Slack":   SlackError("unrecognized_failure"),
	} {
		t.Run(name, func(t *testing.T) {
			f, _, chat, _, _ := accessOrchestrationFixture(t)
			chat.postErr = postErr
			for attempt := 0; attempt < 3; attempt++ {
				if err := f.a.PollAccess(testContext); err == nil {
					t.Fatal("ambiguous posting outcome was confirmed")
				}
			}
			cards, err := f.a.Store.accessCards()
			if err != nil || len(cards) != 1 || cards[0].Phase != "posting" || cards[0].MessageTS != "" || cards[0].TokenHash != "" {
				t.Fatalf("ambiguous post was made retryable or actionable: %+v %v", cards, err)
			}
			if len(chat.posted) != 1 || chat.finds != 2 || len(chat.updated) != 0 {
				t.Fatalf("ambiguous error caused a repost or controls: posts=%d finds=%d updates=%d", len(chat.posted), chat.finds, len(chat.updated))
			}
		})
	}
}

func TestAccessOrchestrationLocalCardValidationDoesNotClaimPost(t *testing.T) {
	for name, mutate := range map[string]func(*AccessProposal){
		"oversized summary": func(p *AccessProposal) { p.Summary = strings.Repeat("x", 3001) },
		"oversized scope":   func(p *AccessProposal) { p.Details = []string{strings.Repeat("x", 3001)} },
		"too many details":  func(p *AccessProposal) { p.Details = make([]string, 17) },
	} {
		t.Run(name, func(t *testing.T) {
			f, backend, chat, _, original := accessOrchestrationFixture(t)
			mutate(&backend.proposals[0])
			if err := f.a.PollAccess(testContext); err == nil {
				t.Fatal("invalid local card was accepted")
			}
			cards, err := f.a.Store.accessCards()
			if err != nil || len(cards) != 1 || cards[0].Phase != "new" || cards[0].MessageTS != "" || cards[0].TokenHash != "" {
				t.Fatalf("local validation marked a post in flight: %+v %v", cards, err)
			}
			if len(chat.posted) != 0 || chat.finds != 0 || len(chat.updated) != 0 {
				t.Fatal("local validation failure reached Slack transport")
			}
			backend.proposals[0] = original
			if err := f.a.PollAccess(testContext); err != nil {
				t.Fatal(err)
			}
			if len(chat.posted) != 1 || chat.finds != 0 || !latestAccessView(t, chat).Actionable {
				t.Fatalf("corrected card could not start a first post: posts=%d finds=%d", len(chat.posted), chat.finds)
			}
		})
	}
}

func TestAccessOrchestrationConcurrentPollAndClicksPreserveOneDecision(t *testing.T) {
	f, backend, chat, _, _ := accessOrchestrationFixture(t)
	if err := f.a.PollAccess(testContext); err != nil {
		t.Fatal(err)
	}
	token := latestAccessView(t, chat).Token
	entered, release := make(chan struct{}), make(chan struct{})
	backend.listHook = func() { close(entered); <-release }
	polled := make(chan error, 1)
	go func() { polled <- f.a.PollAccess(testContext) }()
	<-entered
	for _, decision := range []string{"approve", "approve", "reject"} {
		if err := f.a.ReceiveAccessInteraction(accessClick(token, decision)); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	if err := <-polled; err != nil {
		t.Fatal(err)
	}
	items, err := f.a.Store.pendingAccessActions()
	if err != nil || len(items) != 1 || items[0].Input.Decision != "approve" || latestAccessView(t, chat).Actionable {
		t.Fatalf("poll/click race displaced decision or controls: %+v %v", items, err)
	}
	if err := f.a.DrainAccess(testContext); err != nil {
		t.Fatal(err)
	}
	if len(backend.decisions) != 1 {
		t.Fatalf("%d decisions after repeated clicks", len(backend.decisions))
	}
}

func TestAccessOrchestrationUnqualifiedProposalsCannotGrant(t *testing.T) {
	for name, mutate := range map[string]func(*AccessProposal){
		"wrong kind":                     func(p *AccessProposal) { p.Kind = "host-shell" },
		"wrong task":                     func(p *AccessProposal) { p.TaskID = "other-task" },
		"missing decision qualification": func(p *AccessProposal) { p.CanApprove, p.CanReject = false, false },
		"stale request":                  func(p *AccessProposal) { p.Fresh = false },
		"not actionable":                 func(p *AccessProposal) { p.Actionable = false },
	} {
		t.Run(name, func(t *testing.T) {
			f, backend, chat, _, _ := accessOrchestrationFixture(t)
			mutate(&backend.proposals[0])
			_ = f.a.PollAccess(testContext)
			for _, update := range chat.updated {
				if err := f.a.ReceiveAccessInteraction(accessClick(update.View.Token, "approve")); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.a.DrainAccess(testContext); err != nil {
				t.Fatal(err)
			}
			if len(backend.decisions) != 0 || len(f.p.messages) != 0 {
				t.Fatal("unqualified proposal granted access")
			}
		})
	}
}

func TestAccessOrchestrationRevocationCancelsOnlyDefinitelyUnsentDecisions(t *testing.T) {
	for _, phase := range []string{"pending", "claimed", "reconcile"} {
		t.Run(phase, func(t *testing.T) {
			f, backend, chat, _, _ := accessOrchestrationFixture(t)
			if err := f.a.PollAccess(testContext); err != nil {
				t.Fatal(err)
			}
			action := admitOrchestrationClick(t, f, chat)
			if phase != "pending" {
				if ok, err := f.a.Store.claimAccessAction(action.ID); err != nil || !ok {
					t.Fatalf("claim: %v %v", ok, err)
				}
				if phase == "reconcile" {
					if err := f.a.Store.finishAccessAction(action.ID, "reconcile", nil, ""); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.a.Config.AccessApprovers = nil
			if err := f.a.DrainAccess(testContext); err != nil {
				t.Fatal(err)
			}
			if len(backend.decisions) != 0 || (phase == "pending" && backend.reconciles != 0) || (phase != "pending" && backend.reconciles != 1) {
				t.Fatalf("revocation mishandled admitted outcome: phase=%s decide=%d reconcile=%d", phase, len(backend.decisions), backend.reconciles)
			}
			saved, err := f.a.Store.accessAction(action.CardID)
			if err != nil || saved.Phase != "done" {
				t.Fatalf("revocation outcome not retained: %+v %v", saved, err)
			}
			var result AccessResult
			if err := json.Unmarshal(saved.Result, &result); err != nil {
				t.Fatal(err)
			}
			if phase != "pending" && result.Summary != "Access approved; agent notification delivered" {
				t.Fatalf("revoked approver concealed an already admitted decision: %+v", result)
			}
		})
	}
}

func TestAccessOrchestrationUnsupportedStoredRequestCannotBeSubmitted(t *testing.T) {
	for _, phase := range []string{"pending", "claimed-no-decision", "claimed-uncertain"} {
		t.Run(phase, func(t *testing.T) {
			f, backend, chat, _, _ := accessOrchestrationFixture(t)
			if err := f.a.PollAccess(testContext); err != nil {
				t.Fatal(err)
			}
			action := admitOrchestrationClick(t, f, chat)
			card, err := f.a.Store.accessCard(action.CardID)
			if err != nil {
				t.Fatal(err)
			}
			card.Proposal.Kind = "host-operation"
			raw, err := json.Marshal(card.Proposal)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.a.Store.db.Exec("UPDATE access_cards SET snapshot=? WHERE id=?", string(raw), card.ID); err != nil {
				t.Fatal(err)
			}
			if phase != "pending" {
				if ok, err := f.a.Store.claimAccessAction(action.ID); err != nil || !ok {
					t.Fatal("claim failed", ok, err)
				}
				backend.reconcile = func() (AccessResult, error) {
					return AccessResult{RetryDecision: phase == "claimed-no-decision"}, nil
				}
			}
			if err := f.a.ReceiveAccessInteraction(accessClick(latestAccessView(t, chat).Token, "approve")); err != nil {
				t.Fatal(err)
			}
			if err := f.a.DrainAccess(testContext); err != nil {
				t.Fatal(err)
			}
			wantReconciles := 0
			if phase != "pending" {
				wantReconciles = 1
			}
			if len(backend.decisions) != 0 || backend.reconciles != wantReconciles {
				t.Fatal("unsupported request was dispatched or failed to reconcile")
			}
			saved, err := f.a.Store.accessAction(action.CardID)
			wantPhase := "done"
			if phase == "claimed-uncertain" {
				wantPhase = "reconcile"
			}
			if err != nil || saved.Phase != wantPhase {
				t.Fatal("unsupported decision recovery was incorrect", saved, err)
			}
		})
	}
}
