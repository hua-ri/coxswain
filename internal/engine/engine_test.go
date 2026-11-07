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
	yes := func(context.Context, approval.Approval) (Verdict, action.Actor) { return Approved, human }
	no := func(context.Context, approval.Approval) (Verdict, action.Actor) { return Denied, human }
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

// TestDecisionRecordsApprover: the audit log names the human who decided,
// not the agent that asked (review I2).
func TestDecisionRecordsApprover(t *testing.T) {
	e, _ := newEngine(t)
	dlg := action.Actor{Kind: action.Human, Name: "king", Session: "dialog"}
	yes := func(context.Context, approval.Approval) (Verdict, action.Actor) { return Approved, dlg }
	if out := e.Invoke(context.Background(), agent, "test.run", args(`{"risk":"destructive"}`), yes); out.Status != StatusOK {
		t.Fatalf("%+v", out)
	}
	evs, _ := e.Events.Read(time.Now())
	for _, ev := range evs {
		if ev.Type == eventlog.ApprovalDecided {
			if *ev.Actor != dlg {
				t.Fatalf("decided by %+v, want %+v", *ev.Actor, dlg)
			}
			return
		}
	}
	t.Fatalf("no approval.decided event: %+v", evs)
}

// TestConfirmRace: if someone decides while the dialog is open, the
// approved confirmer falls through to the stored decision.
func TestConfirmRace(t *testing.T) {
	e, runs := newEngine(t)
	racer := func(_ context.Context, ap approval.Approval) (Verdict, action.Actor) {
		e.Decide(ap.ID, true, human) // `cox approve` won the race
		return Approved, human
	}
	out := e.Invoke(context.Background(), agent, "test.run", args(`{"risk":"destructive"}`), racer)
	if out.Status != StatusOK || *runs != 1 || out.ApprovalID == "" {
		t.Fatalf("%+v runs=%d", out, *runs)
	}
	denier := func(_ context.Context, ap approval.Approval) (Verdict, action.Actor) {
		e.Decide(ap.ID, false, human)
		return Approved, human
	}
	out = e.Invoke(context.Background(), agent, "test.run", args(`{"risk":"destructive"}`), denier)
	if out.Status != StatusDenied || out.ApprovalID == "" || *runs != 1 {
		t.Fatalf("%+v runs=%d", out, *runs)
	}
}

func TestRunApprovedChecksSession(t *testing.T) {
	e, runs := newEngine(t)
	ctx := context.Background()
	s1 := action.Actor{Kind: action.Agent, Name: "claude-code", Session: "s1"}
	s2 := action.Actor{Kind: action.Agent, Name: "claude-code", Session: "s2"}
	out := e.Invoke(ctx, s1, "test.run", args(`{"risk":"destructive"}`), nil)
	e.Decide(out.ApprovalID, true, human)
	if got := e.RunApproved(ctx, s2, out.ApprovalID); got.Status != StatusError || got.ApprovalID == "" || *runs != 0 {
		t.Fatalf("other session ran it: %+v", got)
	}
	if got := e.RunApproved(ctx, s1, out.ApprovalID); got.Status != StatusOK {
		t.Fatalf("%+v", got)
	}
}
