package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/clients"
	"github.com/hua-ri/coxswain/internal/engine"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// headers adds the token and client name to every request.
type headers struct {
	token, client string
}

func (h headers) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.token)
	r.Header.Set(ClientHeader, h.client)
	return http.DefaultTransport.RoundTrip(r)
}

func connectMCP(t *testing.T, url, client string) *sdk.ClientSession {
	t.Helper()
	tr := &sdk.StreamableClientTransport{
		Endpoint:   url + "/mcp",
		HTTPClient: &http.Client{Transport: headers{token, client}},
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil).Connect(context.Background(), tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *sdk.ClientSession, name string, args any) (engine.Outcome, error) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return engine.Outcome{}, err
	}
	var out engine.Outcome
	if err := json.Unmarshal([]byte(res.Content[0].(*sdk.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

func TestMCPToolsAndCall(t *testing.T) {
	f := newConfig(t)
	f.cfg.Clients.Store.Trust("codex")
	srv := httptest.NewServer(New(f.cfg))
	t.Cleanup(srv.Close) // after the sessions close
	cs := connectMCP(t, srv.URL, "codex")

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "demo_echo" || res.Tools[0].Annotations == nil ||
		!res.Tools[0].Annotations.ReadOnlyHint || res.Tools[0].Annotations.DestructiveHint == nil {
		t.Fatalf("tools: %+v", res.Tools)
	}
	out, err := callTool(t, cs, "demo_echo", map[string]any{"msg": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := out.Result.(map[string]any); out.Status != "ok" || r["actor"] != "agent:codex" {
		t.Fatalf("%+v", out)
	}
	if f.asked.Load() != 0 {
		t.Fatal("trusted client was asked")
	}
}

func TestMCPUnauthorizedAskedOnce(t *testing.T) {
	f := newConfig(t)
	release := make(chan struct{})
	f.cfg.Clients.Ask = func(context.Context, string) (bool, error) {
		f.asked.Add(1)
		<-release
		return false, nil
	}
	srv := httptest.NewServer(New(f.cfg))
	t.Cleanup(srv.Close) // after the sessions close
	a, b := connectMCP(t, srv.URL, "kimi"), connectMCP(t, srv.URL, "kimi")

	var wg sync.WaitGroup
	outs := make([]engine.Outcome, 2)
	for i, cs := range []*sdk.ClientSession{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], _ = callTool(t, cs, "demo_echo", nil)
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	for _, out := range outs {
		if out.Error != clients.NotAuthorized || out.Hint == "" {
			t.Fatalf("%+v", out)
		}
	}
	if n := f.asked.Load(); n != 1 {
		t.Fatalf("asked %d times", n)
	}
	// The denial holds for a while: no new prompt.
	if out, _ := callTool(t, a, "demo_echo", nil); out.Error != clients.NotAuthorized || f.asked.Load() != 1 {
		t.Fatalf("%+v asked=%d", out, f.asked.Load())
	}
	// Allowing later revives the same session.
	f.cfg.Clients.Allow("kimi")
	if out, _ := callTool(t, a, "demo_echo", nil); out.Status != "ok" {
		t.Fatalf("after allow: %+v", out)
	}
}

// TestMCPRevokeAllow: revoking closes the client's sessions; a new session
// is refused without a dialog; allowing it works at once.
func TestMCPRevokeAllow(t *testing.T) {
	f := newConfig(t)
	f.cfg.Clients.Store.Trust("codex")
	srv := httptest.NewServer(New(f.cfg))
	t.Cleanup(srv.Close) // after the sessions close
	cs := connectMCP(t, srv.URL, "codex")
	if out, err := callTool(t, cs, "demo_echo", nil); err != nil || out.Status != "ok" {
		t.Fatalf("%+v %v", out, err)
	}
	if err := f.cfg.Clients.Revoke("codex"); err != nil {
		t.Fatal(err)
	}
	if out, err := callTool(t, cs, "demo_echo", nil); err == nil && out.Status == "ok" {
		t.Fatalf("revoked session still works: %+v", out)
	}
	again := connectMCP(t, srv.URL, "codex")
	out, err := callTool(t, again, "demo_echo", nil)
	if err != nil || out.Error != clients.NotAuthorized || !strings.Contains(out.Hint, "cox clients allow codex") {
		t.Fatalf("%+v %v", out, err)
	}
	if f.asked.Load() != 0 {
		t.Fatal("asked about a revoked client")
	}
	f.cfg.Clients.Allow("codex")
	if out, err := callTool(t, again, "demo_echo", nil); err != nil || out.Status != "ok" {
		t.Fatalf("after allow: %+v %v", out, err)
	}
}
