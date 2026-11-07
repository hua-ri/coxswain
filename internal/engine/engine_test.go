package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/eventlog"
)

var agent = action.Actor{Kind: action.Agent, Name: "codex"}

// newEngine registers "test.run": args {"risk":"read|write|destructive"}
// on a prod or dev target ({"prod":true}); {"fail":true} makes Run fail.
// It counts runs.
func newEngine(t *testing.T) (*Engine, *int) {
	t.Helper()
	runs := 0
	reg := action.NewRegistry()
	reg.Register(&action.Action{
		ID:    "test.run",
		Input: map[string]any{"type": "object"},
		Assess: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in struct {
				Risk string `json:"risk"`
				Prod bool   `json:"prod"`
			}
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			r := map[string]action.Risk{"read": action.Read, "write": action.Write, "destructive": action.Destructive}[in.Risk]
			env := "dev"
			if in.Prod {
				env = "prod"
			}
			return action.Assessment{Risk: r, Preview: "do " + in.Risk,
				Targets: []action.Target{{Kind: "host", ID: "h", Tags: map[string]string{"env": env}}}}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var in struct{ Fail bool }
			action.Decode(raw, &in)
			if in.Fail {
				return nil, errors.New("boom")
			}
			runs++
			return map[string]int{"runs": runs}, nil
		},
	})
	return &Engine{Registry: reg, Events: eventlog.Open(t.TempDir() + "/events")}, &runs
}

func args(s string) json.RawMessage { return json.RawMessage(s) }

// TestRiskIsOnlyALabel: Coxswain no longer asks; an agent's destructive
// action on prod runs and the audit log records its risk.
func TestRiskIsOnlyALabel(t *testing.T) {
	cases := []struct{ risk, args string }{
		{"read", `{"risk":"read","prod":true}`},
		{"write", `{"risk":"write","prod":true}`},
		{"destructive", `{"risk":"destructive","prod":true}`},
	}
	for _, c := range cases {
		t.Run(c.risk, func(t *testing.T) {
			e, runs := newEngine(t)
			out := e.Invoke(context.Background(), agent, "test.run", args(c.args))
			if out.Status != StatusOK || *runs != 1 {
				t.Fatalf("got %+v runs=%d", out, *runs)
			}
			evs, _ := e.Events.Read(time.Now())
			if len(evs) != 2 || evs[0].Type != eventlog.ActionRequested || evs[1].Type != eventlog.ActionFinished {
				t.Fatalf("events: %+v", evs)
			}
			if evs[0].Risk != c.risk || evs[0].Targets[0].Tags["env"] != "prod" || *evs[0].Actor != agent {
				t.Fatalf("requested event: %+v", evs[0])
			}
		})
	}
}

func TestRunFailure(t *testing.T) {
	e, _ := newEngine(t)
	out := e.Invoke(context.Background(), agent, "test.run", args(`{"fail":true}`))
	if out.Status != StatusError || out.Error != "boom" || out.Err() == nil {
		t.Fatalf("got %+v", out)
	}
	evs, _ := e.Events.Read(time.Now())
	if len(evs) != 2 || evs[1].Type != eventlog.ActionFailed || evs[1].Error != "boom" {
		t.Fatalf("events: %+v", evs)
	}
}

func TestUnknownAction(t *testing.T) {
	e, _ := newEngine(t)
	if out := e.Invoke(context.Background(), agent, "nope.x", nil); out.Status != StatusError {
		t.Fatalf("got %+v", out)
	}
}
