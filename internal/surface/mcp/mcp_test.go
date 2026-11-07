package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/approval"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

var agent = action.Actor{Kind: action.Agent, Name: "claude-code"}

func setup(t *testing.T) (*engine.Engine, *sdk.ClientSession) {
	t.Helper()
	reg := action.NewRegistry()
	reg.Register(&action.Action{
		ID: "demo.do", Summary: "demo", Description: "demo action",
		Input: map[string]any{"type": "object", "properties": map[string]any{"danger": map[string]any{"type": "boolean"}}},
		Assess: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in struct{ Danger bool }
			action.Decode(raw, &in)
			if in.Danger {
				return action.Assessment{Risk: action.Destructive, Preview: "dangerous"}, nil
			}
			return action.Assessment{Risk: action.Read}, nil
		},
		Run: func(context.Context, json.RawMessage) (any, error) { return "done", nil },
	})
	dir := t.TempDir()
	e := &engine.Engine{
		Registry:  reg,
		Approvals: approval.NewStore(dir+"/approvals", time.Minute),
		Events:    eventlog.Open(dir + "/events"),
	}
	ct, st := sdk.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := NewServer(e, agent, nil, "test").Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return e, cs
}

func call(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) (engine.Outcome, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var out engine.Outcome
	if err := json.Unmarshal([]byte(res.Content[0].(*sdk.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	return out, res.IsError
}

func TestListTools(t *testing.T) {
	_, cs := setup(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "cox_approval_wait,demo_do" {
		t.Fatalf("tools = %v", names)
	}
}

func TestApprovalRoundTrip(t *testing.T) {
	e, cs := setup(t)
	out, isErr := call(t, cs, "demo_do", map[string]any{"danger": true})
	if out.Status != engine.StatusApprovalRequired || isErr || !strings.Contains(out.Hint, "cox_approval_wait") {
		t.Fatalf("%+v", out)
	}
	pending, _ := call(t, cs, "cox_approval_wait", map[string]any{"approval_id": out.ApprovalID, "timeout_sec": 1})
	if pending.Status != engine.StatusApprovalPending {
		t.Fatalf("%+v", pending)
	}
	e.Decide(out.ApprovalID, true, action.Actor{Kind: action.Human, Name: "king"})
	done, _ := call(t, cs, "cox_approval_wait", map[string]any{"approval_id": out.ApprovalID})
	if done.Status != engine.StatusOK || done.Result != "done" {
		t.Fatalf("%+v", done)
	}
}

func TestDeniedIsError(t *testing.T) {
	e, cs := setup(t)
	out, _ := call(t, cs, "demo_do", map[string]any{"danger": true})
	e.Decide(out.ApprovalID, false, action.Actor{Kind: action.Human, Name: "king"})
	denied, isErr := call(t, cs, "cox_approval_wait", map[string]any{"approval_id": out.ApprovalID})
	if denied.Status != engine.StatusDenied || !isErr || denied.Hint != engine.DeniedHint {
		t.Fatalf("%+v %v", denied, isErr)
	}
}
