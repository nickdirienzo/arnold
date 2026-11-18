package arnold

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type accessHelperControl struct {
	Response string `json:"response"`
	ExitCode int    `json:"exit_code"`
}

type accessHelperRecord struct {
	Args          []string            `json:"args"`
	Decision      AccessDecisionInput `json:"decision"`
	DecisionMode  uint32              `json:"decision_mode"`
	DirectoryMode uint32              `json:"directory_mode"`
}

func accessExample(t *testing.T, name string) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("testdata/access-wire-v1-examples.json")
	if err != nil {
		t.Fatal(err)
	}
	var examples map[string]json.RawMessage
	if err = json.Unmarshal(raw, &examples); err != nil || examples[name] == nil {
		t.Fatal("invalid exported fixture", name, err)
	}
	return examples[name]
}

func decodeAccessExample(t *testing.T, name string, out any) {
	t.Helper()
	if err := json.Unmarshal(accessExample(t, name), out); err != nil {
		t.Fatal(err)
	}
}

func accessFixtureResponse(t *testing.T) accessWireResponse {
	t.Helper()
	var response accessWireResponse
	decodeAccessExample(t, "list", &response)
	return response
}

func accessDecisionFixture(t *testing.T) AccessDecisionInput {
	t.Helper()
	var input AccessDecisionInput
	decodeAccessExample(t, "decision_input", &input)
	return input
}

func accessFixtureTask(t *testing.T) (task, request string) {
	t.Helper()
	r := accessFixtureResponse(t)
	return r.TaskID, r.Requests[0].ID
}

