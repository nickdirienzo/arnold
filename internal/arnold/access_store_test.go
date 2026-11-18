package arnold

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func boundAccessStore(t *testing.T) (*agentFixture, TaskRef, AccessProposal) {
	t.Helper()
	f := newFixture(t)
	ref := TaskRef{Key: "T1:C1:100", Task: "task1"}
	id := Hash("access-store-binding")
	if ok, err := f.a.Store.Accept(id, ref.Key, Payload{}); err != nil || !ok {
		t.Fatalf("accept binding: %v %v", ok, err)
	}
	if err := f.a.Store.Bind(Receipt{ID: id, Key: ref.Key}, ref.Task, "U1", "engineer-a", "implementation", ""); err != nil {
		t.Fatal(err)
	}
	p := AccessProposal{ID: "4cf929e5-cd07-43c4-a6e4-b0b7efb9ec89", TaskID: ref.Task, Revision: "sha256:" + strings.Repeat("a", 64), Kind: "network", Status: "pending", Summary: "Download dependencies", Details: []string{"registry.npmjs.org:443"}, Actionable: true, CanApprove: true, CanReject: true, Fresh: true}
	return f, ref, p
}

func postedAccessCard(t *testing.T, s *Store, ref TaskRef, p AccessProposal) (accessCard, string) {
	t.Helper()
	c, err := s.saveAccessProposal(ref, p)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.beginAccessPost(c.ID); err != nil || !ok {
		t.Fatalf("begin post: %v %v", ok, err)
	}
	if err := s.bindAccessMessage(c.ID, "101.001"); err != nil {
		t.Fatal(err)
	}
	token, err := s.rotateAccessToken(c.ID)
	if err != nil || token == "" {
		t.Fatalf("rotate token: %q %v", token, err)
	}
	c, err = s.accessCard(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c, token
}

func accessClick(token, decision string) AccessInteraction {
	var i AccessInteraction
	i.Type = "block_actions"
	i.Team.ID, i.User.ID = "T1", "U1"
	i.Container.ChannelID, i.Container.MessageTS = "C1", "101.001"
	i.Actions = append(i.Actions, struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	}{ActionID: "arnold_access_" + decision, Value: token})
	return i
}

func TestAccessStoreRequiresTaskThreadBinding(t *testing.T) {
	f, ref, p := boundAccessStore(t)
	for name, mutate := range map[string]func(*TaskRef, *AccessProposal){
		"wrong task":     func(r *TaskRef, _ *AccessProposal) { r.Task = "task2" },
		"unbound task":   func(r *TaskRef, p *AccessProposal) { r.Task, p.TaskID = "task2", "task2" },
		"wrong thread":   func(r *TaskRef, _ *AccessProposal) { r.Key = "T1:C1:200" },
		"wrong channel":  func(r *TaskRef, _ *AccessProposal) { r.Key = "T1:C2:100" },
		"wrong team":     func(r *TaskRef, _ *AccessProposal) { r.Key = "T2:C1:100" },
		"malformed key":  func(r *TaskRef, _ *AccessProposal) { r.Key = "C1:100" },
		"empty request":  func(_ *TaskRef, p *AccessProposal) { p.ID = "" },
		"empty revision": func(_ *TaskRef, p *AccessProposal) { p.Revision = "" },
		"unknown kind":   func(_ *TaskRef, p *AccessProposal) { p.Kind = "shell" },
	} {
		t.Run(name, func(t *testing.T) {
			r, proposal := ref, p
			mutate(&r, &proposal)
			if _, err := f.a.Store.saveAccessProposal(r, proposal); err == nil {
				t.Fatal("unbound or invalid request accepted")
			}
		})
	}
	if _, err := f.a.Store.saveAccessProposal(ref, p); err != nil {
		t.Fatal(err)
	}
}

func TestAccessStorePostingAndTokenPersistence(t *testing.T) {
	f, ref, p := boundAccessStore(t)
	s := f.a.Store
	c, err := s.saveAccessProposal(ref, p)
	if err != nil {
		t.Fatal(err)
	}
	if token, err := s.rotateAccessToken(c.ID); err != nil || token != "" {
		t.Fatalf("unposted card acquired token: %q %v", token, err)
	}
	if err := s.bindAccessMessage(c.ID, ""); err == nil {
		t.Fatal("missing timestamp accepted")
	}
	for attempt := 0; attempt < 2; attempt++ {
		ok, err := s.beginAccessPost(c.ID)
		if err != nil || ok != (attempt == 0) {
			t.Fatalf("post claim %d: %v %v", attempt, ok, err)
		}
	}
	if err := s.bindAccessMessage(c.ID, "101.001"); err != nil {
		t.Fatal(err)
	}
	token, err := s.rotateAccessToken(c.ID)
	if err != nil || token == "" {
		t.Fatalf("token: %q %v", token, err)
	}
	f.restart()
	c, err = f.a.Store.accessCard(c.ID)
	if err != nil || c.TokenHash != Hash(token) || c.TokenHash == token || c.Phase != "posted" || c.MessageTS != "101.001" {
		t.Fatalf("card recovery: %+v %v", c, err)
	}
	var snapshot string
	if err := f.a.Store.db.QueryRow("SELECT snapshot FROM access_cards WHERE id=?", c.ID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snapshot, token) {
		t.Fatal("raw action token persisted in snapshot")
	}
	if ok, err := f.a.Store.acceptAccessAction(accessClick(token, "approve"), "approve"); err != nil || !ok {
		t.Fatalf("recovered token did not identify card: %v %v", ok, err)
	}
	if next, err := f.a.Store.rotateAccessToken(c.ID); err != nil || next != "" {
		t.Fatalf("decided card acquired another token: %q %v", next, err)
	}
}

