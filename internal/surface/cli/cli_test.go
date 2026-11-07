package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/approval"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
)

// demo.echo returns its arguments; risk comes from the "risk" flag.
func newEngine(t *testing.T) *engine.Engine {
	reg := action.NewRegistry()
	reg.Register(&action.Action{
		ID:      "demo.echo",
		Summary: "echo arguments",
		Input: map[string]any{"type": "object", "properties": map[string]any{
			"name":  map[string]any{"type": "string"},
			"words": map[string]any{"type": "string"},
			"count": map[string]any{"type": "integer"},
			"risk":  map[string]any{"type": "string"},
			"tags":  map[string]any{"type": "array"},
		}},
		Positional: []string{"name", "words"},
		RestArgs:   true,
		Assess: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in struct{ Risk string }
			action.Decode(raw, &in)
			if in.Risk == "destructive" {
				return action.Assessment{Risk: action.Destructive, Preview: "boom"}, nil
			}
			return action.Assessment{Risk: action.Read}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var m map[string]any
			json.Unmarshal(raw, &m)
			return m, nil
		},
	})
	dir := t.TempDir()
	return &engine.Engine{
		Registry:  reg,
		Approvals: approval.NewStore(dir+"/approvals", time.Minute),
		Events:    eventlog.Open(dir + "/events"),
	}
}

type run struct {
	code           int
	stdout, stderr string
}

func execute(t *testing.T, app *App, stdin string, args ...string) run {
	t.Helper()
	var out, errb bytes.Buffer
	app.Stdin, app.Stdout, app.Stderr = strings.NewReader(stdin), &out, &errb
	root := NewRoot(app, "test")
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return run{Code(err), out.String(), errb.String()}
}

var agent = action.Actor{Kind: action.Agent, Name: "codex"}
var human = action.Actor{Kind: action.Human, Name: "king"}

func TestPositionalAndFlags(t *testing.T) {
	app := &App{Engine: newEngine(t), Actor: agent}
	r := execute(t, app, "", "demo", "echo", "--count", "2", "--tags", "a", "--tags", "b", "web", "--", "df", "-h")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	var got map[string]any
	json.Unmarshal([]byte(r.stdout), &got)
	if got["name"] != "web" || got["words"] != "df -h" || got["count"] != float64(2) || len(got["tags"].([]any)) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestApprovalRequiredExitCode(t *testing.T) {
	app := &App{Engine: newEngine(t), Actor: agent}
	r := execute(t, app, "", "-o", "json", "demo", "echo", "--risk", "destructive")
	if r.code != ExitApproval || !strings.Contains(r.stdout, `"approval_required"`) {
		t.Fatalf("%+v", r)
	}
}

func TestAgentCannotApprove(t *testing.T) {
	e := newEngine(t)
	app := &App{Engine: e, Actor: agent}
	out := e.Invoke(context.Background(), agent, "demo.echo", json.RawMessage(`{"risk":"destructive"}`), nil)
	r := execute(t, app, "y\n", "approve", out.ApprovalID)
	if r.code != ExitFailure {
		t.Fatalf("agent approved: %+v", r)
	}
	nonTTY := &App{Engine: e, Actor: human, HumanTTY: false}
	if r := execute(t, nonTTY, "y\n", "approve", out.ApprovalID); r.code != ExitFailure {
		t.Fatalf("script approved: %+v", r)
	}
}

func TestHumanApprovesThenAgentWaitRuns(t *testing.T) {
	e := newEngine(t)
	out := e.Invoke(context.Background(), agent, "demo.echo", json.RawMessage(`{"risk":"destructive","name":"x"}`), nil)

	list := execute(t, &App{Engine: e, Actor: human, HumanTTY: true}, "", "approvals")
	if !strings.Contains(list.stdout, out.ApprovalID) {
		t.Fatalf("list: %+v", list)
	}
	r := execute(t, &App{Engine: e, Actor: human, HumanTTY: true, Approve: button(engine.Approved)}, "", "approve", out.ApprovalID)
	if r.code != 0 || !strings.Contains(r.stdout, "approved") {
		t.Fatalf("approve: %+v", r)
	}
	w := execute(t, &App{Engine: e, Actor: agent}, "", "approvals", "wait", out.ApprovalID, "--timeout", "1s")
	if w.code != 0 || !strings.Contains(w.stdout, `"name": "x"`) {
		t.Fatalf("wait: %+v", w)
	}
}

func TestConfirmerDenied(t *testing.T) {
	no := func(context.Context, approval.Approval) (engine.Verdict, action.Actor) { return engine.Denied, human }
	r := execute(t, &App{Engine: newEngine(t), Actor: human, Confirm: no}, "", "demo", "echo", "--risk", "destructive")
	if r.code != ExitDenied {
		t.Fatalf("%+v", r)
	}
}

func TestFlagsAfterPositionalWithoutRest(t *testing.T) {
	e := newEngine(t)
	e.Registry.Register(&action.Action{
		ID: "demo.tag", Input: map[string]any{"type": "object", "properties": map[string]any{
			"host": map[string]any{"type": "string"}, "set": map[string]any{"type": "array"}}},
		Positional: []string{"host"},
		Assess:     func(context.Context, json.RawMessage) (action.Assessment, error) { return action.Assessment{}, nil },
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var m map[string]any
			json.Unmarshal(raw, &m)
			return m, nil
		},
	})
	r := execute(t, &App{Engine: e, Actor: agent}, "", "demo", "tag", "web", "--set", "env=prod")
	if r.code != 0 || !strings.Contains(r.stdout, `"host": "web"`) || !strings.Contains(r.stdout, "env=prod") {
		t.Fatalf("%+v", r)
	}
	if r := execute(t, &App{Engine: e, Actor: agent}, "", "demo", "tag", "a", "b"); r.code != ExitFailure {
		t.Fatalf("extra positional must fail: %+v", r)
	}
}

