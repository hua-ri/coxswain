package sshx

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/sshx/sshtest"
	"golang.org/x/crypto/ssh"
)

// userKnownHosts is a stand-in for ~/.ssh/known_hosts: read-only for
// Coxswain. It holds content and returns a check that it was not touched.
func userKnownHosts(t *testing.T, content string) (string, func()) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".ssh")
	os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	return p, func() {
		t.Helper()
		b, err := os.ReadFile(p)
		st2, _ := os.Stat(p)
		if err != nil || string(b) != content || !st2.ModTime().Equal(st.ModTime()) {
			t.Fatalf("~/.ssh/known_hosts was modified: %q", b)
		}
	}
}

func trustFile(t *testing.T) string {
	return filepath.Join(t.TempDir(), "cox", "known_hosts")
}

func boxOf(srv *sshtest.Server) inventory.Host {
	return inventory.Host{Name: "box", HostName: srv.Host, Port: srv.Port, IdentityFiles: []string{srv.KeyFile}}
}

func TestScanUnknownThenTrust(t *testing.T) {
	srv := sshtest.Start(t)
	user, untouched := userKnownHosts(t, "# nothing here\n")
	trust := trustFile(t)
	d := &Dialer{KnownHosts: []string{user, trust}, TrustFile: trust}
	h := boxOf(srv)

	if _, err := d.Dial(context.Background(), h); err == nil || !strings.Contains(err.Error(), "not trusted yet") ||
		strings.Contains(err.Error(), "connect once") {
		t.Fatalf("dial before trust: %v", err)
	}
	k, err := d.ScanKey(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if k.Status != KeyUnknown || k.Type != "ssh-ed25519" || !strings.HasPrefix(k.Fingerprint, "SHA256:") || k.Host != "box" {
		t.Fatalf("scan = %+v", k)
	}
	if _, err := d.TrustKey(context.Background(), h, "SHA256:wrong"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("trust with wrong fingerprint: %v", err)
	}
	if _, err := os.Stat(trust); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a mismatched fingerprint was written")
	}
	got, err := d.TrustKey(context.Background(), h, k.Fingerprint)
	if err != nil || got.Status != KeyTrusted {
		t.Fatalf("trust: %+v %v", got, err)
	}
	st, err := os.Stat(trust)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("trust file mode: %v %v", st, err)
	}
	if dst, _ := os.Stat(filepath.Dir(trust)); dst.Mode().Perm() != 0o700 {
		t.Fatalf("trust dir mode %v", dst.Mode())
	}
	c, err := d.Dial(context.Background(), h)
	if err != nil {
		t.Fatalf("dial after trust: %v", err)
	}
	c.Close()
	// Trusting again is a no-op.
	if _, err := d.TrustKey(context.Background(), h, k.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(trust); strings.Count(string(b), "\n") != 1 {
		t.Fatalf("trust file: %q", b)
	}
	if k2, _ := d.ScanKey(context.Background(), h); k2.Status != KeyTrusted {
		t.Fatalf("scan after trust: %+v", k2)
	}
	untouched()
}

// TestScanDoesNotAuthenticate: scanning needs no key at all.
func TestScanDoesNotAuthenticate(t *testing.T) {
	srv := sshtest.Start(t)
	d := &Dialer{KnownHosts: []string{trustFile(t)}}
	k, err := d.ScanKey(context.Background(), inventory.Host{Name: "box", HostName: srv.Host, Port: srv.Port})
	if err != nil || k.Status != KeyUnknown {
		t.Fatalf("%+v %v", k, err)
	}
}

func TestChangedKeyIsNeverTrusted(t *testing.T) {
	srv, other := sshtest.Start(t), sshtest.Start(t)
	trust := trustFile(t)
	os.MkdirAll(filepath.Dir(trust), 0o700)
	// The trust file says srv's address has other's key.
	b, _ := os.ReadFile(other.KnownHosts)
	line := strings.Replace(string(b), knownAddr(other), knownAddr(srv), 1)
	os.WriteFile(trust, []byte(line), 0o600)
	user, untouched := userKnownHosts(t, "")
	d := &Dialer{KnownHosts: []string{user, trust}, TrustFile: trust}
	h := boxOf(srv)

	if _, err := d.Dial(context.Background(), h); err == nil || !strings.Contains(err.Error(), "HOST KEY CHANGED") {
		t.Fatalf("dial: %v", err)
	}
	k, err := d.ScanKey(context.Background(), h)
	if err != nil || k.Status != KeyChanged || len(k.Known) != 1 || !k.Known[0].Coxswain || k.Known[0].Line != 1 {
		t.Fatalf("scan: %+v %v", k, err)
	}
	if _, err := d.TrustKey(context.Background(), h, k.Fingerprint); !errors.Is(err, ErrKeyChanged) {
		t.Fatalf("trust of a changed key: %v", err)
	}
	if b2, _ := os.ReadFile(trust); string(b2) != line {
		t.Fatal("trust file changed")
	}
	n, err := d.ForgetKey(h, "")
	if err != nil || n != 1 {
		t.Fatalf("forget: %d %v", n, err)
	}
	if k, _ = d.ScanKey(context.Background(), h); k.Status != KeyUnknown {
		t.Fatalf("after forget: %+v", k)
	}
	if _, err := d.TrustKey(context.Background(), h, k.Fingerprint); err != nil {
		t.Fatal(err)
	}
	untouched()
}