func newAccessHelper(t *testing.T, response any, exitCode int) (Paddock, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	dir := t.TempDir()
	control, record, binary := filepath.Join(dir, "control.json"), filepath.Join(dir, "record.json"), filepath.Join(dir, "paddock")
	var raw []byte
	var err error
	if bytes, ok := response.(json.RawMessage); ok {
		raw = bytes
	} else {
		raw, err = json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
	}
	controlRaw, err := json.Marshal(accessHelperControl{Response: string(raw), ExitCode: exitCode})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(control, controlRaw, 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec " + shellQuote(os.Args[0]) + " -test.run=TestAccessPaddockHelper -- " + shellQuote(control) + " " + shellQuote(record) + " \"$@\"\n"
	if err = os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return Paddock{Binary: binary}, record
}

func readAccessHelperRecord(t *testing.T, path string) accessHelperRecord {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record accessHelperRecord
	if err = json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestAccessPaddockHelper(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "--" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	for _, key := range []string{"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "GITHUB_TOKEN", "PADDOCK_TASK_ID"} {
		if os.Getenv(key) != "" {
			fmt.Fprintln(os.Stderr, "credential was inherited")
			os.Exit(90)
		}
	}
	var control accessHelperControl
	raw, err := os.ReadFile(os.Args[index+1])
	if err != nil || json.Unmarshal(raw, &control) != nil {
		os.Exit(91)
	}
	record := accessHelperRecord{Args: os.Args[index+3:]}
	for i, arg := range record.Args {
		if arg != "--decision-file" || i+1 == len(record.Args) {
			continue
		}
		path := record.Args[i+1]
		fileInfo, err := os.Stat(path)
		if err != nil {
			os.Exit(92)
		}
		dirInfo, err := os.Stat(filepath.Dir(path))
		if err != nil {
			os.Exit(93)
		}
		record.DecisionMode, record.DirectoryMode = uint32(fileInfo.Mode().Perm()), uint32(dirInfo.Mode().Perm())
		raw, err = os.ReadFile(path)
		if err != nil || json.Unmarshal(raw, &record.Decision) != nil {
			os.Exit(94)
		}
	}
	raw, err = json.Marshal(record)
	if err != nil || os.WriteFile(os.Args[index+2], raw, 0600) != nil {
		os.Exit(95)
	}
	os.Stdout.WriteString(control.Response)
	os.Exit(control.ExitCode)
}

func TestAccessPaddockListExportedContractAndCredentials(t *testing.T) {
	for _, key := range []string{"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "GITHUB_TOKEN", "PADDOCK_TASK_ID"} {
		t.Setenv(key, "must-not-reach-adapter")
	}
	task, _ := accessFixtureTask(t)
	p, path := newAccessHelper(t, accessExample(t, "list"), 0)
	proposals, err := p.AccessList(testContext, task)
	if err != nil || len(proposals) != 1 {
		t.Fatal("exported backend contract rejected", proposals, err)
	}
	proposal := proposals[0]
	if !proposal.Fresh || !proposal.Actionable || !proposal.CanApprove || proposal.CanReject {
		t.Fatal("qualified decisions changed", proposal)
	}
	for _, text := range []string{"registry.npmjs.org", "/usr/local/bin/node", "443", "this sandbox", "not a single download"} {
		if !strings.Contains(strings.Join(proposal.Details, "\n"), text) {
			t.Fatal("authorization scope omitted", text, proposal.Details)
		}
	}
	want := []string{"access", "list", "--task", task, "--status", "all", "--refresh", "--json"}
	if got := readAccessHelperRecord(t, path).Args; !reflect.DeepEqual(got, want) {
		t.Fatal("list CLI contract changed", got)
	}
	p, _ = newAccessHelper(t, accessExample(t, "empty_list"), 0)
	if proposals, err = p.AccessList(testContext, task); err != nil || len(proposals) != 0 {
		t.Fatal("empty list rejected", proposals, err)
	}
}

func TestAccessPaddockListRejectsForeignOrMalformedAuthority(t *testing.T) {
	for name, mutate := range map[string]func(*accessWireResponse){
		"protocol":     func(r *accessWireResponse) { r.Version = 0 },
		"outer task":   func(r *accessWireResponse) { r.TaskID = strings.Repeat("b", 24) },
		"request task": func(r *accessWireResponse) { r.Requests[0].TaskID = strings.Repeat("b", 24) },
		"request id":   func(r *accessWireResponse) { r.Requests[0].ID = "not-a-request" },
		"unsupported kind": func(r *accessWireResponse) {
			r.Requests[0].Kind, r.Requests[0].Snapshot.Kind = "host-operation", "host-operation"
		},
		"revision":         func(r *accessWireResponse) { r.Requests[0].Revision = "arbitrary" },
		"snapshot task":    func(r *accessWireResponse) { r.Requests[0].Snapshot.TaskID = strings.Repeat("b", 24) },
		"snapshot request": func(r *accessWireResponse) { r.Requests[0].Snapshot.RequestID = "different" },
		"snapshot kind":    func(r *accessWireResponse) { r.Requests[0].Snapshot.Kind = "unsupported" },
		"snapshot sandbox": func(r *accessWireResponse) { r.Requests[0].Snapshot.Target.SandboxID = "different" },
		"empty sandbox": func(r *accessWireResponse) {
			r.Requests[0].Target.SandboxID, r.Requests[0].Snapshot.Target.SandboxID = "", ""
		},
		"foreign scope":        func(r *accessWireResponse) { r.Requests[0].Scope = "all_tasks" },
		"missing review":       func(r *accessWireResponse) { r.Requests[0].Review = nil },
		"missing review scope": func(r *accessWireResponse) { r.Requests[0].Review.Details = nil },
		"empty review title":   func(r *accessWireResponse) { r.Requests[0].Review.Summary = "" },
		"missing rule":         func(r *accessWireResponse) { r.Requests[0].Snapshot.Rule = nil },
		"null rule":            func(r *accessWireResponse) { r.Requests[0].Snapshot.Rule = json.RawMessage(`null`) },
		"scalar rule":          func(r *accessWireResponse) { r.Requests[0].Snapshot.Rule = json.RawMessage(`"not a rule"`) },
		"array rule":           func(r *accessWireResponse) { r.Requests[0].Snapshot.Rule = json.RawMessage(`["not a rule"]`) },
		"empty rule":           func(r *accessWireResponse) { r.Requests[0].Snapshot.Rule = json.RawMessage(`{}`) },
	} {
		t.Run(name, func(t *testing.T) {
			r := accessFixtureResponse(t)
			task := r.TaskID
			mutate(&r)
			p, _ := newAccessHelper(t, r, 0)
			if proposals, err := p.AccessList(testContext, task); err == nil || len(proposals) != 0 {
				t.Fatal("foreign or malformed authority accepted", proposals, err)
			}
		})
	}
}

func TestAccessPaddockMalformedJSONCannotEnableControls(t *testing.T) {
	task, request := accessFixtureTask(t)
	p, _ := newAccessHelper(t, json.RawMessage(`{"protocol_version":1,"requests":`), 1)
	if proposals, err := p.AccessList(testContext, task); err == nil || len(proposals) != 0 {
		t.Fatal("malformed list accepted", proposals, err)
	}
	if result, err := p.AccessDecide(testContext, request, accessDecisionFixture(t)); err == nil || result.RetryDecision || result.Terminal {
		t.Fatal("malformed decision response accepted", result, err)
	}
}

func TestAccessPaddockExactScopePreservesLargeIntegerPrecision(t *testing.T) {
	r := accessFixtureResponse(t)
	r.Requests[0].Snapshot.Rule = json.RawMessage(`{"name":"request","opaque_parameter":9223372036854775807,"endpoints":[{"host":"registry.npmjs.org","port":443}],"binaries":[{"path":"/usr/local/bin/node"}]}`)
	r.Requests[0].Review.Details = append(r.Requests[0].Review.Details, "Requested access:\n"+string(r.Requests[0].Snapshot.Rule))
	p, _ := newAccessHelper(t, r, 0)
	proposals, err := p.AccessList(testContext, r.TaskID)
	if err != nil || len(proposals) != 1 {
		t.Fatal(proposals, err)
	}
	if !strings.Contains(strings.Join(proposals[0].Details, "\n"), "9223372036854775807") {
		t.Fatal("numeric scope changed during display normalization", proposals[0].Details)
	}
	v := accessViewFixture()
	v.Details = proposals[0].Details
	blocks, err := accessCardBlocks(v)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, block := range blocks {
		if text, ok := block["text"].(map[string]any); ok {
			if text["type"] != "plain_text" {
				t.Fatal("scope rendered as markup", text)
			}
			found = found || strings.Contains(text["text"].(string), "9223372036854775807")
		}
	}
	if !found {
		t.Fatal("complete normalized scope omitted from the Slack card")
	}
}

func TestAccessPaddockOversizedScopeCannotBecomeAnActionableCard(t *testing.T) {
	r := accessFixtureResponse(t)
	r.Requests[0].Snapshot.Rule = json.RawMessage(`{"name":"request","large_scope":"` + strings.Repeat("x", 4000) + `"}`)
	r.Requests[0].Review.Details = append(r.Requests[0].Review.Details, "Requested access:\n"+string(r.Requests[0].Snapshot.Rule))
	p, _ := newAccessHelper(t, r, 0)
	proposals, err := p.AccessList(testContext, r.TaskID)
	if err != nil {
		return // Normalization may reject oversized scope before rendering.
	}
	if len(proposals) != 1 {
		t.Fatal("scope disappeared", proposals)
	}
	v := accessViewFixture()
	v.Details = proposals[0].Details
	if _, err = accessCardBlocks(v); err == nil {
		t.Fatal("oversized scope was silently truncated or allowed")
	}
}

func TestAccessPaddockListQualificationFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*accessWireResponse){
		"cached":   func(r *accessWireResponse) { r.Refresh.State = "cached" },
		"failed":   func(r *accessWireResponse) { r.Refresh.State = "failed" },
		"features": func(r *accessWireResponse) { r.Features = []string{"task_bound_requests"} },
		"refresh error": func(r *accessWireResponse) {
			r.Refresh.Error = &accessWireError{Code: "offline", Message: "could not inspect"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := accessFixtureResponse(t)
			mutate(&r)
			p, _ := newAccessHelper(t, r, 0)
			proposals, err := p.AccessList(testContext, r.TaskID)
			if err == nil || len(proposals) != 1 || proposals[0].Actionable || proposals[0].CanApprove || proposals[0].CanReject || proposals[0].Fresh {
				t.Fatal("unqualified request enabled controls", proposals, err)
			}
		})
	}
}

