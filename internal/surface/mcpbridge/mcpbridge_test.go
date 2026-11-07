package mcpbridge

import (
	"bufio"
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

// startServer runs a real server in home with demo.echo and codex trusted.
// The returned func stops it and waits.
func startServer(t *testing.T, home string) func() {
	t.Helper()
	reg := action.NewRegistry()
	reg.Register(&action.Action{
		ID: "demo.echo", Summary: "echo", Description: "echo",
		Input: map[string]any{"type": "object", "properties": map[string]any{"msg": map[string]any{"type": "string"}}},
		Label: func(context.Context, json.RawMessage) (action.Assessment, error) { return action.Assessment{}, nil },
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var in struct{ Msg string }
			action.Decode(raw, &in)
			return in.Msg, nil
		},
	})
	store := clients.NewStore(filepath.Join(home, "clients.json"))
	store.Trust("codex")
	cfg := server.Config{
		Engine:  &engine.Engine{Registry: reg, Events: eventlog.Open(filepath.Join(home, "events"))},
		Clients: &clients.Authorizer{Store: store},
		Version: "test",
	}
	ready := make(chan struct{})
	cfg.Ready = func(instance.Info) { close(ready) }
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
	return stop
}

// startBridge runs the bridge on pipes; it returns the agent's ends.
func startBridge(t *testing.T, home string) (io.WriteCloser, io.ReadCloser) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, inR, outW, home, "codex") }()
	t.Cleanup(func() {
		inW.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("bridge did not stop")
		}
	})
	return inW, outR
}

func agentSession(t *testing.T, home string) *sdk.ClientSession {
	t.Helper()
	in, out := startBridge(t, home)
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "agent", Version: "0"}, nil).
		Connect(context.Background(), &sdk.IOTransport{Reader: out, Writer: in}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func echo(cs *sdk.ClientSession, msg string) (string, error) {
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "demo_echo", Arguments: map[string]any{"msg": msg}})
	if err != nil {
		return "", err
	}
	var out engine.Outcome
	json.Unmarshal([]byte(res.Content[0].(*sdk.TextContent).Text), &out)
	s, _ := out.Result.(string)
	return s, nil
}

func TestRoundTrip(t *testing.T) {
	home := t.TempDir()
	startServer(t, home)
	cs := agentSession(t, home)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 1 {
		t.Fatalf("%+v %v", tools, err)
	}
	if got, err := echo(cs, "hi"); err != nil || got != "hi" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestServerRestart(t *testing.T) {
	home := t.TempDir()
	stop := startServer(t, home)
	cs := agentSession(t, home)
	if got, err := echo(cs, "one"); err != nil || got != "one" {
		t.Fatalf("%q %v", got, err)
	}
	stop()
	if _, err := echo(cs, "down"); err == nil || !strings.Contains(err.Error(), instance.ErrNotRunning.Error()) {
		t.Fatalf("server down: %v", err)
	}
	startServer(t, home) // new token, new session
	if got, err := echo(cs, "two"); err != nil || got != "two" {
		t.Fatalf("after restart: %q %v", got, err)
	}
}

type rpc struct {
	ID     any             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func TestNoServerThenServer(t *testing.T) {
	home := t.TempDir()
	in, out := startBridge(t, home)
	lines := bufio.NewScanner(out)
	send := func(s string) {
		if _, err := io.WriteString(in, s+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	recv := func() rpc {
		t.Helper()
		got := make(chan rpc, 1)
		go func() {
			var r rpc
			if lines.Scan() {
				json.Unmarshal(lines.Bytes(), &r)
			}
			got <- r
		}()
		select {
		case r := <-got:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("no reply")
		}
		return rpc{}
	}
	initialize := func(id int) string {
		return `{"jsonrpc":"2.0","id":` + string(rune('0'+id)) + `,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`
	}

	send(initialize(1))
	r := recv()
	if r.Error == nil || r.Error.Code != -32000 || r.Error.Message != instance.ErrNotRunning.Error() || r.ID != float64(1) {
		t.Fatalf("%+v", r)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`) // dropped while down

	startServer(t, home)
	send(initialize(2))
	if r := recv(); r.Error != nil || r.ID != float64(2) || !strings.Contains(string(r.Result), `"cox"`) {
		t.Fatalf("%+v %s", r, r.Result)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"demo_echo","arguments":{"msg":"up"}}}`)
	if r := recv(); r.Error != nil || r.ID != float64(3) || !strings.Contains(string(r.Result), `up`) {
		t.Fatalf("%+v %s", r, r.Result)
	}
}
