# Coxswain (`cox`)

An ops toolbox for you **and your AI agents**. Coxswain keeps your SSH hosts, credentials
and approval policy in one place, and gives Claude Code, Codex and Kimi Code safe access to
them over MCP — risky actions wait for your approval in a native macOS dialog.

> macOS only. Personal use. Early: M0 covers SSH hosts and remote commands.
> Kubernetes, Argo CD / Argo Workflows and the desktop app (`cox desktop`) are next.

## Install

```bash
brew install hua-ri/tap/coxswain
# or
curl -fsSL https://raw.githubusercontent.com/hua-ri/coxswain/main/install.sh | sh
```

## Quick start

```bash
cox init                              # import ~/.ssh/config, connect Claude Code / Codex / Kimi
cox ssh hosts list
cox ssh hosts tag prod-web-01 --set env=prod --note "no restarts 9:00-18:00"
cox ssh exec prod-web-01 -- df -h
```

Then ask your agent, e.g. *"Why is the disk on prod-web-01 filling up?"* It uses the `cox`
tools; anything that changes a production host, or anything destructive anywhere, pops up an
approval dialog first.

## How approvals work

| | read | write | destructive |
|---|---|---|---|
| you, at an interactive terminal | runs | runs | asks `[y/N]` |
| agent, normal host | runs | runs | asks |
| agent, `env=prod` host | runs | asks | asks |

- Anyone without an interactive terminal (an agent, a script, a pipe) counts as the agent
  `unknown`. You must be at a terminal to act as yourself.
- Commands are parsed (not pattern-matched) to rate them. Commands that can't be parsed, or whose
  name is only known at run time, count as destructive. Commands cox doesn't know count as writes.
- Changing a host's `env` or `ai_policy` tag or its note is destructive: the note tells agents
  what they may do.
- Importing from a file other than `~/.ssh/config` is destructive. An import that changes a host,
  or adds a new name for a host's address, needs that host's approval. A new name for an existing
  address gets that host's tags and note.
- An approval is bound to the exact command, used once, and expires after 30 minutes (whether
  approved or not).
- If nobody answers the dialog, run `cox approvals` to see what's waiting and `cox approve <id>`
  to bring the dialog back. You approve by clicking the button; typing `y` doesn't work.
  Agents can't approve their own actions.
- Tag a host `ai_policy=always|balanced|auto` to override its mode.

Everything is recorded in `~/.coxswain/events/YYYY-MM-DD.jsonl`.

## Files

| Path | What |
|---|---|
| `~/.coxswain/hosts.json` | hosts, tags, notes |
| `~/.coxswain/approvals/` | pending and decided approvals |
| `~/.coxswain/events/` | audit log |
| `~/.coxswain/artifacts/` | full copies of truncated outputs |

Credentials stay in `ssh-agent` and your key files; Coxswain never stores or shows them.

## Security model

Coxswain guards against agents that make mistakes or have been prompt-injected, as long as they
work through cox's own tools (MCP or the `cox` CLI). Every action gets a risk rating, follows the
approval policy and is written to the audit log. "Human" means a person at an interactive
terminal that was not started by an agent; anything else counts as an agent.

It is not a sandbox. An agent that has its own unrestricted shell, running as your user, can
edit `~/.coxswain` or use `ssh` directly and skip cox entirely. Keep such agents sandboxed, and
don't let them run raw `ssh`. Approvals are a guardrail; they don't isolate anything.

- Commands are saved in the audit log and in approvals exactly as typed. Secrets you put on a
  command line end up there too.
- Full copies of truncated outputs stay in `~/.coxswain/artifacts/` until you delete them.
