package arnold

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

type accessChat interface {
	PostAccessCard(context.Context, string, string, string, AccessCardView) (string, error)
	UpdateAccessCard(context.Context, string, string, AccessCardView) error
	FindAccessCard(context.Context, string, string, string, string) (string, error)
}

func (a *Agent) accessApprover(id string) bool {
	return userID.MatchString(id) && slices.Contains(a.Config.AccessApprovers, id)
}

// ReceiveAccessInteraction runs only on authenticated Socket Mode intake. It
// performs local validation and durable admission; no host CLI runs before ACK.
func (a *Agent) ReceiveAccessInteraction(i AccessInteraction) error {
	if i.Type != "block_actions" || i.Team.ID != a.Config.TeamID || !a.accessApprover(i.User.ID) || len(i.Actions) != 1 {
		return nil
	}
	decision := ""
	switch i.Actions[0].ActionID {
	case "arnold_access_approve":
		decision = "approve"
	case "arnold_access_reject":
		decision = "reject"
	default:
		return nil
	}
	accepted, err := a.Store.acceptAccessAction(i, decision)
	if err != nil {
		return err
	}
	if accepted {
		select {
		case a.accessWake <- struct{}{}:
		default:
		}
	}
	return nil
}

func (a *Agent) PollAccess(ctx context.Context) error {
	chat, chatOK := a.Slack.(accessChat)
	if !chatOK {
		return nil
	}
	if len(a.Config.AccessApprovers) == 0 {
		return a.renderAccessCards(ctx, chat)
	}
	client, ok := a.Paddock.(accessClient)
	if !ok {
		return fmt.Errorf("Paddock access protocol is unavailable")
	}
	refs, err := a.Store.Tasks()
	if err != nil {
		return err
	}
	var failures []error
	existing, err := a.Store.accessCards()
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		team, _, _, err := splitKey(ref.Key)
		if err != nil || team != a.Config.TeamID {
			continue
		}
		proposals, listErr := client.AccessList(ctx, ref.Task)
		seen := map[string]bool{}
		for _, p := range proposals {
			if listErr != nil {
				p.Fresh, p.Actionable, p.CanApprove, p.CanReject = false, false, false, false
			}
			if p.TaskID != ref.Task {
				failures = append(failures, fmt.Errorf("access proposal belongs to a different task"))
				continue
			}
			if p.Status != "pending" {
				// Terminal enumeration reconciles existing controls. Enabling the
				// feature must not backfill historical requests into Slack.
				for _, c := range existing {
					if c.Key == ref.Key && c.TaskID == ref.Task && c.RequestID == p.ID {
						if err := a.Store.settleAccessCard(c.ID, p); err != nil {
							failures = append(failures, err)
							continue
						}
						seen[c.ID] = true
					}
				}
				continue
			}
			card, err := a.Store.saveAccessProposal(ref, p)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			seen[card.ID] = true
		}
		reason := "This proposal changed or is no longer available. Review the current request in Paddock."
		if listErr != nil {
			reason = "Current access could not be verified. Approval is disabled until Paddock inspection succeeds."
			failures = append(failures, listErr)
			if err := a.accessProtocolNotice(ctx, ref); err != nil {
				failures = append(failures, err)
			}
		}
		if err := a.Store.invalidateAccessCards(ref, seen, reason); err != nil {
			failures = append(failures, err)
		}
	}
	if err := a.renderAccessCards(ctx, chat); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (a *Agent) accessProtocolNotice(ctx context.Context, ref TaskRef) error {
	key := "access-guidance:" + ref.Task
	seen, err := a.Store.Status(key)
	if err != nil || seen != "" {
		return err
	}
	task, err := a.Paddock.Show(ctx, ref.Task)
	if err != nil || !task.Blocked {
		return err
	}
	_, channel, thread, err := splitKey(ref.Key)
	if err != nil {
		return err
	}
	text := fmt.Sprintf("This task has pending access requests, but Arnold could not verify the Paddock approval protocol. An operator can inspect them with `paddock access pending %s`. Slack approval controls stay disabled until inspection succeeds.", ref.Task)
	if err = a.Slack.Post(ctx, channel, thread, text, Hash(key)); err != nil {
		return err
	}
	return a.Store.Observed(key, "reported")
}

