package file

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/transfer"
)

// data is a test payload of n bytes.
func data(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// local writes b to a file in the upload directory, the only place
// agents may upload from.
func local(t *testing.T, f fixture, b []byte) string {
	t.Helper()
	dir := filepath.Join(f.home, "uploads")
	os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, "payload.bin")
	if err := os.WriteFile(p, b, 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUpload(t *testing.T) {
	f := setup(t)
	payload := data(3<<20 + 17)
	src := local(t, f, payload)
	os.Mkdir(filepath.Join(f.srv.Root, "up"), 0o755)

	// A relative path: the test server's commands run in its root too, so
	// the remote checksum can be compared.
	res := mustOK[TransferOutput](t, invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": src, "remote_path": "up/payload.bin"}))
	got, _ := os.ReadFile(filepath.Join(f.srv.Root, "up", "payload.bin"))
	if string(got) != string(payload) {
		t.Fatalf("remote has %d bytes, want %d", len(got), len(payload))
	}
	if res.Status != transfer.Done || res.Direction != transfer.Upload || res.Bytes != int64(len(payload)) ||
		res.Total != int64(len(payload)) || res.SHA256 != sum(payload) || res.Verified == nil || !*res.Verified ||
		res.Owner != codex || res.HostNote != "no restarts" {
		t.Fatalf("%+v", res)
	}
	if list := f.p.Transfers.List(); len(list) != 1 || list[0].ID != res.ID {
		t.Fatalf("list: %+v", list)
	}
	if ev := requested(t, f.e, "file.upload"); ev.Risk != "write" || ev.Targets[0].Tags["env"] != "prod" {
		t.Fatalf("label: %+v", ev)
	}
	// An absolute path in the test server's SFTP root is not where its
	// commands look, so the checksum cannot be compared: not verified,
	// not an error.
	res = mustOK[TransferOutput](t, invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": src, "remote_path": "/up/abs.bin"}))
	if res.Verified != nil || res.SHA256 != sum(payload) {
		t.Fatalf("absolute: %+v", res)
	}
}

func TestUploadResume(t *testing.T) {
	f := setup(t)
	payload := data(1 << 20)
	src := local(t, f, payload)
	// A partial upload is the temporary file next to the target.
	os.WriteFile(filepath.Join(f.srv.Root, ".part.bin.cox-upload"), payload[:300_000], 0o600)
	res := mustOK[TransferOutput](t, invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": src, "remote_path": "part.bin", "resume": true}))
	got, _ := os.ReadFile(filepath.Join(f.srv.Root, "part.bin"))
	if string(got) != string(payload) || res.ResumedFrom != 300_000 || res.Verified == nil || !*res.Verified {
		t.Fatalf("resumed_from %d, remote %d bytes, verified %v", res.ResumedFrom, len(got), res.Verified)
	}
	if _, err := os.Stat(filepath.Join(f.srv.Root, ".part.bin.cox-upload")); !os.IsNotExist(err) {
		t.Fatalf("temporary file left: %v", err)
	}
	// A partial upload that differs from the local file: the checksum
	// catches it and the target is not touched.
	os.WriteFile(filepath.Join(f.srv.Root, "bad.bin"), []byte("old"), 0o644)
	os.WriteFile(filepath.Join(f.srv.Root, ".bad.bin.cox-upload"), make([]byte, 1000), 0o600)
	mustFail(t, invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": src, "remote_path": "bad.bin", "resume": true}), "without resume")
	if got, _ := os.ReadFile(filepath.Join(f.srv.Root, "bad.bin")); string(got) != "old" {
		t.Fatalf("target changed: %q", got)
	}
	// Without resume the upload starts over.
	mustOK[TransferOutput](t, invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": src, "remote_path": "bad.bin"}))
	if got, _ := os.ReadFile(filepath.Join(f.srv.Root, "bad.bin")); string(got) != string(payload) {
		t.Fatal("not replaced")
	}
	if fi, _ := os.Stat(filepath.Join(f.srv.Root, "bad.bin")); fi.Mode().Perm() != 0o644 {
		t.Fatalf("target mode not kept: %v", fi.Mode())
	}
}

// TestUploadIsAtomic: an upload goes to a private temporary file; a
// cancelled one leaves the target as it was, and resume completes it.
func TestUploadIsAtomic(t *testing.T) {
	f := setup(t)
	started := paused(f)
	payload := data(4 << 20)
	src := local(t, f, payload)
	target := filepath.Join(f.srv.Root, "app.bin")
	tmp := filepath.Join(f.srv.Root, ".app.bin.cox-upload")
	os.WriteFile(target, []byte("old"), 0o644)
	done := make(chan engine.Outcome, 1)
	go func() {
		done <- invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": src, "remote_path": "app.bin"})
	}()
	<-started
	if fi, err := os.Stat(tmp); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("temporary file while writing: %v %v", fi, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "old" {
		t.Fatalf("target changed during the upload: %d bytes", len(got))
	}
	f.p.Transfers.Cancel(f.p.Transfers.List()[0].ID, desktop)
	mustFail(t, <-done, ".app.bin.cox-upload") // where the partial upload is
	if got, _ := os.ReadFile(target); string(got) != "old" {
		t.Fatalf("cancelled upload changed the target: %d bytes", len(got))
	}
	part, err := os.Stat(tmp)
	if err != nil || part.Size() == 0 || part.Size() >= int64(len(payload)) {
		t.Fatalf("partial upload: %v %v", part, err)
	}
	f.p.pace = nil
	res := mustOK[TransferOutput](t, invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": src, "remote_path": "app.bin", "resume": true}))
	if got, _ := os.ReadFile(target); string(got) != string(payload) || res.ResumedFrom != part.Size() {
		t.Fatalf("resume: %d bytes, resumed_from %d", len(got), res.ResumedFrom)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

// TestDownloadResumeChecksPrefix: a partial local file that differs from
// the remote one is not continued.
func TestDownloadResumeChecksPrefix(t *testing.T) {
	f := setup(t)
	payload := data(1 << 20)
	os.WriteFile(filepath.Join(f.srv.Root, "big.bin"), payload, 0o644)
	dst := filepath.Join(f.home, "downloads", "prod", "big.bin")
	os.MkdirAll(filepath.Dir(dst), 0o700)
	os.WriteFile(dst, make([]byte, 5000), 0o600)
	mustFail(t, invoke(t, f.e, codex, "file.download", map[string]any{"host": "prod", "remote_path": "big.bin", "resume": true}), "without resume")
	// An absolute path in the test server's root cannot be checked by
	// its commands: resume is refused rather than trusted.
	os.WriteFile(dst, payload[:5000], 0o600)
	mustFail(t, invoke(t, f.e, codex, "file.download", map[string]any{"host": "prod", "remote_path": "/big.bin", "resume": true}), "without resume")
}

func TestUploadLabels(t *testing.T) {
	f := setup(t)
	src := local(t, f, []byte("x"))
	os.Mkdir(filepath.Join(f.srv.Root, "etc"), 0o755)
	invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": src, "remote_path": "/etc/x"})
	if ev := requested(t, f.e, "file.upload"); ev.Risk != "destructive" {
		t.Fatalf("protected remote path: %s", ev.Risk)
	}
	// The user's uploads of credential stores are labelled destructive,
	// also through a symlink.
	secrets := map[string]string{
		".ssh/id_ed25519": "KEY", ".netrc": "machine x", ".docker/config.json": "{}", ".config/gh/hosts.yml": "x",
		".git-credentials": "x", "Library/Keychains/login.keychain-db": "x", "server.json": "token",
	}
	for rel, content := range secrets {
		p := filepath.Join(f.home, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(content), 0o600)
		invoke(t, f.e, desktop, "file.upload", map[string]any{"host": "prod", "local_path": "~/" + rel, "remote_path": "k"})
		if ev := requested(t, f.e, "file.upload"); ev.Risk != "destructive" {
			t.Errorf("%s: risk %s", rel, ev.Risk)
		}
	}
	link := filepath.Join(t.TempDir(), "innocent.txt")
	os.Symlink(filepath.Join(f.home, ".netrc"), link)
	invoke(t, f.e, desktop, "file.upload", map[string]any{"host": "prod", "local_path": link, "remote_path": "k"})
	if ev := requested(t, f.e, "file.upload"); ev.Risk != "destructive" {
		t.Errorf("symlink to .netrc: risk %s", ev.Risk)
	}
	plain := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(plain, []byte("x"), 0o644)
	invoke(t, f.e, desktop, "file.upload", map[string]any{"host": "prod", "local_path": plain, "remote_path": "k"})
	if ev := requested(t, f.e, "file.upload"); ev.Risk != "write" {
		t.Errorf("plain file: risk %s", ev.Risk)
	}
	mustFail(t, invoke(t, f.e, desktop, "file.upload", map[string]any{"host": "prod", "local_path": "relative.txt", "remote_path": "k"}), "absolute")
	mustFail(t, invoke(t, f.e, desktop, "file.upload", map[string]any{"host": "prod", "local_path": filepath.Dir(plain), "remote_path": "k"}), "regular file")
}

// TestAgentUploadConfined: agents upload only from the upload
// directory; symlinks out of it do not count. The user may upload any
// file.
func TestAgentUploadConfined(t *testing.T) {
	f := setup(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("secret"), 0o600)
	uploads := filepath.Join(f.home, "uploads")
	for _, p := range []string{outside, "~/.netrc", filepath.Join(uploads, "..", "server.json")} {
		mustFail(t, invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": p, "remote_path": "k"}), uploads)
	}
	if fi, err := os.Stat(uploads); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("upload directory: %v %v", fi, err)
	}
	os.Symlink(outside, filepath.Join(uploads, "link.txt"))
	mustFail(t, invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": filepath.Join(uploads, "link.txt"), "remote_path": "k"}), uploads)
	if _, err := os.Stat(filepath.Join(f.srv.Root, "k")); !os.IsNotExist(err) {
		t.Fatal("a refused upload reached the host")
	}
	// A path relative to the upload directory works.
	os.WriteFile(filepath.Join(uploads, "app.tgz"), []byte("app"), 0o600)
	mustOK[TransferOutput](t, invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": "app.tgz", "remote_path": "app.tgz"}))
	if got, _ := os.ReadFile(filepath.Join(f.srv.Root, "app.tgz")); string(got) != "app" {
		t.Fatalf("%q", got)
	}
	mustOK[TransferOutput](t, invoke(t, f.e, desktop, "file.upload", map[string]any{"host": "prod", "local_path": outside, "remote_path": "mine"}))
}

func TestDownload(t *testing.T) {
	f := setup(t)
	payload := data(2<<20 + 5)
	os.MkdirAll(filepath.Join(f.srv.Root, "var", "log"), 0o755)
	os.WriteFile(filepath.Join(f.srv.Root, "var", "log", "app.log"), payload, 0o644)

	res := mustOK[TransferOutput](t, invoke(t, f.e, codex, "file.download", map[string]any{"host": "prod", "remote_path": "/var/log/app.log"}))
	want := filepath.Join(f.home, "downloads", "prod", "app.log")
	if res.LocalPath != want || res.Status != transfer.Done || res.Bytes != int64(len(payload)) || res.SHA256 != sum(payload) {
		t.Fatalf("%+v", res)
	}
	if got, _ := os.ReadFile(want); string(got) != string(payload) {
		t.Fatal("content differs")
	}
	if ev := requested(t, f.e, "file.download"); ev.Risk != "read" {
		t.Fatalf("label: %+v", ev)
	}
	// A relative local path is inside the host's download directory.
	res = mustOK[TransferOutput](t, invoke(t, f.e, codex, "file.download", map[string]any{"host": "prod", "remote_path": "/var/log/app.log", "local_path": "old/app.log.1"}))
	if res.LocalPath != filepath.Join(f.home, "downloads", "prod", "old", "app.log.1") {
		t.Fatalf("%+v", res)
	}
	mustFail(t, invoke(t, f.e, codex, "file.download", map[string]any{"host": "prod", "remote_path": "/var/log"}), "regular file")
}

// TestDownloadLocalPath: agents download only into the download
// directory; the user can download anywhere.
func TestDownloadLocalPath(t *testing.T) {
	f := setup(t)
	os.WriteFile(filepath.Join(f.srv.Root, "a.txt"), []byte("abc"), 0o644)
	elsewhere := filepath.Join(t.TempDir(), "a.txt")
	for _, p := range []string{elsewhere, "../../escape.txt", "~/.zshrc", "../other-host/a.txt"} {
		mustFail(t, invoke(t, f.e, codex, "file.download", map[string]any{"host": "prod", "remote_path": "a.txt", "local_path": p}), "download directory")
	}
	res := mustOK[TransferOutput](t, invoke(t, f.e, desktop, "file.download", map[string]any{"host": "prod", "remote_path": "a.txt", "local_path": elsewhere}))
	if got, _ := os.ReadFile(elsewhere); string(got) != "abc" || res.LocalPath != elsewhere {
		t.Fatalf("%q %+v", got, res)
	}
}

func TestDownloadResume(t *testing.T) {
	f := setup(t)
	payload := data(1 << 20)
	os.WriteFile(filepath.Join(f.srv.Root, "big.bin"), payload, 0o644)
	dst := filepath.Join(f.home, "downloads", "prod", "big.bin")
	os.MkdirAll(filepath.Dir(dst), 0o700)
	os.WriteFile(dst, payload[:123_456], 0o600)
	res := mustOK[TransferOutput](t, invoke(t, f.e, codex, "file.download", map[string]any{"host": "prod", "remote_path": "big.bin", "resume": true}))
	if got, _ := os.ReadFile(dst); string(got) != string(payload) || res.ResumedFrom != 123_456 || res.SHA256 != sum(payload) {
		t.Fatalf("resumed_from %d, %d bytes", res.ResumedFrom, len(got))
	}
	// Without resume an existing file is replaced from the start.
	res = mustOK[TransferOutput](t, invoke(t, f.e, codex, "file.download", map[string]any{"host": "prod", "remote_path": "big.bin"}))
	if res.ResumedFrom != 0 || res.SHA256 != sum(payload) {
		t.Fatalf("%+v", res)
	}
}

// paused makes transfers of f stop after their first chunk until their
// context ends; started receives once per transfer when that happens.
func paused(f fixture) <-chan struct{} {
	started := make(chan struct{}, 1)
	var n int
	f.p.pace = func(ctx context.Context) {
		n++
		if n == 2 {
			started <- struct{}{}
			<-ctx.Done()
		}
	}
	return started
}

func TestCancelTransfer(t *testing.T) {
	for _, dir := range []string{"upload", "download"} {
		t.Run(dir, func(t *testing.T) {
			f := setup(t)
			started := paused(f)
			payload := data(4 << 20)
			src := local(t, f, payload)
			os.WriteFile(filepath.Join(f.srv.Root, "big.bin"), payload, 0o644)
			args := map[string]any{"host": "prod", "remote_path": "big.bin"}
			if dir == "upload" {
				args = map[string]any{"host": "prod", "local_path": src, "remote_path": "copy.bin"}
			}
			done := make(chan engine.Outcome, 1)
			go func() { done <- invoke(t, f.e, codex, "file."+dir, args) }()
			<-started
			list := f.p.Transfers.List()
			if len(list) != 1 || list[0].Status != transfer.Running {
				t.Fatalf("%+v", list)
			}
			if err := f.p.Transfers.Cancel(list[0].ID, desktop); err != nil {
				t.Fatal(err)
			}
			mustFail(t, <-done, "cancelled by human:desktop")
			if tr, _ := f.p.Transfers.Get(list[0].ID); tr.Status != transfer.Cancelled || tr.Bytes >= int64(len(payload)) {
				t.Fatalf("%+v", tr)
			}
		})
	}
}

// TestRevokeStopsTransfer: revoking the agent cancels its transfer.
func TestRevokeStopsTransfer(t *testing.T) {
	f := setup(t)
	started := paused(f)
	os.WriteFile(filepath.Join(f.srv.Root, "big.bin"), data(4<<20), 0o644)
	done := make(chan engine.Outcome, 1)
	go func() {
		done <- invoke(t, f.e, codex, "file.download", map[string]any{"host": "prod", "remote_path": "big.bin"})
	}()
	<-started
	f.e.CancelClient("codex", engine.ErrClientRevoked)
	mustFail(t, <-done, "client revoked")
	evs, _ := f.e.Events.Read(f.e.Events.Now())
	var failed bool
	for _, ev := range evs {
		if ev.Type == eventlog.TransferFailed && strings.Contains(ev.Reason, "client revoked") {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("no transfer.failed event: %+v", evs)
	}
	if list := f.p.Transfers.List(); list[0].Status != transfer.Cancelled {
		t.Fatalf("%+v", list)
	}
}

// TestConcurrentUploadsRefused: a second upload to a target that is
// being uploaded is refused; other targets are fine.
func TestConcurrentUploadsRefused(t *testing.T) {
	f := setup(t)
	started := paused(f)
	src := local(t, f, data(4<<20))
	done := make(chan engine.Outcome, 1)
	go func() {
		done <- invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": src, "remote_path": "app.bin"})
	}()
	<-started
	mustFail(t, invoke(t, f.e, desktop, "file.upload", map[string]any{"host": "prod", "local_path": src, "remote_path": "./app.bin"}), "already being uploaded")
	f.p.Transfers.Cancel(f.p.Transfers.List()[0].ID, desktop)
	<-done
	f.p.pace = nil
	mustOK[TransferOutput](t, invoke(t, f.e, desktop, "file.upload", map[string]any{"host": "prod", "local_path": src, "remote_path": "app.bin"}))
}

// TestUploadTempSymlink: a symlink planted where the temporary file
// goes is neither written through by a fresh upload nor resumed.
func TestUploadTempSymlink(t *testing.T) {
	f := setup(t)
	payload := data(100_000)
	src := local(t, f, payload)
	victim := filepath.Join(f.srv.Root, "victim")
	os.WriteFile(victim, []byte("v"), 0o644)
	tmp := filepath.Join(f.srv.Root, ".app.bin.cox-upload")
	os.Symlink("victim", tmp)
	mustFail(t, invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": src, "remote_path": "app.bin", "resume": true}), "regular file")
	mustOK[TransferOutput](t, invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": src, "remote_path": "app.bin"}))
	if got, _ := os.ReadFile(victim); string(got) != "v" {
		t.Fatalf("written through the symlink: %d bytes", len(got))
	}
	if got, _ := os.ReadFile(filepath.Join(f.srv.Root, "app.bin")); string(got) != string(payload) {
		t.Fatal("target wrong")
	}
}
