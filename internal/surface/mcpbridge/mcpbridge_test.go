package mcpbridge

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/clients"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/instance"
	"github.com/hua-ri/coxswain/internal/server"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// blocker lets a test watch demo.block: started when it runs, cancelled
// when its context ends.
type blocker struct {
	started, cancelled chan struct{}
}

func newBlocker() *blocker {
	return &blocker{started: make(chan struct{}, 10), cancelled: make(chan struct{}, 10)}
}

// registry has demo.echo (returns msg) and demo.block (waits for its
// context to end).
func registry(b *blocker) *action.Registry {
	reg := action.NewRegistry()
	label := func(context.Context, json.RawMessage) (action.Assessment, error) { return action.Assessment{}, nil }
	reg.Register(&action.Action{
		ID: "demo.echo", Summary: "echo", Description: "echo",
		Input:       map[string]any{"type": "object", "properties": map[string]any{"msg": map[string]any{"type": "string"}}},
		Annotations: action.Annotations{ReadOnly: true},
		Label:       label,
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var in struct{ Msg string }
			action.Decode(raw, &in)
			return in.Msg, nil
		},
	})
	reg.Register(&action.Action{
		ID: "demo.block", Summary: "block", Input: map[string]any{"type": "object"}, Label: label,
		Run: func(ctx context.Context, _ json.RawMessage) (any, error) {
			b.started <- struct{}{}
			<-ctx.Done()
			b.cancelled <- struct{}{}
			return nil, ctx.Err()
		},
	})
	return reg
}

// startServer runs a real server in home with codex trusted. The returned
// func stops it and waits.
func startServer(t *testing.T, home string, b *blocker) func() {
	t.Helper()
	stop, _ := startServerAuth(t, home, b)
	return stop
}

// startServerAuth is startServer that also returns the server's authorizer.
func startServerAuth(t *testing.T, home string, b *blocker) (func(), *clients.Authorizer) {
	t.Helper()
	store := clients.NewStore(filepath.Join(home, "clients.json"))
	store.Trust("codex")
	ready := make(chan struct{})
	auth := &clients.Authorizer{Store: store}
	cfg := server.Config{
		Engine:  &engine.Engine{Registry: registry(b), Events: eventlog.Open(filepath.Join(home, "events"))},
		Clients: auth,
		Version: "test",
		Ready:   func(instance.Info) { close(ready) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, cfg, home) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("server not ready")
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	t.Cleanup(stop)
	return stop, auth
}

// agentSession runs the bridge on pipes and connects an SDK client to it.
func agentSession(t *testing.T, home string) *sdk.ClientSession {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Stdin: inR, Stdout: outW, Home: home, Client: "codex", Version: "test",
			Registry: registry(newBlocker())})
	}()
	t.Cleanup(func() {
		inW.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("bridge did not stop")
		}
	})
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "agent", Version: "0"}, nil).
		Connect(context.Background(), &sdk.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// echo calls demo_echo and returns the result text and whether it is an error.
func echo(t *testing.T, cs *sdk.ClientSession, msg string) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "demo_echo", Arguments: map[string]any{"msg": msg}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	text := res.Content[0].(*sdk.TextContent).Text
	var out engine.Outcome
	if json.Unmarshal([]byte(text), &out) == nil {
		if s, ok := out.Result.(string); ok {
			text = s
		}
	}
	return text, res.IsError
}

func toolsJSON(t *testing.T, cs *sdk.ClientSession) string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res.Tools)
	return string(b)
}

func TestRoundTrip(t *testing.T) {
	home := t.TempDir()
	startServer(t, home, newBlocker())
	cs := agentSession(t, home)
	if got, isErr := echo(t, cs, "hi"); isErr || got != "hi" {
		t.Fatalf("%q %v", got, isErr)
	}
}

// TestStartsWhileServerDown: the agent's handshake succeeds without a
// server, tools are listed, calls report that the server is down, and the
// same session works once the server starts.
func TestStartsWhileServerDown(t *testing.T) {
	home := t.TempDir()
	cs := agentSession(t, home) // initialize while down
	if cs.InitializeResult().ServerInfo.Name != "cox" || cs.InitializeResult().Capabilities.Tools == nil {
		t.Fatalf("%+v", cs.InitializeResult())
	}
	offline := toolsJSON(t, cs)
	if !strings.Contains(offline, "demo_echo") || !strings.Contains(offline, "demo_block") {
		t.Fatalf("tools: %s", offline)
	}
	if got, isErr := echo(t, cs, "down"); !isErr || got != instance.ErrNotRunning.Error() {
		t.Fatalf("down: %q %v", got, isErr)
	}

	startServer(t, home, newBlocker())
	if got, isErr := echo(t, cs, "up"); isErr || got != "up" {
		t.Fatalf("up: %q %v", got, isErr)
	}
	if online := toolsJSON(t, cs); online != offline {
		t.Fatalf("offline tools differ from the server's:\n%s\n%s", offline, online)
	}
}

