# Arnold

Arnold is a Slack-native engineering assistant, named after Jurassic Park's chief engineer Ray Arnold. It runs as a single Go binary on a local orchestration host and delegates engineering work to Paddock/OpenShell.

A mention starts a conversation in a Slack thread. Channels can also opt into automatic conversations for every new top-level human message. Later messages continue the thread without another mention. Arnold maps authenticated Slack users to configured Paddock identities, forwards work through the public CLI, and relays public progress and answers using Slack's expandable assistant thinking block. It adds 👀 while a request is awaiting its response.

Paddock owns durable tasks, native agent conversations, identities, worktrees and execution. OpenShell provides isolation. Rex provides independent interactive terminal access. Arnold performs deterministic Slack communication and coordination; it runs no model or agent SDK on its host.

## Build or install

Development requires Go 1.27.2 or newer:

```sh
go install github.com/nickdirienzo/arnold/cmd/arnold@latest
```

Or build from a checkout:

```sh
make build
./bin/arnold version
```

`make build` sets `CGO_ENABLED=0` and produces a standalone executable with pure-Go SQLite support. Node.js, npm, a C compiler and a separate database service are not needed at runtime. Paddock and its configured OpenShell runtime must be installed separately.

## Configure

```sh
cp config/example.json config/local.json
cp .env.example .env
chmod 600 .env config/local.json
```

Set the workspace ID, default repository, channel overrides, Slack user IDs and named Paddock identities in the local configuration. Set tokens and optional absolute paths in `.env`. Existing process environment variables take precedence over `.env` values.

Create/install the Slack app using `slack-app-manifest.json`, enable Socket Mode and create an app-level token with `connections:write`. The bot needs the manifest's scopes, including `reactions:write`. Reinstall the app after adding scopes, update the local bot token if it changes, and invite the bot to channels where it should respond. Verify it can read thread context using `conversations.replies`.

The top-level `agent` selects `claude` or `codex`; the example uses Claude, and configurations that omit the field retain Codex. Paddock must have the corresponding OpenShell provider configured. Arnold does not create GitHub Apps, retrieve private keys, or configure model credentials.

Arnold's `profile` configuration field selects Paddock's named launcher through `--launcher`. Keep the `implementation` and `investigation` launchers on OpenShell, with `investigation` configured as read-only; Arnold checks these settings at startup. Existing tasks with legacy `provenance.profile` remain resumable. A dispatch retry that encounters a legacy profile/launcher conflict inspects the existing task, then retries the same external ID and TTL without adding launcher provenance to that task.

New Arnold tasks use a 30-minute idle TTL for both Claude and Codex. Top-level `idleTtl` defaults to `"30m"` when omitted or empty; an explicit positive Go duration overrides it. Zero, negative durations and day suffixes such as `1d` are invalid. Arnold passes the policy through Paddock's `--idle-ttl` option. Paddock starts the countdown after confirmed completion and live idle checks, preserves the task/conversation/workspace, and resumes that conversation on the next Slack reply.