// button is a fake approval dialog that answers v as the human "king".
func button(v engine.Verdict) engine.Confirmer {
	return func(context.Context, approval.Approval) (engine.Verdict, action.Actor) {
		return v, action.Actor{Kind: action.Human, Name: "king", Session: "dialog"}
	}
}

// TestApproveNeedsDialog: a typed "y" no longer approves (review C4); only
// the dialog button does, and the audit log names the dialog.
func TestApproveNeedsDialog(t *testing.T) {
	e := newEngine(t)
	out := e.Invoke(context.Background(), agent, "demo.echo", json.RawMessage(`{"risk":"destructive"}`), nil)
	if r := execute(t, &App{Engine: e, Actor: human, HumanTTY: true}, "y\n", "approve", out.ApprovalID); r.code == 0 {
		t.Fatalf("typed y approved: %+v", r)
	}
	r := execute(t, &App{Engine: e, Actor: human, HumanTTY: true, Approve: button(engine.Undecided)}, "y\n", "approve", out.ApprovalID)
	if r.code != 0 || !strings.Contains(r.stdout, "not approved") {
		t.Fatalf("undecided dialog: %+v", r)
	}
	if ap, _ := e.Approvals.Get(out.ApprovalID); ap.Status != approval.Pending {
		t.Fatalf("status %s", ap.Status)
	}
	r = execute(t, &App{Engine: e, Actor: human, HumanTTY: true, Approve: button(engine.Approved)}, "", "approve", out.ApprovalID)
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	evs, _ := e.Events.Read(time.Now())
	last := evs[len(evs)-1]
	if last.Type != eventlog.ApprovalDecided || last.Actor.Session != "dialog" {
		t.Fatalf("approver: %+v", last)
	}
}

func TestApproveRefusedWithCoxActor(t *testing.T) {
	e := newEngine(t)
	out := e.Invoke(context.Background(), agent, "demo.echo", json.RawMessage(`{"risk":"destructive"}`), nil)
	t.Setenv("COX_ACTOR", "agent:codex")
	r := execute(t, &App{Engine: e, Actor: human, HumanTTY: true, Approve: button(engine.Approved)}, "", "approve", out.ApprovalID)
	if r.code != ExitFailure {
		t.Fatalf("approved with COX_ACTOR set: %+v", r)
	}
}

func TestApprovalsListSanitized(t *testing.T) {
	e := newEngine(t)
	e.Approvals.Create(approval.Approval{ActionID: "demo.echo", Args: json.RawMessage(`{}`), Actor: agent,
		Risk: "destructive", Preview: "rm -rf /data #\rls -la \x1b[2K"})
	r := execute(t, &App{Engine: e, Actor: human, HumanTTY: true}, "", "approvals")
	if strings.ContainsAny(r.stdout, "\r\x1b") || !strings.Contains(r.stdout, `\x0dls`) {
		t.Fatalf("listing not sanitised: %q", r.stdout)
	}
}
