# Coxswain v0.8 设计补充：server 为核心，桌面 / CLI / MCP 都是入口

> 日期：2026-11-07。本文**取代**主设计文档（`2026-11-07-coxswain-design.md`）中的 §4.0 进程模型、§4.3 模式 B（ACP 驱动 agent）和 §4.4 审批部分；其余章节仍然有效。
> 参照：微信开发者工具的「一个本地 server，多个控制入口；人和 agent 操作同一个工具」形态（逆向笔记在仓库外，未公开）。

## 1. 形态

```
终端：cox server                       ← 前台运行，持有全部状态；Ctrl-C / 关终端即停
        │  127.0.0.1:<port>
        ├─ /mcp            ← agent（Claude Code / Codex / Kimi）经 `cox mcp` 桥接
        ├─ /api/v1/...     ← `cox <命令>` 与桌面界面
        └─ /api/v1/events  ← 活动流（SSE）：谁、在哪台主机、做了什么
cox desktop                            ← 打开桌面界面（M1b），连同一个 server；关窗口不影响 server
```

- **Coxswain 自身没有 agent 能力**：不内置聊天、不调用模型、不驱动 agent。它是一个好用的运维工具，人可以直接用，agent 也可以直接用，两者操作的是同一份状态（连接、终端、隧道）。
- **server 是唯一的状态持有者**：SSH 连接池、后续的终端会话、隧道、K8s 客户端都只存在于 server 进程里。
- **不做系统级后台守护**：server 由用户在终端里启动，前台运行；退出即全部停止。
- **客户端不自动拉起 server**：`cox <命令>`、`cox mcp`、`cox desktop` 连不上时直接报错：`Coxswain server is not running. Start it with: cox server`（退出码 2）。

## 2. 实例发现

- server 启动时监听 `127.0.0.1`（端口优先复用上次的，空闲则随机），原子写入 `~/.coxswain/server.json`（0600）：`{addr, pid, version, started_at, token}`；正常退出时删除。
- 客户端读 `server.json` → `GET /healthz`（无鉴权，1s 超时）确认存活；失败即报上面的错误（陈旧文件不删，下次 server 启动覆盖）。
- 单实例：server 启动时若 `server.json` 指向的进程仍存活且 `/healthz` 正常，则拒绝启动并提示已有实例。

## 3. 鉴权与客户端授权

- 每次启动随机生成 `token`（32 字节 base64url），写在 `server.json` 里；所有 `/api/v1/*` 与 `/mcp` 请求带 `Authorization: Bearer <token>`，常量时间比较。`/healthz` 免鉴权。只绑 127.0.0.1，不开 CORS，拒绝带 `Origin` 头的浏览器请求。
- **客户端身份**：请求头 `X-Cox-Client: <name>`（`claude-code` / `codex` / `kimi` / `cli` / `desktop` / 其他）。`cli` 与 `desktop` 是本机用户自己的入口，免授权。
- **首次接入授权**：其他客户端名第一次连接（MCP initialize）时，server 弹一次 macOS 对话框「<name> 想要使用 Coxswain 操作你的服务器」，允许后写入 `~/.coxswain/clients.json`（`{name, authorized_at}`）。拒绝或超时（60s）则该会话所有工具调用返回 `client_not_authorized`。同名并发请求合并为一次弹窗。
- 撤销：`cox clients list` / `cox clients revoke <name>`，撤销立即断开该客户端的会话。
- **威胁模型（明确写进 README）**：保护的是「agent 通过 Coxswain 自身入口」的使用。同一用户下的任意进程都能读 `server.json`、自报客户端名，Coxswain 不是隔离边界；凭据仍只在 server 进程和 ssh-agent 里，agent 拿不到。

## 4. 审批：交给 agent 自己

- **删除** Coxswain 自有的审批体系：approval 存储、`cox approve/approvals/deny`、每次调用的确认弹窗、按 `env=prod` 拦截的 policy 决策。
- 每个工具在 MCP 里如实标注 annotations：`readOnlyHint`（hosts.list、artifact.read 为 true）、`destructiveHint`（ssh.exec、hosts.tag 为 true）、`idempotentHint`、`openWorldHint`。Claude Code / Codex / Kimi 按各自的权限设置决定是否询问用户。
- 命令分类器（`policy.ClassifyCommand`）保留，但**只用于标注**：写进审计日志和活动流的 `risk` 字段，桌面里显示为徽标；不拦截。
- 主机标签与备注保留：`ssh_hosts_list` 返回它们，`ssh_exec` 的结果附带 `host_note`；工具描述提醒 agent「带 env=prod 的主机做修改前先和用户确认」。

## 5. 一份注册表，三种入口

- `action.Registry` 仍是唯一的工具清单。server 用它生成 MCP 工具（含 annotations）和 `/api/v1/actions` 列表；CLI 启动时从 server 拉取列表并生成命令树（server 没开时只显示本地命令：server、mcp、init、clients、version、help）。
- 调用路径统一为 `engine.Invoke(ctx, client, id, args)` → 审计 → Run → 活动流。
- 输出塑形不变：给 agent 的输出截断 + artifact；`cli`/`desktop` 客户端不截断。

## 6. HTTP API（v1，供 CLI 与桌面）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | `{version, pid}`，免鉴权 |
| GET | `/api/v1/actions` | 工具列表：id、summary、description、input schema、positional、rest_args、annotations |
| POST | `/api/v1/actions/{id}` | body 为参数 JSON；返回 `{status:"ok"|"error", result, error}` |
| GET | `/api/v1/events` | SSE 活动流：每条审计事件实时推送 |
| GET | `/api/v1/clients` | 已授权客户端 |
| DELETE | `/api/v1/clients/{name}` | 撤销 |

## 7. `cox mcp` 桥

- `cox mcp --client <name>`：stdio ↔ Streamable HTTP 的消息级桥（go-sdk `StdioTransport` 与 `StreamableClientTransport` 两个 Connection 之间转发 JSON-RPC 消息），附加 `Authorization` 与 `X-Cox-Client` 头。
- 启动时 server 不在也照常运行；收到的每个请求若无法连上 server，返回 JSON-RPC 错误 `Coxswain server is not running. Start it with: cox server`，而不是退出（避免 agent 把它标记成坏掉的 MCP server）。server 之后启动，下一个请求自动连上。

## 8. 不变的部分

SSH 能力（ProxyJump、known_hosts、ssh-agent）、`~/.ssh/config` 导入、主机标签与备注、审计日志、artifact、`cox init`（MCP 命令改为 `cox mcp --client <name>`）、GitHub/Homebrew 发布、仅 macOS、个人使用。

## 9. 里程碑调整

| 阶段 | 内容 |
|---|---|
| **M1a server 化**（本次） | `cox server`、实例发现、token、客户端授权、HTTP API、活动流、MCP over HTTP + `cox mcp` 桥、CLI 改为客户端、删除自有审批、SSH 连接池；发布 v0.2.0 |
| **M1b 桌面** | `cox desktop`（Wails）：主机列表、活动流、终端标签页（agent 开的终端可接手）、客户端授权管理 |
| 之后 | 按原计划：SSH 补全（SFTP、转发、批量）→ K8s → Argo → 指标 → 扩展 |
