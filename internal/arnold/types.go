package arnold

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

type Actor struct {
	Identity        string `json:"identity"`
	Profile         string `json:"profile"`
	RecipientUserID string `json:"recipientUserId,omitempty"`
}
type Route struct {
	Repo              string `json:"repo"`
	AutoSubscribe     bool   `json:"autoSubscribe,omitempty"`
	AutoSubscribeBots bool   `json:"autoSubscribeBots,omitempty"`
}
type Trigger struct {
	ID       string `json:"id"`
	Channel  string `json:"channel"`
	BotID    string `json:"botId,omitempty"`
	UserID   string `json:"userId,omitempty"`
	Contains string `json:"contains"`
}
type Config struct {
	TeamID     string           `json:"teamId"`
	Channels   map[string]Route `json:"channels"`
	Users      map[string]Actor `json:"users"`
	Automation *Actor           `json:"automation,omitempty"`
	Triggers   []Trigger        `json:"triggers"`
	Agent      string           `json:"agent,omitempty"`
	IdleTTL    string           `json:"idleTtl,omitempty"`
}

var channelID = regexp.MustCompile(`^[CG][A-Z0-9]+$`)
var userID = regexp.MustCompile(`^[UW][A-Z0-9]+$`)

func LoadConfig(path string) (Config, error) {
	var c Config
	b, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	if e = json.Unmarshal(b, &c); e != nil {
		return c, e
	}
	return c, c.Validate()
}
func (c Config) AgentName() string {
	if c.Agent == "" {
		return "codex"
	}
	return c.Agent
}
func normalizeIdleTTL(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return "", fmt.Errorf("idleTtl must be a positive Go duration, e.g. 24h; omit for no TTL")
	}
	return duration.String(), nil
}
func (c Config) Validate() error {
	fail := func(s string) error { return fmt.Errorf("configuration: %s", s) }
	if c.TeamID == "" || c.Channels == nil || c.Users == nil {
		return fail("teamId, channels and users are required")
	}
	if c.AgentName() != "codex" && c.AgentName() != "claude" {
		return fail("agent must be codex or claude")
	}
	if _, err := normalizeIdleTTL(c.IdleTTL); err != nil {
		return fail(err.Error())
	}
	if len(c.Triggers) > 0 && c.Automation == nil {
		return fail("configured triggers require an automation identity")
	}
	for id, r := range c.Channels {
		if !channelID.MatchString(id) || r.Repo == "" {
			return fail("channels require Slack IDs and a repository")
		}
		if r.AutoSubscribeBots && (!r.AutoSubscribe || c.Automation == nil) {
			return fail("autoSubscribeBots requires autoSubscribe and an automation identity")
		}
	}
	actors := []Actor{}
	for _, a := range c.Users {
		actors = append(actors, a)
	}
	if c.Automation != nil {
		actors = append(actors, *c.Automation)
		if c.Automation.RecipientUserID != "" && !userID.MatchString(c.Automation.RecipientUserID) {
			return fail("automation recipientUserId must be a Slack member ID")
		}
	}
	for _, a := range actors {
		if a.Identity == "" || (a.Profile != "investigation" && a.Profile != "implementation") {
			return fail("each identity needs an allowed profile")
		}
	}
	seen := map[string]bool{}
	for _, t := range c.Triggers {
		if t.ID == "" || seen[t.ID] || c.Channels[t.Channel].Repo == "" || (t.BotID == "") == (t.UserID == "") || strings.TrimSpace(t.Contains) == "" {
			return fail("triggers need unique IDs, configured channels, one source and nonempty contains")
		}
		seen[t.ID] = true
	}
	return nil
}

