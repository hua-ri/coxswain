package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
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

func TestRefuse(t *testing.T) {
	e, runs := newEngine(t)
	e.Refuse(agent, "test.run", "revoked")
	evs, _ := e.Events.Read(time.Now())
	if len(evs) != 1 || evs[0].Type != eventlog.ActionRefused || evs[0].Reason != "revoked" || *evs[0].Actor != agent || *runs != 0 {
		t.Fatalf("%+v", evs)
	}
}

// TestCancelInFlight: on shutdown, running actions are cancelled and
// recorded as failed.
func TestCancelInFlight(t *testing.T) {
	e, _ := newEngine(t)
	started := make(chan struct{})
	e.Registry.Register(&action.Action{
		ID: "test.block", Input: map[string]any{"type": "object"},
		Label: func(context.Context, json.RawMessage) (action.Assessment, error) { return action.Assessment{}, nil },
		Run: func(ctx context.Context, _ json.RawMessage) (any, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	done := make(chan Outcome, 1)
	go func() { done <- e.Invoke(context.Background(), agent, "test.block", nil) }()
	<-started
	e.Cancel()
	select {
	case out := <-done:
		if out.Status != StatusError || out.Error != "context canceled" {
			t.Fatalf("%+v", out)
		}
	default:
		t.Fatal("Cancel returned before the action ended")
	}
	evs, _ := e.Events.Read(time.Now())
	if last := evs[len(evs)-1]; last.Type != eventlog.ActionFailed || last.Error != "context canceled" {
		t.Fatalf("%+v", last)
	}
	// The engine keeps working afterwards.
	if out := e.Invoke(context.Background(), agent, "test.run", args(`{"risk":"read"}`)); out.Status != StatusOK {
		t.Fatalf("%+v", out)
	}
}

// TestCancelClient: cancelling a client aborts only its running actions,
// which are recorded as failed because the client was revoked.
func TestCancelClient(t *testing.T) {
	e, _ := newEngine(t)
	started := make(chan string, 2)
	e.Registry.Register(&action.Action{
		ID: "test.block", Input: map[string]any{"type": "object"},
		Label: func(context.Context, json.RawMessage) (action.Assessment, error) { return action.Assessment{}, nil },
		Run: func(ctx context.Context, _ json.RawMessage) (any, error) {
			who, _ := action.ActorFrom(ctx)
			started <- who.Name
			<-ctx.Done()
			return nil, errors.New("command stopped: " + ctx.Err().Error())
		},
	})
	kimi := action.Actor{Kind: action.Agent, Name: "kimi"}
	outs := map[string]chan Outcome{"codex": make(chan Outcome, 1), "kimi": make(chan Outcome, 1)}
	for _, who := range []action.Actor{agent, kimi} {
		go func() { outs[who.Name] <- e.Invoke(context.Background(), who, "test.block", nil) }()
	}
	<-started
	<-started
	e.CancelClient("codex", ErrClientRevoked)
	select {
	case out := <-outs["codex"]:
		if out.Status != StatusError || out.Error != "client revoked" {
			t.Fatalf("%+v", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("codex's action was not cancelled")
	}
	select {
	case out := <-outs["kimi"]:
		t.Fatalf("kimi's action was cancelled: %+v", out)
	case <-time.After(100 * time.Millisecond):
	}
	e.Cancel()
	<-outs["kimi"]

	evs, _ := e.Events.Read(time.Now())
	found := false
	for _, ev := range evs {
		if ev.Type == eventlog.ActionFailed && ev.Actor.Name == "codex" {
			found = ev.Error == "client revoked" && ev.Reason == "client revoked"
		}
		if ev.Type == eventlog.ActionFailed && ev.Actor.Name == "kimi" && ev.Reason != "" {
			t.Fatalf("kimi: %+v", ev)
		}
	}
	if !found {
		t.Fatalf("%+v", evs)
	}
}

// TestRedact: an action's Redact shapes the arguments in the audit log;
// Run still gets them all.
func TestRedact(t *testing.T) {
	var got string
	reg := action.NewRegistry()
	reg.Register(&action.Action{
		ID:    "test.secret",
		Input: map[string]any{"type": "object"},
		Label: func(context.Context, json.RawMessage) (action.Assessment, error) { return action.Assessment{}, nil },
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			got = string(raw)
			return nil, nil
		},
		Redact: func(json.RawMessage) json.RawMessage { return json.RawMessage(`{"content":"redacted"}`) },
	})
	e := &Engine{Registry: reg, Events: eventlog.Open(t.TempDir() + "/events")}
	ch, cancel := e.Events.Subscribe()
	defer cancel()
	if out := e.Invoke(context.Background(), agent, "test.secret", args(`{"content":"s3cret"}`)); out.Status != StatusOK {
		t.Fatalf("%+v", out)
	}
	if got != `{"content":"s3cret"}` {
		t.Fatalf("run got %s", got)
	}
	if ev := <-ch; string(ev.Args) != `{"content":"redacted"}` {
		t.Fatalf("live event: %s", ev.Args)
	}
	evs, _ := e.Events.Read(time.Now())
	for _, ev := range evs {
		if string(ev.Args) != "" && string(ev.Args) != `{"content":"redacted"}` {
			t.Fatalf("logged %s", ev.Args)
		}
	}
}

// detailed is an error with structured details.
type detailed struct{}

func (detailed) Error() string { return "conflict" }
func (detailed) Details() any  { return map[string]any{"conflicts": []string{".spec.replicas"}} }

// TestLabelKnowsTheActorAndErrorsCarryDetails: Label can tell who asks
// (some arguments are for the user only), and an error's details reach
// the outcome.
func TestLabelKnowsTheActorAndErrorsCarryDetails(t *testing.T) {
	reg := action.NewRegistry()
	var seen action.Actor
	reg.Register(&action.Action{
		ID:    "test.who",
		Input: map[string]any{"type": "object"},
		Label: func(ctx context.Context, _ json.RawMessage) (action.Assessment, error) {
			seen, _ = action.ActorFrom(ctx)
			return action.Assessment{}, nil
		},
		Run: func(context.Context, json.RawMessage) (any, error) { return nil, fmt.Errorf("wrapped: %w", detailed{}) },
	})
	e := &Engine{Registry: reg, Events: eventlog.Open(t.TempDir() + "/events")}
	out := e.Invoke(context.Background(), agent, "test.who", nil)
	if seen != agent {
		t.Fatalf("label saw %+v", seen)
	}
	b, _ := json.Marshal(out)
	if out.Status != StatusError || !strings.Contains(string(b), `"details":{"conflicts":[".spec.replicas"]}`) {
		t.Fatalf("%s", b)
	}
}

// TestHumanOnlyIsRefused: a Label that refuses an agent with
// action.HumanOnly is recorded as a refused call, and nothing runs.
func TestHumanOnlyIsRefused(t *testing.T) {
	reg := action.NewRegistry()
	ran := false
	reg.Register(&action.Action{
		ID: "test.trust", Input: map[string]any{"type": "object"},
		Label: func(ctx context.Context, _ json.RawMessage) (action.Assessment, error) {
			return action.Assessment{Risk: action.Write}, action.HumanOnly(ctx, "trusting keys", "trust it in cox desktop")
		},
		Run: func(context.Context, json.RawMessage) (any, error) { ran = true; return nil, nil },
	})
	e := &Engine{Registry: reg, Events: eventlog.Open(t.TempDir() + "/events")}
	out := e.Invoke(context.Background(), agent, "test.trust", nil)
	if out.Status != StatusError || !strings.Contains(out.Error, "ask the user to trust it in cox desktop") || ran {
		t.Fatalf("%+v ran=%v", out, ran)
	}
	evs, _ := e.Events.Read(time.Now())
	if len(evs) != 1 || evs[0].Type != eventlog.ActionRefused || !strings.Contains(evs[0].Reason, "for the user") {
		t.Fatalf("%+v", evs)
	}
	if out := e.Invoke(context.Background(), action.Actor{Kind: action.Human, Name: "desktop"}, "test.trust", nil); out.Status != StatusOK || !ran {
		t.Fatalf("%+v", out)
	}
}
