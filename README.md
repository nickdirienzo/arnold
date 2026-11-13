# Arnold

Arnold is a Slack-native engineering assistant, named after Jurassic Park's chief engineer Ray Arnold. It runs as a single Go binary on a local orchestration host and delegates engineering work to Paddock/OpenShell.

A mention starts a conversation in a Slack thread. Later messages continue it without another mention. Arnold maps authenticated Slack users to configured Paddock identities, forwards work through the public CLI, and relays public progress and answers using Slack's expandable assistant thinking block. It adds 👀 while a request is awaiting its response.

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

Set the workspace ID, allowed channels, repository aliases/paths, Slack user IDs and named Paddock identities in the local configuration. Set tokens and optional absolute paths in `.env`. Existing process environment variables take precedence over `.env` values.

Create/install the Slack app using `slack-app-manifest.json`, enable Socket Mode and create an app-level token with `connections:write`. The bot needs the manifest's scopes, including `reactions:write`. Reinstall the app after adding scopes, update the local bot token if it changes, and invite the bot to allowed channels. Verify it can read thread context using `conversations.replies`.

The top-level `agent` selects `claude` or `codex`; the example uses Claude, and configurations that omit the field retain Codex. Paddock must have the corresponding OpenShell provider configured. Arnold does not create GitHub Apps, retrieve private keys, or configure model credentials.

```sh
arnold run
# Explicit paths are also supported:
arnold run --env-file /path/to/.env --config /path/to/config.json --state /path/to/state.sqlite
```

Use `arnold --help` for options. `ARNOLD_CONFIG`, `ARNOLD_STATE`, `PADDOCK_BIN` and `PADDOCK_HOME` remain supported. Run one Arnold instance for a state database; the process lease rejects a second instance, including an older Node instance.

Only the configured workspace and channels can dispatch work. Every human participant needs an identity mapping. Edits, deletions, unrelated chatter, duplicate event copies and Arnold's own messages are ignored. Unknown users receive an actionable notice. Missing reaction permission is logged and retried without blocking work.

## Conversation and identity behavior

A new task receives the bounded earlier Slack context so an initial reply such as “investigate this” has a referent. **Resumed tasks receive only the new authored message.** Paddock already retains previous user/assistant messages in its native session; Arnold does not resend them or fetch thread history on normal follow-ups.

Paddock provenance is immutable. Each participant's identity/profile has a task pointer in the thread. Participants can alternate and resume their respective tasks after a restart. When a new participant or a different agent requires a new task, Arnold includes the predecessor reference and Slack context under the current authenticated identity. An automation task is never reassigned to a human identity.

The configured profile determines available capabilities. Human requests can use `investigation` or `implementation`; autonomous work uses `investigation`. Within those permissions, the agent decides what to investigate or change. Merging and deployment require human review. Arnold exposes no privileged host tools for model-directed operations.

## Automatic triggers

Set `triggers` to `[]` and omit `automation` for a human-only deployment. An automatic trigger requires an explicit channel, exact bot ID or human user ID, a literal text substring, and a designated automation identity. Unlisted bots cannot initiate or continue work.

An automatic event starts or continues the same conversation path as a human mention. There are no incident objects, remediation workflows or independent task scheduler. Set `automation.recipientUserId` to a channel member ID for assistant thinking blocks in bot-initiated conversations. Without a human recipient, answers use ordinary replies and diagnostics remain local.

## State and recovery

SQLite stores routing pointers, pending transport payloads, delivery receipts, output cursors, reaction state and stream metadata. Successfully delivered inbound payloads are removed. Outbound agent text is not retained as a second conversation history.

Socket Mode envelopes are acknowledged after durable intake. Pending dispatches use a stable external ID and frozen prompt, preserving Paddock's idempotency across restarts. Confirmed offline messages can retry; ambiguous `claimed`/`uncertain` receipts require inspection and are not replayed automatically. Missing OpenShell access or credentials never cause a host-execution fallback.

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