type Event struct {
	Type     string `json:"type"`
	Channel  string `json:"channel"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts,omitempty"`
	User     string `json:"user,omitempty"`
	Text     string `json:"text,omitempty"`
	BotID    string `json:"bot_id,omitempty"`
	AppID    string `json:"app_id,omitempty"`
	Subtype  string `json:"subtype,omitempty"`
	Hidden   bool   `json:"hidden,omitempty"`
}
type Envelope struct {
	TeamID string `json:"team_id"`
	Event  Event  `json:"event"`
}
type Binding struct {
	Key      string `json:"key"`
	Task     string `json:"task"`
	Actor    string `json:"actor"`
	Identity string `json:"identity"`
	Profile  string `json:"profile"`
}
type Prepared struct {
	Prompt    string   `json:"prompt"`
	Previous  *Binding `json:"previous"`
	Target    *Binding `json:"target"`
	Identity  string   `json:"identity"`
	Profile   string   `json:"profile"`
	Agent     string   `json:"agent"`
	AfterTurn string   `json:"afterTurn"`
	Repo      string   `json:"repo"`
	IdleTTL   string   `json:"idleTtl,omitempty"`
}
type Payload struct {
	Event    Event     `json:"event"`
	Actor    string    `json:"actor"`
	Prepared *Prepared `json:"prepared,omitempty"`
}
type Receipt struct {
	ID, Key, Phase, Task, Notice string
	Payload                      *Payload
}
type Reaction struct{ ID, Channel, TS, Task, AfterTurn, State string }
type Task struct {
	ID         string `json:"id"`
	Runtime    string `json:"runtime"`
	Status     string `json:"status"`
	Blocked    bool   `json:"blocked"`
	IdleTTL    string `json:"idle_ttl,omitempty"`
	Provenance struct {
		Identity    string `json:"identity"`
		Profile     string `json:"profile"`
		Requester   string `json:"requester"`
		GitHubActor string `json:"github_actor"`
	} `json:"provenance"`
	Session struct {
		Agent          string `json:"agent"`
		ConversationID string `json:"conversation_id"`
	} `json:"session"`
	Terminal struct {
		State string `json:"state"`
	} `json:"terminal"`
	LastTurn struct {
		ID    string `json:"id"`
		State string `json:"state"`
	} `json:"last_turn"`
}
type OutputEvent struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	TurnID string `json:"turn_id"`
	Phase  string `json:"phase,omitempty"`
	Text   string `json:"text,omitempty"`
	Part   int    `json:"part,omitempty"`
	Parts  int    `json:"parts,omitempty"`
	State  string `json:"state,omitempty"`
}
type OutputPage struct {
	TaskID     string        `json:"task_id"`
	Events     []OutputEvent `json:"events"`
	NextCursor string        `json:"next_cursor"`
	HasMore    bool          `json:"has_more"`
}
type RunOptions struct {
	Prompt, Repo, Actor, Identity, Profile, Agent, ExternalID string
	IdleTTL                                                   string
	Context                                                   map[string]string
}
type MessageReceipt struct {
	Status string `json:"status"`
}
type TaskRef struct{ Key, Task string }
type Stream struct {
	TS         string `json:"ts,omitempty"`
	Started    int64  `json:"started,omitempty"`
	Characters int    `json:"characters,omitempty"`
	Tasks      int    `json:"tasks,omitempty"`
	Finished   bool   `json:"finished"`
}
type StreamRef struct {
	Turn string
	Stream
}
type Recipient struct{ TeamID, UserID string }
type StreamEvent struct {
	Task, Channel, ThreadTS, Text string
	Event                         OutputEvent
	Chunk                         int
	Recipient                     Recipient
}

type PaddockClient interface {
	Check(context.Context, Config) error
	Run(context.Context, RunOptions) (Task, error)
	Show(context.Context, string) (Task, error)
	Message(context.Context, string, string, string) (MessageReceipt, error)
	Output(context.Context, string, string) (OutputPage, error)
}
type Chat interface {
	Context(context.Context, string, string, string) (string, error)
	Post(context.Context, string, string, string, string) error
	Reaction(context.Context, string, string, bool) error
	StreamEvent(context.Context, StreamEvent, *Store) error
	StreamStatus(context.Context, string, string, string, *Store) error
}

func Hash(s string) string  { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func jsonHash(v any) string { b, _ := json.Marshal(v); return Hash(string(b)) }
