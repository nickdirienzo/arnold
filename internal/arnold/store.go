package arnold

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct {
	db, leaseDB *sql.DB
	lease       *sql.Conn
}

func OpenStore(path string) (s *Store, err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	s = &Store{}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	// Set modes before SQLite creates its WAL/journal sidecars. Pending Slack
	// delivery payloads can contain private conversation context.
	for _, file := range []string{path, path + ".lock"} {
		f, e := os.OpenFile(file, os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			return nil, e
		}
		e = f.Chmod(0600)
		f.Close()
		if e != nil {
			return nil, e
		}
	}
	s.leaseDB, err = sql.Open("sqlite", path+".lock")
	if err != nil {
		return nil, err
	}
	s.leaseDB.SetMaxOpenConns(1)
	s.lease, err = s.leaseDB.Conn(context.Background())
	if err != nil {
		return nil, err
	}
	if _, err = s.lease.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS lease (id INTEGER); BEGIN EXCLUSIVE;`); err != nil {
		return nil, fmt.Errorf("another Arnold instance owns the state: %w", err)
	}
	s.db, err = sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	s.db.SetMaxOpenConns(1)
	_, err = s.db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;
 CREATE TABLE IF NOT EXISTS threads (key TEXT PRIMARY KEY,task TEXT,actor TEXT,identity TEXT,profile TEXT);
 CREATE TABLE IF NOT EXISTS bindings (key TEXT,actor TEXT,identity TEXT,profile TEXT,task TEXT,PRIMARY KEY(key,actor,identity,profile));
 CREATE TABLE IF NOT EXISTS receipts (id TEXT PRIMARY KEY,key TEXT,payload TEXT,phase TEXT DEFAULT 'pending',task TEXT,notice TEXT);
 CREATE TABLE IF NOT EXISTS observations (task TEXT PRIMARY KEY,status TEXT);
 CREATE TABLE IF NOT EXISTS output_cursors (task TEXT PRIMARY KEY,cursor TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS output_receipts (task TEXT,event TEXT,chunk INTEGER,PRIMARY KEY(task,event,chunk));
 CREATE TABLE IF NOT EXISTS slack_streams (task TEXT,turn TEXT,metadata TEXT,PRIMARY KEY(task,turn));
 CREATE TABLE IF NOT EXISTS message_reactions (receipt TEXT PRIMARY KEY,channel TEXT,ts TEXT,task TEXT,after_turn TEXT DEFAULT '',state TEXT DEFAULT 'add');
 CREATE TABLE IF NOT EXISTS access_cards (id TEXT PRIMARY KEY,key TEXT NOT NULL,task TEXT NOT NULL,request TEXT NOT NULL,revision TEXT NOT NULL,snapshot TEXT NOT NULL,message_ts TEXT NOT NULL DEFAULT '',phase TEXT NOT NULL DEFAULT 'new',token_hash TEXT NOT NULL DEFAULT '',render_hash TEXT NOT NULL DEFAULT '',active INTEGER NOT NULL DEFAULT 1);
 CREATE UNIQUE INDEX IF NOT EXISTS access_card_revision ON access_cards(key,task,request,revision);
 CREATE INDEX IF NOT EXISTS access_card_token ON access_cards(token_hash);
 CREATE TABLE IF NOT EXISTS access_actions (id TEXT PRIMARY KEY,card TEXT NOT NULL UNIQUE,task TEXT NOT NULL,request TEXT NOT NULL,payload TEXT NOT NULL,phase TEXT NOT NULL DEFAULT 'pending',result TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '');
 INSERT OR IGNORE INTO bindings SELECT key,actor,identity,profile,task FROM threads;`)
	if err != nil {
		return nil, err
	}
	err = os.Chmod(path, 0600)
	return s, err
}
func (s *Store) Close() {
	if s == nil {
		return
	}
	if s.db != nil {
		s.db.Close()
	}
	if s.lease != nil {
		s.lease.ExecContext(context.Background(), "ROLLBACK")
		s.lease.Close()
	}
	if s.leaseDB != nil {
		s.leaseDB.Close()
	}
}
func (s *Store) tx(fn func(*sql.Tx) error) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = fn(tx); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) binding(query string, args ...any) (*Binding, error) {
	b := &Binding{}
	e := s.db.QueryRow(query, args...).Scan(&b.Key, &b.Task, &b.Actor, &b.Identity, &b.Profile)
	if e == sql.ErrNoRows {
		return nil, nil
	}
	return b, e
}
func (s *Store) Thread(key string) (*Binding, error) {
	return s.binding("SELECT key,task,actor,identity,profile FROM threads WHERE key=?", key)
}
func (s *Store) Binding(key, actor, identity, profile string) (*Binding, error) {
	return s.binding("SELECT key,task,actor,identity,profile FROM bindings WHERE key=? AND actor=? AND identity=? AND profile=?", key, actor, identity, profile)
}
func (s *Store) Active(key string) (bool, error) {
	var n int
	e := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM threads WHERE key=? UNION ALL SELECT 1 FROM receipts WHERE key=? AND phase='pending')", key, key).Scan(&n)
	return n != 0, e
}
func (s *Store) Accept(id, key string, p Payload) (inserted bool, err error) {
	b, e := json.Marshal(p)
	if e != nil {
		return false, e
	}
	err = s.tx(func(tx *sql.Tx) error {
		r, e := tx.Exec("INSERT OR IGNORE INTO receipts(id,key,payload) VALUES(?,?,?)", id, key, string(b))
		if e != nil {
			return e
		}
		n, e := r.RowsAffected()
		if e != nil {
			return e
		}
		inserted = n > 0
		if inserted {
			_, e = tx.Exec("INSERT INTO message_reactions(receipt,channel,ts) VALUES(?,?,?)", id, p.Event.Channel, p.Event.TS)
		}
		return e
	})
	return
}
func (s *Store) Prepare(id string, p Payload) error {
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	_, e = s.db.Exec("UPDATE receipts SET payload=? WHERE id=?", string(b), id)
	return e
}
func (s *Store) receipts(phase string) ([]Receipt, error) {
	rows, e := s.db.Query("SELECT id,key,COALESCE(payload,''),phase,COALESCE(task,''),COALESCE(notice,'') FROM receipts WHERE phase=? ORDER BY rowid", phase)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Receipt
	for rows.Next() {
		var r Receipt
		var raw string
		if e = rows.Scan(&r.ID, &r.Key, &raw, &r.Phase, &r.Task, &r.Notice); e != nil {
			return nil, e
		}
		if raw != "" {
			r.Payload = &Payload{}
			if e = json.Unmarshal([]byte(raw), r.Payload); e != nil {
				return nil, e
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) Pending() ([]Receipt, error) { return s.receipts("pending") }
func (s *Store) Notices() ([]Receipt, error) { return s.receipts("accepted") }
func (s *Store) Bind(r Receipt, task, actor, identity, profile, afterTurn string) error {
	return s.tx(func(tx *sql.Tx) error {
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{"INSERT OR REPLACE INTO threads VALUES(?,?,?,?,?)", []any{r.Key, task, actor, identity, profile}},
			{"INSERT OR REPLACE INTO bindings VALUES(?,?,?,?,?)", []any{r.Key, actor, identity, profile, task}},
			{"UPDATE receipts SET task=?,phase='accepted',payload=NULL WHERE id=?", []any{task, r.ID}},
			{"UPDATE message_reactions SET task=?,after_turn=? WHERE receipt=?", []any{task, afterTurn, r.ID}},
		} {
			if _, e := tx.Exec(q.sql, q.args...); e != nil {
				return e
			}
		}
		return nil
	})
}
func (s *Store) Finish(id, notice string) error {
	_, e := s.db.Exec("UPDATE receipts SET phase='accepted',payload=NULL,notice=? WHERE id=?", notice, id)
	return e
}
func (s *Store) Done(id string) error {
	return s.tx(func(tx *sql.Tx) error {
		if _, e := tx.Exec("UPDATE message_reactions SET state='remove' WHERE receipt=? AND state!='done' AND EXISTS(SELECT 1 FROM receipts WHERE id=? AND notice IS NOT NULL AND notice!='')", id, id); e != nil {
			return e
		}
		_, e := tx.Exec("UPDATE receipts SET phase='done',notice=NULL WHERE id=?", id)
		return e
	})
}
func (s *Store) Tasks() ([]TaskRef, error) {
	rows, e := s.db.Query("SELECT DISTINCT key,task FROM receipts WHERE task IS NOT NULL ORDER BY rowid")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []TaskRef
	for rows.Next() {
		var r TaskRef
		if e = rows.Scan(&r.Key, &r.Task); e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) Requester(task string) (string, error) {
	var a string
	e := s.db.QueryRow("SELECT actor FROM bindings WHERE task=? LIMIT 1", task).Scan(&a)
	if e == sql.ErrNoRows {
		return "", nil
	}
	return a, e
}
func (s *Store) Status(key string) (string, error) {
	var v string
	e := s.db.QueryRow("SELECT status FROM observations WHERE task=?", key).Scan(&v)
	if e == sql.ErrNoRows {
		return "", nil
	}
	return v, e
}
func (s *Store) Observed(key, value string) error {
	_, e := s.db.Exec("INSERT OR REPLACE INTO observations VALUES(?,?)", key, value)
	return e
}
func (s *Store) Cursor(task string) (string, error) {
	var v string
	e := s.db.QueryRow("SELECT cursor FROM output_cursors WHERE task=?", task).Scan(&v)
	if e == sql.ErrNoRows {
		return "", nil
	}
	return v, e
}
func (s *Store) SaveCursor(task, cursor string) error {
	_, e := s.db.Exec("INSERT OR REPLACE INTO output_cursors VALUES(?,?)", task, cursor)
	return e
}
func (s *Store) Delivered(task, event string, chunk int) (bool, error) {
	var n int
	e := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM output_receipts WHERE task=? AND event=? AND chunk=?)", task, event, chunk).Scan(&n)
	return n != 0, e
}
func (s *Store) RecordOutput(task, event string, chunk int) error {
	_, e := s.db.Exec("INSERT OR IGNORE INTO output_receipts VALUES(?,?,?)", task, event, chunk)
	return e
}
func (s *Store) Stream(task, turn string) (*Stream, error) {
	var raw string
	e := s.db.QueryRow("SELECT metadata FROM slack_streams WHERE task=? AND turn=?", task, turn).Scan(&raw)
	if e == sql.ErrNoRows {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var m Stream
	e = json.Unmarshal([]byte(raw), &m)
	return &m, e
}
func (s *Store) SaveStream(task, turn string, m Stream) error {
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	_, e = s.db.Exec("INSERT OR REPLACE INTO slack_streams VALUES(?,?,?)", task, turn, string(b))
	return e
}
func (s *Store) CommitStream(task, turn string, m Stream, event string, chunk int) error {
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	return s.tx(func(tx *sql.Tx) error {
		if _, e := tx.Exec("INSERT OR REPLACE INTO slack_streams VALUES(?,?,?)", task, turn, string(b)); e != nil {
			return e
		}
		_, e := tx.Exec("INSERT OR IGNORE INTO output_receipts VALUES(?,?,?)", task, event, chunk)
		return e
	})
}
func (s *Store) ActiveStreams(task string) ([]StreamRef, error) {
	rows, e := s.db.Query("SELECT turn,metadata FROM slack_streams WHERE task=?", task)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []StreamRef
	for rows.Next() {
		var r StreamRef
		var raw string
		if e = rows.Scan(&r.Turn, &raw); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(raw), &r.Stream); e != nil {
			return nil, e
		}
		if r.TS != "" && !r.Finished {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}
func scanReaction(row interface{ Scan(...any) error }) (*Reaction, error) {
	r := &Reaction{}
	e := row.Scan(&r.ID, &r.Channel, &r.TS, &r.Task, &r.AfterTurn, &r.State)
	if e == sql.ErrNoRows {
		return nil, nil
	}
	return r, e
}

const reactionFields = "receipt,channel,ts,COALESCE(task,''),COALESCE(after_turn,''),state"

func (s *Store) Reaction(id string) (*Reaction, error) {
	return scanReaction(s.db.QueryRow("SELECT "+reactionFields+" FROM message_reactions WHERE receipt=?", id))
}
func (s *Store) Reactions() ([]Reaction, error) {
	rows, e := s.db.Query("SELECT " + reactionFields + " FROM message_reactions WHERE state IN ('add','remove')")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Reaction
	for rows.Next() {
		r, e := scanReaction(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}
func (s *Store) Reacted(id, before, after string) error {
	_, e := s.db.Exec("UPDATE message_reactions SET state=? WHERE receipt=? AND state=?", after, id, before)
	return e
}
func (s *Store) TurnFinished(task, turn string) error {
	return s.tx(func(tx *sql.Tx) error {
		marker := "reaction-turn:" + task + ":" + turn
		var v string
		e := tx.QueryRow("SELECT status FROM observations WHERE task=?", marker).Scan(&v)
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		if v == "done" {
			return nil
		}
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{"UPDATE message_reactions SET state='remove' WHERE task=? AND after_turn='' AND state IN ('add','active')", []any{task}},
			{"UPDATE message_reactions SET after_turn='' WHERE task=? AND after_turn=?", []any{task, turn}},
			{"INSERT OR REPLACE INTO observations VALUES(?,?)", []any{"turn-seen:" + task, turn}},
			{"INSERT OR REPLACE INTO observations VALUES(?,?)", []any{marker, "done"}},
		} {
			if _, e := tx.Exec(q.sql, q.args...); e != nil {
				return e
			}
		}
		return nil
	})
}
