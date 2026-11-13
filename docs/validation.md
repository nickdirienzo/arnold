# Validation

Run the deterministic suite and syntax checks:

```sh
npm test
npm run check
```

The deterministic suite uses real SQLite and injected Slack/Paddock boundaries. It covers thread routing, authenticated identity mapping, source allowlists, reaction acknowledgements, native follow-up delivery, agent changes, retry idempotency, output pagination/reassembly, cursor recovery and Slack assistant streams.

To include the installed Paddock CLI contract test:

```sh
ARNOLD_PADDOCK_TEST_BIN="$(command -v paddock)" npm test
```

That test uses a disposable registry and a deterministic native-agent RPC fixture. It validates Paddock's actual public output format, opaque cursors, bounded pages and multipart output, followed by simulated Slack delivery failure and restart recovery. It does not launch a live model, access credentials, inspect existing tasks or contact Slack. Without the variable, this one test is skipped.

The full suite has 53 tests. Syntax checks and the rendered launchd plist validation also pass:

```sh
mkdir -p state
node scripts/launchd.js > state/dev.arnold.slack.plist
plutil -lint state/dev.arnold.slack.plist
```

## Live deployment checks

An initial live Slack request has been verified through Paddock/OpenShell, including public progress, an answer and native turn completion. The delivery cursor survived a service restart. Slack accepted a native plan-style assistant stream, and reaction add/remove checks passed after the bot scope was granted. These checks demonstrate those paths in one deployment; they do not establish every integration or capability for another installation.

Validate these behaviors against your own workspace and Paddock configuration before relying on a deployment:

1. A mapped human can mention Arnold and receive an answer from the intended identity and agent.
2. A reply without a mention resumes the conversation, including after a service restart.
3. Multiple mapped participants preserve their own authenticated identities and task provenance.
4. An explicitly allowed event source starts the same conversation path, while other bots cannot initiate work.
5. Agent/tool permission boundaries produce a useful response. Code changes and PR creation use the configured identity, with human review required for merging and deployment.

Live agent availability depends on Paddock's runtime and providers. Claude's native project/MCP consent prompts may require operator action; Arnold does not automate terminal approvals. Workspace-specific configurations, credentials, task identifiers and deployment notes belong in ignored local files.
