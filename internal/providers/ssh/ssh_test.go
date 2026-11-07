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
	"github.com/hua-ri/coxswain/internal/approval"
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
	return &engine.Engine{
		Registry:  reg,
		Approvals: approval.NewStore(filepath.Join(dir, "approvals"), time.Minute),
		Events:    eventlog.Open(filepath.Join(dir, "events")),
	}, srv
}

func execArgs(host, cmd string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"host": host, "command": cmd})
	return b
}

func TestAgentReadsProd(t *testing.T) {
	e, _ := setup(t)
	out := e.Invoke(context.Background(), agent, "ssh.exec", execArgs("prod", "echo hello"), nil)
	if out.Status != engine.StatusOK {
		t.Fatalf("%+v", out)
	}
	res := out.Result.(ExecOutput)
	if res.Stdout.Text != "hello\n" || res.ExitCode != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestAgentWriteOnProdNeedsApproval(t *testing.T) {
	e, srv := setup(t)
	out := e.Invoke(context.Background(), agent, "ssh.exec", execArgs("prod", "touch /tmp/cox-should-not-exist"), nil)
	if out.Status != engine.StatusApprovalRequired || !strings.Contains(out.Preview, "prod") {
		t.Fatalf("%+v", out)
	}
	select {
	case c := <-srv.Commands:
		t.Fatalf("command ran before approval: %q", c)
	default:
	}
}

func TestAgentDestructiveOnDevNeedsApproval(t *testing.T) {
	e, _ := setup(t)
	if out := e.Invoke(context.Background(), agent, "ssh.exec", execArgs("dev", "rm -rf /tmp/x"), nil); out.Status != engine.StatusApprovalRequired {
		t.Fatalf("%+v", out)
	}
	if out := e.Invoke(context.Background(), agent, "ssh.exec", execArgs("dev", "touch /tmp/cox-dev-write"), nil); out.Status != engine.StatusOK {
		t.Fatalf("balanced mode should allow a plain write on dev: %+v", out)
	}
	os.Remove("/tmp/cox-dev-write")
}

func TestAgentOutputIsShaped(t *testing.T) {
	e, _ := setup(t)
	out := e.Invoke(context.Background(), agent, "ssh.exec", execArgs("dev", "seq 1 5000"), nil)
	res := out.Result.(ExecOutput)
	if !res.Stdout.Truncated || res.Stdout.ArtifactID == "" || res.Stdout.TotalLines != 5000 {
		t.Fatalf("agent output not shaped: %+v", res.Stdout)
	}
	hout := e.Invoke(context.Background(), human, "ssh.exec", execArgs("dev", "seq 1 5000"), nil)
	if hres := hout.Result.(ExecOutput); hres.Stdout.Truncated {
		t.Fatal("human output must not be shaped")
	}
}

func TestTagPolicyKeysAreDestructive(t *testing.T) {
	e, _ := setup(t)
	out := e.Invoke(context.Background(), agent, "ssh.hosts.tag", json.RawMessage(`{"host":"prod","unset":["env"]}`), nil)
	if out.Status != engine.StatusApprovalRequired {
		t.Fatalf("agent removing env=prod must need approval: %+v", out)
	}
	out = e.Invoke(context.Background(), human, "ssh.hosts.tag", json.RawMessage(`{"host":"dev","set":["team=pay"],"note":"canary"}`), nil)
	if out.Status != engine.StatusOK {
		t.Fatalf("%+v", out)
	}
	if h := out.Result.(inventory.Host); h.Tags["team"] != "pay" || h.Note != "canary" {
		t.Fatalf("%+v", h)
	}
}

func TestImportAndList(t *testing.T) {
	e, _ := setup(t)
	cfg := filepath.Join(t.TempDir(), "config")
	os.WriteFile(cfg, []byte("Host bastion\n  HostName 10.0.0.1\n"), 0o600)
	out := e.Invoke(context.Background(), human, "ssh.hosts.import", json.RawMessage(fmt.Sprintf(`{"path":%q}`, cfg)), nil)
	if out.Status != engine.StatusOK {
		t.Fatalf("%+v", out)
	}
	list := e.Invoke(context.Background(), agent, "ssh.hosts.list", json.RawMessage(`{"tag":"env=prod"}`), nil)
	if hs := list.Result.([]inventory.Host); len(hs) != 1 || hs[0].Name != "prod" {
		t.Fatalf("%+v", list.Result)
	}
}
