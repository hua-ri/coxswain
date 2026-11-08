# Coxswain M2 — SSH 补全：文件、隧道、批量 设计与实施计划

> 日期：2026-11-08。前置：v0.3.x（server 模型 + 桌面 + 共享终端）。
> **For agentic workers:** TDD per task (failing test first), one commit per task; after every commit `gofmt -l .`
> empty, `go vet ./...`, `go test -race ./...` green, and `npm --prefix web run typecheck && npm --prefix web test
> && npm --prefix web run build` green. No push/tag/remotes; never touch the real `~/.coxswain`, `~/.ssh`, agent
> configs; no real windows/dialogs in tests. Tests must not depend on timing (wait on conditions, not sleeps).

## 原则（沿用）

- 一切能力是 Action：CLI、MCP、桌面三个入口自动可用；MCP 注解如实标注（只读 / 破坏性 / open-world）。
- 有状态的东西（隧道、传输任务、批量任务）只存在 server 里，带 owner；桌面可见、可停；server 停止时全部关闭。
- 给 agent 的输出要小：大内容落盘返回路径，长列表分页、截断走 shaper。
- 审批交给 agent；Coxswain 只做风险标签、审计、首次接入授权。

## 1. 文件（SFTP）

依赖：`github.com/pkg/sftp`（成熟、纯 Go；新增依赖）。连接复用 `sshx.Pool`（SFTP 子系统开在池化连接上，用完归还）。

| Action | 注解 | 说明 |
|---|---|---|
| `file.list` | read-only | `{host, path, all?}` → 名称、类型、大小、权限、修改时间；最多 1000 项，超出截断并提示 |
| `file.stat` | read-only | 单个路径的元数据 |
| `file.read` | read-only | `{host, path, offset?, limit?}` 文本读取；> 1 MiB 拒绝并建议 `file.download`；二进制检测（含 NUL）时拒绝 |
| `file.write` | destructive | `{host, path, content, mode?, create_only?}` 原子写（写临时文件再 rename）；风险标签：受保护路径（复用 `policy.isProtected`）→ destructive，其余 write |
| `file.upload` | destructive | `{host, local_path, remote_path}` 本机 → 远端，带进度，sha256 校验（远端有 `sha256sum` 时） |
| `file.download` | read-only + open-world | `{host, remote_path, local_path?}` → 默认落到 `~/.coxswain/downloads/<host>/<basename>`，返回本地路径、大小、sha256 |

- 上传 / 下载是**传输任务**：`internal/transfer.Manager` 记录 id、方向、主机、路径、已传字节、总字节、状态、owner；事件 `transfer.started/progress(节流 1/s)/finished/failed`；`DELETE /api/v1/transfers/{id}` 取消。Action 同步等待完成（agent 视角是一次调用），桌面实时看到进度。
- 断点续传：下载时目标存在且比远端小，且 `resume=true` → 从已有长度继续；上传同理（远端文件比本地小）。

## 2. 隧道（端口转发）

`internal/tunnel.Manager`：每条隧道有 id、类型（local / remote / socks）、主机、绑定地址、目标、owner、创建时间、连接数、收发字节、状态。用**独立 SSH 连接**（不占池），断线自动重连（指数退避，最多 1 分钟间隔），本地端口保持不变。

| Action | 注解 | 说明 |
|---|---|---|
| `tunnel.list` | read-only | 所有隧道及状态、流量 |
| `tunnel.open` | destructive, open-world | `{host, type, local_port?, remote_host?, remote_port?}`。local：本机 `127.0.0.1:<port>` → 远端 `remote_host:remote_port`；remote：远端 `127.0.0.1:<port>` → 本机目标；socks：本机 SOCKS5 代理。本地端口默认随机，返回实际地址 |
| `tunnel.close` | destructive | agent 只能关自己开的；人可以关任何隧道 |

- 默认只绑定 `127.0.0.1`；`remote` 类型风险标签固定 destructive（把本机暴露给远端）。
- 闲置超时：agent 开的隧道 30 分钟无连接自动关闭（可在参数里设 `idle_minutes`，0 表示不自动关）；人开的不自动关。
- 事件 `tunnel.opened/closed/reconnecting`。

## 3. 批量执行

`ssh.batch` Action：`{hosts?: [names], tag?: "k=v", command, concurrency=5, canary=true, stop_on_failure=true, timeout_sec}`。

- 目标解析：hosts 与 tag 取并集，去重；空集合报错；> 50 台需要 `confirm_count` 参数等于实际台数（防止误选）。
- canary：先在第一台执行，失败（退出码非 0 或连接失败）就停止并返回；成功后按并发度执行其余主机；`stop_on_failure` 时出现失败即停止派发新任务（已在跑的跑完）。
- 结果：每台 `{host, exit_code, stdout(截断), stderr(截断), duration_ms, error?}`，以及汇总 `{total, ok, failed, skipped}`；给 agent 的总输出走 shaper（每台最多 20 行，完整输出在 artifact）。
- 风险标签：`ClassifyCommand(command)`；targets 为全部目标主机（带标签），审计里能看出是否包含 prod。
- 事件：`batch.started/host_done/finished`，桌面显示进度。

## 4. 桌面

- 左侧主机项增加「Files」按钮：文件浏览标签（目录树 / 列表、上下级导航、下载、上传（选择本地文件）、新建目录、删除需确认）。
- 新增「Tunnels」面板（主区固定标签）：列表、流量、状态、关闭按钮、「新建隧道」表单。
- 新增「Transfers」面板：进行中 / 完成的传输，进度条，取消。
- 活动流：批量任务的行可展开看每台主机结果。

## 5. 实施任务

1. **SFTP 基础**：依赖、`sshx` 上开 SFTP 客户端（池化）、`sshtest` 增加 sftp 子系统（`github.com/pkg/sftp` 自带的 server 端，限制在 temp 根目录）；`file.list/stat/read/write` + 风险标签 + 注解；测试覆盖原子写、受保护路径标签、二进制 / 超大拒绝。
2. **传输任务**：`internal/transfer`、`file.upload/download`、进度事件、取消、续传、sha256；API `GET/DELETE /api/v1/transfers`。
3. **隧道**：`internal/tunnel`（local / remote / socks5，自实现最小 SOCKS5：CONNECT、无认证、IPv4/域名/IPv6）、重连、流量统计、闲置关闭、owner 规则；API `GET/POST/DELETE /api/v1/tunnels`；`sshtest` 支持 `tcpip-forward`（remote）与 direct-tcpip（已有）。
4. **批量执行**：`ssh.batch`（并发、canary、stop_on_failure、confirm_count、shaper、事件）。
5. **桌面：文件浏览、传输面板**。
6. **桌面：隧道面板、批量结果展开**。
7. **文档**：README 能力表、SKILL.md（何时用 file.read vs file.download、隧道用完关闭、批量先 canary）、CLI 示例。snapshot 构建通过后停止。

## 6. 验收（与用户一起）

1. 让 Claude 下载 cox-test 上的一个日志并分析（文件落盘、上下文里只有路径与摘要）。
2. 让 Kimi 开一个本地隧道访问 cox-test 上的某个端口，桌面隧道面板出现，人关闭后 agent 再访问失败。
3. 给两台主机打相同标签，让 Codex 批量执行 `uptime`（canary 先跑一台），桌面看到进度和汇总。
4. 人在桌面浏览 cox-test 的文件、上传一个文件，传输面板显示进度。
