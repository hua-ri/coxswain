package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/clients"
	"github.com/hua-ri/coxswain/internal/instance"
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
	if out, err := exec.Command("go", "build", "-ldflags", "-X main.agentAncestors=off", "-o", coxBin, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// sandbox is an isolated HOME with ~/.ssh/config and known_hosts for an
// sshtest server, with prod and dev aliases pointing at it.
type sandbox struct {
	env     []string
	coxHome string
}

func newSandbox(t *testing.T, srv *sshtest.Server) sandbox {
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
	coxHome := filepath.Join(home, ".coxswain")
	env := append(os.Environ(), "HOME="+home, "COX_HOME="+coxHome, "SSH_AUTH_SOCK=", "COX_NO_DIALOG=1")
	return sandbox{env: env, coxHome: coxHome}
}

// trust allows client names, as if the user had clicked Allow.
func (s sandbox) trust(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := clients.NewStore(filepath.Join(s.coxHome, "clients.json")).Trust(n); err != nil {
			t.Fatal(err)
		}
	}
}

func (s sandbox) cox(t *testing.T, args ...string) (string, int) {
	t.Helper()
	return s.coxEnv(t, nil, args...)
}

// coxEnv runs cox with extra environment variables.
func (s sandbox) coxEnv(t *testing.T, extra []string, args ...string) (string, int) {
	t.Helper()
	c := exec.Command(coxBin, args...)
	c.Env = append(append([]string{}, s.env...), extra...)
	out, err := c.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// syncBuffer is a bytes.Buffer safe for a writer process and a reader test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// server runs `cox server --port 0` until stop is called (SIGINT).
type coxServer struct {
	cmd    *exec.Cmd
	stderr *syncBuffer
	done   chan error
	info   instance.Info
	once   sync.Once
}

func (s sandbox) startServer(t *testing.T) *coxServer {
	t.Helper()
	srv := &coxServer{cmd: exec.Command(coxBin, "server", "--port", "0"), stderr: &syncBuffer{}, done: make(chan error, 1)}
	srv.cmd.Env = s.env
	srv.cmd.Stderr = srv.stderr
	if err := srv.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { srv.done <- srv.cmd.Wait() }()
	t.Cleanup(func() { srv.stop(t) })
	deadline := time.Now().Add(15 * time.Second)
	for {
		info, err := instance.Discover(context.Background(), s.coxHome)
		if err == nil {
			srv.info = info
			return srv
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start: %v\n%s", err, srv.stderr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (srv *coxServer) stop(t *testing.T) {
	srv.once.Do(func() { srv.interrupt(t) })
}

func (srv *coxServer) interrupt(t *testing.T) {
	srv.cmd.Process.Signal(syscall.SIGINT)
	select {
	case err := <-srv.done:
		if err != nil {
			t.Errorf("server exit: %v\n%s", err, srv.stderr)
		}
	case <-time.After(10 * time.Second):
		srv.cmd.Process.Kill()
		t.Errorf("server ignored SIGINT\n%s", srv.stderr)
	}
}

func (s sandbox) mcp(t *testing.T, client string) *sdk.ClientSession {
	t.Helper()
	c := exec.Command(coxBin, "mcp", "--client", client)
	c.Env = s.env
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "e2e", Version: "0"}, nil).
		Connect(context.Background(), &sdk.CommandTransport{Command: c}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

type outcome struct {
	Text   string // the raw tool result
	Status string
	Error  string
	Result struct {
		Stdout struct{ Text string }
	}
}

func callExec(t *testing.T, cs *sdk.ClientSession, host, cmd string) (outcome, error) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "ssh_exec", Arguments: map[string]any{"host": host, "command": cmd}})
	if err != nil {
		return outcome{}, err
	}
	out := outcome{Text: res.Content[0].(*sdk.TextContent).Text}
	json.Unmarshal([]byte(out.Text), &out)
	return out, nil
}

const notRunning = "Coxswain server is not running. Start it with: cox server\n"

