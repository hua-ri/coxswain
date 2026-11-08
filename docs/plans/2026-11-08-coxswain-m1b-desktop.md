# Coxswain M1b — `cox desktop` 设计与实施计划

> 日期：2026-11-08。前置：v0.2.0（server 模型，`docs/specs/2026-11-07-coxswain-server-design.md`）。
> **For agentic workers:** TDD per task (failing test first), one commit per task, after every commit `gofmt -l .`
> empty, `go vet ./...`, `go test -race ./...` green, and (from Task 5 on) `npm --prefix web run build` and
> `npm --prefix web run typecheck` green. No push/tag/remotes; never touch the real `~/.coxswain`, `~/.ssh` or agent
> configs; never open real windows or dialogs in tests.

## 1. 目标

像微信开发者工具一样，人和 agent 操作同一个工具、同一份状态：

- `cox desktop` 打开一个原生窗口，连接**已经在运行的** `cox server`（没运行就报错，退出码 2，同 CLI 的提示）。关窗口不影响 server。
- 窗口里能看到：主机列表、**实时活动流**（哪个客户端在哪台主机做了什么、风险标签）、**终端标签页**、客户端授权管理。
- **终端是 server 里的共享会话**：人在桌面开的终端，agent 能读、能输入；agent 通过工具开的终端，会自动出现在桌面的标签页里，人可以直接接手打字。

## 2. 技术选型

| 部分 | 选择 | 理由 |
|---|---|---|
| 窗口 | `github.com/webview/webview_go`（macOS 系统 WKWebView，cgo） | 已验证 arm64/amd64 都能编译；Wails v3 仍是 nightly |
| 前端 | React 19 + TypeScript + Vite，`@xterm/xterm` 6 + `@xterm/addon-fit` | 构建产物 `go:embed` 进二进制 |
| 桌面 ↔ server | **Go 绑定桥**：网页不直接请求 server；Go 侧持 token，网页通过 `webview.Bind` 的函数调用 Go，Go 再调 server HTTP API；SSE 流由 Go 读出后用 `Dispatch + Eval` 推给网页 | server 保持「拒绝任何带 Origin 的请求」，网页拿不到 token |
| 终端传输 | SSE（输出，base64 块）+ POST（输入、resize），仅用标准库 | 本机传输，延迟足够；不加 WebSocket 依赖 |
| 发布 | `CGO_ENABLED=1`，macOS runner 上分别构建 arm64/amd64 后合成 universal；goreleaser before hook 先构建前端；CI 加 setup-node | — |

## 3. Server 端：终端会话（`internal/term`）

- `Manager{Dialer, Events}`；`Session{ID "t_<12hex>", Host, Owner action.Actor, CreatedAt, Cols, Rows, Closed bool, ExitCode *int}`。
- `Open(ctx, host inventory.Host, owner, cols, rows)`：**独立 SSH 连接**（不占连接池），请求 `xterm-256color` pty，起 shell。
- 输出：读 stdout/stderr 合并写入环形缓冲（256 KiB）并广播给订阅者（每订阅者 buffered chan，满了丢最旧的并标记 `gap`）。
- `Subscribe(id) (backlog []byte, ch <-chan Chunk, cancel)`；`Write(id, []byte)`；`Resize(id, cols, rows)`；`Close(id)`；`List()`。
- 远端退出：标记 Closed 和 ExitCode，缓冲保留 10 分钟后回收；server 关闭时关闭全部会话。
- 事件：`terminal.opened`、`terminal.closed`（带 owner、host、id）进入审计和活动流。
- **给 agent 的读取**：`Text(id, lines)` 返回最后 N 行纯文本（去掉 ANSI 控制序列，`\r` 按覆盖处理），经 shaper 截断。M1b 不做完整终端模拟（全屏程序如 top/vim 的读取不保证），在工具描述里说明。

### API（`/api/v1/terminals`）

| 方法 | 路径 | 谁可用 | 说明 |
|---|---|---|---|
| GET | `/api/v1/terminals` | 内置 + 已授权 agent | 列表 |
| POST | `/api/v1/terminals` | 内置 | `{host, cols, rows}` → session（agent 走 action） |
| GET | `/api/v1/terminals/{id}/stream` | 内置 | SSE：`event: data` base64 块（首条为 backlog）、`event: exit {code}`、`event: gap` |
| POST | `/api/v1/terminals/{id}/input` | 内置 | body 原始字节（≤ 64 KiB） |
| POST | `/api/v1/terminals/{id}/resize` | 内置 | `{cols, rows}` |
| DELETE | `/api/v1/terminals/{id}` | 内置 | 关闭 |

### 给 agent 的 Actions

| id | 注解 | 说明 |
|---|---|---|
| `terminal.list` | read-only | 打开的终端：id、host、owner、是否关闭 |
| `terminal.open` | destructive, open-world | `{host}` → id；桌面会立即出现这个标签页 |
| `terminal.send` | destructive, open-world | `{id, text, enter=true}`；风险标签用 `ClassifyCommand(text)`；描述说明：这是人也能看到的共享终端，长时间运行的命令用它，一次性命令用 ssh_exec |
| `terminal.read` | read-only | `{id, lines=100}` → 纯文本（截断规则同 ssh_exec） |
| `terminal.close` | destructive | 只能关闭自己打开的，或人明确允许的（M1b：agent 只能关自己 owner 的） |

