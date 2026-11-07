package actor

import "testing"

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

func TestEnvOverride(t *testing.T) {
	t.Setenv("COX_ACTOR", "agent:kimi")
	if got := Client(true, true); got != "kimi" {
		t.Fatalf("got %q", got)
	}
}

// TestClassify: COX_ACTOR, then a known agent ancestor, name the client;
// without an interactive terminal it is "unknown"; otherwise it is the
// user's own "cli".
func TestClassify(t *testing.T) {
	cases := []struct {
		env, ancestor string
		ok, tty       bool
		want          string
	}{
		{"agent:kimi", "", false, true, "kimi"},
		{"codex", "", false, false, "codex"},
		{"agent:Bad Name", "", false, true, "unknown"},
		{"agent:cli", "", false, true, "unknown"}, // COX_ACTOR never makes the caller the user
		{"", "claude-code", true, true, "claude-code"},
		{"", "", false, false, "unknown"},
		{"", "", false, true, "cli"},
	}
	for _, c := range cases {
		if got := classify(c.env, c.ancestor, c.ok, c.tty); got != c.want {
			t.Errorf("classify(%q, %q, %v, %v) = %q, want %q", c.env, c.ancestor, c.ok, c.tty, got, c.want)
		}
	}
}
