# Coxswain (`cox`)

An ops toolbox for you **and your AI agents**. Coxswain keeps your SSH hosts and connections in
one place and gives Claude Code, Codex and Kimi Code access to them over MCP. You and your agents
use the same tool and see the same activity.

> macOS only. Personal use. Early: SSH hosts, remote commands, batch runs, files, tunnels, shared
> terminals and a desktop app. Kubernetes and Argo CD / Argo Workflows are next.

## Install

```bash
brew install hua-ri/tap/coxswain
# or
curl -fsSL https://raw.githubusercontent.com/hua-ri/coxswain/main/install.sh | sh
```

## Quick start

```bash
cox init      # sync hosts from ~/.ssh/config, connect Claude Code / Codex / Kimi
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

## What it can do

| Capability | Tools (MCP) / commands | Notes |
|---|---|---|
| Hosts | `ssh_hosts_list`, `ssh_hosts_add`, `ssh_hosts_update`, `ssh_hosts_remove`, `ssh_hosts_tag`, `ssh_hosts_import` | synced from `~/.ssh/config` or added by hand; tags and notes are shown to agents |
| Host keys | `ssh_hosts_scan_key`, `ssh_hosts_test`; `trust_key` and `forget_key` for you only | trusted keys go to `~/.coxswain/known_hosts`; `~/.ssh/known_hosts` is only read |
| Clusters | `k8s_contexts`; `k8s_clusters_add`, `k8s_clusters_remove`, `_inspect`, `_test` for you only | your kubeconfig is only read; clusters you add are kept in `~/.coxswain/kubeconfigs/` |
| Commands | `ssh_exec` | one command, output and exit code |
| Batch runs | `ssh_batch` | one command on many hosts (`hosts` plus every host with `tag`), canary first, stops on failure; above 50 hosts needs `confirm_count` |
| Files | `file_list`, `file_stat`, `file_read`, `file_write`, `file_mkdir`, `file_remove` | over SFTP; `file_write` is atomic; `file_read` is for small text files |
| Transfers | `file_upload`, `file_download` | with progress, cancel, resume and SHA-256; downloads go to `~/.coxswain/downloads/<host>/` |
| Tunnels | `tunnel_open`, `tunnel_list`, `tunnel_close` | local (`-L`), remote (`-R`, opened by you only) and SOCKS5 (`-D`), on 127.0.0.1; reconnect by themselves |
| Terminals | `terminal_open`, `terminal_send`, `terminal_read`, `terminal_close` | shared with you in `cox desktop` |
| Large outputs | `artifact_read` | the full copy of anything that was shortened |

Every tool is also a command, e.g.:

```bash
cox file list web-01 /var/log
cox file download web-01 /var/log/nginx/error.log      # → ~/.coxswain/downloads/web-01/error.log
cox file upload web-01 ~/build/app.tgz /tmp/app.tgz
cox tunnel open web-01 local --remote-port 5432         # prints the local address (listen)
cox tunnel list
cox ssh batch --tag role=web -- uptime
cox ssh hosts add web-3 10.0.1.13 --user deploy --proxy-jump bastion --tags env=prod
cox ssh hosts scan_key web-3                             # its key fingerprint: compare it
cox ssh hosts trust_key web-3 --fingerprint SHA256:…     # then trust it (you only)
cox k8s clusters add --path ~/Downloads/prod-sg.yaml --context eks-prod-sg
```

### Syncing from ~/.ssh/config

*Sync from ~/.ssh/config…* (or `cox ssh hosts import`) adds the hosts in your ssh config and
refreshes the address, port, user, keys and jump host of the ones it imported before. It never
changes `~/.ssh/config`, and keeps Coxswain's tags and notes. It shows a preview first
(`cox ssh hosts import --dry-run`): new hosts, what will change (field: old → new), and the hosts
you edited in Coxswain (or added by hand) since, which stay as they are unless you tick
*Overwrite* (`--overwrite NAME`).

### Host keys

Coxswain checks host keys like `ssh` does, against `~/.ssh/known_hosts` (read, never written)
and its own `~/.coxswain/known_hosts`. For a new host, *Test connection* in `cox desktop` (or
`cox ssh hosts scan_key`) shows the key's fingerprint. Compare it with the one the host's admin
gave you, then trust it; Coxswain writes it to its own file only if the fingerprint is still the
one you confirmed. Behind a jump host, the jump host's key comes first. A **changed** key is
never trusted: Coxswain refuses to connect until you remove the old entry (`forget_key`, which
only edits Coxswain's file; for an entry in `~/.ssh/known_hosts` it tells you the `ssh-keygen -R`
command to run). Agents can scan keys but cannot trust or forget them.

### Clusters you add

Coxswain reads your kubeconfig (`$KUBECONFIG` or `~/.kube/config`) and never writes, changes or
switches it. To add a cluster, paste a kubeconfig or pick a file in `cox desktop` (Clusters → +),
or use `cox k8s clusters add`. Coxswain keeps only the context you choose, with its cluster and
credentials, in `~/.coxswain/kubeconfigs/` (0600), and merges those files after yours. A context
name your kubeconfig already has is refused; give the copy another name. *Test* asks the cluster
for its version without saving anything. Only clusters added this way can be removed in Coxswain;
the others show as read-only. Adding and removing clusters is for you only, and the audit log
records a kubeconfig as its size, SHA-256 and context names, never its contents.

Transfers, tunnels and batch runs live in `cox server`, each with its owner (you or an agent).
`cox desktop` shows them and can stop any of them; agents can close only their own tunnels.
They all end when `cox server` stops.

`cox server` holds all state (connections, sessions, transfers, tunnels). `cox` commands and `cox mcp` (what your
agents start) are clients of it. If it isn't running, they say:
`Coxswain server is not running. Start it with: cox server`.

## Desktop

```bash
cox desktop   # needs a running cox server
```

`cox desktop` opens a window on the running server. Closing it leaves the server running; run
`cox desktop` again to come back. It shows:

- **Hosts**, with their tags (`env=prod` in red) and notes. Add one with the + on the Hosts header
  (or *Sync from ~/.ssh/config…* from the same menu); edit or remove one from its page or by
  right-clicking it. Removing a host leaves its open terminals and tunnels until you close them.
  Click *Terminal* to open a terminal on
  one, *Files* to browse its files: open folders, download (to `~/.coxswain/downloads/<host>/`),
  upload a file you pick, create folders, delete (after asking).
- **Activity**: every action as it happens, yours and your agents', with its risk rating, host
  and result. Filter by client or host. Click a terminal row to jump to that terminal; click a
  batch run (`ssh.batch`) to see each host's result and, on request, its output.
- **Transfers**: uploads and downloads, yours and your agents', with progress and speed. Cancel a
  running one; it can be resumed later.
- **Tunnels**: every open tunnel with its route, owner, connections and traffic, and its last
  failed connection (e.g. the host forbids forwarding). Close any of them, or open one with
  *New tunnel*.
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
`file_list`, `file_read`, `terminal_read` and `artifact_read` only read; `ssh_exec`, `ssh_batch`,
`file_write`, `file_upload`, `tunnel_open`, `terminal_send` and `ssh_hosts_tag` / `_add` / `_update` / `_remove` can change things. Claude Code, Codex
and Kimi Code then follow their own permission settings, for example asking you before each
`ssh_exec`. Configure that in your agent.

Coxswain still helps:

- Hosts tagged `env=prod` are production. The tools tell agents to check with you before
  changing them, and every `ssh_exec` result includes the host's tags and note.
- A host note ("no restarts during business hours") is shown to agents, who are told to follow it.
- Agents may add, change and remove hosts; each change is recorded with the agent's name, and
  changing `env` (or `ai`) tags or a note is rated destructive. Trusting or forgetting a host
  key, and adding or removing a cluster, are for you only: agents are refused (and the refusal
  is recorded) and told to ask you.
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

Revoking takes effect at once: whatever the agent is running (a command, a batch run, a file
transfer, a terminal call) is stopped and recorded as failed with "client revoked", and the
tunnels it opened are closed. Terminals it opened stay open, since you may be watching them;
close them yourself in `cox desktop`.

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
| `~/.coxswain/known_hosts` | host keys you trusted in Coxswain (0600) |
| `~/.coxswain/kubeconfigs/` | clusters you added in Coxswain, one kubeconfig each (0600, dir 0700) |
| `~/.coxswain/clusters.json` | tags and notes of Kubernetes contexts |
| `~/.coxswain/events/` | audit log |
| `~/.coxswain/artifacts/` | full copies of truncated outputs (and of every batch run's output) |
| `~/.coxswain/downloads/` | downloaded files, one folder per host |

SSH credentials stay in `ssh-agent` and your key files: Coxswain stores only key paths, never
shows them, and agents never get them. Kubernetes credentials of clusters you add are stored in
`~/.coxswain/kubeconfigs/`, as a kubeconfig would store them.

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
- Local and SOCKS tunnels listen on 127.0.0.1 only, but any local process (not just the agent
  that opened it) can use one while it is open. A remote tunnel (`-R`) exposes a service on your
  Mac to the host. It asks the host to listen on 127.0.0.1, which holds only if the host's sshd
  doesn't allow `GatewayPorts`. Only you can open remote tunnels (in `cox desktop` or with
  `cox tunnel open`); agents can't. No tunnel may lead to `cox server`'s own port.
- Agents' tunnels close after at most 4 hours without connections (30 minutes by default), and
  each agent may have at most 16 open.
- Agents may download only into `~/.coxswain/downloads/<host>/`, and upload only files in
  `~/.coxswain/uploads/` (symlinks out of it don't count). To upload something else, an agent first
  copies it there with its own tools, which its own permission settings cover. You can upload any
  file; uploads of credential stores (`~/.ssh`, `~/.netrc`, `~/.docker`, `~/.config/gh`, Keychains,
  browser data, `~/.coxswain`…) are rated destructive.
