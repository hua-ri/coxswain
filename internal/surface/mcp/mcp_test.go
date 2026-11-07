package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/clients"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

var agent = action.Actor{Kind: action.Agent, Name: "claude-code"}

func setup(t *testing.T) (*engine.Engine, *sdk.ClientSession) {
	return setupGate(t, nil)
}

func setupGate(t *testing.T, gate Gate) (*engine.Engine, *sdk.ClientSession) {
	t.Helper()
	reg := action.NewRegistry()
	reg.Register(&action.Action{
		ID: "demo.do", Summary: "demo", Description: "demo action",
		Input:       map[string]any{"type": "object", "properties": map[string]any{"danger": map[string]any{"type": "boolean"}}},
		Annotations: action.Annotations{Destructive: true, Idempotent: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in struct{ Danger bool }
			action.Decode(raw, &in)
			if in.Danger {
				return action.Assessment{Risk: action.Destructive, Preview: "dangerous"}, nil
			}
			return action.Assessment{Risk: action.Read}, nil
		},
		Run: func(context.Context, json.RawMessage) (any, error) { return "done", nil },
	})
	e := &engine.Engine{Registry: reg, Events: eventlog.Open(t.TempDir() + "/events")}
	ct, st := sdk.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := NewServer(e, agent, "test", gate).Connect(ctx, st, nil); err != nil {
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
	if strings.Join(names, ",") != "demo_do" {
		t.Fatalf("tools = %v", names)
	}
}

// TestDestructiveRuns: no approval round trip; the agent's own permission
// prompt is the only one.
func TestDestructiveRuns(t *testing.T) {
	_, cs := setup(t)
	out, isErr := call(t, cs, "demo_do", map[string]any{"danger": true})
	if out.Status != engine.StatusOK || isErr || out.Result != "done" {
		t.Fatalf("%+v %v", out, isErr)
	}
}

func TestAnnotations(t *testing.T) {
	_, cs := setup(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Tools[0].Annotations
	if a == nil || a.ReadOnlyHint || !a.IdempotentHint || a.DestructiveHint == nil || !*a.DestructiveHint ||
		a.OpenWorldHint == nil || *a.OpenWorldHint || a.Title != "demo" {
		t.Fatalf("annotations: %+v", a)
	}
}

func TestGate(t *testing.T) {
	var calls atomic.Int32
	_, cs := setupGate(t, func(context.Context, *sdk.ServerSession) (bool, string) {
		calls.Add(1)
		return false, clients.ReasonDenied
	})
	out, isErr := call(t, cs, "demo_do", nil)
	if out.Status != engine.StatusError || out.Error != clients.NotAuthorized || out.Hint == "" || !isErr {
		t.Fatalf("%+v", out)
	}
	if calls.Load() < 1 {
		t.Fatal("gate not consulted")
	}
}

func TestOffline(t *testing.T) {
	e, online := setup(t)
	ct, st := sdk.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := NewOffline(e.Registry, "test", "server is down").Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if cs.InitializeResult().Instructions != online.InitializeResult().Instructions || cs.InitializeResult().Instructions == "" {
		t.Fatal("offline instructions differ")
	}
	want, _ := online.ListTools(ctx, nil)
	got, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(want.Tools)
	b, _ := json.Marshal(got.Tools)
	if string(a) != string(b) {
		t.Fatalf("offline tools differ:\n%s\n%s", a, b)
	}
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "demo_do"})
	if err != nil || !res.IsError || res.Content[0].(*sdk.TextContent).Text != "server is down" {
		t.Fatalf("%+v %v", res, err)
	}
}
