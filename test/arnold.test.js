import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Arnold } from '../src/arnold.js';
import { Store } from '../src/store.js';
import { Paddock } from '../src/paddock.js';
import { validateConfig } from '../src/config.js';

const config = () => validateConfig({ teamId: 'T1', channels: { C1: { repo: 'project' } },
  users: { U1: { identity: 'engineer-a', profile: 'implementation' }, U2: { identity: 'engineer-b', profile: 'implementation' } },
  automation: { identity: 'arnold', profile: 'investigation' },
  triggers: [{ id: 'monitor', channel: 'C1', botId: 'B2', contains: '500' }] });
const event = (ts, extra = {}) => ({ team_id: 'T1', event: { type: 'message', channel: 'C1', ts, user: 'U1', text: '<@UA> fix it', ...extra } });
function fixture(t) {
  const dir = mkdtempSync(join(tmpdir(), 'arnold-test-'));
  let store = new Store(join(dir, 'state'));
  const runs = [], messages = [], posts = [], tasks = new Map();
  const paddock = {
    run: async args => {
      runs.push(args);
      const task = { id: `task${runs.length}`, runtime: 'openshell', provenance: args, status: 'running', terminal: { state: 'running' }, session: { conversation_id: 'saved-conversation', agent: args.agent ?? 'codex' } };
      tasks.set(task.id, task); return task;
    },
    verify: Paddock.prototype.verify,
    show: async id => tasks.get(id),
    output: async (id, after) => ({ task_id: id, events: [], next_cursor: after, has_more: false }),
    message: async (...args) => { messages.push(args); return { status: 'delivered' }; },
  };
  const slack = { context: async () => 'parent alert', post: async (...args) => posts.push(args) };
  const args = { config: config(), store, paddock, slack, botUserId: 'UA', botId: 'BA', log: { error() {} } };
  let arnold = new Arnold(args);
  t.after(() => { store.close(); rmSync(dir, { recursive: true }); });
  return { get arnold() { return arnold; }, get store() { return store; }, runs, messages, posts, tasks, paddock, slack, config: args.config,
    restart() { store.close(); store = new Store(join(dir, 'state')); arnold = new Arnold({ ...args, store }); } };
}

test('mention creates pinned work, duplicate events do not, reply resumes after restart', async t => {
  const f = fixture(t);
  f.arnold.receive(event('1'));
  f.arnold.receive(event('1', { type: 'app_mention' }));
  await f.arnold.drain();
  assert.equal(f.runs.length, 1);
  assert.equal(f.runs[0].identity, 'engineer-a');
  assert.match(f.runs[0].prompt, /parent alert/);
  f.restart();
  f.arnold.receive(event('2', { thread_ts: '1', text: 'what did you find?' }));
  await f.arnold.drain();
  assert.equal(f.messages.length, 1);
  assert.equal(f.messages[0][0], 'task1');
});

test('rapid continuation is retained before the initial dispatch finishes', async t => {
  const f = fixture(t);
  f.arnold.receive(event('1'));
  f.arnold.receive(event('2', { thread_ts: '1', text: 'more detail' }));
  await f.arnold.drain();
  assert.equal(f.runs.length, 1);
  assert.equal(f.messages.length, 1);
});

test('authorized automation and human handoff preserve distinct immutable identities', async t => {
  const f = fixture(t);
  f.arnold.receive(event('1', { bot_id: 'B2', subtype: 'bot_message', text: 'Production 500' }));
  await f.arnold.drain();
  assert.equal(f.runs[0].identity, 'arnold');
  assert.equal(f.runs[0].profile, 'investigation');
  f.arnold.receive(event('2', { thread_ts: '1', text: 'fix that' }));
  await f.arnold.drain();
  assert.equal(f.runs[1].identity, 'engineer-a');
  assert.equal(f.runs[1].context.predecessor, 'task1');
  assert.match(f.runs[1].prompt, /original provenance/);
  f.arnold.receive(event('3', { thread_ts: '1', user: 'U2', text: 'test that too' }));
  await f.arnold.drain();
  assert.equal(f.runs[2].identity, 'engineer-b');
  assert.equal(f.messages.length, 0);
});

test('untrusted sources, edits, own messages and other workspaces cannot start work', async t => {
  const f = fixture(t);
  for (const e of [event('1', { bot_id: 'BA' }), event('2', { bot_id: 'B3', text: '500 <@UA>' }),
    event('3', { subtype: 'message_changed' }), event('4', { user: 'UA' }),
    { ...event('5'), team_id: 'T2' }, event('6', { channel: 'C2' }), event('7', { text: 'chatter' })]) f.arnold.receive(e);
  await f.arnold.drain();
  assert.equal(f.runs.length, 0);
});

