package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/approval"
	"github.com/hua-ri/coxswain/internal/eventlog"
)

var (
	agent = action.Actor{Kind: action.Agent, Name: "codex"}
	human = action.Actor{Kind: action.Human, Name: "king"}
)

// newEngine registers "test.run": args {"risk":"read|write|destructive"}
// on a prod or dev target ({"prod":true}); it counts runs.
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
		Run: func(context.Context, json.RawMessage) (any, error) { runs++; return map[string]int{"runs": runs}, nil },
	})
	dir := t.TempDir()
	return &Engine{
		Registry:  reg,
		Approvals: approval.NewStore(dir+"/approvals", 10*time.Minute),
		Events:    eventlog.Open(dir + "/events"),
	}, &runs
}

func args(s string) json.RawMessage { return json.RawMessage(s) }

func TestReadRunsImmediately(t *testing.T) {
	e, runs := newEngine(t)
	out := e.Invoke(context.Background(), agent, "test.run", args(`{"risk":"read","prod":true}`), nil)
	if out.Status != StatusOK || *runs != 1 {
		t.Fatalf("got %+v runs=%d", out, *runs)
	}
	evs, _ := e.Events.Read(time.Now())
	if len(evs) != 2 || evs[0].Type != eventlog.ActionRequested || evs[1].Type != eventlog.ActionFinished {
		t.Fatalf("events: %+v", evs)
	}
}

func TestAgentProdWriteNeedsApproval(t *testing.T) {
	e, runs := newEngine(t)
	ctx := context.Background()
	out := e.Invoke(ctx, agent, "test.run", args(`{"risk":"write","prod":true}`), nil)
	if out.Status != StatusApprovalRequired || out.ApprovalID == "" || *runs != 0 {
		t.Fatalf("got %+v", out)
	}
	if got := e.RunApproved(ctx, agent, out.ApprovalID); got.Status != StatusApprovalPending {
		t.Fatalf("before decision: %+v", got)
	}
	if _, err := e.Decide(out.ApprovalID, true, human); err != nil {
		t.Fatal(err)
	}
	if got := e.RunApproved(ctx, human, out.ApprovalID); got.Status != StatusError {
		t.Fatalf("other actor must not run it: %+v", got)
	}
	if got := e.RunApproved(ctx, agent, out.ApprovalID); got.Status != StatusOK || *runs != 1 {
		t.Fatalf("after approval: %+v", got)
	}
	if got := e.RunApproved(ctx, agent, out.ApprovalID); got.Status != StatusError || *runs != 1 {
		t.Fatalf("approval reused: %+v", got)
	}
}

func TestConfirmerApproveAndDeny(t *testing.T) {
	e, runs := newEngine(t)
	ctx := context.Background()
	yes := func(context.Context, approval.Approval) Verdict { return Approved }
	no := func(context.Context, approval.Approval) Verdict { return Denied }
	if out := e.Invoke(ctx, agent, "test.run", args(`{"risk":"destructive"}`), yes); out.Status != StatusOK || *runs != 1 {
		t.Fatalf("approved: %+v", out)
	}
	out := e.Invoke(ctx, agent, "test.run", args(`{"risk":"destructive"}`), no)
	if out.Status != StatusDenied || out.Hint != DeniedHint || *runs != 1 {
		t.Fatalf("denied: %+v", out)
	}
}

func TestWait(t *testing.T) {
	e, _ := newEngine(t)
	out := e.Invoke(context.Background(), agent, "test.run", args(`{"risk":"destructive"}`), nil)
	go func() {
		time.Sleep(50 * time.Millisecond)
		e.Decide(out.ApprovalID, false, human)
	}()
	st, err := e.Wait(context.Background(), out.ApprovalID, 2*time.Second, 10*time.Millisecond)
	if err != nil || st != approval.Denied {
		t.Fatalf("wait: %v %v", st, err)
	}
}

func TestUnknownAction(t *testing.T) {
	e, _ := newEngine(t)
	if out := e.Invoke(context.Background(), agent, "nope.x", nil, nil); out.Status != StatusError {
		t.Fatalf("got %+v", out)
	}
}
