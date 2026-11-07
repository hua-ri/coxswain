package ssh

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/providers/artifact"
	"github.com/hua-ri/coxswain/internal/shaper"
	"github.com/hua-ri/coxswain/internal/sshx"
	"github.com/hua-ri/coxswain/internal/sshx/sshtest"
)

var (
	agent = action.Actor{Kind: action.Agent, Name: "codex"}
	human = action.Actor{Kind: action.Human, Name: "king"}
)

func setup(t *testing.T) (*engine.Engine, *sshtest.Server) {
	return setupWith(t, false)
}

// setupWith registers the provider, with a connection pool if pooled.
func setupWith(t *testing.T, pooled bool) (*engine.Engine, *sshtest.Server) {
	t.Helper()
	srv := sshtest.Start(t)
	dir := t.TempDir()
	hosts := inventory.NewStore(filepath.Join(dir, "hosts.json"))
	hosts.Merge([]inventory.Host{
		{Name: "dev", HostName: srv.Host, Port: srv.Port, IdentityFiles: []string{srv.KeyFile}},
		{Name: "prod", HostName: srv.Host, Port: srv.Port, IdentityFiles: []string{srv.KeyFile}},
	})
	hosts.Update(func(m map[string]inventory.Host) error {
		h := m["prod"]
		h.Tags = map[string]string{"env": "prod"}
		m["prod"] = h
		return nil
	})
	reg := action.NewRegistry()
	p := &Provider{
		Hosts:     hosts,
		Dialer:    &sshx.Dialer{Hosts: hosts, KnownHosts: []string{srv.KnownHosts}},
		Artifacts: shaper.NewArtifacts(filepath.Join(dir, "artifacts")),
		UserHome:  dir,
	}
	if pooled {
		p.Pool = &sshx.Pool{Dialer: p.Dialer}
		t.Cleanup(func() { p.Pool.Close() })
	}
	p.Register(reg)
	return &engine.Engine{Registry: reg, Events: eventlog.Open(filepath.Join(dir, "events"))}, srv
}

func execArgs(host, cmd string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"host": host, "command": cmd})
	return b
}

func TestAgentReadsProd(t *testing.T) {
	e, _ := setup(t)
	out := e.Invoke(context.Background(), agent, "ssh.exec", execArgs("prod", "echo hello"))
	if out.Status != engine.StatusOK {
		t.Fatalf("%+v", out)
	}
	res := out.Result.(ExecOutput)
	if res.Stdout.Text != "hello\n" || res.ExitCode != 0 {
		t.Fatalf("%+v", res)
	}
}

// TestAgentDestructiveOnProdRuns: Coxswain no longer asks; the agent's own
// permission prompt applies. The command runs and the audit log labels it.
func TestAgentDestructiveOnProdRuns(t *testing.T) {
	e, srv := setup(t)
	dir := t.TempDir()
	out := e.Invoke(context.Background(), agent, "ssh.exec", execArgs("prod", "rm -rf "+dir))
	if out.Status != engine.StatusOK || out.Result.(ExecOutput).ExitCode != 0 {
		t.Fatalf("%+v", out)
	}
	if c := <-srv.Commands; c != "rm -rf "+dir {
		t.Fatalf("remote ran %q", c)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("command did not run: %v", err)
	}
	evs, _ := e.Events.Read(time.Now())
	if evs[0].Type != eventlog.ActionRequested || evs[0].Risk != "destructive" || evs[0].Targets[0].Tags["env"] != "prod" {
		t.Fatalf("requested event: %+v", evs[0])
	}
}

func TestAgentOutputIsShaped(t *testing.T) {
	e, _ := setup(t)
	out := e.Invoke(context.Background(), agent, "ssh.exec", execArgs("dev", "seq 1 5000"))
	res := out.Result.(ExecOutput)
	if !res.Stdout.Truncated || res.Stdout.ArtifactID == "" || res.Stdout.TotalLines != 5000 {
		t.Fatalf("agent output not shaped: %+v", res.Stdout)
	}
	hout := e.Invoke(context.Background(), human, "ssh.exec", execArgs("dev", "seq 1 5000"))
	if hres := hout.Result.(ExecOutput); hres.Stdout.Truncated {
		t.Fatal("human output must not be shaped")
	}
}

func TestTagAndNote(t *testing.T) {
	e, _ := setup(t)
	out := e.Invoke(context.Background(), agent, "ssh.hosts.tag", json.RawMessage(`{"host":"dev","set":["team=pay"],"note":"canary"}`))
	if out.Status != engine.StatusOK {
		t.Fatalf("%+v", out)
	}
	if h := out.Result.(inventory.Host); h.Tags["team"] != "pay" || h.Note != "canary" {
		t.Fatalf("%+v", h)
	}
	evs, _ := e.Events.Read(time.Now())
	if evs[0].Risk != "destructive" {
		t.Fatalf("a note change is labelled destructive: %+v", evs[0])
	}
}

