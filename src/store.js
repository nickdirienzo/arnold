import { DatabaseSync } from 'node:sqlite';

// Transport receipts and task pointers only. Paddock owns the agent conversation.
export class Store {
  constructor(path) {
    this.lease = new DatabaseSync(`${path}.lock`);
    this.lease.exec('CREATE TABLE IF NOT EXISTS lease (id INTEGER); BEGIN EXCLUSIVE;');
    this.db = new DatabaseSync(path);
    this.db.exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;
      CREATE TABLE IF NOT EXISTS threads (key TEXT PRIMARY KEY, task TEXT, actor TEXT, identity TEXT, profile TEXT);
      CREATE TABLE IF NOT EXISTS bindings (key TEXT, actor TEXT, identity TEXT, profile TEXT, task TEXT,
        PRIMARY KEY(key,actor,identity,profile));
      CREATE TABLE IF NOT EXISTS receipts (id TEXT PRIMARY KEY, key TEXT, payload TEXT, phase TEXT DEFAULT 'pending', task TEXT, notice TEXT);
      CREATE TABLE IF NOT EXISTS observations (task TEXT PRIMARY KEY, status TEXT);
      CREATE TABLE IF NOT EXISTS output_cursors (task TEXT PRIMARY KEY, cursor TEXT NOT NULL);
      CREATE TABLE IF NOT EXISTS output_receipts (task TEXT, event TEXT, chunk INTEGER,
        PRIMARY KEY(task,event,chunk));
      CREATE TABLE IF NOT EXISTS slack_streams (task TEXT, turn TEXT, metadata TEXT,
        PRIMARY KEY(task,turn));
      CREATE TABLE IF NOT EXISTS message_reactions (receipt TEXT PRIMARY KEY, channel TEXT, ts TEXT,
        task TEXT, after_turn TEXT DEFAULT '', state TEXT DEFAULT 'add');
      INSERT OR IGNORE INTO bindings SELECT key,actor,identity,profile,task FROM threads;
    `);
  }
  thread(key) { return this.db.prepare('SELECT * FROM threads WHERE key=?').get(key); }
  binding(key, actor, identity, profile) {
    return this.db.prepare('SELECT * FROM bindings WHERE key=? AND actor=? AND identity=? AND profile=?').get(key, actor, identity, profile);
  }
  active(key) { return Boolean(this.thread(key) || this.db.prepare("SELECT 1 FROM receipts WHERE key=? AND phase='pending'").get(key)); }
  prepare(id, payload) { this.db.prepare('UPDATE receipts SET payload=? WHERE id=?').run(JSON.stringify(payload), id); }
  accept(id, key, payload) {
    this.db.exec('BEGIN IMMEDIATE');
    try {
      const inserted = this.db.prepare('INSERT OR IGNORE INTO receipts(id,key,payload) VALUES(?,?,?)').run(id, key, JSON.stringify(payload)).changes;
      if (inserted) this.db.prepare('INSERT INTO message_reactions(receipt,channel,ts) VALUES(?,?,?)').run(id, payload.event.channel, payload.event.ts);
      this.db.exec('COMMIT');
      return Boolean(inserted);
    } catch (error) { this.db.exec('ROLLBACK'); throw error; }
  }
  pending() { return this.db.prepare("SELECT * FROM receipts WHERE phase='pending' ORDER BY rowid").all(); }
  bind(row, task, actor, identity, profile, afterTurn = '') {
    this.db.exec('BEGIN IMMEDIATE');
    try {
      this.db.prepare('INSERT OR REPLACE INTO threads VALUES(?,?,?,?,?)').run(row.key, task, actor, identity, profile);
      this.db.prepare('INSERT OR REPLACE INTO bindings VALUES(?,?,?,?,?)').run(row.key, actor, identity, profile, task);
      this.db.prepare("UPDATE receipts SET task=?,phase='accepted',payload=NULL WHERE id=?").run(task, row.id);
      this.db.prepare('UPDATE message_reactions SET task=?,after_turn=? WHERE receipt=?').run(task, afterTurn, row.id);
      this.db.exec('COMMIT');
    } catch (error) { this.db.exec('ROLLBACK'); throw error; }
  }
  finish(id, notice) {
    this.db.prepare("UPDATE receipts SET phase='accepted',payload=NULL,notice=? WHERE id=?").run(notice, id);
  }
  notices() { return this.db.prepare("SELECT * FROM receipts WHERE phase='accepted'").all(); }
  done(id) {
    if (this.db.prepare('SELECT notice FROM receipts WHERE id=?').get(id)?.notice)
      this.db.prepare("UPDATE message_reactions SET state='remove' WHERE receipt=? AND state!='done'").run(id);
    this.db.prepare("UPDATE receipts SET phase='done',notice=NULL WHERE id=?").run(id);
  }
  reaction(id) { return this.db.prepare('SELECT * FROM message_reactions WHERE receipt=?').get(id); }
  reactions() { return this.db.prepare("SELECT * FROM message_reactions WHERE state IN ('add','remove')").all(); }
  reacted(id, before, after) { this.db.prepare('UPDATE message_reactions SET state=? WHERE receipt=? AND state=?').run(after, id, before); }
  turnFinished(task, turn) {
    const marker = `reaction-turn:${task}:${turn}`;
    if (this.status(marker) === 'done') return;
    // Clear acknowledgements awaiting this answer, while follow-ups queued
    // behind the prior turn keep their eyes until a later native turn ends.
    this.db.exec('BEGIN IMMEDIATE');
    try {
      this.db.prepare("UPDATE message_reactions SET state='remove' WHERE task=? AND after_turn='' AND state IN ('add','active')").run(task);
      this.db.prepare("UPDATE message_reactions SET after_turn='' WHERE task=? AND after_turn=?").run(task, turn);
      this.observed(`turn-seen:${task}`, turn);
      this.observed(marker, 'done');
      this.db.exec('COMMIT');
    } catch (error) { this.db.exec('ROLLBACK'); throw error; }
  }
  tasks() { return this.db.prepare('SELECT DISTINCT key,task FROM receipts WHERE task IS NOT NULL').all(); }
  requester(task) { return this.db.prepare('SELECT actor FROM bindings WHERE task=? LIMIT 1').get(task)?.actor; }
  status(task) { return this.db.prepare('SELECT status FROM observations WHERE task=?').get(task)?.status; }
  observed(task, status) { this.db.prepare('INSERT OR REPLACE INTO observations VALUES(?,?)').run(task, status); }
  outputCursor(task) { return this.db.prepare('SELECT cursor FROM output_cursors WHERE task=?').get(task)?.cursor ?? ''; }
  saveOutputCursor(task, cursor) { this.db.prepare('INSERT OR REPLACE INTO output_cursors VALUES(?,?)').run(task, cursor); }
  outputDelivered(task, event, chunk) {
    return Boolean(this.db.prepare('SELECT 1 FROM output_receipts WHERE task=? AND event=? AND chunk=?').get(task, event, chunk));
  }
  recordOutput(task, event, chunk) {
    this.db.prepare('INSERT OR IGNORE INTO output_receipts VALUES(?,?,?)').run(task, event, chunk);
  }
  stream(task, turn) {
    const row = this.db.prepare('SELECT metadata FROM slack_streams WHERE task=? AND turn=?').get(task, turn);
    return row ? JSON.parse(row.metadata) : undefined;
  }
  saveStream(task, turn, metadata) {
    this.db.prepare('INSERT OR REPLACE INTO slack_streams VALUES(?,?,?)').run(task, turn, JSON.stringify(metadata));
  }
  commitStream(task, turn, metadata, event, chunk) {
    this.db.exec('BEGIN IMMEDIATE');
    try {
      this.saveStream(task, turn, metadata);
      this.recordOutput(task, event, chunk);
      this.db.exec('COMMIT');
    } catch (error) { this.db.exec('ROLLBACK'); throw error; }
  }
  activeStreams(task) {
    return this.db.prepare('SELECT turn,metadata FROM slack_streams WHERE task=?').all(task)
      .map(row => ({ turn: row.turn, ...JSON.parse(row.metadata) })).filter(row => row.ts && !row.finished);
  }
  close() { this.db.close(); this.lease.close(); }
}
