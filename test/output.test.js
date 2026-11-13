import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { Store } from '../src/store.js';
import { relayOutput, slackChunks } from '../src/output.js';

const message = (id, text, extra = {}) => ({ id, type: 'message', turn_id: 'turn1', phase: 'commentary', text, ...extra });
const finish = (state = 'completed', id = 'finish1', turn_id = 'turn1') => ({ id, type: 'turn_finished', turn_id, state });
const page = (events, next_cursor = '', has_more = false) => ({ task_id: 'task1', events, next_cursor, has_more });
function setup(t, pages) {
  const dir = mkdtempSync(join(tmpdir(), 'arnold-output-'));
  const path = join(dir, 'state');
  let store = new Store(path);
  const posts = [], reads = [];
  const args = { task: 'task1', channel: 'C1', threadTs: '1',
    paddock: { output: async (id, after) => {
      reads.push([id, after]);
      assert.ok(pages[after], `unexpected cursor ${after}`);
      return pages[after];
    } }, slack: { post: async (...args) => { posts.push(args); } } };
  t.after(() => { store.close(); rmSync(dir, { recursive: true }); });
  return { get store() { return store; }, posts, reads, args,
    run: () => relayOutput({ ...args, store }),
    restart: () => { store.close(); store = new Store(path); } };
}

test('public updates and final answers are delivered in order; turn ends independently of process lifetime', async t => {
  const f = setup(t, {
    '': page([message('m1', 'Checking logs')], 'one', true),
    one: page([message('m2', 'Root cause found', { phase: 'final_answer' }), finish()], 'two'),
    two: page([], 'two'),
  });
  await f.run();
  assert.equal(f.posts.length, 2);
  assert.equal(f.posts[0][2], 'Checking logs');
  assert.equal(f.posts[1][2], 'Root cause found');
  assert.ok(f.posts.every(p => p[0] === 'C1' && p[1] === '1'));
  assert.equal(f.store.outputCursor('task1'), 'two');
  f.restart(); await f.run();
  assert.equal(f.posts.length, 2);
  assert.deepEqual(f.reads.at(-1), ['task1', 'two']);
});

test('multipart output crosses page boundaries and is reassembled before Slack splitting', async t => {
  const f = setup(t, {
    '': page([message('a', 'first', { part: 1, parts: 2, phase: 'final_answer' })], 'one', true),
    one: page([message('b', ' second', { part: 2, parts: 2, phase: 'final_answer' }), finish()], 'two'),
  });
  f.args.paddock.output = async (id, after) => {
    if (after === '') return page([message('a', 'first', { part: 1, parts: 2, phase: 'final_answer' })], 'one', true);
    assert.equal(f.posts.length, 0, 'no fragment is posted to Slack');
    assert.equal(f.store.outputCursor(id), '', 'no fragment is skipped by the saved cursor');
    return page([message('b', ' second', { part: 2, parts: 2, phase: 'final_answer' }), finish()], 'two');
  };
  await f.run();
  assert.equal(f.posts[0][2], 'first second');
  assert.equal(f.posts.length, 1);
  assert.equal(f.store.outputCursor('task1'), 'two');
});

test('restart during multipart fetch refetches fragments without reposting earlier page messages', async t => {
  const pages = {
    '': page([message('earlier', 'Earlier update'), message('a', 'first', { part: 1, parts: 2 })], 'one', true),
  };
  const f = setup(t, pages);
  await assert.rejects(f.run(), /unexpected cursor one/);
  assert.equal(f.posts.length, 1);
  assert.equal(f.store.outputCursor('task1'), '');
  f.restart();
  pages.one = page([message('b', ' second', { part: 2, parts: 2 })], 'two');
  await f.run();
  assert.equal(f.posts.length, 2);
  assert.match(f.posts[1][2], /first second$/);
  assert.equal(f.store.outputCursor('task1'), 'two');
});

