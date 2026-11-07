// Package actor works out who is running cox: a human, or an agent
// (Claude Code, Codex, Kimi) running it through its shell tool.
package actor

import (
	"os"
	"strings"

	"github.com/hua-ri/coxswain/internal/action"
	"golang.org/x/sys/unix"
)

// agentNames maps a process name (as the kernel reports it, at most
// 16 bytes) to the agent it belongs to.
var agentNames = map[string]string{
	"claude": "claude-code",
	"codex":  "codex",
	"kimi":   "kimi",
}

// Detect works out the actor: COX_ACTOR names an agent; else a known
// agent ancestor makes the caller that agent; else a caller without an
// interactive terminal on stdin is the agent "unknown". A person must be
// at an interactive terminal to act as human. COX_ACTOR can only make the
// caller an agent, never a human.
func Detect(stdinTTY bool) action.Actor {
	env := os.Getenv("COX_ACTOR")
	name, ok := "", false
	if env == "" {
		name, ok = FromAncestors(os.Getppid(), parent)
	}
	return classify(env, name, ok, stdinTTY)
}

// classify is Detect's decision, free of process and environment lookups.
func classify(env, ancestor string, isAgent, tty bool) action.Actor {
	switch {
	case env != "":
		return action.Actor{Kind: action.Agent, Name: strings.TrimPrefix(env, "agent:")}
	case isAgent:
		return action.Actor{Kind: action.Agent, Name: ancestor}
	case !tty:
		return action.Actor{Kind: action.Agent, Name: "unknown"}
	}
	u := os.Getenv("USER")
	if u == "" {
		u = "user"
	}
	return action.Actor{Kind: action.Human, Name: u}
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
