package file

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/hua-ri/coxswain/internal/transfer"
)

var (
	codex   = action.Actor{Kind: action.Agent, Name: "codex"}
	desktop = action.Actor{Kind: action.Human, Name: "desktop"}
)

type fixture struct {
	e    *engine.Engine
	p    *Provider
	srv  *sshtest.Server
	home string // the cox home
}

func setup(t *testing.T) fixture {
	t.Helper()
	srv := sshtest.Start(t)
	dir := t.TempDir()
	hosts := inventory.NewStore(filepath.Join(dir, "hosts.json"))
	hosts.Merge([]inventory.Host{{Name: "prod", HostName: srv.Host, Port: srv.Port, IdentityFiles: []string{srv.KeyFile}}})
	hosts.Update(func(m map[string]inventory.Host) error {
		h := m["prod"]
		h.Tags, h.Note = map[string]string{"env": "prod"}, "no restarts"
		m["prod"] = h
		return nil
	})
	dialer := &sshx.Dialer{Hosts: hosts, KnownHosts: []string{srv.KnownHosts}}
	pool := &sshx.Pool{Dialer: dialer}
	t.Cleanup(func() { pool.Close() })
	events := eventlog.Open(filepath.Join(dir, "events"))
	transfers := &transfer.Manager{Events: events}
	t.Cleanup(transfers.CancelAll)
	p := &Provider{Hosts: hosts, Dialer: dialer, Pool: pool, Artifacts: shaper.NewArtifacts(filepath.Join(dir, "artifacts")),
		Transfers: transfers, DownloadDir: filepath.Join(dir, "downloads"), UploadDir: filepath.Join(dir, "uploads"),
		CoxHome: dir, UserHome: dir}
	reg := action.NewRegistry()
	p.Register(reg)
	return fixture{e: &engine.Engine{Registry: reg, Events: events}, p: p, srv: srv, home: dir}
}

func invoke(t *testing.T, e *engine.Engine, who action.Actor, id string, args any) engine.Outcome {
	t.Helper()
	b, _ := json.Marshal(args)
	return e.Invoke(context.Background(), who, id, b)
}

func mustOK[T any](t *testing.T, out engine.Outcome) T {
	t.Helper()
	if out.Status != engine.StatusOK {
		t.Fatalf("%+v", out)
	}
	return out.Result.(T)
}

func mustFail(t *testing.T, out engine.Outcome, want string) {
	t.Helper()
	if out.Status != engine.StatusError || !strings.Contains(out.Error, want) {
		t.Fatalf("want error containing %q, got %+v", want, out)
	}
}

