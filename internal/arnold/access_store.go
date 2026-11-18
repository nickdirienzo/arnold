package arnold

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

type accessCard struct {
	ID, Key, TaskID, RequestID, Revision    string
	Proposal                                AccessProposal
	MessageTS, Phase, TokenHash, RenderHash string
	Active                                  bool
}

type accessAction struct {
	ID, CardID, TaskID, RequestID string
	Input                         AccessDecisionInput
	Phase                         string
	Result                        json.RawMessage
	Error                         string
}

const accessCardColumns = "id,key,task,request,revision,snapshot,message_ts,phase,token_hash,render_hash,active"

func scanAccessCard(row interface{ Scan(...any) error }) (c accessCard, err error) {
	var raw string
	err = row.Scan(&c.ID, &c.Key, &c.TaskID, &c.RequestID, &c.Revision, &raw, &c.MessageTS, &c.Phase, &c.TokenHash, &c.RenderHash, &c.Active)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &c.Proposal)
	}
	return
}

func (s *Store) saveAccessProposal(ref TaskRef, p AccessProposal) (accessCard, error) {
	if p.TaskID != ref.Task || p.ID == "" || p.Revision == "" || p.Kind != "network" {
		return accessCard{}, fmt.Errorf("invalid access proposal identity")
	}
	if _, _, _, err := splitKey(ref.Key); err != nil {
		return accessCard{}, err
	}
	id := Hash(ref.Key + "\x00" + p.TaskID + "\x00" + p.ID + "\x00" + p.Revision)
	raw, err := json.Marshal(p)
	if err != nil {
		return accessCard{}, err
	}
	err = s.tx(func(tx *sql.Tx) error {
		var bound bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM receipts WHERE key=? AND task=?)", ref.Key, ref.Task).Scan(&bound); err != nil {
			return err
		}
		if !bound {
			return fmt.Errorf("access task is not bound to this thread")
		}
		if _, err := tx.Exec("UPDATE access_cards SET active=0,token_hash='' WHERE key=? AND task=? AND request=? AND id!=?", ref.Key, p.TaskID, p.ID, id); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO access_cards(id,key,task,request,revision,snapshot) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET snapshot=excluded.snapshot,active=1", id, ref.Key, p.TaskID, p.ID, p.Revision, string(raw))
		return err
	})
	if err != nil {
		return accessCard{}, err
	}
	return s.accessCard(id)
}

func (s *Store) accessCard(id string) (accessCard, error) {
	return scanAccessCard(s.db.QueryRow("SELECT "+accessCardColumns+" FROM access_cards WHERE id=?", id))
}

// Settled requests update the card that was actually reviewed, preserving its
// original scope/revision even if the upstream's post-approval view changed.
func (s *Store) settleAccessCard(id string, p AccessProposal) error {
	c, err := s.accessCard(id)
	if err != nil {
		return err
	}
	c.Proposal.Status, c.Proposal.Fresh, c.Proposal.Outcome = p.Status, p.Fresh, p.Outcome
	c.Proposal.Actionable, c.Proposal.CanApprove, c.Proposal.CanReject = false, false, false
	raw, err := json.Marshal(c.Proposal)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE access_cards SET snapshot=?,token_hash='',active=CASE WHEN phase='new' THEN 0 ELSE active END WHERE id=?", string(raw), id)
	return err
}