func TestAccessStoreRejectsUnboundAndMalformedClicks(t *testing.T) {
	for name, mutate := range map[string]func(*AccessInteraction, *string){
		"wrong team":        func(i *AccessInteraction, _ *string) { i.Team.ID = "T2" },
		"wrong channel":     func(i *AccessInteraction, _ *string) { i.Container.ChannelID = "C2" },
		"wrong message":     func(i *AccessInteraction, _ *string) { i.Container.MessageTS = "102.001" },
		"wrong token":       func(i *AccessInteraction, _ *string) { i.Actions[0].Value = "different-token" },
		"empty token":       func(i *AccessInteraction, _ *string) { i.Actions[0].Value = "" },
		"missing actions":   func(i *AccessInteraction, _ *string) { i.Actions = nil },
		"multiple actions":  func(i *AccessInteraction, _ *string) { i.Actions = append(i.Actions, i.Actions[0]) },
		"wrong type":        func(i *AccessInteraction, _ *string) { i.Type = "view_submission" },
		"wrong action":      func(i *AccessInteraction, _ *string) { i.Actions[0].ActionID = "arbitrary_action" },
		"mismatched action": func(i *AccessInteraction, _ *string) { i.Actions[0].ActionID = "arnold_access_reject" },
		"missing user":      func(i *AccessInteraction, _ *string) { i.User.ID = "" },
		"bot user":          func(i *AccessInteraction, _ *string) { i.User.ID = "B123" },
		"unknown decision":  func(_ *AccessInteraction, d *string) { *d = "merge" },
	} {
		t.Run(name, func(t *testing.T) {
			f, ref, p := boundAccessStore(t)
			_, token := postedAccessCard(t, f.a.Store, ref, p)
			i, decision := accessClick(token, "approve"), "approve"
			mutate(&i, &decision)
			if ok, err := f.a.Store.acceptAccessAction(i, decision); err != nil || ok {
				t.Fatalf("invalid click accepted: %v %v", ok, err)
			}
			if actions, err := f.a.Store.pendingAccessActions(); err != nil || len(actions) != 0 {
				t.Fatalf("invalid click left work: %+v %v", actions, err)
			}
		})
	}
	t.Run("removed task binding", func(t *testing.T) {
		f, ref, p := boundAccessStore(t)
		_, token := postedAccessCard(t, f.a.Store, ref, p)
		if _, err := f.a.Store.db.Exec("DELETE FROM receipts WHERE key=? AND task=?", ref.Key, ref.Task); err != nil {
			t.Fatal(err)
		}
		if ok, err := f.a.Store.acceptAccessAction(accessClick(token, "approve"), "approve"); err != nil || ok {
			t.Fatalf("removed binding granted authority: %v %v", ok, err)
		}
	})
}

func TestAccessStoreNewRevisionInvalidatesOldToken(t *testing.T) {
	f, ref, p := boundAccessStore(t)
	old, token := postedAccessCard(t, f.a.Store, ref, p)
	p.Revision = "sha256:" + strings.Repeat("b", 64)
	next, err := f.a.Store.saveAccessProposal(ref, p)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == old.ID {
		t.Fatal("new revision reused old card")
	}
	old, err = f.a.Store.accessCard(old.ID)
	if err != nil || old.Active || old.TokenHash != "" {
		t.Fatalf("stale card stayed active: %+v %v", old, err)
	}
	if ok, err := f.a.Store.acceptAccessAction(accessClick(token, "approve"), "approve"); err != nil || ok {
		t.Fatalf("stale revision accepted: %v %v", ok, err)
	}
	if err := f.a.Store.invalidateAccessCards(ref, map[string]bool{}, "Request unavailable"); err != nil {
		t.Fatal(err)
	}
	next, err = f.a.Store.accessCard(next.ID)
	if err != nil || next.Active || next.Proposal.Fresh || next.Proposal.Actionable || next.Proposal.Outcome != "Request unavailable" {
		t.Fatalf("missing upstream request stayed actionable: %+v %v", next, err)
	}
}