func put(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// requested returns the action.requested event of the last call to id.
func requested(t *testing.T, e *engine.Engine, id string) eventlog.Event {
	t.Helper()
	evs, err := e.Events.Read(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == eventlog.ActionRequested && evs[i].Action == id {
			return evs[i]
		}
	}
	t.Fatalf("no %s request in %+v", id, evs)
	return eventlog.Event{}
}

func TestAnnotations(t *testing.T) {
	f := setup(t)
	want := map[string]action.Annotations{
		"file.list":     {ReadOnly: true, Idempotent: true, OpenWorld: true},
		"file.stat":     {ReadOnly: true, Idempotent: true, OpenWorld: true},
		"file.read":     {ReadOnly: true, Idempotent: true, OpenWorld: true},
		"file.write":    {Destructive: true, Idempotent: true, OpenWorld: true},
		"file.upload":   {Destructive: true, OpenWorld: true},
		"file.download": {OpenWorld: true},
		"file.mkdir":    {Destructive: true, Idempotent: true, OpenWorld: true},
		"file.remove":   {Destructive: true, Idempotent: true, OpenWorld: true},
	}
	for id, ann := range want {
		a, ok := f.e.Registry.Get(id)
		if !ok {
			t.Fatalf("%s not registered", id)
		}
		if a.Annotations != ann {
			t.Errorf("%s: annotations %+v, want %+v", id, a.Annotations, ann)
		}
	}
}

func TestList(t *testing.T) {
	f := setup(t)
	put(t, f.srv.Root, "logs/app.log", "hello\n")
	put(t, f.srv.Root, "logs/.hidden", "x")
	os.Mkdir(filepath.Join(f.srv.Root, "logs", "old"), 0o755)

	res := mustOK[ListOutput](t, invoke(t, f.e, codex, "file.list", map[string]any{"host": "prod", "path": "/logs"}))
	if res.Host != "prod" || res.HostNote != "no restarts" || res.HostTags["env"] != "prod" || res.Truncated {
		t.Fatalf("%+v", res)
	}
	if len(res.Entries) != 2 || res.Entries[0].Name != "app.log" || res.Entries[1].Name != "old" {
		t.Fatalf("entries: %+v", res.Entries)
	}
	if e := res.Entries[0]; e.Type != "file" || e.Size != 6 || e.Mode != "-rw-r--r--" || e.ModTime.IsZero() {
		t.Fatalf("file entry: %+v", e)
	}
	if res.Entries[1].Type != "dir" {
		t.Fatalf("dir entry: %+v", res.Entries[1])
	}
	all := mustOK[ListOutput](t, invoke(t, f.e, codex, "file.list", map[string]any{"host": "prod", "path": "logs", "all": true}))
	if len(all.Entries) != 3 || all.Entries[0].Name != ".hidden" {
		t.Fatalf("all: %+v", all.Entries)
	}
	if ev := requested(t, f.e, "file.list"); ev.Risk != "read" || ev.Targets[0].ID != "prod" || ev.Targets[0].Tags["env"] != "prod" {
		t.Fatalf("label: %+v", ev)
	}
	mustFail(t, invoke(t, f.e, codex, "file.list", map[string]any{"host": "prod", "path": "/nope"}), "nope")
	mustFail(t, invoke(t, f.e, codex, "file.list", map[string]any{"host": "nope", "path": "/"}), "nope")
}

func TestListTruncates(t *testing.T) {
	f := setup(t)
	dir := filepath.Join(f.srv.Root, "many")
	os.Mkdir(dir, 0o755)
	for i := range maxEntries + 5 {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d", i)), nil, 0o644)
	}
	res := mustOK[ListOutput](t, invoke(t, f.e, codex, "file.list", map[string]any{"host": "prod", "path": "/many"}))
	if len(res.Entries) != maxEntries || !res.Truncated || res.Total != maxEntries+5 || !strings.Contains(res.Hint, "1000") {
		t.Fatalf("entries %d truncated %v total %d hint %q", len(res.Entries), res.Truncated, res.Total, res.Hint)
	}
}

func TestStat(t *testing.T) {
	f := setup(t)
	put(t, f.srv.Root, "a.txt", "abc")
	res := mustOK[StatOutput](t, invoke(t, f.e, codex, "file.stat", map[string]any{"host": "prod", "path": "/a.txt"}))
	if res.Entry.Name != "a.txt" || res.Entry.Size != 3 || res.Entry.Type != "file" || res.Path != "/a.txt" {
		t.Fatalf("%+v", res)
	}
	mustFail(t, invoke(t, f.e, codex, "file.stat", map[string]any{"host": "prod", "path": "/missing"}), "missing")
}

func TestRead(t *testing.T) {
	f := setup(t)
	put(t, f.srv.Root, "conf/app.ini", "one\ntwo\nthree\nfour\n")
	res := mustOK[ReadOutput](t, invoke(t, f.e, codex, "file.read", map[string]any{"host": "prod", "path": "/conf/app.ini"}))
	if res.Content.Text != "one\ntwo\nthree\nfour\n" || res.Size != 19 || res.HostNote != "no restarts" {
		t.Fatalf("%+v", res)
	}
	part := mustOK[ReadOutput](t, invoke(t, f.e, codex, "file.read", map[string]any{"host": "prod", "path": "conf/app.ini", "offset": 2, "limit": 2}))
	if part.Content.Text != "two\nthree\n" || part.TotalLines != 4 {
		t.Fatalf("offset/limit: %+v", part)
	}
	// ~ is the SFTP start directory, the root here.
	home := mustOK[ReadOutput](t, invoke(t, f.e, codex, "file.read", map[string]any{"host": "prod", "path": "~/conf/app.ini", "limit": 1}))
	if home.Content.Text != "one\n" {
		t.Fatalf("~: %+v", home)
	}
}

func TestReadShapedForAgents(t *testing.T) {
	f := setup(t)
	var b strings.Builder
	for i := range 5000 {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	put(t, f.srv.Root, "big.log", b.String())
	res := mustOK[ReadOutput](t, invoke(t, f.e, codex, "file.read", map[string]any{"host": "prod", "path": "/big.log"}))
	if !res.Content.Truncated || res.Content.ArtifactID == "" {
		t.Fatalf("agent read not shaped: %+v", res.Content.TotalLines)
	}
	hres := mustOK[ReadOutput](t, invoke(t, f.e, desktop, "file.read", map[string]any{"host": "prod", "path": "/big.log"}))
	if hres.Content.Truncated || hres.Content.Text != b.String() {
		t.Fatal("human read must not be shaped")
	}
}

func TestReadRefusesBinaryAndLarge(t *testing.T) {
	f := setup(t)
	put(t, f.srv.Root, "bin", "abc\x00def")
	mustFail(t, invoke(t, f.e, codex, "file.read", map[string]any{"host": "prod", "path": "/bin"}), "file_download")
	put(t, f.srv.Root, "huge", strings.Repeat("x", maxRead+1))
	mustFail(t, invoke(t, f.e, codex, "file.read", map[string]any{"host": "prod", "path": "/huge"}), "file_download")
	os.Mkdir(filepath.Join(f.srv.Root, "d"), 0o755)
	mustFail(t, invoke(t, f.e, codex, "file.read", map[string]any{"host": "prod", "path": "/d"}), "directory")
}

func TestWriteIsAtomic(t *testing.T) {
	f := setup(t)
	put(t, f.srv.Root, "app/config.yml", "old\n")
	os.Chmod(filepath.Join(f.srv.Root, "app/config.yml"), 0o600)
	before, err := os.Open(filepath.Join(f.srv.Root, "app/config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	defer before.Close()

	res := mustOK[WriteOutput](t, invoke(t, f.e, codex, "file.write", map[string]any{"host": "prod", "path": "/app/config.yml", "content": "new\n"}))
	if res.Size != 4 || res.Path != "/app/config.yml" || res.Created {
		t.Fatalf("%+v", res)
	}
	got, _ := os.ReadFile(filepath.Join(f.srv.Root, "app/config.yml"))
	if string(got) != "new\n" {
		t.Fatalf("content %q", got)
	}
	// Written to a new file and renamed over the old one: an open handle
	// on the old file still sees the old content.
	old, _ := io.ReadAll(before)
	if string(old) != "old\n" {
		t.Fatalf("file was rewritten in place: old handle reads %q", old)
	}
	if fi, _ := os.Stat(filepath.Join(f.srv.Root, "app/config.yml")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode not kept: %v", fi.Mode())
	}
	entries, _ := os.ReadDir(filepath.Join(f.srv.Root, "app"))
	if len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
	if ev := requested(t, f.e, "file.write"); ev.Risk != "write" || ev.Targets[0].ID != "prod" {
		t.Fatalf("label: %+v", ev)
	}
}

func TestWriteModeAndCreateOnly(t *testing.T) {
	f := setup(t)
	res := mustOK[WriteOutput](t, invoke(t, f.e, codex, "file.write", map[string]any{"host": "prod", "path": "run.sh", "content": "#!/bin/sh\n", "mode": "0750", "create_only": true}))
	if !res.Created {
		t.Fatalf("%+v", res)
	}
	fi, err := os.Stat(filepath.Join(f.srv.Root, "run.sh"))
	if err != nil || fi.Mode().Perm() != 0o750 {
		t.Fatalf("%v %v", fi, err)
	}
	mustFail(t, invoke(t, f.e, codex, "file.write", map[string]any{"host": "prod", "path": "run.sh", "content": "x", "create_only": true}), "exists")
	if got, _ := os.ReadFile(filepath.Join(f.srv.Root, "run.sh")); !bytes.Equal(got, []byte("#!/bin/sh\n")) {
		t.Fatalf("create_only overwrote: %q", got)
	}
	entries, _ := os.ReadDir(f.srv.Root)
	if len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
	mustFail(t, invoke(t, f.e, codex, "file.write", map[string]any{"host": "prod", "path": "x", "content": "x", "mode": "rwx"}), "mode")
	mustFail(t, invoke(t, f.e, codex, "file.write", map[string]any{"host": "prod", "path": "/no/such/dir/x", "content": "x"}), "")
}

func TestWriteProtectedPathIsDestructive(t *testing.T) {
	f := setup(t)
	os.Mkdir(filepath.Join(f.srv.Root, "etc"), 0o755)
	for _, p := range []string{"/etc/cox.conf", "~/.ssh/authorized_keys", "../escape"} {
		invoke(t, f.e, codex, "file.write", map[string]any{"host": "prod", "path": p, "content": "x"})
		if ev := requested(t, f.e, "file.write"); ev.Risk != "destructive" {
			t.Errorf("%s: risk %s", p, ev.Risk)
		}
	}
	if _, err := os.Stat(filepath.Join(f.srv.Root, "etc", "cox.conf")); err != nil {
		t.Fatal("labels do not block:", err)
	}
}

func TestMkdirAndRemove(t *testing.T) {
	f := setup(t)
	res := mustOK[PathOutput](t, invoke(t, f.e, desktop, "file.mkdir", map[string]any{"host": "prod", "path": "/a/b/c", "parents": true}))
	if res.Path != "/a/b/c" {
		t.Fatalf("%+v", res)
	}
	if fi, err := os.Stat(filepath.Join(f.srv.Root, "a", "b", "c")); err != nil || !fi.IsDir() {
		t.Fatalf("%v %v", fi, err)
	}
	if ev := requested(t, f.e, "file.mkdir"); ev.Risk != "write" {
		t.Fatalf("%+v", ev)
	}
	mustFail(t, invoke(t, f.e, desktop, "file.mkdir", map[string]any{"host": "prod", "path": "/x/y"}), "")
	// parents=true on an existing directory is fine, like mkdir -p.
	mustOK[PathOutput](t, invoke(t, f.e, desktop, "file.mkdir", map[string]any{"host": "prod", "path": "/a/b", "parents": true}))

	put(t, f.srv.Root, "a/b/c/file.txt", "x")
	mustFail(t, invoke(t, f.e, desktop, "file.remove", map[string]any{"host": "prod", "path": "/a/b/c"}), "")
	mustOK[PathOutput](t, invoke(t, f.e, desktop, "file.remove", map[string]any{"host": "prod", "path": "/a/b/c/file.txt"}))
	if ev := requested(t, f.e, "file.remove"); ev.Risk != "destructive" {
		t.Fatalf("%+v", ev)
	}
	mustOK[PathOutput](t, invoke(t, f.e, desktop, "file.remove", map[string]any{"host": "prod", "path": "/a/b/c"}))
	if _, err := os.Stat(filepath.Join(f.srv.Root, "a", "b", "c")); !os.IsNotExist(err) {
		t.Fatalf("not removed: %v", err)
	}
	mustFail(t, invoke(t, f.e, desktop, "file.remove", map[string]any{"host": "prod", "path": "/a/b/c"}), "")
}

// TestWriteTempIsPrivate: the temporary file is 0600 before any content
// is written.
func TestWriteTempIsPrivate(t *testing.T) {
	f := setup(t)
	checked := make(chan os.FileMode, 1)
	f.p.pace = func(context.Context) {
		matches, _ := filepath.Glob(filepath.Join(f.srv.Root, ".secret.env.cox-*"))
		if len(matches) == 1 {
			fi, _ := os.Stat(matches[0])
			checked <- fi.Mode().Perm()
		}
	}
	mustOK[WriteOutput](t, invoke(t, f.e, codex, "file.write", map[string]any{"host": "prod", "path": "secret.env", "content": "TOKEN=x\n", "mode": "0640"}))
	select {
	case m := <-checked:
		if m != 0o600 {
			t.Fatalf("temporary file mode %v", m)
		}
	default:
		t.Fatal("no temporary file seen")
	}
	if fi, _ := os.Stat(filepath.Join(f.srv.Root, "secret.env")); fi.Mode().Perm() != 0o640 {
		t.Fatalf("final mode %v", fi.Mode())
	}
}

// TestWriteContentNotLogged: the audit log has file.write's size and
// hash, not its content.
func TestWriteContentNotLogged(t *testing.T) {
	f := setup(t)
	mustOK[WriteOutput](t, invoke(t, f.e, codex, "file.write", map[string]any{"host": "prod", "path": "app.env", "content": "PASSWORD=hunter2\n"}))
	ev := requested(t, f.e, "file.write")
	if strings.Contains(string(ev.Args), "hunter2") {
		t.Fatalf("content logged: %s", ev.Args)
	}
	var args struct {
		Host    string
		Path    string
		Content struct {
			Bytes  int
			SHA256 string
		}
	}
	if err := json.Unmarshal(ev.Args, &args); err != nil || args.Host != "prod" || args.Path != "app.env" ||
		args.Content.Bytes != 17 || len(args.Content.SHA256) != 64 {
		t.Fatalf("%s %v", ev.Args, err)
	}
}

// TestWriteKeysExact: "Content" would be written (encoding/json ignores
// case) but missed by the audit redaction; such arguments are refused.
func TestWriteKeysExact(t *testing.T) {
	f := setup(t)
	raw := json.RawMessage(`{"host":"prod","path":"x.txt","Content":"hunter2"}`)
	if out := f.e.Invoke(context.Background(), desktop, "file.write", raw); out.Status != "error" || !strings.Contains(out.Error, "Content") {
		t.Fatalf("%+v", out)
	}
	if got := string(redactContent(raw)); strings.Contains(got, "hunter2") {
		t.Fatalf("redacted: %s", got)
	}
	evs, _ := f.e.Events.Read(time.Now())
	for _, ev := range evs {
		if strings.Contains(string(ev.Args), "hunter2") {
			t.Fatalf("audited: %s", ev.Args)
		}
	}
}
