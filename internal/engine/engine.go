// Package engine runs Actions: assess → policy → (approval) → run → audit.
// Every surface (CLI, MCP) goes through Engine; none calls Action.Run directly.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/approval"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/policy"
)

// Outcome statuses.
const (
	StatusOK               = "ok"
	StatusError            = "error"
	StatusApprovalRequired = "approval_required"
	StatusApprovalPending  = "approval_pending"
	StatusDenied           = "denied"
)

// DeniedHint is returned to agents whose action was refused, so that
// they do not try to reach the same result another way.
const DeniedHint = "The action was rejected and will not be executed. Do not pursue the same outcome " +
	"through a workaround, indirect execution, or policy circumvention. Continue only with a " +
	"materially safer alternative; otherwise stop and ask the user."

type Outcome struct {
	Status     string `json:"status"`
	Result     any    `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
	Risk       string `json:"risk,omitempty"`
	Preview    string `json:"preview,omitempty"`
	Hint       string `json:"hint,omitempty"`
}

// Verdict is a human decision on an approval.
type Verdict int

const (
	Undecided Verdict = iota
	Approved
	Denied
)

// Confirmer asks a human about a pending approval right away (terminal
// prompt, macOS dialog). It returns Undecided if nobody answered.
type Confirmer func(ctx context.Context, ap approval.Approval) Verdict

type Engine struct {
	Registry  *action.Registry
	Approvals *approval.Store
	Events    *eventlog.Log
}

// Invoke runs one Action for an actor. If policy asks for approval, an
// approval record is created and confirm (may be nil) is consulted.
func (e *Engine) Invoke(ctx context.Context, actor action.Actor, id string, args json.RawMessage, confirm Confirmer) Outcome {
	a, ok := e.Registry.Get(id)
	if !ok {
		return Outcome{Status: StatusError, Error: fmt.Sprintf("unknown action %q", id)}
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	as, err := a.Assess(ctx, args)
	if err != nil {
		return Outcome{Status: StatusError, Error: err.Error()}
	}
	dec := policy.Decide(actor, as)
	if err := e.Events.Append(eventlog.Event{
		Type: eventlog.ActionRequested, Actor: &actor, Action: id, Args: args,
		Risk: as.Risk.String(), Targets: as.Targets, Decision: string(dec),
	}); err != nil {
		return Outcome{Status: StatusError, Error: "audit log unavailable, refusing to run: " + err.Error()}
	}
	if dec == policy.Allow {
		return e.run(ctx, actor, a, args, "")
	}

	ap, err := e.Approvals.Create(approval.Approval{
		ActionID: id, Args: args, Actor: actor, Risk: as.Risk.String(),
		Targets: as.Targets, Preview: as.Preview,
	})
	if err != nil {
		return Outcome{Status: StatusError, Error: err.Error()}
	}
	if confirm != nil {
		switch confirm(ctx, ap) {
		case Approved:
			if _, err := e.Decide(ap.ID, true, actor); err != nil {
				return Outcome{Status: StatusError, Error: err.Error()}
			}
			return e.RunApproved(ctx, actor, ap.ID)
		case Denied:
			if _, err := e.Decide(ap.ID, false, actor); err != nil {
				return Outcome{Status: StatusError, Error: err.Error()}
			}
			return Outcome{Status: StatusDenied, ApprovalID: ap.ID, Hint: DeniedHint}
		}
	}
	return Outcome{
		Status: StatusApprovalRequired, ApprovalID: ap.ID, Risk: ap.Risk, Preview: ap.Preview,
		Hint: fmt.Sprintf("This action needs the user's approval. Tell the user what you want to do and why; "+
			"they approve it with `cox approve %s`. Then wait for the decision.", ap.ID),
	}
}

// Decide records a human decision on a pending approval.
func (e *Engine) Decide(id string, approve bool, by action.Actor) (approval.Approval, error) {
	ap, err := e.Approvals.Decide(id, approve)
	if err != nil {
		return ap, err
	}
	dec := "denied"
	if approve {
		dec = "approved"
	}
	return ap, e.Events.Append(eventlog.Event{
		Type: eventlog.ApprovalDecided, Actor: &by, Action: ap.ActionID, ApprovalID: id, Decision: dec,
	})
}

// RunApproved runs the exact invocation stored in an approved approval.
// It reports approval_pending if the user has not decided yet.
func (e *Engine) RunApproved(ctx context.Context, actor action.Actor, id string) Outcome {
	ap, err := e.Approvals.Get(id)
	if err != nil {
		return Outcome{Status: StatusError, Error: err.Error()}
	}
	if ap.Actor.Kind != actor.Kind || ap.Actor.Name != actor.Name {
		return Outcome{Status: StatusError, Error: fmt.Sprintf("approval %s belongs to %s", id, ap.Actor)}
	}
	switch ap.Status {
	case approval.Pending:
		return Outcome{Status: StatusApprovalPending, ApprovalID: id, Preview: ap.Preview}
	case approval.Denied:
		return Outcome{Status: StatusDenied, ApprovalID: id, Hint: DeniedHint}
	case approval.Approved:
	default:
		return Outcome{Status: StatusError, ApprovalID: id, Error: fmt.Sprintf("approval %s is %s", id, ap.Status)}
	}
	ap, err = e.Approvals.Consume(id)
	if err != nil {
		return Outcome{Status: StatusError, Error: err.Error()}
	}
	a, ok := e.Registry.Get(ap.ActionID)
	if !ok {
		return Outcome{Status: StatusError, Error: fmt.Sprintf("unknown action %q", ap.ActionID)}
	}
	return e.run(ctx, actor, a, ap.Args, id)
}

// Wait polls an approval until it is decided, ctx ends, or timeout passes.
func (e *Engine) Wait(ctx context.Context, id string, timeout, every time.Duration) (approval.Status, error) {
	deadline := time.Now().Add(timeout)
	for {
		ap, err := e.Approvals.Get(id)
		if err != nil {
			return "", err
		}
		if ap.Status != approval.Pending || time.Now().After(deadline) {
			return ap.Status, nil
		}
		select {
		case <-ctx.Done():
			return ap.Status, ctx.Err()
		case <-time.After(every):
		}
	}
}

func (e *Engine) run(ctx context.Context, actor action.Actor, a *action.Action, args json.RawMessage, approvalID string) Outcome {
	start := time.Now()
	res, err := a.Run(action.WithActor(ctx, actor), args)
	ev := eventlog.Event{
		Type: eventlog.ActionFinished, Actor: &actor, Action: a.ID, ApprovalID: approvalID,
		DurationMs: time.Since(start).Milliseconds(),
	}
	if err != nil {
		ev.Type, ev.Error = eventlog.ActionFailed, err.Error()
	}
	logErr := e.Events.Append(ev)
	if err != nil {
		return Outcome{Status: StatusError, Error: err.Error(), ApprovalID: approvalID}
	}
	out := Outcome{Status: StatusOK, Result: res, ApprovalID: approvalID}
	if logErr != nil {
		out.Hint = "warning: audit log write failed: " + logErr.Error()
	}
	return out
}

// ErrStatus lets surfaces map an Outcome to a Go error.
func (o Outcome) Err() error {
	switch o.Status {
	case StatusOK:
		return nil
	case StatusError:
		return errors.New(o.Error)
	default:
		return errors.New(o.Status)
	}
}
