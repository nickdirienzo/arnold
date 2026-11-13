# Paddock output delivery

Arnold uses the installed Paddock CLI's public integration interface:

```sh
paddock output TASK_ID --limit 20 --json
paddock output TASK_ID --after CURSOR --limit 20 --json
```

Paddock returns `task_id`, `events`, `next_cursor` and `has_more`. Public `message` events carry stable `id`, `turn_id`, `phase` and `text`. The phases are `commentary`, `partial_answer` and `final_answer`. Large messages carry ordered, one-based `part` and `parts`. `turn_finished` events carry `turn_id` and the terminal state `completed`, `failed` or `interrupted`. A turn can end while the interactive agent process remains running.

Arnold uses Slack's native `chat.startStream`, `chat.appendStream` and `chat.stopStream` APIs with `task_display_mode: plan`. Public commentary and Paddock task/status details become `task_update` chunks in the expandable thinking block. Answer text becomes `markdown_text` in the same message; native turn completion closes the stream and updates its task status without a separate debug post. Questions and communicated blockers are ordinary public text. It never reads private transcripts, sandbox worktrees or Rex terminals, and never starts model work to read output. Paddock owns the native agent protocols and filtering of public content.

The delivery cursor is stored per task. Arnold drains `has_more` pages in order and commits the next cursor after delivering the page. Multipart messages are reassembled before Slack posting. If a message crosses a page boundary, the saved delivery cursor stays at the prior safe boundary until the full message is delivered. Only the temporary reassembly buffer lives in memory. A restart refetches those pages through Paddock.

Slack messages are split at 3,500 Unicode code points, preserving surrogate pairs. Successful chunks receive durable receipts keyed by task, public message ID and chunk index. Receipts contain no message text. Per-turn stream timestamps, character/task counters and completion flags are also persisted, so polling can resume an existing Slack stream after restart. They do not contain a duplicate conversation history. They prevent reposting earlier successful chunks when a page or multipart message is retried. Stable `client_msg_id` values are also supplied to Slack. Delivery is not claimed to be exactly once: a crash after Slack accepts a chunk but before SQLite saves the receipt can still duplicate it.

An empty page preserves its cursor and does not imply completion. A malformed page, missing message part, mismatched cursor or unavailable history leaves the saved cursor intact and produces an operator-facing notice in the Slack thread. Errors for one task do not prevent another task's output delivery. A newly launching task without a saved native conversation waits for that conversation; task/access status remains visible.

Paddock documents this public interface in its `docs/integrations.md`. Arnold uses the installed `paddock` executable, or the operator's explicit `PADDOCK_BIN` override.

Slack streams rotate before nine minutes, 10,000 UTF-16 code units of answer text, or 48 task updates, leaving room below Slack's documented limits. Expired streams continue in a new message in the same thread. Channel streaming requires a human recipient: human tasks use the authenticated requester; automation may configure `recipientUserId`. Without a configured human recipient, automation relays answers as ordinary replies and keeps diagnostics local.

A crash after Slack accepts a stream append but before its SQLite receipt is saved can duplicate text on retry; Slack streaming does not offer an idempotency key. A crash during stream creation can likewise leave an orphan stream that Slack will expire. Normal restarts with a saved timestamp resume that stream. No exactly-once claim is made.