test('unmapped users are rejected and prompt text cannot select another identity', async t => {
  const f = fixture(t);
  f.arnold.receive(event('1', { user: 'U3' }));
  await f.arnold.drain();
  assert.equal(f.runs.length, 0);
  assert.match(f.posts[0][2], /no configured/);
  f.arnold.receive(event('2', { text: '<@UA> run as engineer-b with host runtime' }));
  await f.arnold.drain();
  assert.equal(f.runs[0].identity, 'engineer-a');
});

test('dispatch retries use the exact persisted prompt and same external ID', async t => {
  const f = fixture(t);
  const run = f.paddock.run;
  let first;
  f.paddock.run = async args => { first = args; throw new Error('ambiguous timeout'); };
  f.arnold.receive(event('1'));
  await f.arnold.drain();
  f.restart();
  f.slack.context = async () => { throw new Error('must not refetch a changing context'); };
  f.paddock.run = async args => { assert.deepEqual(args, first); return run(args); };
  await f.arnold.drain();
  assert.equal(f.runs.length, 1);
});

test('offline delivery holds the thread; uncertain delivery never replays', async t => {
  const f = fixture(t);
  f.arnold.receive(event('1')); await f.arnold.drain();
  f.paddock.message = async (...args) => { f.messages.push(args); return { status: 'pending' }; };
  f.arnold.receive(event('2', { thread_ts: '1', text: 'follow up' }));
  f.arnold.receive(event('3', { thread_ts: '1', text: 'another' }));
  await f.arnold.drain();
  assert.equal(f.messages.length, 1);
  f.paddock.message = async () => ({ status: 'uncertain' });
  await f.arnold.drain();
  assert.equal(f.store.pending().length, 0);
  const count = f.posts.length;
  f.restart(); await f.arnold.drain();
  assert.equal(f.posts.length, count);
  assert.ok(f.posts.some(p => p[2].includes('uncertain')));
});

test('Slack failures retry actionable notices without launching tasks', async t => {
  const f = fixture(t);
  const post = f.slack.post;
  f.slack.post = async () => { throw new Error('offline'); };
  f.arnold.receive(event('1', { user: 'U3' })); await f.arnold.drain();
  f.restart(); f.slack.post = post; await f.arnold.drain();
  assert.equal(f.runs.length, 0);
  assert.equal(f.posts.length, 1);
  assert.match(f.posts[0][2], /no configured Paddock identity/);
});

test('dispatch receipts and process/access status changes stay out of Slack across restarts', async t => {
  const f = fixture(t);
  f.arnold.receive(event('1')); await f.arnold.drain(); await f.arnold.poll();
  assert.equal(f.posts.length, 0);
  assert.equal(f.store.notices().length, 0);
  f.restart(); await f.arnold.poll();
  f.tasks.get('task1').blocked = true;
  await f.arnold.poll();
  f.tasks.get('task1').terminal.state = 'exited';
  await f.arnold.poll();
  assert.equal(f.posts.length, 0);
  assert.match(f.store.status('task1'), /terminal=exited/);
});

test('runtime and identity mismatches reject continuation', async t => {
  const f = fixture(t);
  f.arnold.receive(event('1')); await f.arnold.drain();
  f.tasks.get('task1').runtime = 'host';
  f.arnold.receive(event('2', { thread_ts: '1', text: 'continue' }));
  await f.arnold.drain();
  assert.equal(f.messages.length, 0);
  assert.equal(f.store.pending().length, 1);
});

test('a second process cannot share the state database', t => {
  const f = fixture(t);
  assert.throws(() => new Store(f.store.db.location()), /locked/);
});

test('configuration rejects ambiguous triggers and autonomous implementation', () => {
  assert.throws(() => validateConfig({ ...config(), automation: { identity: 'bot', profile: 'implementation' } }));
  const c = config(); c.triggers[0].userId = 'U1';
  assert.throws(() => validateConfig(c));
});