## 4. 桌面端（`internal/desktop` + `web/`）

- `cox desktop`：发现 server（不在就报错退出 2）→ 创建 webview 窗口（标题 Coxswain，1280×800，可调整）→ 加载内嵌前端。
- Go 侧 `Bridge`（与 webview 解耦，便于测试）：
  - `api(method, path string, body any) (any, error)`：带 `Authorization` 与 `X-Cox-Client: desktop` 调 server。
  - `subscribe(kind string, id string) (subID string)`：kind = `events` 或 `terminal`；Go 读 SSE，每条用 `window.__cox.emit(subID, payload)` 推给网页（通过 `Dispatch` 回到 UI 线程）。
  - `unsubscribe(subID)`；`input(termID, base64)`、`resize(termID, cols, rows)` 走快速路径。
  - server 断开：推送 `{type:"disconnected"}`，网页显示横幅「server 已停止」，每 2 秒重试发现，恢复后自动重新订阅。
- 前端布局：
  - 左侧栏：主机列表（名字、user@host:port、标签徽章，env=prod 红色；备注悬浮显示），点击「打开终端」；下方「客户端」：已允许 / 已撤销列表，允许、撤销按钮。
  - 主区标签页：固定的「活动」标签 + 终端标签。活动：实时事件表（时间、客户端、动作、主机、风险徽章、耗时 / 结果），可按客户端、主机过滤；点 terminal 事件跳到对应终端。
  - 终端标签：xterm.js，标题「host · 开启者」（agent 开的显示 agent 名和图标）；agent 正在输入时标签上有闪烁点；人随时可以打字；关闭标签 = 关闭会话（agent 的终端关闭前确认）。
  - 状态栏：server 地址、版本、已连接 agent 数。
  - 主题：跟随系统浅色 / 深色。

## 5. 实施任务

1. **term.Manager 核心**（`internal/term`）：Open / Write / Resize / Subscribe（backlog + 实时 + gap）/ Close / List / 退出码 / 10 分钟回收；ANSI 去除 + `Text(lines)`。测试用 `sshtest`——需给 sshtest 增加 `pty-req`、`shell`、`window-change` 支持（用 `/bin/sh -i` 或在测试服务器里起 pty：可用 `github.com/creack/pty`？**不加依赖**：测试服务器对 `shell` 请求运行 `/bin/sh`，无 pty 也能回显 `echo`；记录 window-change 尺寸供断言）。
2. **终端 API**：路由、权限（内置 / 已授权）、SSE 流（backlog、data、exit、gap）、输入大小限制；事件 `terminal.opened/closed`。httptest 测试。
3. **终端 Actions**：terminal.list/open/send/read/close + annotations + 风险标签 + owner 规则；MCP 工具表随之更新（离线工具表一致性测试会覆盖）。e2e：MCP 打开终端 → API 能看到并读到输出 → 人通过 API 输入 → agent `terminal.read` 读到。
4. **桌面 Bridge（Go，无 webview）**：`internal/desktop/bridge.go` 实现 api / subscribe / input / resize / 断线重连，接口 `Window{Eval(js string); Dispatch(func())}` 可注入；用假 Window + 真 server（httptest）测试：订阅事件收到推送、终端订阅收到 backlog 与新数据、server 停止后推 disconnected、恢复后重新订阅。
5. **前端脚手架**：`web/`（Vite + React + TS，`npm run build` 输出到 `internal/desktop/ui/dist`，`npm run typecheck`）；`internal/desktop/ui` 用 `go:embed`，仓库里提交一个占位 `dist/index.html`（无 node 时 `go build` 仍可用，显示「前端未构建」）；`.gitignore` 忽略构建产物但保留占位文件；`window.__cox` 客户端封装（类型化的 api / subscribe）。
6. **前端：主机 + 活动**：左侧主机列表、客户端管理、活动表（过滤、风险徽章）、断线横幅、状态栏。
7. **前端：终端标签**：xterm + fit、输入 / resize 走 bridge、backlog 回放、owner 标识、agent 输入指示、关闭确认、从活动跳转。
8. **`cox desktop` 命令**：webview 窗口、加载内嵌 UI、绑定函数、server 不在时退出 2；窗口关闭只退出桌面进程。`cmd/cox` 测试只覆盖「server 不在 → 退出 2」（不开真窗口）。
9. **构建与发布**：goreleaser `CGO_ENABLED=1`、before hook `npm --prefix web ci && npm --prefix web run build`；CI 加 setup-node 并跑 typecheck + build；README「Desktop」一节；SKILL.md 增加终端工具说明（一次性命令用 ssh_exec，长时间 / 需要人看着的用 terminal）。本地 snapshot 构建通过后**停止**。

## 6. 验收（与用户一起）

1. 终端里 `cox server`，另开 `cox desktop`：看到主机列表与活动流。
2. 在桌面给 cox-test 开终端，执行 `uptime`；在 Claude Code 里让它「读一下我在 cox-test 打开的终端最后几行」→ 读到。
3. 让 Codex「在 cox-test 开一个终端跑 `top -b -n 3`」→ 桌面自动出现 Codex 的终端标签，人能在里面继续打字。
4. 活动流实时显示三家 agent 的调用；撤销 / 允许按钮生效。
5. 关闭桌面窗口，server 仍在；再次 `cox desktop` 恢复。
