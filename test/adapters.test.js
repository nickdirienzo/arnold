import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Paddock } from '../src/paddock.js';
import { Slack } from '../src/slack.js';
import { WebClient } from '@slack/web-api';
import { SocketModeClient } from '@slack/socket-mode';

// A separate executable exercises execFile, flags, prompt files, environment and error stdout.
function cli(t, program) {
  const dir = mkdtempSync(join(tmpdir(), 'arnold-cli-'));
  const path = join(dir, 'paddock');
  writeFileSync(path, `#!${process.execPath}\nimport fs from 'node:fs';\nconst args=process.argv.slice(2);\nconst flag=k=>args[args.indexOf(k)+1];\n${program}`, { mode: 0o700 });
  // .mjs detection isn't available for a bare executable; use an enclosing ESM package.
  writeFileSync(join(dir, 'package.json'), '{"type":"module"}');
  t.after(() => rmSync(dir, { recursive: true }));
  return new Paddock(path);
}

test('CLI adapter uses explicit OpenShell and identity; credentials and parent task are excluded', async t => {
  const p = cli(t, `
    if(args[0]!=='run') process.exit(2);
    const prompt=fs.readFileSync(flag('--prompt-file'),'utf8');
    const context=JSON.parse(fs.readFileSync(flag('--context-file'),'utf8'));
    console.log(JSON.stringify({id:'one',runtime:flag('--runtime'),session:{agent:flag('--agent')},provenance:{identity:flag('--identity'),profile:flag('--profile')},
      args,prompt,context,secret:process.env.SLACK_BOT_TOKEN,parent:process.env.PADDOCK_TASK_ID}));
  `);
  for (const [key, value] of Object.entries({ SLACK_BOT_TOKEN: 'sentinel-secret', PADDOCK_TASK_ID: 'unrelated-task' })) {
    const original = process.env[key];
    process.env[key] = value;
    t.after(() => { if (original === undefined) delete process.env[key]; else process.env[key] = original; });
  }
  const r = await p.run({ prompt: 'Literal $(echo nope) and `pwd`', repo: 'repo', actor: 'U1', identity: 'engineer', profile: 'implementation', agent: 'claude', externalId: 'stable-id', context: { channel: 'C1' } });
  assert.equal(r.runtime, 'openshell');
  assert.equal(r.session.agent, 'claude');
  assert.equal(r.prompt, 'Literal $(echo nope) and `pwd`');
  assert.deepEqual(r.context, { channel: 'C1' });
  assert.equal(r.secret, undefined);
  assert.equal(r.parent, undefined);
  assert.ok(r.args.includes('--detach'));
  assert.equal(r.args[r.args.indexOf('--requester-source') + 1], 'arnold-slack');
});

test('nonzero CLI message exits preserve durable uncertainty receipts', async t => {
  const p = cli(t, `console.log(JSON.stringify({status:'uncertain', task_id:args[1]})); process.exitCode=1;`);
  assert.deepEqual(await p.message('task1', 'follow-up', 'stable'), { status: 'uncertain', task_id: 'task1' });
});

test('old CLI preflight fails without launching work', async t => {
  const p = cli(t, `console.error('unknown command identity'); process.exitCode=1;`);
  await assert.rejects(p.check());
});

test('configured identities must be active App identities with the allowed profile', async t => {
  const p = cli(t, `console.log(JSON.stringify({bot:{mode:'gh'}}));`);
  const config = { users: {}, automation: { identity: 'bot', profile: 'investigation' } };
  await assert.rejects(p.check(config), /active GitHub App/);
});

test('Slack context paginates, excludes the current and later messages and marks truncation', async () => {
  let calls = 0;
  const slack = new Slack({ conversations: { replies: async () => ({
    messages: [{ ts: String(++calls), user: 'U1', text: 'x'.repeat(7000) }, { ts: '100', text: 'future' }],
    response_metadata: { next_cursor: 'more' },
  }) } });
  const text = await slack.context('C1', '1', '10');
  assert.equal(calls, 4);
  assert.match(text, /^\[Context truncated\]/);
  assert.ok(text.length < 24100);
  assert.ok(!text.includes('future'));
});

test('Slack context permission failure propagates instead of silently losing evidence', async () => {
  const slack = new Slack({ conversations: { replies: async () => { throw new Error('missing_scope'); } } });
  await assert.rejects(slack.context('C1', '1', '2'), /missing_scope/);
});

test('Slack notices use stable UUIDs and disable unfurls', async () => {
  const posts = [];
  const slack = new Slack({ chat: { postMessage: async args => posts.push(args) } });
  await slack.post('C1', '1', 'hello', 'a'.repeat(64));
  await slack.post('C1', '1', 'hello', 'a'.repeat(64));
  assert.equal(posts[0].client_msg_id, posts[1].client_msg_id);
  assert.match(posts[0].client_msg_id, /^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-8[a-f0-9]{3}-[a-f0-9]{12}$/);
  assert.equal(posts[0].unfurl_links, false);
});

test('official SDK entrypoints load with the selected module format', () => {
  assert.equal(typeof WebClient, 'function');
  assert.equal(typeof SocketModeClient, 'function');
});

test('Paddock output adapter passes opaque cursors literally and requests bounded pages', async t => {
  const p = cli(t, `console.log(JSON.stringify({args}));`);
  const first = await p.output('task1');
  assert.deepEqual(first.args, ['output', 'task1', '--limit', '20', '--json']);
  const next = await p.output('task1', 'opaque-$(echo nope)');
  assert.deepEqual(next.args, ['output', 'task1', '--after', 'opaque-$(echo nope)', '--limit', '20', '--json']);
});

test('human-only identity preflight checks just the configured human App', async t => {
  const p = cli(t, `console.log(JSON.stringify({engineer:{mode:'app',profiles:['investigation']}}));`);
  await p.check({ users: { U1: { identity: 'engineer', profile: 'investigation' } }, triggers: [] });
});

test('eyes reaction uses the source timestamp and tolerates idempotent Slack responses', async () => {
  const calls = [];
  const slack = new Slack({ reactions: {
    add: async args => { calls.push(args); throw { data: { error: 'already_reacted' } }; },
    remove: async args => { calls.push(args); throw { data: { error: 'no_reaction' } }; },
  } });
  await slack.reaction('C1', '123.456', true);
  await slack.reaction('C1', '123.456', false);
  assert.deepEqual(calls, [{ channel: 'C1', timestamp: '123.456', name: 'eyes' }, { channel: 'C1', timestamp: '123.456', name: 'eyes' }]);
});