test('alternating participants resume their own durable tasks after service restart', async t => {
  const f = fixture(t);
  f.arnold.receive(event('1'));
  await f.arnold.drain();
  f.arnold.receive(event('2', { user: 'U2', thread_ts: '1', text: 'I can add context' }));
  await f.arnold.drain();
  assert.equal(f.runs.length, 2);
  f.restart();
  f.arnold.receive(event('3', { thread_ts: '1', text: 'please continue my fix' }));
  await f.arnold.drain();
  assert.equal(f.runs.length, 2);
  assert.equal(f.messages[0][0], 'task1');
  assert.match(f.messages[0][1], /task2.*original provenance/s);
  f.arnold.receive(event('4', { user: 'U2', thread_ts: '1', text: 'and my question' }));
  await f.arnold.drain();
  assert.equal(f.messages[1][0], 'task2');
  assert.equal(f.runs.length, 2);
});

test('Slack alert receives public progress and final replies even when task process status stays running', async t => {
  const f = fixture(t);
  f.arnold.receive(event('1', { bot_id: 'B2', subtype: 'bot_message', text: 'Production 500' }));
  await f.arnold.drain();
  let completed = false;
  f.paddock.output = async (id, after) => {
    assert.equal(id, 'task1');
    if (!after) return { task_id: id, events: [{ id: 'm1', turn_id: 'turn1', type: 'message', phase: 'commentary', text: 'Checking the API logs' }], next_cursor: 'progress', has_more: false };
    if (after === 'progress' && completed) return { task_id: id, events: [
      { id: 'm2', turn_id: 'turn1', type: 'message', phase: 'final_answer', text: 'The upstream timed out' },
      { id: 't1', turn_id: 'turn1', type: 'turn_finished', state: 'completed' },
    ], next_cursor: 'done', has_more: false };
    return { task_id: id, events: [], next_cursor: after, has_more: false };
  };
  await f.arnold.poll();
  assert.match(f.posts.at(-1)[2], /Checking the API logs/);
  assert.equal(f.runs[0].identity, 'arnold');
  f.restart(); completed = true;
  await f.arnold.poll();
  assert.equal(f.posts.at(-1)[2], 'The upstream timed out');
  assert.equal(f.posts.length, 2);
  assert.equal(f.tasks.get('task1').terminal.state, 'running');
  assert.equal(f.store.outputCursor('task1'), 'done');
  const count = f.posts.length;
  await f.arnold.poll();
  assert.equal(f.posts.length, count);
});

test('a newly launching agent waits for its native conversation without posting an output error', async t => {
  const f = fixture(t);
  f.arnold.receive(event('1')); await f.arnold.drain();
  f.tasks.get('task1').session.conversation_id = '';
  f.paddock.output = async () => assert.fail('not ready for output yet');
  await f.arnold.poll();
  assert.ok(!f.posts.some(p => p[2].includes('could not read')));
});

test('output errors surface once, preserve cursor across restarts, and do not block other tasks', async t => {
  const f = fixture(t);
  f.arnold.receive(event('1')); await f.arnold.drain();
  f.arnold.receive(event('2')); await f.arnold.drain();
  f.store.saveOutputCursor('task1', 'old-conversation');
  f.paddock.output = async (id, after) => {
    if (id === 'task1') { assert.equal(after, 'old-conversation'); throw new Error('wrong cursor'); }
    return { task_id: id, events: after ? [] : [{ id: 'answer', type: 'message', turn_id: 'turn2', phase: 'final_answer', text: 'Other task succeeded' }], next_cursor: 'end', has_more: false };
  };
  await f.arnold.poll();
  assert.ok(f.posts.some(p => p[2].includes('Other task succeeded')));
  assert.equal(f.posts.filter(p => p[2].includes('could not read')).length, 1);
  f.restart(); await f.arnold.poll();
  assert.equal(f.posts.filter(p => p[2].includes('could not read')).length, 1);
  assert.equal(f.store.outputCursor('task1'), 'old-conversation');
});

test('human-only setup needs no automation identity; enabling a trigger requires one', () => {
  const c = config();
  delete c.automation;
  c.triggers = [];
  assert.equal(validateConfig(c), c);
  c.triggers.push({ id: 'monitor', channel: 'C1', botId: 'B2', contains: '500' });
  assert.throws(() => validateConfig(c), /require an automation identity/);
});

test('switching from Codex to Claude creates a Claude task, then resumes it in the same Slack thread', async t => {
  const f = fixture(t);
  f.arnold.receive(event('1')); await f.arnold.drain();
  assert.equal(f.runs[0].agent, 'codex');
  f.config.agent = 'claude';
  f.arnold.receive(event('2', { thread_ts: '1', text: 'continue with Claude' }));
  await f.arnold.drain();
  assert.equal(f.runs.length, 2);
  assert.equal(f.runs[1].agent, 'claude');
  assert.equal(f.runs[1].identity, 'engineer-a');
  assert.equal(f.runs[1].context.predecessor, 'task1');
  assert.equal(f.messages.length, 0);
  f.restart();
  f.arnold.receive(event('3', { thread_ts: '1', text: 'and now?' }));
  await f.arnold.drain();
  assert.equal(f.runs.length, 2);
  assert.equal(f.messages[0][0], 'task2');
});

