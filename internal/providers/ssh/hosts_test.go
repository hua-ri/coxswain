package ssh

import (
	"context"
	"encoding/json"
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
	"golang.org/x/crypto/ssh"
)

type hostsFixture struct {
	e        *engine.Engine
	hosts    *inventory.Store
	srv      *sshtest.Server
	trust    string // Coxswain's known_hosts
	userKH   string // stand-in for ~/.ssh/known_hosts
	userOrig []byte
	userStat os.FileInfo
	events   *eventlog.Log
}

// setupHosts has one saved host, "bastion" (not on any server), and a
// server whose key nobody trusts yet.
func setupHosts(t *testing.T) *hostsFixture {
	t.Helper()
	dir := t.TempDir()
	f := &hostsFixture{srv: sshtest.Start(t), trust: filepath.Join(dir, "cox", "known_hosts")}
	f.hosts = inventory.NewStore(filepath.Join(dir, "hosts.json"))
	f.hosts.Merge([]inventory.Host{{Name: "bastion", HostName: "10.0.0.1", Port: 22}})
	os.MkdirAll(filepath.Join(dir, ".ssh"), 0o700)
	f.userKH = filepath.Join(dir, ".ssh", "known_hosts")
	f.userOrig = []byte("# the user's own\n")
	os.WriteFile(f.userKH, f.userOrig, 0o600)
	f.userStat, _ = os.Stat(f.userKH)
	reg := action.NewRegistry()
	p := &Provider{
		Hosts:     f.hosts,
		Dialer:    &sshx.Dialer{Hosts: f.hosts, KnownHosts: []string{f.userKH, f.trust}, TrustFile: f.trust},
		Artifacts: shaper.NewArtifacts(filepath.Join(dir, "artifacts")),
		UserHome:  dir,
	}
	p.Register(reg)
	f.events = eventlog.Open(filepath.Join(dir, "events"))
	f.e = &engine.Engine{Registry: reg, Events: f.events}
	t.Cleanup(func() {
		b, _ := os.ReadFile(f.userKH)
		st, _ := os.Stat(f.userKH)
		if string(b) != string(f.userOrig) || !st.ModTime().Equal(f.userStat.ModTime()) {
			t.Errorf("~/.ssh/known_hosts was written: %q", b)
		}
	})
	return f
}