func TestAccessStoreDecisionQualification(t *testing.T) {
	for _, kind := range []string{"network"} {
		for _, decision := range []string{"approve", "reject"} {
			for name, mutate := range map[string]func(*AccessProposal){
				"unqualified decision": func(p *AccessProposal) { p.CanApprove, p.CanReject = decision != "approve", decision != "reject" },
				"stale inspection":     func(p *AccessProposal) { p.Fresh = false },
				"not actionable":       func(p *AccessProposal) { p.Actionable = false },
			} {
				t.Run(kind+"/"+decision+"/"+name, func(t *testing.T) {
					f, ref, p := boundAccessStore(t)
					p.Kind = kind
					mutate(&p)
					_, token := postedAccessCard(t, f.a.Store, ref, p)
					if ok, err := f.a.Store.acceptAccessAction(accessClick(token, decision), decision); err != nil || ok {
						t.Fatalf("unqualified decision accepted: %v %v", ok, err)
					}
				})
			}
		}
	}
}

func TestAccessStoreConcurrentDecisionsHaveOneWinner(t *testing.T) {
	f, ref, p := boundAccessStore(t)
	card, token := postedAccessCard(t, f.a.Store, ref, p)
	var winners atomic.Int32
	var wg sync.WaitGroup
	for n := 0; n < 24; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			decision := "approve"
			if n%2 != 0 {
				decision = "reject"
			}
			i := accessClick(token, decision)
			if n%3 == 0 {
				i.User.ID = "U2"
			}
			ok, err := f.a.Store.acceptAccessAction(i, decision)
			if err != nil {
				t.Errorf("decision claim: %v", err)
			}
			if ok {
				winners.Add(1)
			}
		}(n)
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("%d decision winners", winners.Load())
	}
	before, err := f.a.Store.accessAction(card.ID)
	if err != nil || before == nil {
		t.Fatalf("winning decision missing: %+v %v", before, err)
	}
	opposite := "reject"
	if before.Input.Decision == "reject" {
		opposite = "approve"
	}
	i := accessClick(token, opposite)
	i.User.ID = "U3"
	if ok, err := f.a.Store.acceptAccessAction(i, opposite); err != nil || ok {
		t.Fatalf("later decision displaced winner: %v %v", ok, err)
	}
	after, err := f.a.Store.accessAction(card.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("decision payload changed: before=%+v after=%+v err=%v", before, after, err)
	}
	if before.Input.TaskID != ref.Task || before.Input.Revision != p.Revision || before.Input.Actor.Source != "arnold-slack" || before.Input.Actor.WorkspaceID != "T1" || before.Input.ExternalID == "" || before.Input.Reason == "" {
		t.Fatalf("winning payload lost identity: %+v", before.Input)
	}
}

func TestAccessStoreRestartsRetainFrozenDecisionAndPhase(t *testing.T) {
	f, ref, p := boundAccessStore(t)
	card, token := postedAccessCard(t, f.a.Store, ref, p)
	if ok, err := f.a.Store.acceptAccessAction(accessClick(token, "reject"), "reject"); err != nil || !ok {
		t.Fatalf("accept decision: %v %v", ok, err)
	}
	before, err := f.a.Store.accessAction(card.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.restart()
	actions, err := f.a.Store.pendingAccessActions()
	if err != nil || len(actions) != 1 || actions[0].Phase != "pending" || !reflect.DeepEqual(actions[0].Input, before.Input) {
		t.Fatalf("pending recovery: %+v %v", actions, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		ok, err := f.a.Store.claimAccessAction(before.ID)
		if err != nil || ok != (attempt == 0) {
			t.Fatalf("claim attempt %d: %v %v", attempt, ok, err)
		}
	}
	f.restart()
	actions, err = f.a.Store.pendingAccessActions()
	if err != nil || len(actions) != 1 || actions[0].Phase != "claimed" || !reflect.DeepEqual(actions[0].Input, before.Input) {
		t.Fatalf("claimed recovery: %+v %v", actions, err)
	}
	result := json.RawMessage(`{"state":"uncertain","notification":"pending"}`)
	if err := f.a.Store.finishAccessAction(before.ID, "reconcile", result, "Inspect upstream state"); err != nil {
		t.Fatal(err)
	}
	f.restart()
	after, err := f.a.Store.accessAction(card.ID)
	if err != nil || after.Phase != "reconcile" || after.Error != "Inspect upstream state" || string(after.Result) != string(result) || !reflect.DeepEqual(after.Input, before.Input) {
		t.Fatalf("uncertain recovery: %+v %v", after, err)
	}
	if err := f.a.Store.finishAccessAction(before.ID, "done", result, ""); err != nil {
		t.Fatal(err)
	}
	if actions, err := f.a.Store.pendingAccessActions(); err != nil || len(actions) != 0 {
		t.Fatalf("completed decision was requeued: %+v %v", actions, err)
	}
	if err := f.a.Store.finishAccessAction(before.ID, "pending", nil, ""); err != nil {
		t.Fatal(err)
	}
	after, err = f.a.Store.accessAction(card.ID)
	if err != nil || after.Phase != "done" {
		t.Fatalf("terminal decision was reopened: %+v %v", after, err)
	}
}
