package arnold

import "encoding/json"

// AccessProposal is the UI's normalized view of a versioned Paddock snapshot.
// Revision is opaque and must be passed back unchanged when deciding access.
type AccessProposal struct {
	ID         string   `json:"id"`
	TaskID     string   `json:"task_id"`
	Revision   string   `json:"revision"`
	Kind       string   `json:"kind"`
	Status     string   `json:"status"`
	Summary    string   `json:"summary"`
	Details    []string `json:"details"`
	Actionable bool     `json:"actionable"`
	CanApprove bool     `json:"can_approve"`
	CanReject  bool     `json:"can_reject"`
	Fresh      bool     `json:"fresh"`
	Outcome    string   `json:"outcome,omitempty"`
}

type AccessActor struct {
	Source      string `json:"source"`
	WorkspaceID string `json:"workspace_id"`
	UserID      string `json:"user_id"`
}

type AccessDecisionInput struct {
	ProtocolVersion int         `json:"protocol_version"`
	TaskID          string      `json:"task_id"`
	Revision        string      `json:"revision"`
	Decision        string      `json:"decision"`
	ExternalID      string      `json:"external_id"`
	Actor           AccessActor `json:"actor"`
	Reason          string      `json:"reason"`
}

// AccessResult retains Paddock's complete receipt for later reconciliation.
type AccessResult struct {
	Terminal      bool
	ReviewEnded   bool
	RetryDecision bool
	Summary       string
	Receipt       json.RawMessage
}

// AccessCardView contains display text and an opaque action token. Paddock
// authority and decision payloads are never reconstructed from Slack text.
type AccessCardView struct {
	CardID     string
	TaskID     string
	RequestID  string
	Kind       string
	Summary    string
	Details    []string
	Status     string
	Actionable bool
	CanApprove bool
	CanReject  bool
	Token      string
}

// AccessInteraction is received only through the authenticated Socket Mode
// connection. The stored card and configured approvers supply authorization.
type AccessInteraction struct {
	Type string `json:"type"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Container struct {
		ChannelID string `json:"channel_id"`
		MessageTS string `json:"message_ts"`
	} `json:"container"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}
