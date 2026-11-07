---
name: cox
description: Operate the user's servers through Coxswain (cox) — list SSH hosts, run commands on them, read large outputs. Use for any server inspection or ops task instead of raw ssh.
---

# Coxswain (cox)

Coxswain holds the user's SSH hosts and credentials and enforces their approval policy.
Use it for every remote operation. Never ask the user for passwords or keys, and never
use raw `ssh` to reach hosts that Coxswain manages.

## Tools

Prefer the MCP tools when they are available:

| Tool | Use |
|---|---|
| `ssh_hosts_list` | hosts with tags (`env=prod` is production) and notes |
| `ssh_exec` | run one non-interactive command on a host |
| `artifact_read` | read more of a truncated output (`grep`, `start`/`end`) |
| `cox_approval_wait` | wait for the user's decision on a pending action |

Without MCP, use the CLI with JSON output:

```bash
cox -o json ssh hosts list
cox -o json ssh exec <host> -- <command>
cox -o json artifact read <artifact-id> --grep ERROR
cox -o json approvals wait <approval-id>
```

## Rules

1. Look before you act: list hosts, read the host note, inspect state with read-only commands first.
2. A host note can forbid things ("no restarts during business hours"). Follow it; if it blocks the task, tell the user.
3. Changes may need approval. Usually a macOS dialog appears and your call waits. If you get
   `approval_required`, tell the user exactly what you want to run and why, then call
   `cox_approval_wait` (or `cox approvals wait <id>`). Never run `cox approve` yourself.
4. If an action is denied, do not reach the same result another way. Offer a safer alternative or stop.
5. Use `sudo -n`, never a password prompt. Do not start interactive programs or `tail -f`.
6. Outputs over ~200 lines are truncated; read the rest with `artifact_read` instead of re-running.
7. Do not edit files under ~/.coxswain and do not use raw ssh to reach managed hosts.
