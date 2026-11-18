# Slack access approvals through Paddock

Arnold presents access requests in the Slack thread that owns a task. It authenticates button decisions and presents their outcomes. Paddock validates requests, applies access, records decisions, and notifies the existing agent conversation. OpenShell remains authoritative for sandbox network rules.

Enable the integration with an explicit `accessApprovers` list of Slack member IDs and a Paddock executable supporting access protocol v1. The default list is empty. User mappings and thread participation do not confer approval authority. An older or unqualified backend leaves controls disabled and provides operator guidance. Each request independently advertises which decisions it supports; the UI does not invent backend guarantees.

## Ownership and boundaries

| Component | Behavior | Boundary |
| --- | --- | --- |
| Arnold task dispatch | Records the Slack team, channel, and thread in Paddock provenance context and keeps local participant bindings. | Use this association to route request cards without changing the task identity. |
| Arnold polling | Polls access requests independently from conversation output. | Reconcile cards after turn completion and session expiry, without replaying agent work or backfilling historical settled requests. |
| Arnold Socket Mode | Decodes event and interactive payloads separately. | Durably accept valid button decisions before acknowledgment; perform no host calls on intake. |
| Arnold Slack posting | Posts informational cards, binds returned message timestamps, then adds qualified controls. | Recover uncertain posts by bot/thread/card identity instead of blindly reposting. |
| Paddock access inbox | Exposes versioned task-bound requests and receipts alongside the legacy operator commands. | Complete proposal inspection, guarded decisions, and durable execution remain host responsibilities. |
| Paddock agent notification | Owns native delivery and recovery after a decision. | Use stable message keys; distinguish access outcomes from notification delivery. |
| Agent access requests | Uses OpenShell's advisor for network proposals. | Requests cannot grant themselves authority or execute arbitrary host commands. |

Relevant Arnold sources are [access orchestration](../internal/arnold/access.go), [cards](../internal/arnold/slack_access.go), [interactive transport](../internal/arnold/slack.go), [access CLI adapter](../internal/arnold/access_paddock.go), [types](../internal/arnold/access_types.go), and [durable state](../internal/arnold/access_store.go). Paddock's companion implementation owns the access engine, OpenShell adapter, and native notification outbox.

## Request and decision flow

1. The agent submits a network proposal through OpenShell's advisor. Submission requests review; it grants no permission. Missing advisor instructions must surface as a readiness failure with an actionable remedy.
2. Arnold polls requests only for tasks associated with its Slack threads. It obtains a fresh proposal snapshot and complete plain-text review from Paddock, including the exact endpoints, executable constraints, protocol and methods, scope, and rationale. Agent-supplied rationale is displayed as untrusted plain text.
3. Arnold stores the snapshot and a random action token, then posts one card in the original thread. Each card names the task and states that network approval applies to that sandbox. It must not describe a network rule as a one-time operation or promise automatic expiry that does not exist.
4. A button click arrives on the authenticated Slack Socket Mode connection. Arnold checks the workspace, configured approver, action, token, and originating channel/message against its stored card. A request in another thread or a stale card cannot authorize a different task.
5. Arnold atomically records the first valid decision and a durable work item, then acknowledges the envelope within Slack's acknowledgment window. It runs no host calls on the socket intake path. Duplicate envelopes and repeated clicks resolve to that same work item.
6. Arnold passes the frozen decision to Paddock. Paddock verifies the current task, sandbox identity, and proposal revision, claims the decision, and applies the exact approved proposal. Conflicting decisions cannot both win, including a race with the operator CLI.
7. Paddock reconciles the actual grant and sends a durable native message to the original conversation. Arnold updates the card to show grant status and notification status separately. It does not send a second continuation or simulate terminal input.

