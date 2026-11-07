# Coxswain M1a — server 化 实施计划

> **For agentic workers:** execute task-by-task with TDD (failing test first), one commit per task. Spec:
> `docs/specs/2026-11-07-coxswain-server-design.md` (read it fully first). Repo `/Users/king/workspace-test/coxswain`,
> module `github.com/hua-ri/coxswain`, Go 1.26, macOS only. Keep the existing package style (short doc comments,
> small files, table tests). After every task: `gofmt -l .` empty, `go vet ./...`, `go test -race ./...` green.

**Goal:** `cox server` runs in the foreground and owns all state; `cox <cmd>` and `cox mcp` become clients of it over
127.0.0.1 HTTP; Coxswain's own approval system is removed (agents' own permission prompts apply); first-time agent
clients are authorized once; an activity stream (SSE) exposes every action for the coming desktop.

**Safety rules for the implementer:** tests must never touch the real `~/.coxswain`, `~/.ssh` or agent configs (use
`COX_HOME`/`HOME` in temp dirs); never trigger a real macOS dialog (inject fakes; `COX_NO_DIALOG=1` makes the
authorizer deny-by-default unless a test injects an approver); never push, tag or create remotes.

## Target package layout (changes)

| Path | Change |
|---|---|
| `internal/approval/`, `internal/surface/cli/approvals.go` | **delete** |
| `internal/policy/decide.go` (+test) | **delete**; keep `classify*.go` (labels only) |
| `internal/engine/` | simplify: `Invoke(ctx, client action.Actor, id, args) Outcome` → audit `action.requested` (with `risk` label) → Run → `action.finished/failed`; publish every event to a bus |
| `internal/eventlog/` | add `Bus` (fan-out to SSE subscribers, non-blocking, drop on slow subscriber) or make `Log` call an `OnAppend` hook |
| `internal/action/` | add `Annotations{ReadOnly, Destructive, Idempotent, OpenWorld bool}` and `Labeler func(args) (risk Risk, targets []Target)` replacing `Assess`… — see Task 2 |
| `internal/instance/` | **new**: `server.json` write/read/remove, healthz probe, single-instance check |
| `internal/clients/` | **new**: trusted-clients store (`clients.json`, filelock), authorizer with merged in-flight prompts |
| `internal/server/` | **new**: HTTP server: `/healthz`, `/api/v1/*`, `/mcp` (go-sdk `NewStreamableHTTPHandler`), auth middleware |
| `internal/client/` | **new**: Go client for the HTTP API used by CLI (and later desktop) |
| `internal/surface/mcp/` | build the per-session MCP server from the registry with annotations; no approval tools |
| `internal/surface/mcpbridge/` | **new**: stdio ↔ streamable-HTTP bridge for `cox mcp` |
| `internal/surface/cli/` | commands generated from `GET /api/v1/actions`; invocation via `internal/client` |
| `internal/sshx/` | add a connection pool (`Pool.Get(ctx, host) (*Conn, release func())`, idle close after 5 min, drop broken conns) |
| `cmd/cox/main.go` | commands: `server`, `mcp`, `init`, `clients list|revoke`, `version`, plus generated action commands |
| `internal/install/` | MCP command becomes `cox mcp --client <name>`; SKILL.md updated |

## Tasks

### Task 1 — Remove the approval system
Delete `internal/approval`, `internal/confirm` approval paths (keep a generic `confirm.Dialog`-style *yes/no* prompt
helper for client authorization: `confirm.Ask(ctx, title, message string, timeout) (bool, error)` with an injectable
osascript runner; sanitize the message), `policy/decide.go`, `surface/cli/approvals.go`, engine approval code, the
`cox approve/approvals/deny` commands and related tests. Engine becomes `Invoke(ctx, client, id, args)`.
Tests: existing engine/provider/cli/mcp tests updated — a destructive `ssh.exec` by an agent now runs (the remote
command executes) and the `action.requested` event carries `risk:"destructive"`.
Commit: `refactor: remove Coxswain's own approval system`.

