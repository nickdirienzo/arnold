package arnold

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

type accessClient interface {
	AccessList(context.Context, string) ([]AccessProposal, error)
	AccessDecide(context.Context, string, AccessDecisionInput) (AccessResult, error)
	AccessReconcile(context.Context, string, string) (AccessResult, error)
}

type accessWireError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}
type accessTarget struct {
	SandboxID   string `json:"sandbox_id"`
	SandboxName string `json:"sandbox_name"`
	Gateway     string `json:"gateway"`
	Workspace   string `json:"workspace"`
}
type accessSnapshot struct {
	Version   int             `json:"schema_version"`
	TaskID    string          `json:"task_id"`
	RequestID string          `json:"request_id"`
	Kind      string          `json:"kind"`
	Target    accessTarget    `json:"target"`
	Rule      json.RawMessage `json:"rule"`
}
type accessReview struct {
	Summary string   `json:"summary"`
	Details []string `json:"details"`
}
type accessOutcome struct {
	State   string `json:"state"`
	Summary string `json:"summary"`
}
type accessWireRequest struct {
	Review    *accessReview   `json:"review"`
	Outcome   *accessOutcome  `json:"outcome"`
	ID        string          `json:"id"`
	TaskID    string          `json:"task_id"`
	Revision  string          `json:"revision"`
	Kind      string          `json:"kind"`
	Status    string          `json:"status"`
	Scope     string          `json:"scope"`
	Target    accessTarget    `json:"target"`
	Snapshot  *accessSnapshot `json:"snapshot"`
	Supported []string        `json:"supported_decisions"`

	Actionable bool               `json:"actionable"`
	Receipt    *accessWireReceipt `json:"receipt"`
	Error      *accessWireError   `json:"error"`
}
type accessWireReceipt struct {
	Version    int    `json:"protocol_version"`
	ID         string `json:"id"`
	TaskID     string `json:"task_id"`
	RequestID  string `json:"request_id"`
	Revision   string `json:"revision"`
	ExternalID string `json:"external_id"`
	Decision   struct {
		Value string           `json:"value"`
		State string           `json:"state"`
		Actor AccessActor      `json:"actor"`
		Error *accessWireError `json:"error"`
	} `json:"decision"`
	Grant        json.RawMessage `json:"grant"`
	Notification json.RawMessage `json:"notification"`
	// Network receipts cannot admit a host operation from an unsupported backend.
	Execution json.RawMessage `json:"execution,omitempty"`
}
type accessWireResponse struct {
	Outcome   *accessOutcome `json:"outcome"`
	Version   int            `json:"protocol_version"`
	TaskID    string         `json:"task_id"`
	RequestID string         `json:"request_id"`
	Features  []string       `json:"features"`
	Refresh   struct {
		State string           `json:"state"`
		Error *accessWireError `json:"error"`
	} `json:"refresh"`
	Advisor struct {
		State string           `json:"state"`
		Error *accessWireError `json:"error"`
	} `json:"advisor"`
	Requests []accessWireRequest `json:"requests"`
	Receipt  *accessWireReceipt  `json:"receipt"`
	Error    *accessWireError    `json:"error"`
}

