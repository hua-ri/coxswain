---
name: cox
description: Operate the user's servers through Coxswain (cox) — list SSH hosts, run commands on one or many of them, browse and transfer files, open tunnels, use shared terminals the user can watch, read large outputs. Use for any server inspection or ops task instead of raw ssh.
---

# Coxswain (cox)

Coxswain holds the user's SSH hosts and connections. Use it for every remote operation.
Never ask the user for passwords or keys, and never use raw `ssh` to reach hosts that
Coxswain manages.

Coxswain must be running: the user starts it with `cox server` in a terminal. If a tool says
"Coxswain server is not running", ask the user to start it, then try again.

## Tools

Prefer the MCP tools when they are available:

| Tool | Use |
|---|---|
| `ssh_hosts_list` | hosts with tags (`env=prod` is production) and notes |
| `ssh_exec` | run one non-interactive command on a host |
| `ssh_batch` | run one command on many hosts (`hosts` and/or `tag`), canary first |
| `file_list` / `file_stat` | list a remote directory / one path's metadata |
| `file_read` | read a small remote text file (up to 1 MiB) into the conversation |
| `file_write` | write a remote text file atomically |
| `file_mkdir` / `file_remove` | create a directory / remove a file or empty directory |
| `file_download` | copy a remote file to this machine (`~/.coxswain/downloads/<host>/`); returns the path, not the content |
| `file_upload` | copy a file from `~/.coxswain/uploads/` to a host |
| `tunnel_open` / `tunnel_list` / `tunnel_close` | port forwards through a host (local, remote, socks) |
| `terminal_list` | shared terminals: yours, the user's and other agents' |
| `terminal_open` | open a shared terminal on a host; it appears in the user's desktop app |
| `terminal_send` | type a command (or text) into a shared terminal |
| `terminal_read` | read the last lines of a shared terminal |
| `terminal_close` | close a terminal you opened |
| `artifact_read` | read more of a truncated output (`grep`, `start`/`end`) |

### `ssh_exec` or a terminal?

- For one-off commands (inspect, check, a quick fix) use `ssh_exec`. It returns the output and exit code.
- Long-running work, or work the user should watch or may want to take over (builds, deploys,
  migrations, following logs): `terminal_open`, then `terminal_send`, then `terminal_read` to see
  what happened. Read again later for slow commands instead of sending them twice.
- Shared terminals are visible to the user, who may be typing in them. Ask before typing into a
  terminal the user opened. Never close the user's terminals; close yours with `terminal_close`
  when you are done.
- `terminal_read` returns plain text, not a screen: use batch modes (`top -b -n 1`), not full-screen
  programs.

### Files: `file_read` or `file_download`?

- `file_read` for small text files you need to see (configs, short logs): the content comes back
  to you. It refuses binary files and files over 1 MiB.
- `file_download` for large or binary files (big logs, dumps, archives): the file lands on this
  machine and you get its path, size and SHA-256. Inspect the local copy with your own tools
  (grep, tail) instead of pulling it all into the conversation. `resume=true` continues a
  cancelled download.
- `file_upload` uploads only files in `~/.coxswain/uploads/` (give `local_path` relative to it):
  copy the file there first. Upload only files the user asked you to; never upload keys, tokens or
  anything under `~/.ssh` or `~/.coxswain`.
- The user sees every transfer in the desktop app and may cancel it. If a transfer fails as
  cancelled, do not restart it on your own; ask.

### Tunnels

- `tunnel_open` returns the address to use (`listen`). Close every tunnel you open with
  `tunnel_close` as soon as you are done; unused ones close themselves after `idle_minutes`
  (default 30).
- A `remote` tunnel exposes a service on the user's machine to the host: ask the user first.
- The user can close any tunnel. If yours disappears, it was closed on purpose: do not reopen it
  without asking.

### Batch runs

- Use `ssh_batch` rather than many `ssh_exec` calls for the same command on several hosts.
- Keep the defaults `canary=true` and `stop_on_failure=true`: the first host runs alone, and a
  failure stops the rest. Prefer a read-only command first to check the target list.
- Above 50 hosts it asks for `confirm_count`: check the list with `ssh_hosts_list`, and set it to
  the real number only if the targets are right. Never guess it.
- Each host's output is shortened to 20 lines; the full output is in `artifact_id`
  (`artifact_read`, one section per host starting `=== <host>: …`).

Without MCP, use the CLI with JSON output:

```bash
cox -o json ssh hosts list
cox -o json ssh exec <host> -- <command>
cox -o json ssh batch --tag role=web -- <command>
cox -o json file list <host> <path>
cox -o json file download <host> <remote-path>
cox -o json tunnel open <host> local --remote-port 5432
cox -o json tunnel close <tunnel-id>
cox -o json terminal open <host>
cox -o json terminal send <terminal-id> <command>
cox -o json terminal read <terminal-id> --lines 50
cox -o json artifact read <artifact-id> --grep ERROR
```

## Rules

1. Look before you act: list hosts, read the host note, inspect state with read-only commands first.
2. A host note can forbid things ("no restarts during business hours"). Follow it; if it blocks the task, tell the user.
3. Hosts tagged `env=prod` are production: confirm with the user before changing anything on them.
4. If a tool says `client_not_authorized`, the user has not allowed you to use Coxswain. Tell them;
   do not try to reach the servers another way.
5. Use `sudo -n`, never a password prompt. Do not start interactive programs or `tail -f` with
   `ssh_exec`; use a shared terminal for those.
6. Outputs over ~200 lines are truncated; read the rest with `artifact_read` instead of re-running.
7. Do not edit files under ~/.coxswain and do not use raw ssh to reach managed hosts.
8. Clean up after yourself: close your tunnels and terminals when the task is done.
