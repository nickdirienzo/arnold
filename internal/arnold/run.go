package arnold

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Serve acknowledges only durably recorded envelopes. Agent work remains in
// Paddock; these goroutines handle transport intake, receipts and polling only.
func Serve(ctx context.Context, c Config, store *Store, p PaddockClient, slack *Slack, interval time.Duration, log *slog.Logger) error {
	if e := p.Check(ctx, c); e != nil {
		return e
	}
	auth, e := slack.Auth(ctx)
	if e != nil {
		return e
	}
	if auth.TeamID != c.TeamID || auth.UserID == "" || auth.BotID == "" {
		return fmt.Errorf("Slack token does not match the configured workspace/bot")
	}
	agent := &Agent{Config: c, Store: store, Paddock: p, Slack: slack, BotUserID: auth.UserID, BotID: auth.BotID, Log: log}
	socketDone := make(chan struct{})
	go func() {
		defer close(socketDone)
		slack.Socket(ctx, agent.Receive, func(id string) { agent.ReactAsync(ctx, id) })
	}()
	defer func() { <-socketDone; agent.WaitReactions() }()
	for ctx.Err() == nil {
		if e := agent.Drain(ctx); e != nil && ctx.Err() == nil {
			agent.logError("Slack delivery pass failed", e)
		}
		if e := agent.Poll(ctx); e != nil && ctx.Err() == nil {
			agent.logError("Paddock output pass failed", e)
		}
		if e := pause(ctx, interval); e != nil {
			break
		}
	}
	return nil
}