// TestChangedKeyInUserFile: an old key in ~/.ssh/known_hosts cannot be
// forgotten by Coxswain; the scan says where it is.
func TestChangedKeyInUserFile(t *testing.T) {
	srv, other := sshtest.Start(t), sshtest.Start(t)
	b, _ := os.ReadFile(other.KnownHosts)
	content := "# first\n" + strings.Replace(string(b), knownAddr(other), knownAddr(srv), 1)
	user, untouched := userKnownHosts(t, content)
	trust := trustFile(t)
	d := &Dialer{KnownHosts: []string{user, trust}, TrustFile: trust}
	k, err := d.ScanKey(context.Background(), boxOf(srv))
	if err != nil || k.Status != KeyChanged || len(k.Known) != 1 || k.Known[0].Coxswain || k.Known[0].File != user || k.Known[0].Line != 2 {
		t.Fatalf("scan: %+v %v", k, err)
	}
	if n, err := d.ForgetKey(boxOf(srv), ""); err != nil || n != 0 {
		t.Fatalf("forget: %d %v", n, err)
	}
	untouched()
}

func TestScanThroughJump(t *testing.T) {
	jump, target := sshtest.Start(t), sshtest.Start(t)
	trust := trustFile(t)
	store := inventory.NewStore(filepath.Join(t.TempDir(), "hosts.json"))
	store.Merge([]inventory.Host{{Name: "jump", HostName: jump.Host, Port: jump.Port, IdentityFiles: []string{jump.KeyFile}}})
	d := &Dialer{Hosts: store, KnownHosts: []string{trust}, TrustFile: trust}
	h := inventory.Host{Name: "web", HostName: target.Host, Port: target.Port, IdentityFiles: []string{target.KeyFile}, ProxyJump: "jump"}

	// The jump host is unknown first: its key is what needs trusting.
	k, err := d.ScanKey(context.Background(), h)
	if err != nil || k.Host != "jump" || k.Status != KeyUnknown || len(k.Via) != 0 {
		t.Fatalf("first scan: %+v %v", k, err)
	}
	if _, err := d.TrustKey(context.Background(), h, k.Fingerprint); err != nil {
		t.Fatal(err)
	}
	k, err = d.ScanKey(context.Background(), h)
	if err != nil || k.Host != "web" || k.Status != KeyUnknown || strings.Join(k.Via, ",") != "jump" {
		t.Fatalf("second scan: %+v %v", k, err)
	}
	if want := fingerprintOf(t, target); k.Fingerprint != want {
		t.Fatalf("fingerprint %s, want the target's %s", k.Fingerprint, want)
	}
	if _, err := d.TrustKey(context.Background(), h, k.Fingerprint); err != nil {
		t.Fatal(err)
	}
	c, err := d.Dial(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	// Forgetting takes an address in the chain only.
	if _, err := d.ForgetKey(h, "10.9.9.9:22"); err == nil {
		t.Fatal("forgot an address outside the chain")
	}
	if n, err := d.ForgetKey(h, k.Address); err != nil || n != 1 {
		t.Fatalf("forget target: %d %v", n, err)
	}
}

func TestTrustNeedsTrustFile(t *testing.T) {
	srv := sshtest.Start(t)
	d := &Dialer{KnownHosts: []string{trustFile(t)}}
	k, _ := d.ScanKey(context.Background(), boxOf(srv))
	if _, err := d.TrustKey(context.Background(), boxOf(srv), k.Fingerprint); err == nil {
		t.Fatal("trusted without a trust file")
	}
	ssh := filepath.Join(t.TempDir(), ".ssh", "known_hosts")
	d = &Dialer{KnownHosts: []string{ssh}, TrustFile: ssh}
	if _, err := d.TrustKey(context.Background(), boxOf(srv), k.Fingerprint); err == nil || !strings.Contains(err.Error(), ".ssh") {
		t.Fatalf("wrote into ~/.ssh: %v", err)
	}
}

func knownAddr(s *sshtest.Server) string {
	return "[" + s.Host + "]:" + strings.TrimPrefix(s.Addr, s.Host+":")
}

func fingerprintOf(t *testing.T, s *sshtest.Server) string {
	b, _ := os.ReadFile(s.KnownHosts)
	_, _, key, _, _, err := ssh.ParseKnownHosts(bytes.TrimSpace(b))
	if err != nil {
		t.Fatal(err)
	}
	return ssh.FingerprintSHA256(key)
}
