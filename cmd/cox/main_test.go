package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hua-ri/coxswain/internal/sshx/sshtest"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

var coxBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cox-bin")
	if err != nil {
		panic(err)
	}
	coxBin = filepath.Join(dir, "cox")
	if out, err := exec.Command("go", "build", "-o", coxBin, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// env builds an isolated HOME with ~/.ssh/config and known_hosts for srv,
// with prod and dev aliases pointing at it.
func env(t *testing.T, srv *sshtest.Server) []string {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	os.MkdirAll(sshDir, 0o700)
	kh, _ := os.ReadFile(srv.KnownHosts)
	os.WriteFile(filepath.Join(sshDir, "known_hosts"), kh, 0o600)
	cfg := ""
	for _, name := range []string{"prod", "dev"} {
		cfg += fmt.Sprintf("Host %s\n  HostName %s\n  Port %d\n  User %s\n  IdentityFile %s\n", name, srv.Host, srv.Port, srv.User, srv.KeyFile)
	}
	os.WriteFile(filepath.Join(sshDir, "config"), []byte(cfg), 0o600)
	return append(os.Environ(), "HOME="+home, "COX_HOME="+filepath.Join(home, ".coxswain"), "SSH_AUTH_SOCK=", "COX_NO_DIALOG=1")
}

func cox(t *testing.T, env []string, args ...string) (string, int) {
	t.Helper()
	c := exec.Command(coxBin, args...)
	c.Env = env
	out, err := c.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func TestCLIEndToEnd(t *testing.T) {
	srv := sshtest.Start(t)
	e := append(env(t, srv), "COX_ACTOR=agent:codex")

	if out, code := cox(t, e, "init", "--skip-agents"); code != 0 || !strings.Contains(out, "added:2") {
		t.Fatalf("init: %d %s", code, out)
	}
	if out, code := cox(t, e, "ssh", "hosts", "tag", "prod", "--set", "env=prod"); code != 3 {
		t.Fatalf("agent tagging env must need approval: %d %s", code, out)
	}
	if out, code := cox(t, e, "ssh", "exec", "dev", "--", "echo", "hi"); code != 0 || out != "hi\n" {
		t.Fatalf("exec: %d %q", code, out)
	}
	if out, code := cox(t, e, "ssh", "exec", "dev", "--", "exit 7"); code != 7 {
		t.Fatalf("exit code: %d %q", code, out)
	}
	out, code := cox(t, e, "ssh", "exec", "dev", "--", "rm", "-rf", "/tmp/cox-e2e")
	if code != 3 || !strings.Contains(out, "cox approve ap_") {
		t.Fatalf("destructive: %d %s", code, out)
	}
	if out, code := cox(t, e, "approve", strings.Fields(out[strings.Index(out, "ap_"):])[0]); code != 1 || !strings.Contains(out, "person at an interactive terminal") {
		t.Fatalf("agent approved its own action: %d %s", code, out)
	}
}

func TestMCPEndToEnd(t *testing.T) {
	srv := sshtest.Start(t)
	e := env(t, srv)
	if out, code := cox(t, e, "init", "--skip-agents"); code != 0 {
		t.Fatalf("init: %s", out)
	}
	c := exec.Command(coxBin, "mcp", "--actor", "claude-code")
	c.Env = e
	ctx := context.Background()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "e2e", Version: "0"}, nil).Connect(ctx, &sdk.CommandTransport{Command: c}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "ssh_exec", Arguments: map[string]any{"host": "prod", "command": "uname"}})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Status string
		Result struct {
			Stdout struct{ Text string }
		}
	}
	json.Unmarshal([]byte(res.Content[0].(*sdk.TextContent).Text), &out)
	if out.Status != "ok" || out.Result.Stdout.Text != "Darwin\n" {
		t.Fatalf("%s", res.Content[0].(*sdk.TextContent).Text)
	}

	events, _ := filepath.Glob(filepath.Join(strings.TrimPrefix(e[len(e)-3], "COX_HOME="), "events", "*.jsonl"))
	if len(events) != 1 {
		t.Fatalf("event files: %v", events)
	}
	b, _ := os.ReadFile(events[0])
	if !strings.Contains(string(b), `"name":"claude-code"`) || !strings.Contains(string(b), `"action.finished"`) {
		t.Fatalf("events: %s", b)
	}
}
