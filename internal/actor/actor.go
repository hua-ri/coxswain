// Package actor works out which client is running the cox CLI: the user,
// or an agent (Claude Code, Codex, Kimi) running it through its shell
// tool. The name is sent to the server as X-Cox-Client.
package actor

import (
	"os"
	"strings"

	"github.com/hua-ri/coxswain/internal/clients"
	"golang.org/x/sys/unix"
)

// agentNames maps a process name (as the kernel reports it, at most
// 16 bytes) to the agent it belongs to.
var agentNames = map[string]string{
	"claude": "claude-code",
	"codex":  "codex",
	"kimi":   "kimi",
}

// Client names the caller: COX_ACTOR names an agent; else (if
// ancestors) a known agent ancestor makes the caller that agent; else a
// caller without an interactive terminal on stdin is the agent "unknown";
// else it is the user ("cli"). COX_ACTOR can only name an agent, never
// the user.
func Client(stdinTTY, ancestors bool) string {
	env := os.Getenv("COX_ACTOR")
	name, ok := "", false
	if env == "" && ancestors {
		name, ok = FromAncestors(os.Getppid(), parent)
	}
	return classify(env, name, ok, stdinTTY)
}

// classify is Client's decision, free of process and environment lookups.
func classify(env, ancestor string, isAgent, tty bool) string {
	switch {
	case env != "":
		name := strings.TrimPrefix(env, "agent:")
		if !clients.ValidName(name) || clients.BuiltIn(name) {
			return "unknown"
		}
		return name
	case isAgent:
		return ancestor
	case !tty:
		return "unknown"
	}
	return "cli"
}

// StdinTTY reports whether stdin is an interactive terminal.
func StdinTTY() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TIOCGETA)
	return err == nil
}

// FromAncestors walks up from pid (at most 64 levels) and returns the
// agent name of the first ancestor that is a known agent.
func FromAncestors(pid int, lookup func(int) (ppid int, comm string, ok bool)) (string, bool) {
	for i := 0; i < 64 && pid > 1; i++ {
		ppid, comm, ok := lookup(pid)
		if !ok {
			return "", false
		}
		if name, isAgent := agentNames[comm]; isAgent {
			return name, true
		}
		pid = ppid
	}
	return "", false
}

func parent(pid int) (int, string, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, "", false
	}
	return int(kp.Eproc.Ppid), unix.ByteSliceToString(kp.Proc.P_comm[:]), true
}
