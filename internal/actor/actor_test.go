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
	if a := Detect(true); a.Kind != action.Agent || a.Name != "kimi" {
		t.Fatalf("got %+v", a)
	}
}

// TestClassify: a caller without an interactive terminal is an agent
// ("unknown") unless something says otherwise (review C2).
func TestClassify(t *testing.T) {
	t.Setenv("USER", "king")
	cases := []struct {
		env, ancestor string
		ok, tty       bool
		want          action.Actor
	}{
		{"agent:kimi", "", false, true, action.Actor{Kind: action.Agent, Name: "kimi"}},
		{"codex", "", false, false, action.Actor{Kind: action.Agent, Name: "codex"}},
		{"", "claude-code", true, true, action.Actor{Kind: action.Agent, Name: "claude-code"}},
		{"", "", false, false, action.Actor{Kind: action.Agent, Name: "unknown"}},
		{"", "", false, true, action.Actor{Kind: action.Human, Name: "king"}},
	}
	for _, c := range cases {
		if got := classify(c.env, c.ancestor, c.ok, c.tty); got != c.want {
			t.Errorf("classify(%q, %q, %v, %v) = %+v, want %+v", c.env, c.ancestor, c.ok, c.tty, got, c.want)
		}
	}
}
