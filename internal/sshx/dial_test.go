package sshx

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/sshx/sshtest"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func TestDialAndExec(t *testing.T) {
	srv := sshtest.Start(t)
	d := &Dialer{KnownHosts: []string{srv.KnownHosts}}
	h := inventory.Host{Name: "box", HostName: srv.Host, Port: srv.Port, User: srv.User, IdentityFiles: []string{srv.KeyFile}}
	c, err := d.Dial(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	res, err := Exec(context.Background(), c.Client, "echo out; echo err >&2; exit 3", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "out\n" || res.Stderr != "err\n" || res.ExitCode != 3 {
		t.Fatalf("res = %+v", res)
	}
}

func TestExecCancel(t *testing.T) {
	srv := sshtest.Start(t)
	d := &Dialer{KnownHosts: []string{srv.KnownHosts}}
	c, err := d.Dial(context.Background(), inventory.Host{Name: "box", HostName: srv.Host, Port: srv.Port, IdentityFiles: []string{srv.KeyFile}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	marker := filepath.Join(t.TempDir(), "ran")
	start := time.Now()
	if _, err := Exec(ctx, c.Client, "sleep 2; touch "+marker, 1024); err == nil || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("cancelled command kept running on the host")
	}
}

// TestAgentConnectionClosed: the ssh-agent connection opened for a Dial
// is closed with the Conn.
func TestAgentConnectionClosed(t *testing.T) {
	srv := sshtest.Start(t)
	b, _ := os.ReadFile(srv.KeyFile)
	key, err := ssh.ParseRawPrivateKey(b)
	if err != nil {
		t.Fatal(err)
	}
	keyring := agent.NewKeyring()
	keyring.Add(agent.AddedKey{PrivateKey: key})
	dir, _ := os.MkdirTemp("/tmp", "coxag") // unix socket paths must be short
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var open atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			open.Add(1)
			go func() { agent.ServeAgent(keyring, c); c.Close(); open.Add(-1) }()
		}
	}()

	d := &Dialer{KnownHosts: []string{srv.KnownHosts}, AgentSocket: sock}
	c, err := d.Dial(context.Background(), inventory.Host{Name: "box", HostName: srv.Host, Port: srv.Port})
	if err != nil {
		t.Fatal(err) // no IdentityFile: only the agent can authenticate
	}
	if _, err := Exec(context.Background(), c.Client, "true", 1024); err != nil {
		t.Fatal(err)
	}
	c.Close()
	deadline := time.Now().Add(2 * time.Second)
	for open.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := open.Load(); n != 0 {
		t.Fatalf("%d agent connections left open", n)
	}
}

func TestDialThroughJump(t *testing.T) {
	jump, target := sshtest.Start(t), sshtest.Start(t)
	store := inventory.NewStore(filepath.Join(t.TempDir(), "hosts.json"))
	store.Merge([]inventory.Host{{Name: "jump", HostName: jump.Host, Port: jump.Port, IdentityFiles: []string{jump.KeyFile}}})
	d := &Dialer{Hosts: store, KnownHosts: []string{jump.KnownHosts, target.KnownHosts}}
	c, err := d.Dial(context.Background(), inventory.Host{
		Name: "target", HostName: target.Host, Port: target.Port, IdentityFiles: []string{target.KeyFile}, ProxyJump: "jump",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, err := Exec(context.Background(), c.Client, "echo via-jump", 1024)
	if err != nil || res.Stdout != "via-jump\n" {
		t.Fatalf("res = %+v %v", res, err)
	}
	if len(c.jumps) != 1 {
		t.Fatalf("jumps = %d", len(c.jumps))
	}
}

func TestUnknownHostKeyIsExplained(t *testing.T) {
	srv := sshtest.Start(t)
	other := sshtest.Start(t) // its known_hosts does not contain srv's key
	d := &Dialer{KnownHosts: []string{other.KnownHosts}}
	_, err := d.Dial(context.Background(), inventory.Host{Name: "box", HostName: srv.Host, Port: srv.Port, IdentityFiles: []string{srv.KeyFile}})
	if err == nil || !strings.Contains(err.Error(), "not trusted yet") {
		t.Fatalf("err = %v", err)
	}
}
