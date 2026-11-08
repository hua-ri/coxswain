# Coxswain (`cox`)

An ops toolbox for you **and your AI agents**. Coxswain keeps your SSH hosts and connections in
one place and gives Claude Code, Codex and Kimi Code access to them over MCP. You and your agents
use the same tool and see the same activity.

> macOS only. Personal use. Early: SSH hosts, remote commands, shared terminals and a desktop app.
> Kubernetes and Argo CD / Argo Workflows are next.

## Install

```bash
brew install hua-ri/tap/coxswain
# or
curl -fsSL https://raw.githubusercontent.com/hua-ri/coxswain/main/install.sh | sh
```

## Quick start

```bash
cox init      # import ~/.ssh/config, connect Claude Code / Codex / Kimi
cox server    # run Coxswain in a terminal; leave it open (Ctrl-C stops it)
```

In another terminal:

```bash
cox ssh hosts list
cox ssh hosts tag prod-web-01 --set env=prod --note "no restarts 9:00-18:00"
cox ssh exec prod-web-01 -- df -h
```

Then ask your agent, e.g. *"Why is the disk on prod-web-01 filling up?"* It uses the `cox`
tools. `cox server` prints one line for every action, whether you or an agent ran it.

`cox server` holds all state (connections, sessions). `cox` commands and `cox mcp` (what your
agents start) are clients of it. If it isn't running, they say:
`Coxswain server is not running. Start it with: cox server`.

## Desktop

```bash
cox desktop   # needs a running cox server
```

`cox desktop` opens a window on the running server. Closing it leaves the server running; run
`cox desktop` again to come back. It shows:

- **Hosts**, with their tags (`env=prod` in red) and notes. Click *Terminal* to open a terminal on one.
- **Activity**: every action as it happens, yours and your agents', with its risk rating, host
  and result. Filter by client or host. Click a terminal row to jump to that terminal.
- **Terminals**, one tab per shared terminal.
- **Agents**: allow or revoke them, like `cox clients allow|revoke`.

Terminals live in `cox server` and are shared. Your agents can read a terminal you opened (ask
yours to "read the last lines of my terminal on web-01"). A terminal an agent opens appears as a
tab, marked with the agent's name, and you can type in it at any time. A dot blinks on the tab
while the agent types. Agents can close only their own terminals. When you close an agent's
terminal, the desktop asks first.

Agents use terminals through the `terminal_*` tools: `ssh_exec` for one-off commands, a terminal
for long-running work you may want to watch. Terminals end when `cox server` stops. Output of
an ended terminal stays readable for 10 minutes.

## Who asks before an agent changes something

Your agent does, not Coxswain. Every cox tool tells the agent what it does: `ssh_hosts_list`,
`terminal_read` and `artifact_read` only read; `ssh_exec`, `terminal_send` and `ssh_hosts_tag` can
change things. Claude Code, Codex
and Kimi Code then follow their own permission settings, for example asking you before each
`ssh_exec`. Configure that in your agent.

Coxswain still helps:

- Hosts tagged `env=prod` are production. The tools tell agents to check with you before
  changing them, and every `ssh_exec` result includes the host's tags and note.
- A host note ("no restarts during business hours") is shown to agents, who are told to follow it.
- Every command gets a risk rating (read, write or destructive) in the audit log and in
  `cox server`'s output. The rating is a label; nothing is blocked. Commands are parsed, not
  pattern-matched. Commands that can't be parsed, or whose name is only known at run time, count
  as destructive. Commands cox doesn't know count as writes.
- When an import adds a new name for an existing host address, the new name gets that host's
  tags and note, so it can't drop `env=prod`.

Everything is recorded in `~/.coxswain/events/YYYY-MM-DD.jsonl`.

## Allowing agents

The first time an agent uses Coxswain (Claude Code, Codex, Kimi…), a macOS dialog asks:
*"codex wants to use Coxswain to operate your servers."* If you allow it, Coxswain remembers that.
If you deny it, or nobody answers within 60 seconds, its calls return `client_not_authorized`
and you won't be asked about it again for a minute.

```bash
cox clients list           # agents you allowed or revoked
cox clients allow codex    # allow without the dialog (takes effect at once)
cox clients revoke codex   # cut it off now; it is refused, without asking, until you allow it again
```

Revoking takes effect at once: whatever the agent is running (a command, a terminal call) is
stopped and recorded as failed with "client revoked". Terminals it opened stay open, since you may
be watching them; close them yourself in `cox desktop`.

This also covers agents that run `cox` through their own shell. Coxswain recognises Claude Code,
Codex and Kimi as parent processes (or `COX_ACTOR=agent:<name>`); a `cox` run without a terminal
counts as the agent `unknown`. Their calls need your permission, their output is shortened like
over MCP, and the audit log names them. Only you, at a terminal, are `cli`, which needs no
permission; agents can't use `cox clients`.

## Files

| Path | What |
|---|---|
| `~/.coxswain/server.json` | address, pid and access token of the running server (0600) |
| `~/.coxswain/clients.json` | agents you allowed or revoked |
| `~/.coxswain/hosts.json` | hosts, tags, notes |
| `~/.coxswain/events/` | audit log |
| `~/.coxswain/artifacts/` | full copies of truncated outputs |

Credentials stay in `ssh-agent` and your key files. Coxswain never stores or shows them, and
agents never get them.

## Security model

`cox server` listens on 127.0.0.1 only. Every request needs the random token from
`~/.coxswain/server.json`, which changes each time the server starts. Requests from web browsers
(with an `Origin` header) are refused. The desktop app's page never sees the token: the `cox
desktop` process makes the requests for it.

Client authorization covers agents that use Coxswain through its own entry points (`cox mcp`, the
`cox` CLI). It is **not** an isolation boundary. Any process running as your user can read
`server.json` and claim any client name, including `cli`. An agent with an
unrestricted shell can also edit `~/.coxswain` or run `ssh` directly. Keep such agents sandboxed
and don't let them run raw `ssh`. Your credentials still never leave the server process and
`ssh-agent`.

- Commands are saved in the audit log exactly as typed. Secrets you put on a command line end up
  there too.
- Full copies of truncated outputs stay in `~/.coxswain/artifacts/` until you delete them.
