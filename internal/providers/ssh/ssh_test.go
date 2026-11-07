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
	"github.com/hua-ri/coxswain/internal/shaper"
	"github.com/hua-ri/coxswain/internal/sshx"
	"github.com/hua-ri/coxswain/internal/sshx/sshtest"
)

var (
	agent = action.Actor{Kind: action.Agent, Name: "codex"}
	human = action.Actor{Kind: action.Human, Name: "king"}
)

func setup(t *testing.T) (*engine.Engine, *sshtest.Server) {
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
	(&Provider{
		Hosts:     hosts,
		Dialer:    &sshx.Dialer{Hosts: hosts, KnownHosts: []string{srv.KnownHosts}},
		Artifacts: shaper.NewArtifacts(filepath.Join(dir, "artifacts")),
		UserHome:  dir,
	}).Register(reg)
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

func TestImportAssess(t *testing.T) {
	e, srv := setup(t)
	a, _ := e.Registry.Get("ssh.hosts.import")
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	os.WriteFile(cfg, []byte(fmt.Sprintf("Host dev\n  HostName %s\n  Port %d\nHost fresh\n  HostName 192.0.2.9\n  User ops\n", srv.Host, srv.Port)), 0o600)
	as, err := a.Assess(context.Background(), importArgs(t, cfg))
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
	as, _ = a.Assess(context.Background(), importArgs(t, cfg))
	if !strings.Contains(as.Preview, "add n19 -> 192.0.2.19:22") || strings.Contains(as.Preview, "n20") ||
		!strings.HasSuffix(as.Preview, "…") {
		t.Fatalf("capped preview: %s", as.Preview)
	}
}
