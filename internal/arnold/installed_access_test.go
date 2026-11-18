package arnold

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the real CLI's versioned error boundary against an empty disposable
// registry. This must not inspect host tasks, connect to a gateway, or grant
// access. Successful backend execution is covered by Paddock's adapter tests.
func TestInstalledPaddockAccessProtocol(t *testing.T) {
	binary := os.Getenv("ARNOLD_PADDOCK_TEST_BIN")
	if binary == "" {
		t.Skip("set ARNOLD_PADDOCK_TEST_BIN to exercise the installed access CLI")
	}
	registry := t.TempDir()
	t.Setenv("PADDOCK_HOME", registry)
	p := Paddock{Binary: binary}
	task := "0123456789abcdef01234567"
	request := "4cf929e5-cd07-43c4-a6e4-b0b7efb9ec89"
	r, err := p.accessCall(testContext, []string{"access", "list", "--task", task, "--status", "all", "--refresh", "--json"})
	if err == nil || r.Version != 1 || r.TaskID != task || r.Error == nil || r.Requests == nil {
		t.Fatalf("Paddock lacks the access-v1 structured error contract: %+v %v", r, err)
	}
	proposals, err := p.AccessList(testContext, task)
	if err == nil || len(proposals) != 0 {
		t.Fatalf("unknown task became actionable: %+v %v", proposals, err)
	}
	input := AccessDecisionInput{ProtocolVersion: 1, TaskID: task, Revision: "sha256:" + strings.Repeat("a", 64), Decision: "approve", ExternalID: "installed-access-fixture", Actor: AccessActor{Source: "arnold-slack", WorkspaceID: "T1", UserID: "U1"}, Reason: "Synthetic contract fixture"}
	result, err := p.AccessDecide(testContext, request, input)
	if err == nil || result.Terminal || result.RetryDecision {
		t.Fatalf("unknown task admitted an access decision: %+v %v", result, err)
	}
	var envelope accessWireResponse
	if json.Unmarshal(result.Receipt, &envelope) != nil || envelope.Version != 1 || envelope.TaskID != task || envelope.RequestID != request || envelope.Error == nil || envelope.Receipt != nil {
		t.Fatalf("real decide CLI discarded structured pre-admission error: %s", result.Receipt)
	}
	result, err = p.AccessReconcile(testContext, task, request)
	if err == nil || result.Terminal || result.RetryDecision {
		t.Fatalf("unknown task reconciliation allowed retry: %+v %v", result, err)
	}

	// A known retained request with a complete empty decision ledger proves
	// the local click was never submitted. No gateway or model is involved.
	writeRecord := func(dir, name string, value any) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(registry, dir), 0700); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(registry, dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeRecord("tasks", task+".json", map[string]any{"version": 1, "id": task, "runtime": "openshell", "status": "stopped", "repo": t.TempDir(), "session": map[string]string{"agent": "codex", "conversation_id": "retained-fixture"}, "openshell": map[string]any{"id": "retained-sandbox", "name": "pd-fixture", "ready": true, "config": map[string]string{"gateway": "fixture", "workspace": "default"}}})
	writeRecord("access-v1", task+".json", map[string]any{"schema_version": 1, "task_id": task, "requests": map[string]any{request: map[string]any{"view": map[string]any{"id": request, "task_id": task, "kind": "network", "status": "pending"}, "snapshots": map[string]any{}}}, "decisions": map[string]any{}, "idempotency": map[string]any{}, "advisor": map[string]string{"state": "unknown"}})
	result, err = p.AccessReconcile(testContext, task, request)
	if err == nil || !result.RetryDecision || result.Terminal {
		t.Fatalf("real CLI did not preserve authoritative absence: %+v %v", result, err)
	}
	if err = os.WriteFile(filepath.Join(registry, "access-v1", task+".json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = p.AccessReconcile(testContext, task, request)
	if err == nil || result.RetryDecision || result.Terminal {
		t.Fatalf("corrupt ledger was treated as no intent: %+v %v", result, err)
	}
}