func (a *Agent) accessView(c accessCard, action *accessAction) AccessCardView {
	p := c.Proposal
	v := AccessCardView{CardID: c.ID, TaskID: c.TaskID, RequestID: c.RequestID, Kind: p.Kind, Summary: p.Summary, Details: p.Details, Status: p.Outcome, Actionable: p.Kind == "network" && c.Active && p.Fresh && p.Actionable && len(a.Config.AccessApprovers) > 0, CanApprove: p.CanApprove, CanReject: p.CanReject}
	if v.Status == "" {
		v.Status = "Request: " + p.Status
	}
	if !c.Active {
		v.Status = "This proposal changed or is unavailable. This card cannot grant access."
	}
	if !p.Fresh {
		v.Status = "Current request could not be verified. Approval is disabled."
	}
	if len(a.Config.AccessApprovers) == 0 {
		v.Status = "Slack approvals are disabled. An operator can review this request in Paddock."
	}
	if action != nil {
		v.Actionable = false
		v.Status = "Decision accepted; awaiting Paddock."
		if len(action.Result) > 0 {
			var result AccessResult
			if json.Unmarshal(action.Result, &result) == nil && result.Summary != "" {
				v.Status = result.Summary
			}
		}
		if action.Error != "" {
			v.Status += " " + action.Error
		}
	}
	return v
}