### Task 2 — Annotations and labels on actions
`action.Action`: replace `Assess` with `Label func(ctx, args) (Assessment, error)` (same struct: Risk, Targets,
Preview — now purely descriptive) and add `Annotations` (static, per tool). Set: `ssh.hosts.list` and
`artifact.read` ReadOnly+Idempotent; `ssh.hosts.import` Idempotent; `ssh.hosts.tag` Destructive+Idempotent;
`ssh.exec` Destructive+OpenWorld. `ssh.exec` description adds: "Hosts tagged env=prod are production: confirm with
the user before changing them. Respect the host note." and the result includes `host_note` and `host_tags`.
Tests: registry validation (Label required), annotations exposed.
Commit: `feat(action): tool annotations; risk is a label`.

### Task 3 — Event bus
`eventlog.Log` gets `Subscribe() (<-chan Event, cancel func())`; `Append` writes the file then delivers to every
subscriber without blocking (buffered chan 256; on full, drop and count). Tests: subscriber receives events in order;
slow subscriber does not block Append; cancel stops delivery.
Commit: `feat(eventlog): live subscription for the activity stream`.

### Task 4 — Instance file and discovery
`internal/instance`: `type Info{Addr, PID, Version, StartedAt, Token}`; `Write(home, Info)` atomic 0600;
`Read(home) (Info, error)`; `Remove(home, pid)` (only if the file's PID matches); `Probe(ctx, Info) error` (GET
`/healthz`, 1s, verifies pid/version); `ErrNotRunning` with the exact message from the spec.
Tests: round trip and mode; Remove ignores other PIDs; Probe against an `httptest` server; missing file →
ErrNotRunning.
Commit: `feat(instance): server.json discovery`.

### Task 5 — Trusted clients and authorizer
`internal/clients`: `Store` (`clients.json`, `filelock`): `IsTrusted(name)`, `Trust(name)`, `Revoke(name)`, `List()`.
`Authorizer{Store, Ask func(ctx, name) (bool, error)}` with `Authorize(ctx, name) (bool, error)`: `cli`/`desktop`
always true; trusted → true; otherwise one in-flight prompt per name (others wait for its result); approve →
Trust. Name validation `^[a-z0-9][a-z0-9._-]{0,63}$`.
Tests: built-ins; concurrent Authorize for the same name prompts once; deny not persisted; revoke.
Commit: `feat(clients): first-time client authorization`.

### Task 6 — HTTP server
`internal/server`: `New(Config{Engine, Clients *clients.Authorizer, Token, Version, Events}) http.Handler` and
`Run(ctx, Config, home)` (listen 127.0.0.1, reuse previous port from old server.json if free, write instance file,
remove on shutdown, single-instance check via Probe).
Middleware: reject requests with an `Origin` header (403); `/healthz` open; everything else requires
`Authorization: Bearer <token>` (constant-time) and a valid `X-Cox-Client`.
Routes per spec §6. `/api/v1/events` is SSE (`text/event-stream`, one `data:` JSON per event, 15s comment
keep-alive). `POST /api/v1/actions/{id}`: client actor = `{Kind: human if cli/desktop else agent, Name: client}`.
Tests (`httptest`): auth failures (no token, wrong token, Origin set), actions list, invoke ok/error, events stream
receives an event after an invoke, clients list/revoke.
Commit: `feat(server): local HTTP API`.

### Task 7 — MCP over HTTP
`internal/surface/mcp`: `NewServer(e, actor, version)` builds tools from the registry with `Annotations` mapped to
`sdk.ToolAnnotations`; no `cox_approval_wait`; instructions text updated (no approval talk; mention env=prod and
notes). In `internal/server`, mount `/mcp` with `sdk.NewStreamableHTTPHandler(getServer, nil)` where `getServer`
reads `X-Cox-Client` and returns a server whose tool handlers first call `Authorizer.Authorize` (once per session;
cache the result) and return `{status:"error", error:"client_not_authorized", hint:"…"}` when denied.
Tests: in-process HTTP + go-sdk `StreamableClientTransport`: list tools (annotations present), call `ssh_hosts_list`,
unauthorized client gets client_not_authorized and the fake Ask was called once for two concurrent sessions.
Commit: `feat(mcp): serve tools over streamable HTTP`.

