# Arnold

Arnold is a local Slack adapter for Paddock, named after Jurassic Park's chief engineer Ray Arnold. It is an independent Node.js project with no organization-specific dependencies. Slack threads identify conversations; Paddock owns tasks, native agent history, identities, worktrees and execution. Arnold never invokes a model or executes model-selected commands on its host.

Arnold supports Claude and Codex through Paddock, allowlisted automatic triggers, multi-participant thread continuation, expandable assistant progress, final replies and 👀 acknowledgements. Local SQLite stores routing and delivery metadata so conversations can recover after a restart. See [output delivery](docs/paddock-output.md) and [validation](docs/validation.md) for behavior, test coverage and deployment checks.

## Run locally

Requires Node.js 24+ and a Paddock build supporting named GitHub App identities, OpenShell and `output TASK_ID --after CURSOR --limit 20 --json`. Verify that your installed Paddock CLI provides these interfaces. Every dispatch explicitly requests OpenShell. A missing sandbox or credential fails through Paddock with no host fallback.

```sh
npm ci
cp config/example.json config/local.json
cp .env.example .env
chmod 600 .env config/local.json
```

Edit local configuration with the Slack workspace ID, allowed channel IDs, registered Paddock repository aliases (or absolute paths), Slack user IDs and their named Paddock identities. Set absolute executable and state paths in `.env`. Arnold does not create or change Paddock identity credentials. Have the operator verify each named App identity against the configured repository using Paddock's identity commands.

Set the top-level `agent` to `claude` or `codex`; the example uses Claude. Configurations that omit it retain the earlier Codex default. Paddock owns the corresponding OpenShell model provider; Arnold runs no agent SDK on the host. Restart Arnold after changing configuration. A thread previously handled by the other agent starts a new Paddock task with the Slack context and predecessor reference, then resumes that new task for follow-ups.

Create/install a Slack app using `slack-app-manifest.json`, enable Socket Mode and create an app token with `connections:write`. Set `SLACK_BOT_TOKEN` and `SLACK_APP_TOKEN` locally, then invite Arnold to allowed channels. Thread context uses `conversations.replies`; verify the installed token has access to read threads in those channels. A context-fetch permission error leaves the delivery pending; Arnold does not silently discard the context.

```sh
npm test
npm run check
npm start
```

To exercise the installed Paddock output command against a disposable test registry, without live agent or Slack calls:

```sh
ARNOLD_PADDOCK_TEST_BIN="$(command -v paddock)" npm test
```

Arnold adds 👀 to accepted messages before dispatch and removes it after their response finishes or an actionable rejection is delivered. This requires the `reactions:write` bot scope and reinstalling the Slack app after adding that scope. Failed reactions are retried without blocking work or generating extra channel messages. Reaction metadata survives restarts.

Only configured channels in the configured workspace can cause dispatch. A human mention starts a conversation. Later human messages in that thread continue without a mention; every participant must have an identity mapping. Incoming `message` and `app_mention` copies deduplicate by workspace/channel/message timestamp. Edits, deletions, unrelated chatter and Arnold's own messages are ignored.

Triggers match a configured channel, exact authenticated bot ID (or human user ID), and a literal text substring. No regexes, model routing or dynamically selected identities are involved. The example is illustrative and authorizes nothing until an operator replaces the IDs. Automation always selects the configured investigation identity. Set `automation.recipientUserId` to a channel member ID to enable the assistant thinking block for bot-initiated conversations; without a human recipient, answers use ordinary Slack replies and diagnostics stay local. Bot messages without an explicit matching trigger cannot initiate or continue work.

For a human-only setup, set `triggers` to `[]` and omit `automation`. Enabling any trigger requires an explicitly configured automation identity.

Human requests use the profile explicitly configured by the operator. Within that profile, the Paddock agent decides whether to investigate, edit, test or prepare a PR. Merging and deployment require human review; Arnold exposes no host tool for those actions. Capabilities and network/database/observability access belong to Paddock/OpenShell configuration.

## Identity transitions

Paddock task provenance is immutable. Each participant has a task pointer for their identity and profile in the thread. Follow-ups resume that task through Paddock's native `message` interface, including when participants alternate or Arnold restarts. A participant's first request creates a task under their configured identity and includes the previous participant's task reference and bounded Slack thread context. Arnold retains those references for status reporting. A human following an automated alert uses their own task; Arnold never rewrites the automation task as a human task. Internal agent reasoning remains in Paddock; only publicly shared Slack context transfers across identities.

