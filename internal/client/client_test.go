package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
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
)

func setup(t *testing.T) (*Client, *clients.Store) {
	t.Helper()
	reg := action.NewRegistry()
	reg.Register(&action.Action{
		ID: "demo.echo", Summary: "echo", Input: map[string]any{"type": "object"},
		Label: func(context.Context, json.RawMessage) (action.Assessment, error) { return action.Assessment{}, nil },
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var in struct{ Fail bool }
			action.Decode(raw, &in)
			if in.Fail {
				return nil, errors.New("boom")
			}
			return map[string]string{"hello": "world"}, nil
		},
	})
	dir := t.TempDir()
	store := clients.NewStore(filepath.Join(dir, "clients.json"))
	srv := httptest.NewServer(server.New(server.Config{
		Engine:  &engine.Engine{Registry: reg, Events: eventlog.Open(filepath.Join(dir, "events"))},
		Clients: &clients.Authorizer{Store: store},
		Token:   "tok", Version: "v",
	}))
	t.Cleanup(srv.Close)
	return New(instance.Info{Addr: strings.TrimPrefix(srv.URL, "http://"), Token: "tok"}, "cli"), store
}

func TestActionsAndInvoke(t *testing.T) {
	c, _ := setup(t)
	ctx := context.Background()
	acts, err := c.Actions(ctx)
	if err != nil || len(acts) != 1 || acts[0].ID != "demo.echo" {
		t.Fatalf("%+v %v", acts, err)
	}
	out, err := c.Invoke(ctx, "demo.echo", nil)
	if err != nil || out.Status != engine.StatusOK || string(out.Result) != `{"hello":"world"}` {
		t.Fatalf("%+v %s %v", out, out.Result, err)
	}
	out, err = c.Invoke(ctx, "demo.echo", json.RawMessage(`{"fail":true}`))
	if err != nil || out.Status != engine.StatusError || out.Error != "boom" {
		t.Fatalf("%+v %v", out, err)
	}
	out, err = c.Invoke(ctx, "nope.x", nil)
	if err != nil || out.Status != engine.StatusError {
		t.Fatalf("unknown action: %+v %v", out, err)
	}
}

func TestBadToken(t *testing.T) {
	c, _ := setup(t)
	c.Info.Token = "wrong"
	if _, err := c.Actions(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("%v", err)
	}
}

func TestClients(t *testing.T) {
	c, store := setup(t)
	store.Trust("codex")
	ctx := context.Background()
	list, err := c.Clients(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "codex" {
		t.Fatalf("%+v %v", list, err)
	}
	if err := c.Revoke(ctx, "codex"); err != nil {
		t.Fatal(err)
	}
	if err := c.Revoke(ctx, "codex"); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("revoke twice: %v", err)
	}
}

func TestEvents(t *testing.T) {
	c, _ := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evs, err := c.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c.Invoke(context.Background(), "demo.echo", nil)
	for _, want := range []string{eventlog.ActionRequested, eventlog.ActionFinished} {
		select {
		case ev := <-evs:
			if ev.Type != want || ev.Action != "demo.echo" {
				t.Fatalf("%+v", ev)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
		}
	}
	cancel()
	for range evs { // closes once ctx ends
	}
}

func TestNotRunning(t *testing.T) {
	if _, err := Discover(context.Background(), t.TempDir(), "cli"); !errors.Is(err, instance.ErrNotRunning) {
		t.Fatalf("%v", err)
	}
	c := New(instance.Info{Addr: "127.0.0.1:1", Token: "t"}, "cli")
	if _, err := c.Actions(context.Background()); !errors.Is(err, instance.ErrNotRunning) {
		t.Fatalf("server gone: %v", err)
	}
}