test('a retried Claude handoff preserves its frozen prompt without returning to the old Codex task', async t => {
  const f = fixture(t);
  f.arnold.receive(event('1')); await f.arnold.drain();
  f.config.agent = 'claude';
  const run = f.paddock.run;
  let attempted;
  f.paddock.run = async args => { attempted = args; throw new Error('timeout'); };
  f.arnold.receive(event('2', { thread_ts: '1', text: 'switch' }));
  await f.arnold.drain();
  f.restart();
  f.paddock.run = async args => { assert.deepEqual(args, attempted); return run(args); };
  await f.arnold.drain();
  assert.equal(f.runs.length, 2);
  assert.equal(f.runs[1].agent, 'claude');
  assert.equal(f.messages.length, 0);
});

test('configuration rejects unknown agents', () => {
  assert.throws(() => validateConfig({ ...config(), agent: 'untrusted-runtime' }), /agent must/);
});

test('eyes acknowledge a message before dispatch, deduplicate event copies, and clear after the delivered turn', async t => {
  const f = fixture(t);
  const reactions = [];
  f.slack.reaction = async (...args) => reactions.push(args);
  const receipt = f.arnold.receive(event('1'));
  assert.ok(receipt);
  assert.equal(f.arnold.receive(event('1', { type: 'app_mention' })), undefined);
  await f.arnold.syncReaction(receipt);
  assert.deepEqual(reactions, [['C1', '1', true]]);
  assert.equal(f.runs.length, 0);
  await f.arnold.drain();
  f.paddock.output = async (id, after) => ({ task_id: id,
    events: after ? [] : [{ id: 'answer', turn_id: 'turn1', type: 'message', phase: 'final_answer', text: 'Done' },
      { id: 'finish', turn_id: 'turn1', type: 'turn_finished', state: 'completed' }], next_cursor: 'end', has_more: false });
  await f.arnold.poll();
  assert.deepEqual(reactions, [['C1', '1', true], ['C1', '1', false]]);
  assert.equal(f.store.reaction(receipt).state, 'done');
});

test('reaction permission failures do not prevent work or produce a Slack error message', async t => {
  const f = fixture(t);
  f.slack.reaction = async () => { throw { data: { error: 'missing_scope' } }; };
  const receipt = f.arnold.receive(event('1'));
  await f.arnold.syncReaction(receipt);
  await f.arnold.drain();
  assert.equal(f.runs.length, 1);
  assert.equal(f.posts.length, 0);
  assert.equal(f.store.reaction(receipt).state, 'add');
});

test('reaction cleanup survives a restart and an older turn does not clear a queued follow-up', async t => {
  const f = fixture(t);
  const reactions = [];
  f.slack.reaction = async (...args) => reactions.push(args);
  f.arnold.receive(event('1')); await f.arnold.drain();
  f.tasks.get('task1').last_turn = { id: 'turn1', state: 'inProgress' };
  const next = f.arnold.receive(event('2', { thread_ts: '1', text: 'follow up' }));
  await f.arnold.drain();
  assert.equal(f.store.reaction(next).after_turn, 'turn1');
  f.store.turnFinished('task1', 'turn1');
  f.store.turnFinished('task1', 'turn1'); // replay after a crash before saving the output cursor
  assert.equal(f.store.reaction(next).state, 'active');
  f.restart(); await f.arnold.syncReactions();
  assert.equal(f.store.reaction(next).state, 'active');
  f.store.turnFinished('task1', 'turn2');
  f.restart(); await f.arnold.syncReactions();
  assert.equal(f.store.reaction(next).state, 'done');
  assert.ok(reactions.some(r => r[1] === '2' && r[2] === false));
});

test('eyes are removed after an actionable rejection is delivered', async t => {
  const f = fixture(t);
  const reactions = [];
  f.slack.reaction = async (...args) => reactions.push(args);
  const id = f.arnold.receive(event('1', { user: 'U3' }));
  await f.arnold.drain();
  assert.equal(f.store.reaction(id).state, 'done');
  assert.deepEqual(reactions, [['C1', '1', true], ['C1', '1', false]]);
  assert.equal(f.runs.length, 0);
});