func TestServerRestart(t *testing.T) {
	home := t.TempDir()
	stop := startServer(t, home, newBlocker())
	cs := agentSession(t, home)
	if got, isErr := echo(t, cs, "one"); isErr || got != "one" {
		t.Fatalf("%q %v", got, isErr)
	}
	stop()
	if got, isErr := echo(t, cs, "down"); !isErr || got != instance.ErrNotRunning.Error() {
		t.Fatalf("server down: %q %v", got, isErr)
	}
	startServer(t, home, newBlocker()) // new token, new session
	if got, isErr := echo(t, cs, "two"); isErr || got != "two" {
		t.Fatalf("after restart: %q %v", got, isErr)
	}
}

// TestServerDiesDuringCall: a call in flight when the server stops ends
// promptly with an error instead of hanging.
func TestServerDiesDuringCall(t *testing.T) {
	home := t.TempDir()
	b := newBlocker()
	stop := startServer(t, home, b)
	cs := agentSession(t, home)
	type res struct {
		r   *sdk.CallToolResult
		err error
	}
	got := make(chan res, 1)
	go func() {
		r, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "demo_block"})
		got <- res{r, err}
	}()
	<-b.started
	stop()
	select {
	case r := <-got:
		if r.err == nil && !r.r.IsError {
			t.Fatalf("call succeeded: %+v", r.r)
		}
		if r.err != nil && !strings.Contains(r.err.Error(), instance.ErrNotRunning.Error()) {
			t.Fatalf("error: %v", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("call hung after the server stopped")
	}
}

// TestCancelThroughBridge: cancelling a call in the agent cancels the
// action on the server.
func TestCancelThroughBridge(t *testing.T) {
	home := t.TempDir()
	b := newBlocker()
	startServer(t, home, b)
	cs := agentSession(t, home)
	ctx, cancel := context.WithCancel(context.Background())
	go cs.CallTool(ctx, &sdk.CallToolParams{Name: "demo_block"})
	<-b.started
	cancel()
	select {
	case <-b.cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the action was not cancelled")
	}
	if got, isErr := echo(t, cs, "after"); isErr || got != "after" {
		t.Fatalf("%q %v", got, isErr)
	}
}

// TestRevokedBetweenCalls: when the server closes the agent's session
// (revoke) between two calls, the next call reconnects and gets the
// server's answer, not "server is not running".
func TestRevokedBetweenCalls(t *testing.T) {
	home := t.TempDir()
	_, auth := startServerAuth(t, home, newBlocker())
	cs := agentSession(t, home)
	if got, isErr := echo(t, cs, "one"); isErr || got != "one" {
		t.Fatalf("%q %v", got, isErr)
	}
	if err := auth.Revoke("codex"); err != nil {
		t.Fatal(err)
	}
	if got, isErr := echo(t, cs, "two"); !isErr || !strings.Contains(got, clients.NotAuthorized) {
		t.Fatalf("after revoke: %q %v", got, isErr)
	}
	auth.Allow("codex")
	if got, isErr := echo(t, cs, "three"); isErr || got != "three" {
		t.Fatalf("after allow: %q %v", got, isErr)
	}
}

// TestRevokedDuringCall: revoking the agent while its call runs returns at
// once and ends the call (the action is cancelled), through the bridge.
func TestRevokedDuringCall(t *testing.T) {
	home := t.TempDir()
	b := newBlocker()
	_, auth := startServerAuth(t, home, b)
	cs := agentSession(t, home)
	type res struct {
		r   *sdk.CallToolResult
		err error
	}
	got := make(chan res, 1)
	go func() {
		r, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "demo_block"})
		got <- res{r, err}
	}()
	<-b.started
	start := time.Now()
	auth.Revoke("codex")
	if d := time.Since(start); d > time.Second {
		t.Fatalf("revoke took %v", d)
	}
	select {
	case <-b.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the action was not cancelled")
	}
	select {
	case r := <-got:
		if r.err == nil && !r.r.IsError {
			t.Fatalf("call succeeded: %+v", r.r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("call hung after revoke")
	}
	if got, isErr := echo(t, cs, "next"); !isErr || !strings.Contains(got, clients.NotAuthorized) {
		t.Fatalf("after revoke: %q %v", got, isErr)
	}
}