func (a *Agent) renderAccessCards(ctx context.Context, chat accessChat) error {
	cards, err := a.Store.accessCards()
	if err != nil {
		return err
	}
	var failures []error
	for _, c := range cards {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		team, channel, thread, err := splitKey(c.Key)
		if err != nil || team != a.Config.TeamID {
			continue
		}
		action, err := a.Store.accessAction(c.ID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		v := a.accessView(c, action)
		if c.MessageTS == "" {
			if !c.Active {
				continue
			}
			var ts string
			if c.Phase == "new" {
				initial := v
				initial.Actionable = false
				if _, err := accessCardArgs(channel, initial); err != nil {
					failures = append(failures, err)
					continue
				}
				claimed, err := a.Store.beginAccessPost(c.ID)
				if err != nil {
					failures = append(failures, err)
					continue
				}
				if !claimed {
					continue
				}
				// Post without controls; a crash cannot expose buttons before the
				// originating message is durably bound to this card.
				ts, err = chat.PostAccessCard(ctx, channel, thread, c.ID, initial)
				if err != nil {
					var slackErr SlackError
					var httpErr *slackHTTPError
					if (errors.As(err, &slackErr) && slices.Contains([]string{"invalid_blocks", "invalid_arguments", "not_in_channel", "channel_not_found", "missing_scope", "account_inactive", "token_revoked", "invalid_auth", "not_authed", "is_archived", "restricted_action", "no_permission"}, string(slackErr))) || (errors.As(err, &httpErr) && httpErr.Status == 429) {
						// These explicit Slack rejections prove no message was
						// posted. Transport/server failures remain unresolved.
						if resetErr := a.Store.resetAccessPost(c.ID); resetErr != nil {
							failures = append(failures, resetErr)
						}
					}
					failures = append(failures, fmt.Errorf("access card posting outcome unconfirmed: %w", err))
					continue
				}
			} else {
				ts, err = chat.FindAccessCard(ctx, channel, thread, c.ID, a.BotUserID)
				if err != nil {
					failures = append(failures, err)
					continue
				}
				if ts == "" {
					failures = append(failures, fmt.Errorf("access card %s posting is unresolved; automatic repost is disabled", c.ID))
					continue
				}
			}
			if err = a.Store.bindAccessMessage(c.ID, ts); err != nil {
				failures = append(failures, err)
				continue
			}
			c, err = a.Store.accessCard(c.ID)
			if err != nil {
				failures = append(failures, err)
				continue
			}
		}
		hash := jsonHash(v)
		if c.RenderHash == hash {
			continue
		}
		if v.Actionable {
			v.Token, err = a.Store.rotateAccessToken(c.ID)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if v.Token == "" {
				continue
			} // A click won the race with rendering.
		}
		if err = chat.UpdateAccessCard(ctx, channel, c.MessageTS, v); err != nil {
			failures = append(failures, err)
			continue
		}
		if err = a.Store.accessRenderDone(c.ID, hash); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (a *Agent) DrainAccess(ctx context.Context) error {
	client, ok := a.Paddock.(accessClient)
	if !ok {
		return nil
	}
	actions, err := a.Store.pendingAccessActions()
	if err != nil {
		return err
	}
	var failures []error
	for _, action := range actions {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		card, err := a.Store.accessCard(action.CardID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if action.Phase == "pending" && card.Proposal.Kind != "network" {
			result, _ := json.Marshal(AccessResult{Terminal: true, Summary: "This request kind is unsupported; Arnold did not submit this decision."})
			if err := a.Store.finishAccessAction(action.ID, "done", result, ""); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		if action.Phase == "pending" && (!a.accessApprover(action.Input.Actor.UserID) || action.Input.Actor.WorkspaceID != a.Config.TeamID) {
			result, _ := json.Marshal(AccessResult{Terminal: true, Summary: "The approver is no longer authorized; Arnold did not submit this decision."})
			if err := a.Store.finishAccessAction(action.ID, "done", result, ""); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		var result AccessResult
		var operationErr error
		if action.Phase == "pending" {
			claimed, err := a.Store.claimAccessAction(action.ID)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if !claimed {
				continue
			}
			result, operationErr = client.AccessDecide(ctx, action.RequestID, action.Input)
		} else {
			result, operationErr = client.AccessReconcile(ctx, action.TaskID, action.RequestID)
			var bindingErr error
			result, bindingErr = accessActionResult(action, result)
			operationErr = errors.Join(operationErr, bindingErr)
		}
		if card.Proposal.Kind != "network" && result.RetryDecision {
			// Authoritative absence proves this unsupported action never ran.
			// Ambiguous admitted outcomes continue reconciliation below.
			result.Terminal, result.RetryDecision = true, false
			result.Summary = "This request kind is unsupported; Paddock confirmed this decision was never submitted."
			operationErr = nil
		}
		phase := "reconcile"
		if result.Terminal {
			phase = "done"
		}
		if result.RetryDecision && card.Proposal.Kind == "network" {
			phase = "pending"
		}
		message := ""
		if operationErr != nil {
			message = "Paddock outcome is unconfirmed; Arnold will reconcile before retrying."
			failures = append(failures, operationErr)
		}
		raw, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			failures = append(failures, marshalErr)
			continue
		}
		if err := a.Store.finishAccessAction(action.ID, phase, raw, message); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// Request reconciliation can return an earlier attempt or another operator's
// winner. Preserve that attribution instead of treating it as our own receipt.
func accessActionResult(action accessAction, result AccessResult) (AccessResult, error) {
	if len(result.Receipt) == 0 {
		return result, nil
	}
	var response accessWireResponse
	if err := json.Unmarshal(result.Receipt, &response); err != nil {
		return AccessResult{}, fmt.Errorf("invalid reconciled access receipt")
	}
	r := response.Receipt
	if r == nil {
		return result, nil
	}
	if r.TaskID != action.TaskID || r.RequestID != action.RequestID {
		return AccessResult{}, fmt.Errorf("reconciled access receipt belongs to another task or request")
	}
	if r.ExternalID == action.Input.ExternalID {
		if r.Revision != action.Input.Revision || r.Decision.Value != action.Input.Decision || r.Decision.Actor != action.Input.Actor {
			result.Terminal, result.RetryDecision = false, false
			result.Summary = "The saved decision does not match the original approval. Operator inspection is required."
			return result, fmt.Errorf("reconciled access decision content changed")
		}
		return result, nil
	}
	if result.ReviewEnded {
		// Retry the SAME idempotency key after observing a settled earlier
		// attempt. Paddock returns our existing intent if it has one, and never
		// reissues an uncertain mutation. A newer winning claim still conflicts.
		result.Terminal, result.RetryDecision = false, true
		result.Summary = "An earlier review ended. Paddock will check the original decision key and current preconditions."
	} else {
		result.Summary = "Another recorded review owns this request. " + result.Summary
	}
	return result, nil
}

func (a *Agent) runAccessPoller(ctx context.Context, interval time.Duration) {
	for ctx.Err() == nil {
		if err := a.PollAccess(ctx); err != nil && ctx.Err() == nil {
			a.logError("Access card reconciliation deferred", err)
		}
		if pause(ctx, interval) != nil {
			return
		}
	}
}

func (a *Agent) runAccessDecisions(ctx context.Context, interval time.Duration) {
	for ctx.Err() == nil {
		if err := a.DrainAccess(ctx); err != nil && ctx.Err() == nil {
			a.logError("Access decision reconciliation deferred", err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-a.accessWake:
			timer.Stop()
		case <-timer.C:
		}
	}
}
