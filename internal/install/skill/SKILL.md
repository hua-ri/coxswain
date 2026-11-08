---
name: cox
description: Operate the user's servers through Coxswain (cox) — list SSH hosts, run commands on them, use shared terminals the user can watch, read large outputs. Use for any server inspection or ops task instead of raw ssh.
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

Without MCP, use the CLI with JSON output:

```bash
cox -o json ssh hosts list
cox -o json ssh exec <host> -- <command>
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
