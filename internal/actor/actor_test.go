package actor

import (
	"testing"

	"github.com/hua-ri/coxswain/internal/action"
)

func fakeTree(procs map[int]struct {
	ppid int
	comm string
}) func(int) (int, string, bool) {
	return func(pid int) (int, string, bool) {
		p, ok := procs[pid]
		return p.ppid, p.comm, ok
	}
}

func TestFromAncestors(t *testing.T) {
	tree := fakeTree(map[int]struct {
		ppid int
		comm string
	}{
		30: {20, "zsh"},
		20: {10, "codex"},
		10: {1, "launchd"},
		40: {1, "zsh"},
	})
	if name, ok := FromAncestors(30, tree); !ok || name != "codex" {
		t.Fatalf("got %q %v", name, ok)
	}
	if _, ok := FromAncestors(40, tree); ok {
		t.Fatal("plain shell must not be an agent")
	}
}

func TestEnvOverrideMakesAgent(t *testing.T) {
	t.Setenv("COX_ACTOR", "agent:kimi")
	if a := Detect(); a.Kind != action.Agent || a.Name != "kimi" {
		t.Fatalf("got %+v", a)
	}
}