func (s *Store) accessCards() ([]accessCard, error) {
	rows, err := s.db.Query("SELECT " + accessCardColumns + " FROM access_cards ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cards []accessCard
	for rows.Next() {
		c, err := scanAccessCard(rows)
		if err != nil {
			return nil, err
		}
		cards = append(cards, c)
	}
	return cards, rows.Err()
}

func (s *Store) invalidateAccessCards(ref TaskRef, seen map[string]bool, reason string) error {
	cards, err := s.accessCards()
	if err != nil {
		return err
	}
	for _, c := range cards {
		if c.Key != ref.Key || c.TaskID != ref.Task || seen[c.ID] || !c.Active {
			continue
		}
		c.Proposal.Fresh, c.Proposal.Actionable, c.Proposal.Outcome = false, false, reason
		raw, err := json.Marshal(c.Proposal)
		if err != nil {
			return err
		}
		if _, err = s.db.Exec("UPDATE access_cards SET snapshot=?,active=0,token_hash='' WHERE id=?", string(raw), c.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) beginAccessPost(id string) (bool, error) {
	r, err := s.db.Exec("UPDATE access_cards SET phase='posting' WHERE id=? AND phase='new'", id)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (s *Store) resetAccessPost(id string) error {
	_, err := s.db.Exec("UPDATE access_cards SET phase='new' WHERE id=? AND phase='posting' AND message_ts=''", id)
	return err
}

func (s *Store) bindAccessMessage(id, ts string) error {
	if ts == "" {
		return fmt.Errorf("access card message timestamp is missing")
	}
	_, err := s.db.Exec("UPDATE access_cards SET message_ts=?,phase='posted' WHERE id=? AND message_ts=''", ts, id)
	return err
}

func (s *Store) accessRenderDone(id, hash string) error {
	_, err := s.db.Exec("UPDATE access_cards SET render_hash=? WHERE id=?", hash, id)
	return err
}

func (s *Store) rotateAccessToken(id string) (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(bytes[:])
	r, err := s.db.Exec("UPDATE access_cards SET token_hash=? WHERE id=? AND active=1 AND phase='posted' AND NOT EXISTS(SELECT 1 FROM access_actions WHERE card=?)", Hash(token), id, id)
	if err != nil {
		return "", err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", nil
	}
	return token, nil
}

// acceptAccessAction does only local work. The socket may acknowledge the click
// after this transaction commits; the worker later calls Paddock.
func (s *Store) acceptAccessAction(i AccessInteraction, decision string) (bool, error) {
	if i.Type != "block_actions" || len(i.Actions) != 1 || !userID.MatchString(i.User.ID) || i.Actions[0].Value == "" || i.Actions[0].ActionID != "arnold_access_"+decision || (decision != "approve" && decision != "reject") {
		return false, nil
	}
	accepted := false
	err := s.tx(func(tx *sql.Tx) error {
		c, err := scanAccessCard(tx.QueryRow("SELECT "+accessCardColumns+" FROM access_cards WHERE token_hash=? AND token_hash!=''", Hash(i.Actions[0].Value)))
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		team, channel, _, err := splitKey(c.Key)
		if err != nil {
			return err
		}
		if team != i.Team.ID || channel != i.Container.ChannelID || c.MessageTS != i.Container.MessageTS || c.Phase != "posted" || c.Proposal.Kind != "network" || !c.Active || !c.Proposal.Fresh || !c.Proposal.Actionable {
			return nil
		}
		if (decision == "approve" && !c.Proposal.CanApprove) || (decision == "reject" && !c.Proposal.CanReject) {
			return nil
		}
		var bound bool
		if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM receipts WHERE key=? AND task=?)", c.Key, c.TaskID).Scan(&bound); err != nil {
			return err
		}
		if !bound {
			return nil
		}
		id := Hash("arnold-access-decision\x00" + c.ID)
		input := AccessDecisionInput{ProtocolVersion: 1, TaskID: c.TaskID, Revision: c.Revision, Decision: decision, ExternalID: id, Actor: AccessActor{Source: "arnold-slack", WorkspaceID: team, UserID: i.User.ID}, Reason: "Approved by operator through Slack"}
		if decision == "reject" {
			input.Reason = "Rejected by operator through Slack"
		}
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		r, err := tx.Exec("INSERT OR IGNORE INTO access_actions(id,card,task,request,payload) VALUES(?,?,?,?,?)", id, c.ID, c.TaskID, c.RequestID, string(raw))
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return err
		}
		accepted = n == 1
		return nil
	})
	return accepted, err
}

func scanAccessAction(row interface{ Scan(...any) error }) (a accessAction, err error) {
	var input, result string
	err = row.Scan(&a.ID, &a.CardID, &a.TaskID, &a.RequestID, &input, &a.Phase, &result, &a.Error)
	if err == nil {
		err = json.Unmarshal([]byte(input), &a.Input)
	}
	if result != "" {
		a.Result = json.RawMessage(result)
	}
	return
}

func (s *Store) accessAction(card string) (*accessAction, error) {
	a, err := scanAccessAction(s.db.QueryRow("SELECT id,card,task,request,payload,phase,result,error FROM access_actions WHERE card=?", card))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &a, err
}

func (s *Store) pendingAccessActions() ([]accessAction, error) {
	rows, err := s.db.Query("SELECT id,card,task,request,payload,phase,result,error FROM access_actions WHERE phase!='done' ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var actions []accessAction
	for rows.Next() {
		a, err := scanAccessAction(rows)
		if err != nil {
			return nil, err
		}
		actions = append(actions, a)
	}
	return actions, rows.Err()
}

func (s *Store) claimAccessAction(id string) (bool, error) {
	r, err := s.db.Exec("UPDATE access_actions SET phase='claimed' WHERE id=? AND phase='pending'", id)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (s *Store) finishAccessAction(id, phase string, result json.RawMessage, message string) error {
	_, err := s.db.Exec("UPDATE access_actions SET phase=?,result=?,error=? WHERE id=? AND phase!='done'", phase, string(result), message, id)
	return err
}

// AccessWatches enrolls tasks only through authored activity handled by Bind.
// Enabling approval polling must not backfill historical receipt/task bindings.
func (s *Store) accessWatches() ([]TaskRef, error) {
	rows, err := s.db.Query("SELECT key,task FROM access_watches ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskRef
	for rows.Next() {
		var ref TaskRef
		if err = rows.Scan(&ref.Key, &ref.Task); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// A restart is not fresh conversation activity. Existing cards/actions retain
// their own recovery state, but historical bindings cannot create new posts.
func (s *Store) resetAccessWatches() error {
	_, err := s.db.Exec("DELETE FROM access_watches")
	return err
}
