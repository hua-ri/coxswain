package sshx

import (
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/hua-ri/coxswain/internal/inventory"
	"golang.org/x/crypto/ssh"
)

func TestChainResolvesAliasesAndSpecs(t *testing.T) {
	store := inventory.NewStore(filepath.Join(t.TempDir(), "hosts.json"))
	store.Merge([]inventory.Host{
		{Name: "outer", HostName: "1.1.1.1", Port: 22},
		{Name: "bastion", HostName: "10.0.0.1", Port: 22, ProxyJump: "outer"},
	})
	d := &Dialer{Hosts: store}
	hops, err := d.chain(inventory.Host{Name: "web", HostName: "10.0.1.1", ProxyJump: "bastion"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range hops {
		got = append(got, h.Name)
	}
	if len(got) != 3 || got[0] != "outer" || got[1] != "bastion" || got[2] != "web" {
		t.Fatalf("chain = %v", got)
	}

	hop, err := d.resolveJump("ops@jump.example.com:2200")
	if err != nil || hop.User != "ops" || hop.HostName != "jump.example.com" || hop.Port != 2200 {
		t.Fatalf("spec = %+v %v", hop, err)
	}
}

func TestCapped(t *testing.T) {
	c := &capped{max: 5}
	c.Write([]byte("abc"))
	c.Write([]byte("defgh"))
	if c.String() != "abcde" || !c.over {
		t.Fatalf("got %q over=%v", c.String(), c.over)
	}
}

// TestNoKnownHosts: without any known_hosts file every host is unknown
// (and can be trusted from Coxswain), rather than a dead end.
func TestNoKnownHosts(t *testing.T) {
	d := &Dialer{KnownHosts: []string{filepath.Join(t.TempDir(), "missing")}}
	if _, err := d.clientConfig(inventory.Host{Name: "x", HostName: "x"}, "x:22", nil); err != nil {
		t.Fatal(err)
	}
}

func TestChainLoops(t *testing.T) {
	store := inventory.NewStore(filepath.Join(t.TempDir(), "hosts.json"))
	store.Merge([]inventory.Host{
		{Name: "self", HostName: "10.0.0.9", ProxyJump: "self"},
		{Name: "a", HostName: "10.0.0.1", ProxyJump: "b"},
		{Name: "b", HostName: "10.0.0.2", ProxyJump: "a"},
	})
	d := &Dialer{Hosts: store}
	for name, want := range map[string]string{"self": "ProxyJump loop: self -> self", "a": "ProxyJump loop: a -> b -> a"} {
		h, _ := store.Get(name)
		if _, err := d.chain(h); err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", name, err, want)
		}
	}
}

func TestChainDepth(t *testing.T) {
	store := inventory.NewStore(filepath.Join(t.TempDir(), "hosts.json"))
	var hosts []inventory.Host
	for i := 0; i < 12; i++ {
		h := inventory.Host{Name: fmt.Sprintf("h%d", i), HostName: "10.0.0.1"}
		if i > 0 {
			h.ProxyJump = fmt.Sprintf("h%d", i-1)
		}
		hosts = append(hosts, h)
	}
	store.Merge(hosts)
	d := &Dialer{Hosts: store}
	if _, err := d.chain(hosts[11]); err == nil {
		t.Fatal("12-hop chain accepted")
	}
	if hops, err := d.chain(hosts[5]); err != nil || len(hops) != 6 {
		t.Fatalf("hops %d %v", len(hops), err)
	}
}

func TestExitCode(t *testing.T) {
	if code, err := exitCode(&ssh.ExitMissingError{}); err != nil || code != -1 {
		t.Fatalf("exit missing: %d %v", code, err)
	}
	if code, err := exitCode(fmt.Errorf("wrapped: %w", &ssh.ExitMissingError{})); err != nil || code != -1 {
		t.Fatalf("wrapped exit missing: %d %v", code, err)
	}
	if _, err := exitCode(io.EOF); err != io.EOF {
		t.Fatalf("other errors pass through: %v", err)
	}
}