### Task 8 — `cox mcp` bridge
`internal/surface/mcpbridge`: `Run(ctx, stdin, stdout, home, client string)`: stdio Connection ↔ lazily created
StreamableClientTransport Connection (custom `http.Client` transport adding the two headers). If the server is not
running when a request arrives, reply with a JSON-RPC error (code -32000, message from spec) and keep serving;
retry connecting on the next request. Notifications while down are dropped.
Tests: bridge against a real in-process server (initialize + tools/call round trip); with no server: initialize gets
the not-running error, then start the server and the next request succeeds.
Commit: `feat(mcp): stdio bridge to the running server`.

### Task 9 — SSH connection pool
`sshx.Pool{Dialer, IdleTimeout}`: `Get(ctx, host) (*Conn, func(broken bool), error)`; key = host name + hostname +
port + user; reuse healthy connections (`SendRequest("keepalive@openssh.com")` check before reuse); close idle after
timeout; `Close()` closes all. `providers/ssh` uses the pool when given one.
Tests with `sshtest`: two sequential execs reuse one TCP connection (count accepts on the test server); broken
connection is replaced; Close closes.
Commit: `feat(sshx): connection pool`.

### Task 10 — CLI as a client; new commands
`internal/client`: `Client{Info, HTTP}` with `Actions(ctx)`, `Invoke(ctx, id, args)`, `Clients(ctx)`,
`Revoke(ctx, name)`, `Events(ctx) (<-chan Event, error)`. `surface/cli` builds action commands from `Actions()`
(same flag/positional/RestArgs rules as now — move that logic over unchanged). `cmd/cox`:
- `cox server [--port N]` — runs `server.Run` in the foreground, logs one line per event to stderr, Ctrl-C shuts down
  cleanly (closes SSH pool, removes server.json).
- `cox mcp --client <name>` — bridge.
- `cox clients list|revoke <name>`; `cox version`; `cox init` (unchanged behaviour; imports via the server if
  running, otherwise directly with the local engine — init must work before the server is started).
- Any action command with no server → stderr message from spec, exit code 2.
Tests: cmd/cox e2e rewritten: start `cox server` as a subprocess (temp HOME/COX_HOME, `--port 0`), wait for
server.json, run CLI commands against it, run `cox mcp --client codex` via go-sdk CommandTransport with a
pre-trusted client, check events file; stop server and check the not-running error and exit code 2.
Commit: `feat(cli): cox server; CLI and MCP are clients`.

### Task 11 — Installer, Skill, docs
`install`: MCP args `mcp --client claude-code|codex|kimi`. SKILL.md: "Coxswain must be running (`cox server` in a
terminal). If tools say the server is not running, ask the user to start it." Remove all approval text; keep rules on
host notes, env=prod, sudo -n, artifacts, no raw ssh, don't edit ~/.coxswain. README: new quick start
(`cox init`, `cox server`, then use your agent), how agents' own permission prompts apply, client authorization and
`cox clients revoke`, threat model (spec §3). Main design doc: add a pointer at the top to the v0.8 addendum.
Commit: `docs: server model`.

### Task 12 — Release v0.2.0
Snapshot build (`HOMEBREW_TAP_DEPLOY_KEY=unused GOSUMDB=sum.golang.org GOTOOLCHAIN=go1.27.3 go run
github.com/goreleaser/goreleaser/v2@v2.20.0 release --snapshot --clean`) must succeed. **Stop here** — the controller
pushes and tags.

## Acceptance (controller, with the user)
`cox server` in a terminal; Claude Code / Codex / Kimi each: first connection pops the authorization dialog once;
list hosts and df on cox-test; a write on cox-test is confirmed by **the agent's own permission prompt** (not by
Coxswain); events appear live in `cox server`'s output; `cox clients revoke codex` cuts Codex off; stopping the server
makes tool calls return "not running".