var accessTaskID = regexp.MustCompile(`^[0-9a-f]{24}$`)
var accessRequestID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var accessRevision = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func (p Paddock) accessCall(ctx context.Context, args []string) (accessWireResponse, error) {
	timeout := 45 * time.Second
	if len(args) > 1 && args[1] == "decide" {
		timeout = 180 * time.Second
	}
	if len(args) > 1 && args[1] == "reconcile" {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var response accessWireResponse
	err := p.call(ctx, args, &response, 4<<20)
	var ce *commandError
	if errors.As(err, &ce) && len(ce.stdout) > 0 {
		// Paddock returns a receipt even when a post-admission command fails.
		if json.Unmarshal(ce.stdout, &response) != nil {
			return accessWireResponse{}, err
		}
	}
	return response, err
}

func accessWireIdentity(r accessWireResponse, task string) error {
	if !accessTaskID.MatchString(task) || r.Version != 1 || r.TaskID != task {
		return fmt.Errorf("Paddock access protocol is unavailable or returned an unexpected task")
	}
	return nil
}

func (p Paddock) AccessList(ctx context.Context, task string) ([]AccessProposal, error) {
	r, callErr := p.accessCall(ctx, []string{"access", "list", "--task", task, "--status", "all", "--refresh", "--json"})
	if err := accessWireIdentity(r, task); err != nil {
		return nil, err
	}
	if r.Requests == nil {
		return nil, fmt.Errorf("Paddock returned an invalid access request list")
	}
	featuresOK := slices.Contains(r.Features, "integration_results")
	fresh := callErr == nil && r.Error == nil && r.Refresh.State == "fresh" && r.Refresh.Error == nil && featuresOK
	var proposals []AccessProposal
	seen := map[string]bool{}
	for _, request := range r.Requests {
		if seen[request.ID] {
			return nil, fmt.Errorf("Paddock returned a duplicate access request")
		}
		seen[request.ID] = true
		proposal, err := normalizeAccessRequest(task, request, fresh)
		if err != nil {
			return nil, err
		}
		proposals = append(proposals, proposal)
	}
	if !fresh {
		return proposals, fmt.Errorf("Paddock access inspection is not fresh or lacks required approval guarantees")
	}
	return proposals, nil
}

func normalizeAccessRequest(task string, r accessWireRequest, fresh bool) (AccessProposal, error) {
	if r.TaskID != task || !accessRequestID.MatchString(r.ID) || r.Kind != "network" {
		return AccessProposal{}, fmt.Errorf("Paddock returned an invalid access request identity")
	}
	// An unqualified legacy request can be visible without an immutable snapshot;
	// it cannot produce an actionable card.
	revision := r.Revision
	if revision == "" && r.Snapshot == nil {
		revision = "unqualified"
	}
	if revision != "unqualified" && !accessRevision.MatchString(revision) {
		return AccessProposal{}, fmt.Errorf("Paddock returned an invalid proposal revision")
	}
	p := AccessProposal{ID: r.ID, TaskID: task, Revision: revision, Kind: r.Kind, Status: r.Status, Fresh: fresh}
	if r.Review == nil || r.Review.Summary == "" || (r.Actionable && len(r.Review.Details) == 0) || !validAccessOutcome(r.Outcome) {
		return AccessProposal{}, fmt.Errorf("Paddock returned an incomplete integration review")
	}
	p.Summary, p.Details, p.Outcome = r.Review.Summary, append([]string(nil), r.Review.Details...), r.Outcome.Summary
	if r.Snapshot == nil {
		return p, nil
	}
	s := r.Snapshot
	if s.Version != 1 || s.TaskID != task || s.RequestID != r.ID || s.Kind != r.Kind || s.Target != r.Target || r.Target.SandboxID == "" || r.Target.SandboxName == "" || r.Target.Gateway == "" || r.Target.Workspace == "" {
		return AccessProposal{}, fmt.Errorf("Paddock access snapshot does not match its request and sandbox")
	}
	if r.Scope != "sandbox" {
		return AccessProposal{}, fmt.Errorf("Paddock access scope is unsupported")
	}

	var rule map[string]json.RawMessage
	if json.Unmarshal(s.Rule, &rule) != nil || len(rule) == 0 {
		return AccessProposal{}, fmt.Errorf("Paddock access snapshot is missing its complete scope")
	}
	qualified := fresh && r.Actionable && r.Error == nil && r.Status == "pending" && r.Receipt == nil
	p.CanApprove = qualified && slices.Contains(r.Supported, "approve")
	p.CanReject = qualified && slices.Contains(r.Supported, "reject")
	p.Actionable = p.CanApprove || p.CanReject
	if r.Receipt != nil {
		if err := validateAccessReceipt(*r.Receipt, task, r.ID); err != nil {
			return AccessProposal{}, err
		}
	}
	return p, nil
}

func validAccessOutcome(outcome *accessOutcome) bool {
	return outcome != nil && outcome.Summary != "" && slices.Contains([]string{"unresolved", "settled", "review_ended", "not_submitted"}, outcome.State)
}

func validateAccessReceipt(r accessWireReceipt, task, request string) error {
	if r.Version != 1 || r.TaskID != task || r.RequestID != request || !accessTaskID.MatchString(r.ID) || !accessRevision.MatchString(r.Revision) {
		return fmt.Errorf("Paddock access receipt identity is invalid")
	}
	if r.ExternalID == "" || !slices.Contains([]string{"approve", "reject"}, r.Decision.Value) {
		return fmt.Errorf("Paddock returned an invalid decision identity")
	}
	if len(r.Execution) != 0 && !bytes.Equal(bytes.TrimSpace(r.Execution), []byte("null")) {
		return fmt.Errorf("Paddock returned an unsupported host operation")
	}
	return nil
}

func accessResultFromResponse(r accessWireResponse, task, request string) (AccessResult, error) {
	if err := accessWireIdentity(r, task); err != nil {
		return AccessResult{}, err
	}
	if r.RequestID != request {
		return AccessResult{}, fmt.Errorf("Paddock returned an unexpected access request")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return AccessResult{}, err
	}
	if !validAccessOutcome(r.Outcome) {
		return AccessResult{}, fmt.Errorf("Paddock did not return a supported integration outcome")
	}
	result := AccessResult{Receipt: raw, Summary: r.Outcome.Summary, Terminal: r.Outcome.State == "settled" || r.Outcome.State == "review_ended", RetryDecision: r.Outcome.State == "not_submitted", ReviewEnded: r.Outcome.State == "review_ended"}
	if r.Receipt == nil {
		if result.Terminal {
			return AccessResult{}, fmt.Errorf("Paddock returned a terminal outcome without a receipt")
		}
		return result, fmt.Errorf("Paddock has not confirmed an access decision receipt")
	}
	if result.RetryDecision {
		return AccessResult{}, fmt.Errorf("Paddock returned an inconsistent decision outcome")
	}
	if err := validateAccessReceipt(*r.Receipt, task, request); err != nil {
		return AccessResult{}, err
	}
	return result, nil
}

func (p Paddock) AccessDecide(ctx context.Context, request string, input AccessDecisionInput) (AccessResult, error) {
	if !accessRequestID.MatchString(request) || !accessTaskID.MatchString(input.TaskID) || !accessRevision.MatchString(input.Revision) || input.ProtocolVersion != 1 || (input.Decision != "approve" && input.Decision != "reject") {
		return AccessResult{}, fmt.Errorf("invalid access decision")
	}
	dir, err := os.MkdirTemp("", "arnold-access-")
	if err != nil {
		return AccessResult{}, err
	}
	defer os.RemoveAll(dir)
	raw, err := json.Marshal(input)
	if err != nil {
		return AccessResult{}, err
	}
	path := filepath.Join(dir, "decision.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		return AccessResult{}, err
	}
	r, callErr := p.accessCall(ctx, []string{"access", "decide", request, "--decision-file", path, "--json"})
	result, parseErr := accessResultFromResponse(r, input.TaskID, request)
	if parseErr != nil {
		return result, errors.Join(callErr, parseErr)
	}
	if r.Receipt.ExternalID != input.ExternalID || r.Receipt.Revision != input.Revision || r.Receipt.Decision.Value != input.Decision || r.Receipt.Decision.Actor != input.Actor {
		return AccessResult{}, fmt.Errorf("Paddock returned a receipt for a different access decision")
	}
	return result, nil
}

func (p Paddock) AccessReconcile(ctx context.Context, task, request string) (AccessResult, error) {
	if !accessRequestID.MatchString(request) || !accessTaskID.MatchString(task) {
		return AccessResult{}, fmt.Errorf("invalid access request")
	}
	r, callErr := p.accessCall(ctx, []string{"access", "reconcile", request, "--task", task, "--json"})
	result, parseErr := accessResultFromResponse(r, task, request)
	return result, errors.Join(callErr, parseErr)
}
