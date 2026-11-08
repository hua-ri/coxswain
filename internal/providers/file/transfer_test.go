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

func local(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(p, b, 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUpload(t *testing.T) {
	f := setup(t)
	payload := data(3<<20 + 17)
	src := local(t, payload)
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
	src := local(t, payload)
	os.WriteFile(filepath.Join(f.srv.Root, "part.bin"), payload[:300_000], 0o644)
	res := mustOK[TransferOutput](t, invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": src, "remote_path": "part.bin", "resume": true}))
	got, _ := os.ReadFile(filepath.Join(f.srv.Root, "part.bin"))
	if string(got) != string(payload) || res.ResumedFrom != 300_000 || res.Verified == nil || !*res.Verified {
		t.Fatalf("resumed_from %d, remote %d bytes, verified %v", res.ResumedFrom, len(got), res.Verified)
	}
	// A remote prefix that differs from the local file: the checksum
	// catches it.
	os.WriteFile(filepath.Join(f.srv.Root, "bad.bin"), make([]byte, 1000), 0o644)
	mustFail(t, invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": src, "remote_path": "bad.bin", "resume": true}), "checksum mismatch")
	// Without resume the file is replaced.
	mustOK[TransferOutput](t, invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": src, "remote_path": "bad.bin"}))
	if got, _ := os.ReadFile(filepath.Join(f.srv.Root, "bad.bin")); string(got) != string(payload) {
		t.Fatal("not replaced")
	}
}

func TestUploadLabels(t *testing.T) {
	f := setup(t)
	src := local(t, []byte("x"))
	os.Mkdir(filepath.Join(f.srv.Root, "etc"), 0o755)
	invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": src, "remote_path": "/etc/x"})
	if ev := requested(t, f.e, "file.upload"); ev.Risk != "destructive" {
		t.Fatalf("protected remote path: %s", ev.Risk)
	}
	key := filepath.Join(f.home, ".ssh", "id_ed25519")
	os.MkdirAll(filepath.Dir(key), 0o700)
	os.WriteFile(key, []byte("KEY"), 0o600)
	invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": "~/.ssh/id_ed25519", "remote_path": "k"})
	if ev := requested(t, f.e, "file.upload"); ev.Risk != "destructive" {
		t.Fatalf("credential upload: %s", ev.Risk)
	}
	mustFail(t, invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": "relative.txt", "remote_path": "k"}), "absolute")
	mustFail(t, invoke(t, f.e, codex, "file.upload", map[string]any{"host": "prod", "local_path": filepath.Dir(src), "remote_path": "k"}), "regular file")
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
	for _, p := range []string{elsewhere, "../../escape.txt", "~/.zshrc"} {
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
			src := local(t, payload)
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