test('Slack failure mid-page does not advance cursor; restart retries only the undelivered event', async t => {
  const pages = { '': page([message('a', 'A'), message('b', 'B'), finish()], 'end') };
  const f = setup(t, pages);
  const post = f.args.slack.post;
  let attempts = 0;
  f.args.slack.post = async (...args) => { if (++attempts === 2) throw new Error('Slack offline'); return post(...args); };
  await assert.rejects(f.run(), /Slack offline/);
  assert.equal(f.posts.length, 1);
  assert.equal(f.store.outputCursor('task1'), '');
  f.restart(); f.args.slack.post = post; await f.run();
  assert.equal(f.posts.length, 2);
  assert.equal(f.posts[1][2], 'B');
  assert.equal(f.store.outputCursor('task1'), 'end');
});

test('long Unicode answers survive chunk-delivery failure and restart without loss or duplicate chunks', async t => {
  const text = 'ø🦖'.repeat(20000);
  const events = [message('a', text.slice(0, 30000), { part: 1, parts: 2, phase: 'final_answer' }),
    message('b', text.slice(30000), { part: 2, parts: 2, phase: 'final_answer' })];
  const f = setup(t, { '': page(events, 'end') });
  const post = f.args.slack.post;
  let attempts = 0;
  let failedId;
  f.args.slack.post = async (...args) => { if (++attempts === 3) { failedId = args[3]; throw new Error('offline'); } return post(...args); };
  await assert.rejects(f.run(), /offline/);
  assert.equal(f.posts.length, 2);
  f.restart(); f.args.slack.post = post; await f.run();
  assert.equal(f.posts[2][3], failedId);
  const full = f.posts.map(p => p[2]).join('');
  assert.equal(full, text);
  assert.ok(f.posts.every(p => Array.from(p[2]).length <= 3500));
  assert.equal(new Set(f.posts.map(p => p[3])).size, f.posts.length);
});

test('follow-up output after a completed turn uses the saved cursor', async t => {
  const pages = { '': page([finish()], 'first'), first: page([message('m2', 'Follow-up answer', { turn_id: 'turn2', phase: 'final_answer' }), finish('completed', 'finish2', 'turn2')], 'second') };
  const f = setup(t, pages);
  await f.run(); await f.run();
  assert.equal(f.posts.length, 1);
  assert.equal(f.posts[0][2], 'Follow-up answer');
  assert.equal(f.store.outputCursor('task1'), 'second');
});

test('failed and interrupted turns are reported even without final answers', async t => {
  const f = setup(t, { '': page([finish('failed'), finish('interrupted', 'finish2', 'turn2')], 'end') });
  await f.run();
  assert.match(f.posts[0][2], /couldn't finish/);
  assert.match(f.posts[1][2], /interrupted/);
});

test('unknown or expired cursors are preserved rather than replaying history', async t => {
  const f = setup(t, {});
  f.store.saveOutputCursor('task1', 'expired');
  f.args.paddock.output = async (id, after) => { assert.equal(after, 'expired'); throw new Error('cursor does not match conversation'); };
  await assert.rejects(f.run(), /does not match/);
  assert.equal(f.store.outputCursor('task1'), 'expired');
  assert.equal(f.posts.length, 0);
});

test('empty pages preserve cursor and never imply completion', async t => {
  const f = setup(t, { '': page([]) });
  await f.run();
  assert.equal(f.posts.length, 0);
  assert.equal(f.store.outputCursor('task1'), '');
});

test('malformed pages or broken multipart sequences cannot advance the delivery cursor', async t => {
  for (const broken of [
    { ...page([], ''), task_id: 'another-task' },
    page([message('a', 'x')], ''),
    page([], '', true),
    page([message('a', 'x', { part: 2, parts: 2 })], 'end'),
    page([message('a', 'x', { part: 1, parts: 2 }), finish()], 'end'),
    page([message('a', 'x', { part: 1, parts: 2 })], 'end'),
    page([{ id: 'private', turn_id: 'turn1', type: 'reasoning', text: 'private' }], 'end'),
  ]) {
    const f = setup(t, { '': broken });
    await assert.rejects(f.run());
    assert.equal(f.store.outputCursor('task1'), '');
    assert.equal(f.posts.length, 0);
  }
});

test('Slack chunks preserve Unicode code points', () => {
  assert.deepEqual(slackChunks('a🦖b', 2), ['a🦖', 'b']);
});