func TestAccessPaddockUsesServerQualifiedActions(t *testing.T) {
	for _, test := range []struct {
		name            string
		mutate          func(*accessWireRequest)
		approve, reject bool
	}{
		{"approve only", func(*accessWireRequest) {}, true, false},
		{"reject only", func(r *accessWireRequest) { r.Supported = []string{"reject"} }, false, true},
		{"unavailable", func(r *accessWireRequest) { r.Supported = []string{} }, false, false},
		{"inactive", func(r *accessWireRequest) { r.Actionable = false }, false, false},
		{"resolved", func(r *accessWireRequest) { r.Status = "approved" }, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := accessFixtureResponse(t)
			test.mutate(&r.Requests[0])
			p, _ := newAccessHelper(t, r, 0)
			proposals, err := p.AccessList(testContext, r.TaskID)
			if err != nil || len(proposals) != 1 || proposals[0].CanApprove != test.approve || proposals[0].CanReject != test.reject {
				t.Fatal(proposals, err)
			}
		})
	}
}

func TestAccessPaddockPartialRefreshDisablesControls(t *testing.T) {
	for _, code := range []string{"backend_unavailable", "storage_error", "unauthorized"} {
		t.Run(code, func(t *testing.T) {
			r := accessFixtureResponse(t)
			r.Refresh.State = "partial"
			r.Refresh.Error = &accessWireError{Code: code, Message: "Inspection failed", Retryable: true}
			p, _ := newAccessHelper(t, r, 0)
			proposals, err := p.AccessList(testContext, r.TaskID)
			if err == nil || len(proposals) != 1 || proposals[0].Fresh || proposals[0].Actionable {
				t.Fatalf("partial inspection enabled approval: %+v %v", proposals, err)
			}
		})
	}
}

