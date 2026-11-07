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
| you, in a terminal | runs | runs | asks `[y/N]` |
| agent, normal host | runs | runs | asks |
| agent, `env=prod` host | runs | asks | asks |

- Commands are parsed (not pattern-matched) to rate them; anything that cannot be understood
  counts as destructive.
- An approval is bound to the exact command, used once, and expires after 30 minutes.
- If nobody answers the dialog, approve later with `cox approvals` / `cox approve <id>`.
  Agents cannot approve their own actions.
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