func args(v map[string]any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// requested is the last action.requested event for id.
func (f *hostsFixture) last(t *testing.T, typ, id string) eventlog.Event {
	t.Helper()
	evs, err := f.events.Read(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == typ && evs[i].Action == id {
			return evs[i]
		}
	}
	t.Fatalf("no %s event for %s in %+v", typ, id, evs)
	return eventlog.Event{}
}

func TestHostsAdd(t *testing.T) {
	f := setupHosts(t)
	out := f.e.Invoke(context.Background(), agent, "ssh.hosts.add", args(map[string]any{
		"name": "web-1", "hostname": "10.0.1.1", "port": 2222, "user": "deploy", "proxy_jump": "bastion",
		"identity_files": []string{"~/.ssh/id_web"}, "tags": []string{"role=web"},
	}))
	if out.Status != engine.StatusOK {
		t.Fatalf("%+v", out)
	}
	h, err := f.hosts.Get("web-1")
	if err != nil || h.Port != 2222 || h.User != "deploy" || h.ProxyJump != "bastion" || h.Tags["role"] != "web" ||
		h.Source != "manual" || h.IdentityFiles[0] != filepath.Join(filepath.Dir(filepath.Dir(f.trust)), ".ssh", "id_web") {
		t.Fatalf("%+v %v", h, err)
	}
	if ev := f.last(t, eventlog.ActionRequested, "ssh.hosts.add"); ev.Risk != "write" || ev.Actor.Name != "codex" {
		t.Fatalf("label: %+v", ev)
	}
	// Production and notes instruct agents: adding them is destructive.
	out = f.e.Invoke(context.Background(), agent, "ssh.hosts.add", args(map[string]any{
		"name": "db", "hostname": "10.0.2.1", "tags": []string{"env=prod"}}))
	if out.Status != engine.StatusOK || f.last(t, eventlog.ActionRequested, "ssh.hosts.add").Risk != "destructive" {
		t.Fatalf("%+v", out)
	}
	for _, bad := range []map[string]any{
		{"name": "web-1", "hostname": "10.0.1.2"},                      // exists
		{"name": "a b", "hostname": "x"},                               // name
		{"name": "a", "hostname": "x", "port": 0x10000},                // port
		{"name": "a", "hostname": "x", "proxy_jump": "nosuchhost"},     // jump
		{"name": "a", "hostname": "x", "tags": []string{"novalue"}},    // tag
		{"name": "a", "hostname": "x", "tags": []string{"=v"}},         // tag
		{"name": "a", "hostname": "x", "identity_files": []string{""}}, // file
	} {
		if out := f.e.Invoke(context.Background(), human, "ssh.hosts.add", args(bad)); out.Status == engine.StatusOK {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestHostsUpdate(t *testing.T) {
	f := setupHosts(t)
	f.hosts.Merge([]inventory.Host{{Name: "web", HostName: "10.0.1.1", ProxyJump: "bastion", Note: "be nice"}})
	f.hosts.Merge([]inventory.Host{{Name: "inner", HostName: "10.0.1.9", ProxyJump: "web,bastion"}})

	out := f.e.Invoke(context.Background(), agent, "ssh.hosts.update", args(map[string]any{"name": "web", "port": 2200}))
	if out.Status != engine.StatusOK || f.last(t, eventlog.ActionRequested, "ssh.hosts.update").Risk != "write" {
		t.Fatalf("%+v", out)
	}
	if h, _ := f.hosts.Get("web"); h.Port != 2200 || h.HostName != "10.0.1.1" || h.Note != "be nice" {
		t.Fatalf("partial update: %+v", h)
	}
	// Changing the note is destructive, like ssh.hosts.tag.
	f.e.Invoke(context.Background(), agent, "ssh.hosts.update", args(map[string]any{"name": "web", "note": "careful"}))
	if r := f.last(t, eventlog.ActionRequested, "ssh.hosts.update").Risk; r != "destructive" {
		t.Fatalf("note change risk %s", r)
	}
	f.e.Invoke(context.Background(), agent, "ssh.hosts.update", args(map[string]any{"name": "web", "tags": []string{"env=prod"}}))
	if r := f.last(t, eventlog.ActionRequested, "ssh.hosts.update").Risk; r != "destructive" {
		t.Fatalf("env change risk %s", r)
	}
	// The same tags again are a write.
	f.e.Invoke(context.Background(), agent, "ssh.hosts.update", args(map[string]any{"name": "web", "tags": []string{"env=prod"}, "note": "careful"}))
	if r := f.last(t, eventlog.ActionRequested, "ssh.hosts.update").Risk; r != "write" {
		t.Fatalf("unchanged tags risk %s", r)
	}
	// Renaming keeps tags and note, and follows jump references.
	out = f.e.Invoke(context.Background(), human, "ssh.hosts.update", args(map[string]any{"name": "web", "new_name": "web-1"}))
	if out.Status != engine.StatusOK {
		t.Fatalf("%+v", out)
	}
	if _, err := f.hosts.Get("web"); err == nil {
		t.Fatal("old name still there")
	}
	if h, _ := f.hosts.Get("web-1"); h.Tags["env"] != "prod" || h.Note != "careful" {
		t.Fatalf("renamed: %+v", h)
	}
	if h, _ := f.hosts.Get("inner"); h.ProxyJump != "web-1,bastion" {
		t.Fatalf("jump not renamed: %q", h.ProxyJump)
	}
	for _, bad := range []map[string]any{
		{"name": "nope", "port": 1},
		{"name": "web-1", "new_name": "bastion"},
		{"name": "web-1", "new_name": "a/b"},
		{"name": "web-1", "proxy_jump": "web-1"},
		{"name": "web-1", "hostname": ""},
	} {
		if out := f.e.Invoke(context.Background(), human, "ssh.hosts.update", args(bad)); out.Status == engine.StatusOK {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestHostsRemove(t *testing.T) {
	f := setupHosts(t)
	f.hosts.Merge([]inventory.Host{{Name: "web", HostName: "10.0.1.1", ProxyJump: "bastion"}})
	out := f.e.Invoke(context.Background(), agent, "ssh.hosts.remove", args(map[string]any{"name": "bastion"}))
	if out.Status == engine.StatusOK || !strings.Contains(out.Error, "web") {
		t.Fatalf("removed a jump host in use: %+v", out)
	}
	out = f.e.Invoke(context.Background(), agent, "ssh.hosts.remove", args(map[string]any{"name": "web"}))
	if out.Status != engine.StatusOK || f.last(t, eventlog.ActionRequested, "ssh.hosts.remove").Risk != "destructive" {
		t.Fatalf("%+v", out)
	}
	if _, err := f.hosts.Get("web"); err == nil {
		t.Fatal("still there")
	}
}

func TestHostKeyActions(t *testing.T) {
	f := setupHosts(t)
	box := map[string]any{"name": "box", "hostname": f.srv.Host, "port": f.srv.Port, "identity_files": []string{f.srv.KeyFile}}

	// An unsaved host can be scanned (by anyone: it only reads).
	out := f.e.Invoke(context.Background(), agent, "ssh.hosts.scan_key", args(box))
	if out.Status != engine.StatusOK {
		t.Fatalf("%+v", out)
	}
	k := out.Result.(sshx.HostKey)
	if k.Status != sshx.KeyUnknown || k.Fingerprint != serverFingerprint(t, f.srv) {
		t.Fatalf("%+v", k)
	}
	if ev := f.last(t, eventlog.ActionRequested, "ssh.hosts.scan_key"); ev.Risk != "read" {
		t.Fatalf("%+v", ev)
	}

	// Agents may not trust keys; the refusal is audited and says why.
	trust := map[string]any{}
	for k, v := range box {
		trust[k] = v
	}
	trust["fingerprint"] = k.Fingerprint
	out = f.e.Invoke(context.Background(), agent, "ssh.hosts.trust_key", args(trust))
	if out.Status == engine.StatusOK || !strings.Contains(out.Error, "ask the user") {
		t.Fatalf("agent trusted a key: %+v", out)
	}
	if ev := f.last(t, eventlog.ActionRefused, "ssh.hosts.trust_key"); ev.Actor.Name != "codex" {
		t.Fatalf("%+v", ev)
	}
	if _, err := os.Stat(f.trust); err == nil {
		t.Fatal("trust file written for an agent")
	}

	// The user can; then the host connects.
	out = f.e.Invoke(context.Background(), human, "ssh.hosts.trust_key", args(trust))
	if out.Status != engine.StatusOK || out.Result.(sshx.HostKey).Status != sshx.KeyTrusted {
		t.Fatalf("%+v", out)
	}
	if st, err := os.Stat(f.trust); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
	out = f.e.Invoke(context.Background(), agent, "ssh.hosts.test", args(box))
	if out.Status != engine.StatusOK || out.Result.(TestOutput).OS == "" {
		t.Fatalf("test: %+v", out)
	}

	// Saved hosts are addressed by name.
	add := map[string]any{}
	for k, v := range box {
		add[k] = v
	}
	if out := f.e.Invoke(context.Background(), human, "ssh.hosts.add", args(add)); out.Status != engine.StatusOK {
		t.Fatal(out)
	}
	if out := f.e.Invoke(context.Background(), agent, "ssh.hosts.scan_key", args(map[string]any{"name": "box"})); out.Status != engine.StatusOK ||
		out.Result.(sshx.HostKey).Status != sshx.KeyTrusted {
		t.Fatalf("%+v", out)
	}

	// Forgetting is for the user too.
	if out := f.e.Invoke(context.Background(), agent, "ssh.hosts.forget_key", args(map[string]any{"name": "box"})); out.Status == engine.StatusOK {
		t.Fatal("agent forgot a key")
	}
	out = f.e.Invoke(context.Background(), human, "ssh.hosts.forget_key", args(map[string]any{"name": "box"}))
	if out.Status != engine.StatusOK || f.last(t, eventlog.ActionRequested, "ssh.hosts.forget_key").Risk != "destructive" {
		t.Fatalf("%+v", out)
	}
	if out := f.e.Invoke(context.Background(), agent, "ssh.hosts.scan_key", args(map[string]any{"name": "box"})); out.Result.(sshx.HostKey).Status != sshx.KeyUnknown {
		t.Fatalf("%+v", out)
	}
}

func serverFingerprint(t *testing.T, s *sshtest.Server) string {
	b, _ := os.ReadFile(s.KnownHosts)
	_, _, key, _, _, err := ssh.ParseKnownHosts(b)
	if err != nil {
		t.Fatal(err)
	}
	return ssh.FingerprintSHA256(key)
}