func TestAccessPaddockRequiresIntegrationResults(t *testing.T) {
	r := accessFixtureResponse(t)
	r.Features = []string{"task_bound_requests", "immutable_revision", "decision_idempotency", "single_winner", "notification_receipts", "network_review_token"}
	p, _ := newAccessHelper(t, r, 0)
	proposals, err := p.AccessList(testContext, r.TaskID)
	if err == nil || len(proposals) != 1 || proposals[0].Actionable || proposals[0].Fresh {
		t.Fatal("older backend enabled controls", proposals, err)
	}
}

func TestAccessPaddockUnqualifiedLegacyRequestRemainsInformational(t *testing.T) {
	r := accessFixtureResponse(t)
	r.Requests[0].Revision, r.Requests[0].Snapshot = "", nil
	p, _ := newAccessHelper(t, r, 0)
	proposals, err := p.AccessList(testContext, r.TaskID)
	if err != nil || len(proposals) != 1 || proposals[0].Actionable || proposals[0].CanApprove || proposals[0].CanReject || proposals[0].Outcome == "" {
		t.Fatal("legacy proposal became actionable or disappeared", proposals, err)
	}
}

func TestAccessPaddockDecisionFileAndCLIContract(t *testing.T) {
	task, request := accessFixtureTask(t)
	input := accessDecisionFixture(t)
	// Literal shell metacharacters remain JSON data in the private file.
	input.Reason = "Literal $(echo nope) and `pwd`"
	p, path := newAccessHelper(t, accessExample(t, "decision_response"), 0)
	result, err := p.AccessDecide(testContext, request, input)
	if err != nil || result.Terminal || result.RetryDecision || len(result.Receipt) == 0 || !strings.Contains(result.Summary, "Agent notification: pending") {
		t.Fatal("pending notification receipt was lost", result, err)
	}
	record := readAccessHelperRecord(t, path)
	if record.DecisionMode != 0600 || record.DirectoryMode != 0700 || record.Decision != input {
		t.Fatal("decision payload was not private and literal", record)
	}
	if len(record.Args) != 6 || record.Args[0] != "access" || record.Args[1] != "decide" || record.Args[2] != request || record.Args[3] != "--decision-file" || !filepath.IsAbs(record.Args[4]) || record.Args[5] != "--json" {
		t.Fatal("decision CLI contract changed", record.Args)
	}
	if _, err = os.Stat(record.Args[4]); !os.IsNotExist(err) {
		t.Fatal("temporary decision file was retained", err)
	}
	p, path = newAccessHelper(t, accessExample(t, "decision_response"), 0)
	if _, err = p.AccessReconcile(testContext, task, request); err != nil {
		t.Fatal(err)
	}
	if args := readAccessHelperRecord(t, path).Args; !reflect.DeepEqual(args, []string{"access", "reconcile", request, "--task", task, "--json"}) {
		t.Fatal("reconcile is not task bound", args)
	}
}

func TestAccessPaddockDecisionPreservesNonzeroStructuredReceipt(t *testing.T) {
	_, request := accessFixtureTask(t)
	p, _ := newAccessHelper(t, accessExample(t, "uncertain_response"), 1)
	result, err := p.AccessDecide(testContext, request, accessDecisionFixture(t))
	if result.Terminal || result.RetryDecision || len(result.Receipt) == 0 || !strings.Contains(result.Summary, "uncertain") {
		t.Fatal("uncertain receipt was discarded or treated as success", result, err)
	}
	task, _ := accessFixtureTask(t)
	result, err = p.AccessReconcile(testContext, task, request)
	if err == nil || result.Terminal || result.RetryDecision || len(result.Receipt) == 0 {
		t.Fatal("nonzero reconciliation lost its receipt", result, err)
	}
}

