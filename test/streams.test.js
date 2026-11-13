import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Store } from '../src/store.js';
import { Slack } from '../src/slack.js';
import { relayOutput } from '../src/output.js';

const message = (id, text, phase = 'commentary') => ({ id, turn_id: 'turn1', type: 'message', phase, text });
const finish = state => ({ id: 'done', turn_id: 'turn1', type: 'turn_finished', state });
function fixture(t) {
  const dir = mkdtempSync(join(tmpdir(), 'arnold-stream-'));
  const path = join(dir, 'state');
  let store = new Store(path);
  const calls = [];
  let serial = 0;
  const chat = {
    startStream: async args => { const ts = String(++serial); calls.push({ method: 'start', ...args, ts }); return { ok: true, ts }; },
    appendStream: async args => { calls.push({ method: 'append', ...args }); return { ok: true }; },
    stopStream: async args => { calls.push({ method: 'stop', ...args }); return { ok: true }; },
    postMessage: async args => { calls.push({ method: 'post', ...args }); return { ok: true }; },
  };
  const slack = new Slack({ chat });
  t.after(() => { store.close(); rmSync(dir, { recursive: true }); });
  return { get store() { return store; }, calls, chat, slack,
    send: (event, text = event.text, chunk = 0) => slack.streamEvent({ task: 'task1', event, text, chunk,
      channel: 'C1', threadTs: 'root', recipient: { teamId: 'T1', userId: 'U1' }, store }),
    restart: () => { store.close(); store = new Store(path); },
  };
}

test('Assistant plan contains task diagnostics and public progress; answer is markdown in the same stream', async t => {
  const f = fixture(t);
  f.store.observed('task1', 'running; terminal=running; access=clear');
  await f.send(message('progress', 'Checking logs'));
  await f.slack.streamStatus({ task: 'task1', channel: 'C1', status: 'access=pending review', store: f.store });
  await f.send(message('answer', 'Found the cause', 'final_answer'));
  await f.send(finish('completed'));
  assert.equal(f.calls[0].task_display_mode, 'plan');
  assert.equal(f.calls[0].recipient_team_id, 'T1');
  assert.equal(f.calls[0].recipient_user_id, 'U1');
  assert.equal(f.calls[0].thread_ts, 'root');
  assert.equal(f.calls[0].chunks[0].type, 'plan_update');
  assert.match(f.calls[0].chunks[1].details, /Paddock task task1.*Request received/s);
  assert.equal(f.calls[1].chunks[0].type, 'task_update');
  assert.equal(f.calls[1].chunks[0].details, 'Checking logs');
  assert.equal(f.calls[2].chunks[0].details, 'Paddock task task1\naccess=pending review');
  assert.deepEqual(f.calls[3].chunks, [{ type: 'markdown_text', text: 'Found the cause' }]);
  assert.equal(f.calls[4].method, 'stop');
  assert.equal(f.calls[4].chunks[0].status, 'complete');
  assert.equal(f.calls.filter(c => c.method === 'post').length, 0);
  assert.equal(new Set(f.calls.map(c => c.ts)).size, 1);
  assert.equal(f.store.stream('task1', 'turn1').finished, true);
});

test('persisted stream timestamps let polling resume after restart without repeating progress', async t => {
  const f = fixture(t);
  let ready = false;
  const paddock = { output: async (id, after) => ({ task_id: id,
    events: !after ? [message('progress', 'Checking logs')] : ready && after === 'progress' ? [message('answer', 'Done', 'final_answer'), finish('completed')] : [],
    next_cursor: !after ? 'progress' : ready ? 'done' : after, has_more: false }) };
  const run = () => relayOutput({ task: 'task1', channel: 'C1', threadTs: 'root', recipient: { teamId: 'T1', userId: 'U1' }, paddock, slack: f.slack, store: f.store });
  await run();
  const ts = f.store.stream('task1', 'turn1').ts;
  f.restart(); ready = true; await run();
  assert.equal(f.calls.filter(c => c.method === 'start').length, 1);
  assert.ok(f.calls.every(c => c.ts === ts));
  assert.equal(f.store.outputCursor('task1'), 'done');
  const count = f.calls.length;
  await run(); assert.equal(f.calls.length, count);
});

test('append failure preserves stream and cursor for retry without starting a replacement stream', async t => {
  const f = fixture(t);
  const append = f.chat.appendStream;
  f.chat.appendStream = async () => { throw new Error('offline'); };
  const paddock = { output: async () => ({ task_id: 'task1', events: [message('answer', 'Result', 'final_answer'), finish('completed')], next_cursor: 'end', has_more: false }) };
  const run = () => relayOutput({ task: 'task1', channel: 'C1', threadTs: 'root', recipient: { teamId: 'T1', userId: 'U1' }, paddock, slack: f.slack, store: f.store });
  await assert.rejects(run(), /offline/);
  assert.equal(f.store.outputCursor('task1'), '');
  f.restart(); f.chat.appendStream = append; await run();
  assert.equal(f.calls.filter(c => c.method === 'start').length, 1);
  assert.equal(f.store.outputCursor('task1'), 'end');
});

test('long answers and long-running tasks rotate streams before Slack limits and retain all text', async t => {
  const f = fixture(t);
  const text = '🦖'.repeat(3500);
  for (let i = 0; i < 4; i++) await f.send(message('answer', text, 'final_answer'), text, i);
  assert.equal(f.calls.filter(c => c.method === 'start').length, 4);
  assert.ok(f.calls.filter(c => c.method === 'stop').every(c => c.session_status === 'processing'));
  assert.equal(f.calls.flatMap(c => c.chunks).filter(c => c.type === 'markdown_text').map(c => c.text).join(''), text.repeat(4));
  const metadata = f.store.stream('task1', 'turn1');
  metadata.started = Date.now() - 10 * 60_000;
  f.store.saveStream('task1', 'turn1', metadata);
  await f.send(message('progress', 'Still working'));
  assert.equal(f.calls.filter(c => c.method === 'start').length, 5);
});

test('Slack-expired streams continue in a new stream and finish failure is visible', async t => {
  const f = fixture(t);
  await f.send(message('first', 'Checking logs'));
  const append = f.chat.appendStream;
  f.chat.appendStream = async () => { f.chat.appendStream = append; throw { data: { error: 'message_not_in_streaming_state' } }; };
  await f.send(message('second', 'More progress'));
  await f.send(finish('failed'));
  assert.equal(f.calls.filter(c => c.method === 'start').length, 2);
  assert.equal(f.calls.at(-1).method, 'stop');
  assert.equal(f.calls.at(-1).chunks[0].status, 'error');
  assert.match(f.calls.at(-1).chunks[1].text, /failed/);
});

test('automatic conversations without a human recipient avoid channel debug chatter and keep answers', async t => {
  const f = fixture(t);
  for (const event of [message('progress', 'Checking logs'), message('answer', 'Result', 'final_answer'), finish('completed')]) {
    await f.slack.streamEvent({ task: 'task1', event, text: event.text, chunk: 0, channel: 'C1', threadTs: 'root', store: f.store });
  }
  assert.equal(f.calls.length, 1);
  assert.equal(f.calls[0].method, 'post');
  assert.equal(f.calls[0].text, 'Result');
});
