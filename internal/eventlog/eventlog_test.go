package eventlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
)

func TestAppendAndRead(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2026, 11, 7, 10, 0, 0, 0, time.Local)
	l := Open(dir)
	l.Now = func() time.Time { return day }

	actor := &action.Actor{Kind: action.Agent, Name: "codex"}
	if err := l.Append(Event{Type: ActionRequested, Actor: actor, Action: "ssh.exec", Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(Event{Type: ActionFinished, Action: "ssh.exec", DurationMs: 12}); err != nil {
		t.Fatal(err)
	}

	got, err := l.Read(day)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Actor.Name != "codex" || got[1].DurationMs != 12 {
		t.Fatalf("unexpected events: %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, "2026-11-07.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestReadMissingDay(t *testing.T) {
	got, err := Open(t.TempDir()).Read(time.Now())
	if err != nil || got != nil {
		t.Fatalf("got %v, %v", got, err)
	}
}