[Slack Socket Mode](https://docs.slack.dev/apis/events-api/using-socket-mode/) supports interactive payloads on the existing connection. [Button interactions](https://docs.slack.dev/reference/interaction-payloads/block_actions-payload/) identify the user and originating message.

Paddock installs a bundled guide at `/sandbox/.paddock/access-guide.md` during sandbox bootstrap and shortcut upgrades by the shared poller. `paddock --help` exposes its path. Paddock includes the pointer in its launch instructions and newly authored OpenShell message deliveries. Keyed deliveries freeze the augmented native body alongside the original authored idempotency input, so retries retain their exact content across upgrades. Arnold does not know sandbox paths or inject runtime instructions. This covers OpenShell's [workload guide-installation issue](https://github.com/NVIDIA/OpenShell/issues/3698) without changing global gateway settings.

## Paddock CLI contract

The companion Paddock implementation retains legacy operator commands and exposes these versioned integration operations:

```sh
paddock access list --task TASK --status all --refresh --json
paddock access show REQUEST --task TASK --refresh --json
paddock access decide REQUEST --decision-file /absolute/path/decision.json --json
paddock access reconcile REQUEST --task TASK --json
```

`list` returns a versioned envelope with the full task ID, supported request kinds/features, refresh status/time/error, and requests including terminal records. This also provides feature discovery: Arnold must not enable buttons when required guarantees are absent. `show --refresh` returns a fresh authoritative snapshot or an explicit inspection failure. Current pending lists and cached `show` output support discovery; they are not proof that a displayed proposal is still current. A terminal request remains inspectable after it disappears from the pending list. A proposal that disappears upstream must be reconciled against history or supersession rather than silently deleted.

Only a fresh authoritative inspection can enable controls. Partial refreshes and inspection errors leave approval disabled.

The decision file is private, bounded JSON. `task_id` inside the file supplies the task binding for `decide`:

```json
{
  "protocol_version": 1,
  "task_id": "full-paddock-task-id",
  "revision": "sha256:complete-proposal-digest",
  "decision": "approve",
  "external_id": "stable-arnold-decision-id",
  "actor": {
    "source": "arnold-slack",
    "workspace_id": "T0123456789",
    "user_id": "U0123456789"
  },
  "reason": "Approved through the task's Slack access card"
}
```

Reject decisions require a reason; the initial UI can supply a fixed reason such as “Rejected by operator through Slack.” Actor fields are assertions from the trusted Arnold process. Paddock separately records the authenticated host caller; a Slack ID is not host authentication and never changes task provenance.

The receipt scopes the external ID to the authenticated caller and task, and binds it to the complete decision payload. An identical retry returns the same receipt; changed content conflicts. Its JSON reports these states separately:

- Request identity, exact revision, task and sandbox identity.
- Accepted decision, asserted Slack actor, authenticated caller, reason, and timestamps.
- Decision execution: claimed, applying, confirmed, stale, conflict, or uncertain.
- Observed proposal outcome: pending, approved, rejected, or a proven withdrawn/superseded condition.
- Runtime policy application: loaded, pending, error, or unknown, with the policy version/hash and a diagnostic. Gateway approval alone does not prove the sandbox loaded the policy.
- Notification receipt: pending, delivered, claimed, uncertain, or unavailable, with a stable message key.

Arnold requires `integration_results` and validates protocol version, task/request/sandbox identity, freshness, complete review text, normalized outcome shape, and decision attribution. Paddock owns action qualification, review formatting, and receipt-state interpretation. A nonzero CLI exit can still carry a durable receipt. Arnold reconciles that receipt before another action. The backend's `not_submitted` outcome proves no decision was admitted and allows replay of the original frozen decision and idempotency key; an inspection failure does not. Pending native notifications can retry with their original message key; claimed or uncertain notifications require inspection.

### Exact proposal binding

Paddock binds the revision to the complete proposal, full task ID, actual OpenShell sandbox ID, and captured review token. The display parser used by older Paddock versions is insufficient. The network contract is approval of the exact reviewed rule for that sandbox. It does not freeze policy changes made independently by authorized administrators. Evaluated policy hashes are context, separate from the policy version/hash reported after application. Paddock checks backend `exact_reviewed_rule` semantics before returning qualified actions; stronger full-revision semantics may also be supported by a backend. Arnold consumes those actions without implementing a second policy interpreter.

OpenShell's `PolicyChunk.review_token` identifies an evaluated proposal, and `ApproveDraftChunkRequest.review_token` validates that reviewed token. Its API also accepts a UUID `request_id` for durable admission with a bounded replay window. Paddock retains the reviewed token on the host and submits it through a structured adapter; it does not use the CLI path that fetches a replacement token at approval time. These fields are defined in the [OpenShell protocol](https://github.com/NVIDIA/OpenShell/blob/v0.1.6/proto/openshell.proto).

Paddock rereads the task and proposal under its operation lease and passes the stored review token and stable backend request ID. A fresh read followed by unconditional approval is insufficient. Backend rejection must decline a pending request without revoking a separately approved rule; unsupported rejection stays unavailable. A changed policy can invalidate other pending evaluations; those need new cards and operator review rather than silent substitution of tokens.

Persist decision intent before the external call. Reconcile after a crash or timeout rather than assuming approval failed. Route legacy `approve` and `reject` through the same claim/reconciliation path so a Slack click cannot race past a terminal decision. Authorization changes through any backend must invalidate or resolve the old Slack card.

## Authorization and recovery

`accessApprovers` defaults to empty. Ordinary user mappings, the task requester, channel subscription, and automation identity confer no approval power. Paddock separately authorizes the local integration principal for bound tasks. Processes sharing that OS account remain in the same host trust boundary; asserted Slack identities do not create another boundary.

Use dedicated SQLite records for request snapshots, Slack cards, and decision work items. Retain at least the task/request/revision, owning team/channel/thread, card message timestamp, hashed random action token, asserted approver, frozen decision payload, and Paddock receipt. Claim decisions with a conditional transaction. Do not reuse conversational intake receipts as authorization evidence.

Keep card delivery separate from decision execution. Persist posting intent and reconcile an uncertain `chat.postMessage` outcome before retrying. An ambiguous post must never cause an access change; duplicate visible cards, if reconciliation cannot identify one canonical message, must be disabled or resolve to the same server-side decision. A card is actionable only once its originating message has been verified and stored. Token possession alone is insufficient without approver and message checks.

Use plain-text Block Kit fields for proposal rationale and escape any formatted fields, preventing agent text from adding mentions, links, or controls. Button values contain only opaque tokens. Paddock receives structured arguments/files, never a shell command assembled from Slack content. Retain audit data with owner-only permissions and never include credentials in cards or receipts.

The access client/card interfaces extend the existing adapters without changing ordinary dispatch. Socket Mode dispatches `events_api` and `interactive` separately. Interaction trust comes from the authenticated connection; future HTTP callbacks would require Slack signature and replay validation. Separate access polling and decision workers keep slow grants and agent resumes off the socket/conversation paths. They share the existing `--poll-interval` setting; a click wakes the decision worker immediately.

## Repository trust and tool configuration

Repository and operator configuration select trusted tools and resources through Paddock's existing trust controls. Slack routing and approver IDs stay in Arnold configuration. Reviewed hosts, ports, methods, and executable scope stay in the OpenShell policy selected by the launcher; runtime images and trusted MCP connections use the existing launcher and repository trust configuration. Identity binding, exact revision checks, idempotency, and uncertain-outcome handling remain code invariants. This approval integration handles task-bound sandbox network rules. Provider clients, account details, credentials, and workload-specific operations belong in the configured development environment or trusted tools.

Network approval does not provide authentication or grant access to host tools. Paddock's existing `repo trust --mcp` controls which configured MCP connections a repository can use; it remains separate from a task-specific network approval. This change does not add a host-command executor or provider-specific adapter.

## Session expiry and recovery

Pending OpenShell proposals currently protect workspace retention but do not prevent idle TTL from stopping a safely completed agent. Keep the card available after that stop. Revalidate the retained sandbox before applying a decision and use the same task's saved native conversation for notification/resume. An active input or native approval wait is a different state from an OpenShell network proposal.

An approved rule does not prove that the agent received its notification or that the original experiment succeeded. Cards should show “Access approved; agent notification pending” when appropriate. If no resumable conversation or valid sandbox exists, show that limitation without creating a replacement task or granting access elsewhere. Permission to access one sandbox never transfers to a replacement sandbox automatically.

Retain the workspace while a decision intent or notification remains unresolved, even after the original proposal becomes approved/rejected and stops protecting retention. Release the operation lease before delivering a native message, because messaging acquires that lease itself. Out-of-band OpenShell changes are recorded as externally observed outcomes; do not attribute them to the Slack approver.

## Validation and rollout

1. **Paddock:** install the companion CLI and qualify its OpenShell connection, advisor, supported actions, and notification delivery. Unit fixtures do not establish that the deployed gateway is reachable.
2. **Arnold:** install the updated Slack manifest or enable Interactivity, configure explicit approver IDs, and restart with the compatible Paddock binary. Existing conversation state remains intact; historical settled requests are not backfilled.
3. **Live network validation:** use a disposable task to request a denied dependency endpoint, approve it in its owning thread, and verify the intended install succeeds. Repeat after agent expiry. Stock OpenShell network rejection remains unavailable; its existing rejection API can revoke a previously approved rule. Do not grant access to a real task as part of automated tests.

Required automated tests cover unauthorized users/workspaces, wrong thread/message/task/sandbox, stale proposal revisions, missing protocol features, escaped proposal text, duplicate envelopes/clicks, concurrent approve/reject including the legacy CLI, request mutation, restart after durable intake, ambiguous application, ambiguous card posting, and native notification that is pending or uncertain. Fixtures should model actual CLI JSON and local Slack HTTP/WebSocket traffic. Add a disposable installed-CLI contract test in the style of `installed_paddock_test.go`.