Automatic expiry supports qualified OpenShell Codex runtimes and Claude Code 2.1.325 integration-only sessions. It requires an updated Paddock executable, runtime image and compatible v9 runtime poller. For Claude TTL sessions, submit all input through Arnold or `paddock message`; viewing the TUI is supported, but direct typing or inbox writes are outside the expiry contract. Paddock disables background/autonomous work for these sessions while preserving foreground subagents and native approvals. Older Claude images must be rebuilt before creating TTL tasks. See [Paddock's integration contract](https://github.com/nickdirienzo/paddock/blob/main/docs/integrations.md#per-task-idle-ttl-contract) for runtime requirements.

Changing `idleTtl` affects newly prepared task dispatches. Existing tasks keep their policy, and retries keep the policy frozen before the first dispatch, including omission in older pending requests. Arnold does not expire Slack thread bindings, invoke workspace GC or close Rex panes itself. Delegated children need their own `--idle-ttl 30m`; Paddock does not inherit the parent's TTL.

```sh
arnold run
# Explicit paths are also supported:
arnold run --env-file /path/to/.env --config /path/to/config.json --state /path/to/state.sqlite
```

Use `arnold --help` for options. `ARNOLD_CONFIG`, `ARNOLD_STATE`, `PADDOCK_BIN` and `PADDOCK_HOME` remain supported. Run one Arnold instance for a state database; the process lease rejects a second instance, including an older Node instance.

Only the configured workspace can dispatch work. Set top-level `defaultRepo` to a repository alias/path to let human @mentions start conversations in any public or private channel Arnold is invited to. Channel entries override the repository and control automatic subscriptions. Unlisted channels require a mention to activate a thread; human replies then continue it without another mention. Direct messages are excluded. Omit `defaultRepo` to restrict dispatch to configured channels. Restart Arnold after changing configuration; earlier ignored mentions are not replayed.

Human participants use their `users` mapping when present, otherwise Arnold's `automation` identity and profile serve as the default. The authenticated Slack user remains the requester, with a separate task binding for each participant. If neither a user mapping nor `automation` is configured, Arnold posts an actionable notice. Edits, deletions, unrelated chatter, duplicate event copies and Arnold's own messages are ignored. Missing reaction permission is logged and retried without blocking work.

Set `autoSubscribe: true` on a channel to start a conversation for every new top-level human message, without requiring a mention:

```json
"channels": {
  "C0123456789": {
    "repo": "example-project",
    "autoSubscribe": true
  }
}
```

Invite Arnold to the channel so it receives messages. Replies continue an activated thread, including replies received while its first request is pending. Replies to older, inactive threads still need a mention. The default is `false`; bot messages require the additional setting below or explicit automatic triggers. Restart Arnold after changing configuration. Enabling the setting does not backfill existing messages.

To include every bot-posted alert in that channel, also set `autoSubscribeBots: true` and configure `automation` with a named identity and profile. This explicitly trusts all bot sources in the selected channel. Each new top-level bot message starts a conversation; bot replies continue activated threads. Arnold's own messages are always ignored. Set `automation.recipientUserId` for Slack's assistant thinking blocks on these conversations. For selected bot sources or text matches, use automatic triggers instead.

## Conversation and identity behavior

A new task receives the bounded earlier Slack context so an initial reply such as “investigate this” has a referent. **Resumed tasks receive the new authored message**. Paddock owns runtime guidance and freezes its guide pointer into authored OpenShell message deliveries. Paddock already retains previous user/assistant messages in its native session; Arnold does not resend them or fetch thread history on normal follow-ups.

Paddock provenance is immutable. Each participant's identity/profile has a task pointer in the thread. Participants can alternate and resume their respective tasks after a restart. When a new participant or a different agent requires a new task, Arnold includes the predecessor reference and Slack context under the current authenticated identity. An automation task is never reassigned to a human identity.

Human requests and autonomous work can use the same `implementation` profile, allowing investigation, code changes and pull requests. The optional `investigation` profile restricts either kind of work to investigation. Within the configured permissions, the agent decides what to investigate or change. Merging and deployment require human review. Arnold exposes no privileged host tools for model-directed operations.

## Access requests

Set `accessApprovers` to an explicit list of Slack member IDs to enable access request cards in the task's original thread:

```json
"accessApprovers": ["U0123456789"]
```

The default is empty. User mappings, channel membership, automation identities, and being the requester do not confer approval authority. Arnold verifies the clicking user, workspace, originating message, and stored proposal before submitting a durable decision to Paddock. It does not interpret conversational text such as “approved” as an access grant.

This requires Paddock's access protocol v1 (`access list`, `show`, `decide`, and `reconcile`) with the `integration_results` feature. Requests remain informational when the installed backend cannot safely support an action. Network approval applies to matching requests in the task's sandbox. Paddock owns enforcement and agent notification, including resuming a saved conversation after idle expiry. Paddock supplies the complete review and recovery outcome; Arnold validates the association and renders them as plain text. An approved request and a delivered notification are reported separately.

Stock OpenShell network requests offer approval only; its rejection API can revoke an already approved rule, so Paddock leaves that action unavailable.

Install the updated Slack manifest or enable Interactivity in the Slack app's settings. Button payloads use the existing Socket Mode connection. Restart Arnold after configuring approvers. The existing `--poll-interval` setting controls conversation polling, access discovery, and recovery; button clicks wake the decision worker immediately. Repository trust and tool configuration remain separate from network approval. Adding approver IDs does not provision credentials or grant access. See [access approvals](docs/access-approvals.md) for the contract, recovery behavior, and backend requirements.

## Automatic triggers

Set `triggers` to `[]`, leave `autoSubscribeBots` disabled and omit `automation` for a human-only deployment. An automatic trigger requires an explicit channel, exact bot ID or human user ID, a literal text substring, and a designated automation identity. Bots cannot initiate or continue work unless an explicit trigger matches or their channel enables `autoSubscribeBots`.

An automatic event starts or continues the same conversation path as a human mention. There are no incident objects, remediation workflows or independent task scheduler. Set `automation.recipientUserId` to a channel member ID for assistant thinking blocks in bot-initiated conversations. Without a human recipient, answers use ordinary replies and diagnostics remain local.

## State and recovery

SQLite stores routing pointers, pending transport payloads, delivery receipts, output cursors, reaction state and stream metadata. Successfully delivered inbound payloads are removed. Outbound agent text is not retained as a second conversation history.

Socket Mode envelopes are acknowledged after durable intake. Pending dispatches use a stable external ID, frozen prompt and frozen idle TTL, preserving Paddock's idempotency across restarts. Confirmed offline messages can retry; ambiguous `claimed`/`uncertain` receipts require inspection and are not replayed automatically. Missing OpenShell access or credentials never cause a host-execution fallback.

Output is read through `paddock output --limit 20 --json`. Arnold drains pages, reassembles multipart messages and advances cursors only after delivery. A restart resumes saved Slack streams and skips recorded delivery chunks. It surfaces expired cursors or unavailable history rather than resetting and replaying output. Native `turn_finished` events determine when a response ends, independently of process exit. See [output delivery](docs/paddock-output.md) for limits and delivery guarantees.

Temporary downtime can miss Slack events outside Slack's retry window, while accepted conversations remain recoverable. Initial context is text-only, bounded to four API pages and 24,000 UTF-16 code units; truncation is marked. Files and images are not downloaded.

## launchd

Install the binary, render the LaunchAgent and inspect it before enabling supervision:

```sh
mkdir -p ~/.local/bin state
cp bin/arnold ~/.local/bin/arnold
arnold launchd --directory "$PWD" --binary "$HOME/.local/bin/arnold" > state/dev.arnold.slack.plist
plutil -lint state/dev.arnold.slack.plist
cp state/dev.arnold.slack.plist ~/Library/LaunchAgents/dev.arnold.slack.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.arnold.slack.plist
```

The LaunchAgent runs the binary directly. Tokens remain in the owner-only `.env` file; they are not embedded in the plist. Run it as the intended service account, whose OS identity authenticates Paddock calls. Stop supervision with `launchctl bootout gui/$(id -u)/dev.arnold.slack`. Rotate local logs as appropriate.

## Upgrade from the Node implementation

Stop the old service before starting the binary. Keep the same `.env`, `config/local.json` and `state/arnold.sqlite`. The schema and serialized pending-delivery format are compatible, so no export/import is needed. Existing task bindings, output cursors, stream timestamps and reactions continue from the same database. Back up the database and replace the old LaunchAgent with output from `arnold launchd`.

## Validation

```sh
make test
make check
ARNOLD_PADDOCK_TEST_BIN="$(command -v paddock)" go test -race ./...
```

The suite covers routing/identity policy, minimal follow-ups, retry and restart behavior, Node-generated state compatibility, SQLite single-instance locking, native CLI boundaries, Socket Mode acknowledgements/reconnects, reactions and assistant streams. The optional installed-CLI test uses a disposable registry and deterministic native fixture, with no live model or Slack calls. See [validation](docs/validation.md) for deployment checks and known integration boundaries.