func TestEndToEnd(t *testing.T) {
	sshSrv := sshtest.Start(t)
	s := newSandbox(t, sshSrv)

	// No server: action commands fail with the spec's message and exit 2;
	// init still works.
	for _, args := range [][]string{{"ssh", "hosts", "list"}, {"clients", "list"}} {
		if out, code := s.cox(t, args...); code != 2 || out != notRunning {
			t.Fatalf("%v without server: %d %q", args, code, out)
		}
	}
	if out, code := s.cox(t, "init", "--skip-agents"); code != 0 || !strings.Contains(out, "added 2") {
		t.Fatalf("init without server: %d %s", code, out)
	}
	for _, name := range []string{"cli", "desktop"} {
		if out, code := s.cox(t, "mcp", "--client", name); code != 1 || !strings.Contains(out, "built in") {
			t.Fatalf("mcp --client %s: %d %s", name, code, out)
		}
	}
	if out, code := s.cox(t, "version"); code != 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("version: %d %q", code, out)
	}

	srv := s.startServer(t)
	// These cox commands run without a terminal, as the client "unknown".
	s.trust(t, "unknown")
	if st, err := os.Stat(instance.Path(s.coxHome)); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("server.json: %v %v", st, err)
	}
	if out, code := s.cox(t, "init", "--skip-agents"); code != 0 || !strings.Contains(out, "updated 2") {
		t.Fatalf("init via server: %d %s", code, out)
	}

	// CLI against the server.
	if out, code := s.cox(t, "ssh", "hosts", "tag", "prod", "--set", "env=prod"); code != 0 {
		t.Fatalf("tag: %d %s", code, out)
	}
	if out, code := s.cox(t, "ssh", "exec", "dev", "--", "echo", "hi"); code != 0 || out != "hi\n" {
		t.Fatalf("exec: %d %q", code, out)
	}
	if out, code := s.cox(t, "ssh", "exec", "dev", "--", "exit 7"); code != 7 {
		t.Fatalf("exit code: %d %q", code, out)
	}
	dir := t.TempDir()
	if out, code := s.cox(t, "ssh", "exec", "prod", "--", "rm", "-rf", dir); code != 0 {
		t.Fatalf("destructive: %d %s", code, out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("destructive command did not run: %v", err)
	}
	if out, code := s.cox(t, "ssh", "hosts", "list"); code != 0 || !strings.Contains(out, `"env": "prod"`) {
		t.Fatalf("list: %d %s", code, out)
	}

	// A pre-trusted agent over `cox mcp`.
	s.trust(t, "codex")
	codex := s.mcp(t, "codex")
	out, err := callExec(t, codex, "prod", "uname")
	if err != nil || out.Status != "ok" || out.Result.Stdout.Text != "Darwin\n" {
		t.Fatalf("codex exec: %+v %v", out, err)
	}

	// An untrusted agent is refused (COX_NO_DIALOG denies).
	kimi := s.mcp(t, "kimi")
	if out, err := callExec(t, kimi, "dev", "true"); err != nil || out.Error != "client_not_authorized" {
		t.Fatalf("kimi: %+v %v", out, err)
	}

	// Revoking cuts codex off.
	if out, code := s.cox(t, "clients", "list"); code != 0 || !strings.Contains(out, "codex") {
		t.Fatalf("clients list: %d %s", code, out)
	}
	if out, code := s.cox(t, "clients", "revoke", "codex"); code != 0 {
		t.Fatalf("revoke: %d %s", code, out)
	}
	if out, err := callExec(t, codex, "dev", "true"); err != nil || out.Error != "client_not_authorized" {
		t.Fatalf("revoked codex: %+v %v", out, err)
	}
	if out, code := s.cox(t, "clients", "list"); code != 0 || !strings.Contains(out, "revoked") {
		t.Fatalf("clients list after revoke: %d %s", code, out)
	}
	if out, code := s.cox(t, "clients", "allow", "codex"); code != 0 {
		t.Fatalf("allow: %d %s", code, out)
	}
	if out, err := callExec(t, codex, "dev", "true"); err != nil || out.Status != "ok" {
		t.Fatalf("codex after allow: %+v %v", out, err)
	}

	// Activity on the server's stderr and in the audit log.
	for _, want := range []string{"agent:codex", "ssh.exec", "human:cli", "agent:unknown", "listening on " + srv.info.Addr} {
		if !strings.Contains(srv.stderr.String(), want) {
			t.Fatalf("server output lacks %q:\n%s", want, srv.stderr)
		}
	}
	events, _ := filepath.Glob(filepath.Join(s.coxHome, "events", "*.jsonl"))
	b, _ := os.ReadFile(events[0])
	if !strings.Contains(string(b), `"name":"codex"`) || !strings.Contains(string(b), `"risk":"destructive"`) {
		t.Fatalf("events: %s", b)
	}

	// Ctrl-C stops the server and removes server.json.
	srv.stop(t)
	t.Logf("server output:\n%s", srv.stderr)
	if _, err := os.Stat(instance.Path(s.coxHome)); !os.IsNotExist(err) {
		t.Fatalf("server.json left behind: %v", err)
	}
	if out, code := s.cox(t, "ssh", "exec", "dev", "--", "true"); code != 2 || out != notRunning {
		t.Fatalf("after stop: %d %q", code, out)
	}
	if out, err := callExec(t, kimi, "dev", "true"); err != nil || out.Text != strings.TrimSpace(notRunning) {
		t.Fatalf("mcp after stop: %+v %v", out, err)
	}
}

func TestSecondServerRefused(t *testing.T) {
	s := newSandbox(t, sshtest.Start(t))
	s.startServer(t)
	if out, code := s.cox(t, "server", "--port", "0"); code == 0 || !strings.Contains(out, "already running") {
		t.Fatalf("%d %s", code, out)
	}
}

// TestCLIAsAgent: an agent running cox through its shell is that agent:
// it must be allowed, its output is shaped and it is audited by name.
func TestCLIAsAgent(t *testing.T) {
	s := newSandbox(t, sshtest.Start(t))
	if out, code := s.cox(t, "init", "--skip-agents"); code != 0 {
		t.Fatalf("init: %s", out)
	}
	srv := s.startServer(t)
	s.trust(t, "codex")
	codex := []string{"COX_ACTOR=agent:codex"}
	out, code := s.coxEnv(t, codex, "ssh", "exec", "dev", "--", "seq", "1", "5000")
	if code != 0 || strings.Count(out, "\n") > 1000 || !strings.Contains(out, "1\n") {
		t.Fatalf("agent output must be shaped: %d lines, exit %d", strings.Count(out, "\n"), code)
	}
	if !strings.Contains(srv.stderr.String(), "agent:codex ssh.exec requested") {
		t.Fatalf("not audited as codex:\n%s", srv.stderr)
	}
	out, code = s.coxEnv(t, []string{"COX_ACTOR=agent:kimi"}, "ssh", "hosts", "list")
	if code != 1 || !strings.Contains(out, "client_not_authorized") {
		t.Fatalf("untrusted agent: %d %s", code, out)
	}
	if out, code := s.coxEnv(t, codex, "clients", "list"); code == 0 {
		t.Fatalf("an agent managed clients: %s", out)
	}
}