func TestImportAndList(t *testing.T) {
	e, _ := setup(t)
	cfg := filepath.Join(t.TempDir(), "config")
	os.WriteFile(cfg, []byte("Host bastion\n  HostName 10.0.0.1\n"), 0o600)
	out := e.Invoke(context.Background(), human, "ssh.hosts.import", json.RawMessage(fmt.Sprintf(`{"path":%q}`, cfg)))
	if out.Status != engine.StatusOK {
		t.Fatalf("%+v", out)
	}
	list := e.Invoke(context.Background(), agent, "ssh.hosts.list", json.RawMessage(`{"tag":"env=prod"}`))
	if hs := list.Result.([]inventory.Host); len(hs) != 1 || hs[0].Name != "prod" {
		t.Fatalf("%+v", list.Result)
	}
}

func importArgs(t *testing.T, path string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"path": path})
	return b
}

func TestImportLabel(t *testing.T) {
	e, srv := setup(t)
	a, _ := e.Registry.Get("ssh.hosts.import")
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	os.WriteFile(cfg, []byte(fmt.Sprintf("Host dev\n  HostName %s\n  Port %d\nHost fresh\n  HostName 192.0.2.9\n  User ops\n", srv.Host, srv.Port)), 0o600)
	as, err := a.Label(context.Background(), importArgs(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, tg := range as.Targets {
		ids = append(ids, tg.ID)
	}
	if strings.Join(ids, ",") != "dev,prod" || as.Risk != action.Destructive {
		t.Fatalf("targets %v risk %v", ids, as.Risk)
	}
	if !strings.Contains(as.Preview, "add fresh -> ops@192.0.2.9:22") ||
		!strings.Contains(as.Preview, fmt.Sprintf("change dev -> %s:%d", srv.Host, srv.Port)) {
		t.Fatalf("preview: %s", as.Preview)
	}

	var many strings.Builder
	for i := 0; i < 25; i++ {
		fmt.Fprintf(&many, "Host n%02d\n  HostName 192.0.2.%d\n", i, i)
	}
	os.WriteFile(cfg, []byte(many.String()), 0o600)
	as, _ = a.Label(context.Background(), importArgs(t, cfg))
	if !strings.Contains(as.Preview, "add n19 -> 192.0.2.19:22") || strings.Contains(as.Preview, "n20") ||
		!strings.HasSuffix(as.Preview, "…") {
		t.Fatalf("capped preview: %s", as.Preview)
	}
}

func TestAnnotations(t *testing.T) {
	e, _ := setup(t)
	artifact.Register(e.Registry, shaper.NewArtifacts(t.TempDir()))
	want := map[string]action.Annotations{
		"ssh.hosts.list":   {ReadOnly: true, Idempotent: true},
		"ssh.hosts.import": {Idempotent: true},
		"ssh.hosts.tag":    {Destructive: true, Idempotent: true},
		"ssh.exec":         {Destructive: true, OpenWorld: true},
		"artifact.read":    {ReadOnly: true, Idempotent: true},
	}
	for id, w := range want {
		a, ok := e.Registry.Get(id)
		if !ok || a.Annotations != w {
			t.Errorf("%s: got %+v want %+v", id, a.Annotations, w)
		}
	}
	if a, _ := e.Registry.Get("ssh.exec"); !strings.Contains(a.Description, "env=prod are production: confirm with the user") ||
		!strings.Contains(a.Description, "Respect the host note.") {
		t.Errorf("ssh.exec description: %s", a.Description)
	}
}

func TestExecReturnsHostNoteAndTags(t *testing.T) {
	e, _ := setup(t)
	e.Invoke(context.Background(), human, "ssh.hosts.tag", json.RawMessage(`{"host":"prod","note":"read only"}`))
	out := e.Invoke(context.Background(), agent, "ssh.exec", execArgs("prod", "true"))
	res := out.Result.(ExecOutput)
	if res.HostNote != "read only" || res.HostTags["env"] != "prod" {
		t.Fatalf("%+v", res)
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), `"host_note":"read only"`) || !strings.Contains(string(b), `"host_tags":{"env":"prod"}`) {
		t.Fatalf("json: %s", b)
	}
}

func TestExecUsesPool(t *testing.T) {
	e, srv := setupWith(t, true)
	for range 2 {
		if out := e.Invoke(context.Background(), agent, "ssh.exec", execArgs("dev", "true")); out.Status != engine.StatusOK {
			t.Fatalf("%+v", out)
		}
	}
	if n := srv.Accepts.Load(); n != 1 {
		t.Fatalf("%d connections, want 1", n)
	}
}