func TestAccessPaddockRejectsMismatchedDecisionReceipts(t *testing.T) {
	for name, mutate := range map[string]func(*accessWireResponse){
		"outer task":            func(r *accessWireResponse) { r.TaskID = strings.Repeat("b", 24) },
		"outer request":         func(r *accessWireResponse) { r.RequestID = "different" },
		"receipt id":            func(r *accessWireResponse) { r.Receipt.ID = "wrong" },
		"receipt task":          func(r *accessWireResponse) { r.Receipt.TaskID = strings.Repeat("b", 24) },
		"receipt request":       func(r *accessWireResponse) { r.Receipt.RequestID = "different" },
		"revision":              func(r *accessWireResponse) { r.Receipt.Revision = "sha256:" + strings.Repeat("b", 64) },
		"external id":           func(r *accessWireResponse) { r.Receipt.ExternalID = "foreign" },
		"decision value":        func(r *accessWireResponse) { r.Receipt.Decision.Value = "reject" },
		"actor user":            func(r *accessWireResponse) { r.Receipt.Decision.Actor.UserID = "U-foreign" },
		"actor workspace":       func(r *accessWireResponse) { r.Receipt.Decision.Actor.WorkspaceID = "T-foreign" },
		"actor source":          func(r *accessWireResponse) { r.Receipt.Decision.Actor.Source = "foreign" },
		"missing outcome":       func(r *accessWireResponse) { r.Outcome = nil },
		"unknown outcome":       func(r *accessWireResponse) { r.Outcome.State = "future" },
		"empty outcome summary": func(r *accessWireResponse) { r.Outcome.Summary = "" },
		"inconsistent outcome":  func(r *accessWireResponse) { r.Outcome.State = "not_submitted" },
		"unsupported host operation": func(r *accessWireResponse) {
			r.Receipt.Execution = json.RawMessage(`{"state":"succeeded"}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var response accessWireResponse
			decodeAccessExample(t, "decision_response", &response)
			mutate(&response)
			p, _ := newAccessHelper(t, response, 0)
			_, request := accessFixtureTask(t)
			if result, err := p.AccessDecide(testContext, request, accessDecisionFixture(t)); err == nil || result.Terminal || result.RetryDecision {
				t.Fatal("foreign receipt accepted", result, err)
			}
		})
	}
}

func TestAccessPaddockUsesBackendRecoveryOutcomes(t *testing.T) {
	task, request := accessFixtureTask(t)
	for _, state := range []string{"unresolved", "settled", "review_ended"} {
		t.Run(state, func(t *testing.T) {
			var r accessWireResponse
			decodeAccessExample(t, "decision_response", &r)
			r.Outcome = &accessOutcome{State: state, Summary: "Backend outcome: " + state}
			// Internal state changes need no new state machine in the Slack adapter.
			r.Receipt.Decision.State = "opaque_backend_state"
			p, _ := newAccessHelper(t, r, 0)
			result, err := p.AccessReconcile(testContext, task, request)
			if err != nil || result.RetryDecision || result.Terminal != (state != "unresolved") || result.ReviewEnded != (state == "review_ended") || result.Summary != r.Outcome.Summary {
				t.Fatal(result, err)
			}
		})
	}
}

func TestAccessPaddockNoDecisionRetryRequiresExplicitMatchingEnvelope(t *testing.T) {
	task, request := accessFixtureTask(t)
	for _, test := range []struct {
		name      string
		code      string
		retryable bool
		version   int
		task      string
		request   string
		want      bool
	}{
		{"explicit retry", "no_decision", true, 1, task, request, true},
		{"not retryable", "no_decision", false, 1, task, request, false},
		{"other error", "offline", true, 1, task, request, false},
		{"wrong protocol", "no_decision", true, 0, task, request, false},
		{"foreign task", "no_decision", true, 1, strings.Repeat("b", 24), request, false},
		{"foreign request", "no_decision", true, 1, task, "different", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := "unresolved"
			if test.code == "no_decision" && test.retryable {
				state = "not_submitted"
			}
			response := map[string]any{"protocol_version": test.version, "task_id": test.task, "request_id": test.request, "receipt": nil, "error": accessWireError{Code: test.code, Message: "No decision was admitted", Retryable: test.retryable}, "outcome": accessOutcome{State: state, Summary: "Backend recovery disposition"}}
			p, _ := newAccessHelper(t, response, 1)
			result, err := p.AccessReconcile(testContext, task, request)
			if err == nil || result.RetryDecision != test.want || result.Terminal {
				t.Fatal("unsafe no-decision retry", result, err)
			}
		})
	}
}
