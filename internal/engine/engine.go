// Package engine runs Actions: assess → audit → run → audit.
// Every surface (CLI, MCP, HTTP) goes through Engine; none calls Action.Run
// directly. Risk is only a label: Coxswain does not ask for approval, the
// agent's own permission prompts do.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/eventlog"
)

// Outcome statuses.
const (
	StatusOK    = "ok"
	StatusError = "error"
)

type Outcome struct {
	Status string `json:"status"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

type Engine struct {
	Registry *action.Registry
	Events   *eventlog.Log
}

// Invoke runs one Action for a client. The request is audited (with its
// risk label and targets) before the Action runs; if the audit log cannot
// be written, it does not run.
func (e *Engine) Invoke(ctx context.Context, client action.Actor, id string, args json.RawMessage) Outcome {
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
	if err := e.Events.Append(eventlog.Event{
		Type: eventlog.ActionRequested, Actor: &client, Action: id, Args: args,
		Risk: as.Risk.String(), Targets: as.Targets,
	}); err != nil {
		return Outcome{Status: StatusError, Error: "audit log unavailable, refusing to run: " + err.Error()}
	}

	start := time.Now()
	res, err := a.Run(action.WithActor(ctx, client), args)
	ev := eventlog.Event{
		Type: eventlog.ActionFinished, Actor: &client, Action: id,
		DurationMs: time.Since(start).Milliseconds(),
	}
	if err != nil {
		ev.Type, ev.Error = eventlog.ActionFailed, err.Error()
	}
	logErr := e.Events.Append(ev)
	if err != nil {
		return Outcome{Status: StatusError, Error: err.Error()}
	}
	out := Outcome{Status: StatusOK, Result: res}
	if logErr != nil {
		out.Hint = "warning: audit log write failed: " + logErr.Error()
	}
	return out
}

// Err lets surfaces map an Outcome to a Go error.
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
