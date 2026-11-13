import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { Paddock } from '../src/paddock.js';
import { Store } from '../src/store.js';
import { relayOutput } from '../src/output.js';

// Opt-in contract test: the installed Paddock CLI reads a synthetic task in a
// disposable registry. A deterministic RPC fixture replaces OpenShell/Codex;
// no model, gateway, credentials, existing tasks or external services are used.
test('installed Paddock output contract delivers paginated multipart replies and recovers after a Slack failure',
  { skip: !process.env.ARNOLD_PADDOCK_TEST_BIN }, async t => {
    const dir = mkdtempSync(join(tmpdir(), 'arnold-installed-'));
    const registry = join(dir, 'paddock');
    mkdirSync(join(registry, 'tasks'), { recursive: true });
    const id = '0123456789abcdef01234567';
    const conversation = '11111111-1111-4111-8111-111111111111';
    const cwd = '/sandbox/fixture';
    const answer = 'ø🦖'.repeat(16000);
    const items = Array.from({ length: 19 }, (_, i) => ({ turnId: 'turn1', completedAtMs: i + 1,
      item: { id: `progress${i}`, type: 'agentMessage', phase: 'commentary', text: `Progress ${i}` } }));
    items.push({ turnId: 'turn1', completedAtMs: 30,
      item: { id: 'answer', type: 'agentMessage', phase: 'final_answer', text: answer } });
    const gateway = join(dir, 'rpc-fixture.mjs');
    writeFileSync(gateway, `#!${process.execPath}\nimport { createInterface } from 'node:readline';
const items = ${JSON.stringify(items)};
if (!process.argv.includes('app-server') || !process.argv.includes('sandbox')) process.exit(2);
for await (const line of createInterface({ input: process.stdin })) {
  const request = JSON.parse(line);
  if (!request.id) continue;
  let result;
  switch (request.method) {
    case 'initialize': result = {}; break;
    case 'thread/read': result = {thread:{id:${JSON.stringify(conversation)},cwd:${JSON.stringify(cwd)}}}; break;
    case 'thread/turns/list': result = {data:[{id:'turn1',status:'completed'}],nextCursor:null}; break;
    case 'thread/items/list': result = {data:items,nextCursor:null}; break;
    default: process.exit(3);
  }
  process.stdout.write(JSON.stringify({id:request.id,result})+'\\n');
}
`, { mode: 0o700 });
    writeFileSync(join(registry, 'tasks', `${id}.json`), JSON.stringify({ version: 1, id, runtime: 'openshell', worktree: cwd,
      status: 'running', session: { agent: 'codex', agent_path: '/fixture/codex', conversation_id: conversation },
      openshell: { name: 'fixture', ready: true, config: { binary: gateway, gateway: 'fixture' } } }));
    const original = process.env.PADDOCK_HOME;
    process.env.PADDOCK_HOME = registry;
    let store = new Store(join(dir, 'arnold.sqlite'));
    t.after(() => {
      store.close(); rmSync(dir, { recursive: true });
      if (original === undefined) delete process.env.PADDOCK_HOME; else process.env.PADDOCK_HOME = original;
    });
    const paddock = new Paddock(process.env.ARNOLD_PADDOCK_TEST_BIN);
    const first = await paddock.output(id);
    assert.equal(first.events.length, 20);
    assert.equal(first.has_more, true);
    assert.equal(first.events[19].part, 1);
    assert.ok(first.events[19].parts > 1);
    const posts = [];
    let fail = true;
    const slack = { post: async (...args) => {
      if (fail && posts.length === 21) { fail = false; throw new Error('simulated Slack disconnect'); }
      posts.push(args);
    } };
    const relay = () => relayOutput({ task: id, channel: 'C1', threadTs: '1', paddock, slack, store });
    await assert.rejects(relay(), /simulated Slack disconnect/);
    assert.equal(store.outputCursor(id), '');
    store.close(); store = new Store(join(dir, 'arnold.sqlite'));
    await relay();
    assert.equal(posts.slice(19).map(p => p[2]).join(''), answer);
    assert.ok(!posts.some(p => p[2].includes('turn `turn1` completed')));
    const count = posts.length;
    await relay();
    assert.equal(posts.length, count);
    assert.ok(store.outputCursor(id));
    assert.equal(new Set(posts.map(p => p[3])).size, posts.length);
  });
