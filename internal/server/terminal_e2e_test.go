package server

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/providers/terminal"
	"github.com/hua-ri/coxswain/internal/shaper"
	"github.com/hua-ri/coxswain/internal/term"
)

// TestSharedTerminal: an agent opens a terminal over MCP; the desktop
// sees it, watches the agent's command and types in it; the agent reads
// what the person ran.
func TestSharedTerminal(t *testing.T) {
	f := newConfig(t)
	withTerminals(t, &f)
	(&terminal.Provider{
		Hosts: f.cfg.Terminals.Hosts, Terms: f.cfg.Terminals,
		Artifacts: shaper.NewArtifacts(filepath.Join(t.TempDir(), "artifacts")),
	}).Register(f.cfg.Engine.Registry)
	f.cfg.Clients.Store.Trust("codex")
	h := New(f.cfg)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cs := connectMCP(t, srv.URL, "codex")

	out, err := callTool(t, cs, "terminal_open", map[string]any{"host": "dev"})
	if err != nil || out.Status != "ok" {
		t.Fatalf("%+v %v", out, err)
	}
	id, _ := out.Result.(map[string]any)["id"].(string)

	w := request(t, h, "GET", "/api/v1/terminals", "", map[string]string{"X-Cox-Client": "desktop"})
	var list []term.Session
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 || list[0].ID != id || list[0].Owner.String() != "agent:codex" {
		t.Fatalf("%s", w.Body)
	}
	opened := false
	for _, ev := range lastEvents(t, f) {
		if ev.Type == eventlog.TerminalOpened && ev.Terminal == id && ev.Actor.String() == "agent:codex" {
			opened = true
		}
	}
	if !opened {
		t.Fatal("no terminal.opened event")
	}

	ch, _ := stream(t, srv.URL, id, "desktop")
	if out, _ := callTool(t, cs, "terminal_send", map[string]any{"id": id, "text": "echo from-agent"}); out.Status != "ok" {
		t.Fatalf("%+v", out)
	}
	readUntil(t, ch, "from-agent\n")

	if w := request(t, h, "POST", "/api/v1/terminals/"+id+"/input", "echo from-human\r", map[string]string{"X-Cox-Client": "desktop"}); w.Code != 204 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := callTool(t, cs, "terminal_read", map[string]any{"id": id, "lines": 10})
		if err != nil || out.Status != "ok" {
			t.Fatalf("%+v %v", out, err)
		}
		text := out.Result.(map[string]any)["output"].(map[string]any)["text"].(string)
		if strings.Contains(text, "from-human") {
			if !strings.Contains(text, "from-agent") {
				t.Fatalf("%q", text)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent never read the person's command: %q", text)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if out, _ := callTool(t, cs, "terminal_close", map[string]any{"id": id}); out.Status != "ok" {
		t.Fatalf("%+v", out)
	}
	if list := f.cfg.Terminals.List(); len(list) != 0 {
		t.Fatalf("%+v", list)
	}
}
