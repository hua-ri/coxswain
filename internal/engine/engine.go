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
	"sync"
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
	// Details describe an error for programs (e.g. apply conflicts).
	Details any `json:"details,omitempty"`
}

// Detailer is an error with structured details for the outcome.
type Detailer interface {
	error
	Details() any
}

// ErrClientRevoked is why a revoked client's running actions stop.
var ErrClientRevoked = errors.New("client revoked")

type Engine struct {
	Registry *action.Registry
	Events   *eventlog.Log

	mu       sync.Mutex
	inflight map[*int]running
	wg       sync.WaitGroup
}

// running is one action in progress.
type running struct {
	client action.Actor
	cancel context.CancelCauseFunc
}

// Refuse records that client was not allowed to run id.
func (e *Engine) Refuse(client action.Actor, id, reason string) {
	e.Events.Append(eventlog.Event{Type: eventlog.ActionRefused, Actor: &client, Action: id, Reason: reason})
}

// Cancel cancels every running action and waits for them to finish (and
// be recorded). Used on shutdown; the engine keeps working afterwards.
func (e *Engine) Cancel() {
	e.mu.Lock()
	for _, r := range e.inflight {
		r.cancel(nil)
	}
	e.mu.Unlock()
	e.wg.Wait()
}

// CancelClient cancels the running actions of the agent named name, with
// cause (e.g. ErrClientRevoked) as the reason they fail. It does not wait.
func (e *Engine) CancelClient(name string, cause error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.inflight {
		if r.client.Kind == action.Agent && r.client.Name == name {
			r.cancel(cause)
		}
	}
}

// track makes ctx cancellable by Cancel and CancelClient until done is
// called.
func (e *Engine) track(ctx context.Context, client action.Actor) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	key := new(int)
	e.mu.Lock()
	if e.inflight == nil {
		e.inflight = map[*int]running{}
	}
	e.inflight[key] = running{client: client, cancel: cancel}
	e.wg.Add(1)
	e.mu.Unlock()
	return ctx, func() {
		e.mu.Lock()
		delete(e.inflight, key)
		e.mu.Unlock()
		cancel(nil)
		e.wg.Done()
	}
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
	// Label knows who asks: some arguments are for the user only.
	as, err := a.Label(action.WithActor(ctx, client), args)
	if errors.Is(err, action.ErrHumanOnly) {
		e.Refuse(client, id, err.Error())
	}
	if err != nil {
		return Outcome{Status: StatusError, Error: err.Error()}
	}
	logged := args
	if a.Redact != nil {
		logged = a.Redact(args)
	}
	if err := e.Events.Append(eventlog.Event{
		Type: eventlog.ActionRequested, Actor: &client, Action: id, Args: logged,
		Risk: as.Risk.String(), Targets: as.Targets,
	}); err != nil {
		return Outcome{Status: StatusError, Error: "audit log unavailable, refusing to run: " + err.Error()}
	}

	ctx, done := e.track(ctx, client)
	defer done()
	start := time.Now()
	res, err := a.Run(action.WithAssessment(action.WithActor(ctx, client), as), args)
	ev := eventlog.Event{
		Type: eventlog.ActionFinished, Actor: &client, Action: id,
		DurationMs: time.Since(start).Milliseconds(),
	}
	if cause := context.Cause(ctx); err != nil && errors.Is(cause, ErrClientRevoked) {
		err = cause // whatever the action said, it stopped because of this
		ev.Reason = cause.Error()
	}
	if err != nil {
		ev.Type, ev.Error = eventlog.ActionFailed, err.Error()
	}
	logErr := e.Events.Append(ev)
	if err != nil {
		out := Outcome{Status: StatusError, Error: err.Error()}
		var d Detailer
		if errors.As(err, &d) {
			out.Details = d.Details()
		}
		return out
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