## Local state and recovery

SQLite holds thread-to-task pointers, transport receipt IDs, temporary pending delivery payloads, output cursors and the latest reported task status. Successful delivery deletes the pending inbound message payload. Outbound agent text is never retained in Arnold's database; only delivery IDs, cursors and Slack stream metadata (timestamps, counters and completion flags) are stored. One exclusive database lease prevents two Arnold processes using the same state. Run one instance and protect its state/configuration as trusted orchestration data.

Socket Mode envelopes are acknowledged after synchronous durable intake. A service restart resumes pending transport delivery using the same Paddock external ID and byte-identical prompt. The pending prompt is frozen before first dispatch so later Slack messages cannot invalidate Paddock idempotency. A known offline `pending` receipt can retry. Ambiguous `claimed`/`uncertain` messages are surfaced for operator inspection and never automatically replayed. A failed launch with a durable Paddock task is not replaced by a new task ID. Recover it in Paddock.

Task status and pending access are polled through `show --json`. Arnold polls `output --limit 20 --json` separately, follows `has_more`, reassembles multipart messages, and posts public progress and answers into the originating Slack thread. Slack's assistant streaming UI groups task IDs, runtime/access status and public progress in an expandable thinking block, using the `plan` display mode. Answer text appears below that block in the same message. `turn_finished` finalizes the stream without posting a separate completion notice, even while the agent process remains running. Newly launching agents wait for their saved native conversation before output is read.

Output cursors advance after successful delivery of complete pages. If a multipart message crosses a page boundary, Arnold holds the cursor until the message is assembled and delivered. Per-message Slack chunk receipts let a restarted service refetch those pages and skip already delivered chunks. The Slack stream timestamp is also persisted so a restart can append to the existing message. Streams rotate before Slack's ten-minute timeout, 12,000-character answer limit or 50-task limit. Expired/mismatched cursors and unavailable history are surfaced without resetting the saved cursor. Process exit alone is reported as stopped, not as evidence of successful work.

Slack failures retry delivery without rerunning tasks. Stable `client_msg_id` values reduce duplicate Slack posts, but a crash between Slack accepting a post and saving its receipt can still duplicate a message. No exactly-once Slack delivery is claimed.

Temporary downtime can miss Slack events outside Slack's retry window. Existing accepted conversations/tasks remain recoverable. Arnold does not backfill all channel history or invent a second task queue. Thread context is text-only, capped at four API pages and 24,000 characters, with truncation marked; files and images are not downloaded.

## launchd

After manual configuration and live validation, render and inspect a LaunchAgent:

```sh
mkdir -p state
node scripts/launchd.js > state/dev.arnold.slack.plist
plutil -lint state/dev.arnold.slack.plist
cp state/dev.arnold.slack.plist ~/Library/LaunchAgents/dev.arnold.slack.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.arnold.slack.plist
```

Run as the intended trusted service account; Paddock authenticates its local caller using OS identity, while Arnold asserts authenticated Slack requester IDs. The `.env` file supplies tokens without embedding them in the plist. Keep its permissions at 0600. Stop supervision with `launchctl bootout gui/$(id -u)/dev.arnold.slack`. No LaunchAgent is installed automatically. Rotate local logs as appropriate.

## Deployment validation

The automated suite uses real SQLite, injected Slack/Paddock boundaries and an optional installed-CLI contract test with a deterministic agent fixture. It proves transport and output recovery behavior, not a completed live model turn or actual Slack delivery. For a deployment, verify:

1. Configure real Slack and Paddock identities, repository access and OpenShell capabilities.
2. Mention Arnold, observe the attributed sandbox task, receive progress and a final answer in the same Slack thread, then follow up without a mention.
3. Repeat with an allowlisted monitoring bot; verify an unlisted bot cannot dispatch, and a human follow-up uses the human identity while retaining automation provenance.
4. Restart Arnold during delivery and during task execution; verify recovery without duplicate execution and with response cursors preserved.

No credentials, active monitoring sources, GitHub Apps, or external messages are created by this project.
