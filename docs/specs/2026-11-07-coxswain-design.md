# Coxswain（`cox`）：AI 原生运维工具箱 —— 需求梳理与方案设计

> 版本：v0.7（评审稿）　日期：2026-11-07
> v0.2 变更：**放弃 DSH 插件路线**，`dsh-ssh` 代码不复用；DSH 只作为**设计参考**（见 §8）。UI 改为自建。
> v0.3 变更：加入 Termark 1.7.7 静态逆向结论（§3.1），据此修改审批（§4.4）、指标（§4.6）、Agent 接入（§4.3）。原始逆向报告为本地私有笔记，不随仓库公开。
> v0.4 变更：**opsd 不常驻**，生命周期跟随桌面应用（§4.0）；UI 载体因此定为桌面壳（Tauri + Go sidecar）。
> v0.5 变更：**产品形态是一个 CLI**，通过 GitHub 分发；装好即可直接用 CLI，也可以由 CLI 启动桌面（主流程）。桌面改为同一个 Go 二进制内嵌 WebView（Wails），取代 Tauri（§4.0、§6、§11）。
> v0.6 变更：定名 **Coxswain**，命令 `cox`（§0.1）；GitHub **公开仓库**；**只做 macOS**（CLI 与桌面）；**面向个人用户**，不做团队功能。
> v0.7 变更：**AI agent 辅助是核心**，桌面 AI 工作台与“交给 AI”提前到 M1（§4.3、§4.3.1、§9）；领域范围定为 **SSH + K8s + Argo CD + Argo Workflows**（§4.6）；仓库放在个人账号 `hua-ri`。
> 本文定位：从零建设一个「**Agent 无关、可持续扩展的运维能力平台**」，借鉴 DSH 与业界工具的设计。

---

## 0. 结论先行

1. **定位**：不依附任何 agent 宿主。同时服务 Claude Code、Codex、Kimi Code 等多个 agent，运维能力**不挂在任何一个 agent 的进程里**，由自己的 CLI 核心持有，桌面 UI 也自建。
2. **一个核心、四种接口、两个方向**：
   - **一个核心**：一个 Go 二进制 `cox`（项目名 Coxswain），**就是产品本身**。CLI 子命令直接可用；`cox desktop` 打开桌面窗口（主流程）。没有任何常驻进程：谁启动、谁持有，进程退出即一切停止。连接池、隧道、凭据、采集器、审批、审计都在这个核心库里，CLI、MCP、桌面三种运行方式共用。
   - **四种接口**：能力以 **Action** 为单位只注册一次，自动生成 **CLI / MCP / HTTP+WS(UI) / Skill** 四种接口。新工具接进来，四个入口同时可用。
   - **两个方向**：一是 **Agent 调工具**（MCP / CLI，三家 agent 都能直接用）；二是 **工具调 Agent**（opsd 作为 ACP 客户端，在 UI 里“交给 AI”，把上下文一并带给指定的 agent）。
3. **安全闸门放在工具侧**：三家 agent 的权限模型各不相同（例如 codex-acp 默认就是自动审批），**不能依赖 agent 自身的权限确认**。风险分级、审批、脱敏、审计全部在 opsd 内统一执行。
4. **扩展分三档**：声明式 YAML 工具集（分钟级接入）→ 外部进程或 MCP 挂载（复用现成的 MCP 服务和内部 CLI）→ 原生 Go Provider（有状态的重能力）。
5. **技术栈与分发**：**Go 单二进制**，GitHub Releases + Homebrew tap + 安装脚本分发，`cox update` 自更新；桌面用 **Wails（系统 WebView）+ React + xterm.js**，前端资源编进同一个二进制，`cox desktop` 直接开窗口（§4.0、§6）。

### 0.1 名字：Coxswain（`cox`）

**Coxswain（舵手 / 艇长）**：赛艇上唯一不划桨的人，负责掌舵、喊节奏、协调所有桨手。对应本产品的角色：**它自己不是 agent，而是指挥 Claude Code、Codex、Kimi 这些“桨手”协同干活，并掌着方向盘（审批）**。

- 命令 `cox`：3 个字母，好敲；macOS 自带命令和 Homebrew formula/cask 中均无同名（2026-11-07 查证）。
- 仓库 `hua-ri/coxswain`；`brew install hua-ri/tap/coxswain`，安装出的命令是 `cox`；配置目录 `~/.coxswain/`；MCP server 名 `cox`。
- GitHub 上有几个同名的小项目（几十 star，赛艇、Ruby 等领域，与运维无关），不构成冲突。
- 备选名：`tug`（拖船，引导大船进港）、`wharf`（码头）。

### 0.2 产品的两条腿

1. **集成**：把 SSH、K8s、Argo CD、Argo Workflows 等运维能力收拢到一个工具里，有统一的资产、凭据、审批和审计。
2. **AI 辅助（核心，效率提升的关键）**：每一个能力从第一天起就同时给人和 agent 用；桌面里每个对象都能“交给 AI”；人负责判断和审批，agent 负责查、比对、执行。

**设计约束**：任何新能力上线时都必须同时满足三条：有 CLI、有 MCP 工具（含风险等级和面向 LLM 的描述）、在桌面里有“交给 AI”入口。缺一条不算完成。

---

## 1. 现状盘点

| 资产 | 处理 | 说明 |
|---|---|---|
| `dsh-ssh`（DSH 插件） | **废弃，代码不复用** | 质量不可用。只保留教训：UI 受宿主限制（无 xterm.js、无 fetch）会严重拖累体验，这正是要自建 UI 的原因 |
| `kubessh` v2~v5 设计稿 | 部分参考 | 读写分类、Action 目录（附录 A）可以参考；以 DSH 为中心的部分作废 |
| 本机 skills 生态 | **首批外部工具** | 本机已有的一批运维 skill（K8s 资源查询、云资源、统一认证、CI/CD 故障诊断等）已经是 **“CLI + SKILL.md”** 模式，可直接接入 |

**判断**：**CLI + Skill** 这个形态已被证明好用。缺的是**中间那层“共享的、有状态的、受控的”运维内核**，以及一个不受宿主限制的 UI。

---

## 2. 场景梳理

### 2.1 三类参与者

| 参与者 | 入口 | 诉求 |
|---|---|---|
| **人** | 自建 UI（Web / 桌面壳）、终端 CLI | 快、可视、随时接管、看得见 AI 在干什么 |
| **交互式 Agent** | Claude Code / Codex / Kimi Code（及任何支持 MCP / ACP 的 agent） | 能用工具、拿到干净的上下文、不被大输出淹没 |
| **自动化** | 定时任务、告警、K8s Warning 事件 | 无人值守时**只读**排查并出报告，修复必须由人批准 |

### 2.2 核心场景

| # | 场景 | 典型链路 | 引出的需求 |
|---|---|---|---|
| S1 | **故障排查**（最高频、最有 AI 价值） | 服务 5xx → Pod events/logs → 所在 Node → SSH 到节点看 dmesg/磁盘/负载 → 结论 + 证据 | 跨域关联（Pod→Node→Host）、默认只读、证据打包 |
| S2 | **访问内网资源** | 经跳板 `-L` 访问 DB/Prometheus/内部控制台；agent 需要查内网 HTTP 接口 | 隧道由 opsd 托管（应用打开期间）；agent 可**申请**一条隧道并拿到本地地址 |
| S3 | **批量变更** | 20 台机器改配置、重启 → 计划 → dry-run → 金丝雀 → 全量 → 验证 → 回滚 | 审批、分批、失败即停、回滚预案 |
| S4 | **文件传输** | 拉远程日志给 agent 分析；上传配置或二进制 | 大文件**不能经过 LLM 上下文**、断点续传、校验 |
| S5 | **机器指标** | 已连接机器的 CPU/内存/磁盘/网络/负载、趋势、异常 | 远端零安装、低开销、有历史、给 agent 的是**摘要**而不是原始序列 |
| S6 | **K8s 日常** | get/describe/logs/exec/scale/restart/rollback/编辑 YAML | 多集群/多 context、写操作先 diff、Secret 脱敏 |
| S7 | **复盘交接** | 自动生成事件时间线：谁、何时、做了什么、看到什么 | 统一事件日志（人和 agent 的操作都记录） |
| S8 | **接入新工具** | 内部 CLI（IaC 工单、CI 流水线等）、HTTP API、现成 MCP 服务 | 低成本接入，自动获得 CLI/MCP/UI/Skill 与审批审计 |

### 2.3 挖掘出的隐性需求（这部分最关键）

| # | 需求 | 不做会怎样 |
|---|---|---|
| N1 | **凭据不出 opsd**：agent 只拿到主机别名，拿不到密码、私钥、token | 凭据进入模型上下文 / 会话日志 / 第三方云端 |
| N2 | **审批闸门在工具侧统一实现** | 三家 agent 的权限语义不一致，任何一家配置松了就会失守 |
| N3 | **三维风险判定**：动作风险 × 目标环境（prod/staging 标签）× 命令内容（`rm -rf`、`kubectl delete ns`） | 只按工具粒度判断太粗：`ssh_exec` 既能 `df` 也能 `reboot` |
| N4 | **有状态资源的生命周期独立于 agent 会话**（隧道、tail、采集器、终端），并且有 owner 和 TTL | agent 会话结束后隧道或资源泄漏，或者人和 agent 互相踩踏 |
| N5 | **面向 LLM 塑形输出**：结构化、截断、分页；大输出存为 **artifact 句柄**，按需 head/tail/grep | token 爆炸、上下文被日志冲掉 |
| N6 | **人机共享上下文**：UI 中选中的主机/Pod/一段终端输出可以“一键交给 AI”；AI 的操作在 UI 中实时可见、可打断 | 人和 AI 各干各的，来回复制粘贴 |
| N7 | **审计与可回放**：actor（人 / 哪个 agent / 哪个会话）、参数、审批人、结果 | 出事无法追溯；无法生成复盘 |
| N8 | **多 agent 并存**：同时开 Claude Code 和 Codex；写操作对同一目标要加锁 | 两个 agent 同时重启同一个服务 |
| N9 | **资产拓扑**：Host / Cluster / Node / Namespace 之间的关联，带标签 | agent 不知道 Pod 所在节点对应哪台 SSH 主机 |
| N10 | **企业认证接入**：统一认证 / SSO / 跳板机链 | 内部工具接不进来 |
| N11 | **知识沉淀**：runbook 以 Skill 形式随工具集分发，三家 agent 共用 | 每个 agent 各写一套提示词 |
| N12 | **急停开关**：一键冻结所有 agent 的写操作、关闭 agent 申请的隧道 | 失控时只能杀进程 |

---

## 3. 业界参考：借鉴什么

| 参考 | 借鉴点 | 落到本方案 |
|---|---|---|
| **DeepSeek Harness** | 插件化内核、资源随插件生命周期回收、分层配置、预设模式、追加写会话日志、ACP + MCP、编排子 agent | 详见 §8，是本方案最主要的架构参考 |
| **MCP** | 工具标准协议，三家 agent 都支持 stdio / streamable HTTP | 主要的 Agent→工具 通道 |
| **ACP**（Agent Client Protocol） | 编辑器驱动 agent 的标准协议：`kimi acp` 原生支持；Claude Code、Codex 通过 `@agentclientprotocol/claude-agent-acp` / `codex-acp` 适配 | 工具→Agent 通道（Agent Runner） |
| **Agent Skills / AGENTS.md** | 渐进披露的知识包 | 每个工具集附带 runbook skill |
| **HolmesGPT**（CNCF） | **YAML 工具集**（命令模板 + 参数）、HTTP connector、默认只读、写能力单独开启 | 声明式工具集 T1（§6）、默认只读 |
| **k8sgpt** | Analyzer：规则化地先筛问题，再交给 LLM 解释 | `k8s.diagnose` 先跑规则分析，再给证据包 |
| **kubectl-ai / kubernetes-mcp-server** | 自然语言 → kubectl；读写开关 | K8s Action 设计参照；也可以作为 T2 外部 MCP 挂载 |
| **Teleport** | 访问面统一、会话录制、Access Request（按需申请、限时审批） | 审批绑定参数哈希与 TTL；会话录制 |
| **Ansible** | Inventory + 标签分组、check mode、幂等 | 资产模型、dry-run、批量执行分批 |
| **Rundeck / StackStorm** | Runbook、事件→规则→动作 | 事件驱动模式 C（§4.3） |
| **k9s** | 资源导航 / XRay、YAML 插件 | UI 交互与资源关联视图 |
| **Warp / Termius / Termark** | 终端里的 Agent 审批卡片、命令块、资产体验 | UI 交互范式 |
| **VS Code / Backstage** | Contribution Points：插件声明自己贡献了什么 | 工具集 manifest 声明 Action / UI / Skill |


### 3.1 Termark 逆向结论（1.7.7，静态分析，证据为本地调研笔记，未公开）

**它是什么**：Electron 外壳 + Go 后端 `termark-server`（单二进制，同时是服务端和 CLI）。后端只监听 `127.0.0.1` 的随机端口，每次启动生成随机 Bearer token；渲染层直接用 HTTP/WS/SSE 访问后端，约 150 条 REST 路由。存储用 bbolt + 记录级 AES-GCM，数据密钥放在系统钥匙串。Agent 循环在 Go 后端。**没有 MCP，没有 ACP，没有 K8s，没有插件系统**；外部 agent 只能用 CLI + Skill 接入。

| Termark 的做法 | 评价 | coxswain 怎么用 |
|---|---|---|
| **四档审批**：`always` / `balanced`（默认）/ `auto_review` / `auto`，可按主机覆盖；路由结果 `none` / `user` / `auto_review` | 成熟，直接借用 | §4.4 改为“四档 × 环境”的策略模型 |
| **三层风险判定**：模型自报（写类工具强制填 `explanation` + `confirmation`）+ `danger.go` 规则（shell 分词、剥离 sudo/env、rm -r 越界、mkfs/dd、`drop database`、`compose down -v`、写受保护路径）+ 敏感路径读写 | 规则清单很实用 | 规则清单作为初始规则集；coxswain 更严格：**无法解析一律 ask**；Action.Risk 由工具声明，不信任模型自报 |
| **独立 AI 审查员**：可单独配置便宜模型，输出严格 JSON `{risk_level, user_authorization, outcome: allow/ask_user/deny/revise, rationale}`；`revise` 让主模型改写命令以脱敏；超时或失败转人工 | 很好的中间档 | 新增 `auto_review` 档；审查结果原样写入审计事件 |
| **拒绝回填文本**：“不要通过变通、间接执行或绕过策略达成同一目的” | 有效的防绕过措施 | 审批被拒时返回给 agent 的标准文本 |
| **主机备注作为提示词硬约束**（`[Current host context]`，要解除限制必须先改备注） | 便宜、用户友好 | Inventory 增加 `ai_note` 字段，在 MCP 工具描述或 Context Pack 中注入；**只作提示，硬拦截仍靠 policy** |
| **渐进披露**：snippet、skill 先注入 CSV 索引，按 id 再读全文；读 `AGENTS.md` | 和我们的思路一致 | `cox_find_actions` / runbook 索引沿用这种格式 |
| **两级上下文压缩**：确定性分组移除 + 语义 checkpoint（固定标题：Goal / Constraints / Key Decisions / Progress / …） | 可借鉴 | 长时间的 Agent Runner 会话和复盘报告使用这个模板 |
| **工具执行状态机 + WS 控制协议**：客户端 `prompt / send_now / tool_decision / compact / cancel / retry_from_message`；服务端 `assistant_text / reasoning / tool_execution / usage（含缓存命中）/ retrying` | UI 协议参考 | §4.3 模式 B 中 UI ↔ Agent Runner 的消息协议参照它设计（ACP 事件映射到这套 UI 状态） |
| **审批卡片 UX**：Enter 批准、Esc 拒绝、“同一命令本会话不再询问”、等待时发系统通知 | 直接借用 | M1 AI 面板 |
| **三种命令执行通道**：后台通道 / 写入可见终端 / 受限 shell；写入可见终端会污染 history，所以提供一键清理 | 有启发 | agent 默认走后台 exec 通道；“在我的终端里执行”作为显式选项，人能看到 |
| **终端 → 全局控制台**：单机对话可以“转到控制台继续”，用 `@主机` / `@分组` 扩展到多机 | 很好的交互 | “交给 AI”支持从单目标升级为多目标 |
| **终端上下文**：只附带“最近 50 行 + 会话 id”，或用户选中后“分析选区” | 克制，token 友好 | Context Pack 默认附带 artifact 句柄 + 最近 N 行 |
| **指标采集**：每次请求在已有 SSH 连接上执行一段脚本，用 NUL 分段，脚本内 `sleep 1` 双采样 `/proc/stat` 和 `/proc/net/dev`，可选 nvidia-smi；不落库 | 脚本细节可复用 | 复用脚本结构；coxswain 额外提供持续采集 + 历史存储 + 异常摘要（§4.6） |
| **运维片段**：进程、网络、systemd、docker 都用 POSIX 片段，`sudo -n` 绝不交互，逐级降级（`ss → lsof → netstat`、BusyBox 回退） | 直接借用原则 | 内置 `host-toolbox` 工具集照此编写 |
| **外部 agent 接入**：`termark` CLI（`--json` 是稳定契约）；`instance.json` 保存 `{addr, token}`（原子写 + 0600）；一键把 SKILL.md 写进 `~/.claude`、`~/.codex`、`~/.agents`、`~/.config/opencode` 的 skills 目录；Skill 里写明“不要向用户索要凭据” | 正好验证了我们模式 A 中 CLI + Skill 这条路 | `cox skills install` 照做；`cox mcp` / CLI 通过 `instance.json` 发现 opsd |
| **进程模型**：`--desktop-stdin`，stdin 关闭即退出，后端跟随 Electron 生命周期 | **采用（形式不同）** | 不拆成两个进程：桌面就是 `cox desktop` 进程本身，关窗即退出（§4.0） |
| **中间件链**：auth → 恢复模式 → 应用锁 → 需要重启；Desktop-Trust token 只给主进程 | 可借鉴 | opsd 的 HTTP 层分两种 token：UI token 与 agent token（不同 actor，不同权限） |
| **资产导入**：ssh_config、known_hosts、SecureCRT、Xshell、MobaXterm、PuTTY、Termius；`termark://connect` 与 Xshell/PuTTY 启动参数兼容（给堡垒机唤起用） | 用户迁移成本低 | 导入器作为 P1；堡垒机唤起兼容视需求而定 |
| **终端里的 Agent 感知**：识别 OSC 9;4 / 777 通知，显示“Agent 正在执行任务” | 小而巧 | 在 coxswain 终端里跑 claude/codex 时同样识别 |

**Termark 的空白 = coxswain 的差异化**：① MCP/ACP，让任何 agent 都能用、也能被编排；② K8s；③ 策略按环境（prod 等标签）分级，而不只是按主机；④ 资源 owner/TTL 和统一审计；⑤ 可扩展的工具集（YAML / MCP 挂载）；⑥ 指标历史与异常摘要。

---

## 4. 总体架构

> 下文的 **opsd** 指“当前持有状态的那个 cox 进程”：桌面开着时是桌面进程，否则是 CLI 或 MCP 进程自身。

```
┌──────────────────────────────── 入口层 ─────────────────────────────────┐
│  桌面（cox desktop）   CLI `cox`   MCP（cox mcp）   Skills 导出          │
└──────────────┬─────────────────────┬─────────────┬──────────────┬────────┘
               │ HTTP + WebSocket    │ 本地 RPC     │ stdio / HTTP │ 文件
┌──────────────▼─────────────────────▼─────────────▼──────────────▼────────┐
│     cox 核心库（桌面进程 / CLI 进程 / MCP 进程 共用，谁启动谁持有）          │
│  ┌───────────────┐ ┌──────────────────┐ ┌─────────────┐ ┌──────────────┐ │
│  │Action Registry│→│Policy & Approval │→│  Executor   │→│ Output Shaper│ │
│  │ schema/风险/   │ │ 风险×环境×命令    │ │ 锁/超时/取消 │ │ 截断/脱敏/    │ │
│  │ 目标解析       │ │ allow/ask/deny   │ │             │ │ artifact     │ │
│  └───────────────┘ └──────────────────┘ └─────────────┘ └──────────────┘ │
│  Resource Manager（隧道/终端/tail/采集器：owner + TTL）                    │
│  Inventory（主机/集群/标签/拓扑）   Vault（凭据）   Event Log（审计/回放）  │
│  Agent Runner（ACP 客户端：claude-agent-acp / codex-acp / kimi acp）     │
├──────────────────────────────── Providers ───────────────────────────────┤
│ [原生] ssh · sftp · forward · metrics · k8s · local                      │
│ [声明式 YAML] nginx-check · jvm-dump · 内部 IaC CLI 包装 · HTTP API …       │
│ [外部挂载] 任意 MCP 服务 / JSON-RPC 进程（kubernetes-mcp, grafana-mcp…）  │
└──────────────────────────────────────────────────────────────────────────┘
```

### 4.0 产品形态、运行模式与生命周期（**一个 CLI，不常驻**）

**原则**：装好就是一个命令行工具；桌面是这个命令行启动出来的窗口；**谁启动谁持有，进程退出即一切停止**，没有后台守护进程。

**用户主流程**：

```bash
brew install hua-ri/tap/coxswain          # 或 curl -fsSL https://github.com/hua-ri/coxswain/releases/latest/download/install.sh | sh
cox init                            # 导入 ~/.ssh/config、发现 kubeconfig、可选：把 MCP/Skill 装进 Claude Code/Codex/Kimi
cox desktop                         # 主流程：打开桌面窗口（资产、终端、文件、转发、K8s、指标、AI）
cox ssh exec prod-web-01 -- df -h   # 也可以不开桌面，直接用 CLI
```

**同一个二进制，三种运行方式**（核心库完全相同）：

| 运行方式 | 怎么启动 | 进程寿命 | 有状态资源（隧道、采集、终端） | 审批在哪里 |
|---|---|---|---|---|
| **桌面** `cox desktop` | 用户 | **窗口关闭即退出** | 由桌面进程持有，窗口关闭时有序关闭 | 桌面审批卡片 + 系统通知 |
| **CLI** `cox <cmd>` | 用户 / 脚本 / agent | 单次命令；长任务（`cox forward -L`、`cox metrics watch`、`cox k8s logs -f`）在前台运行，Ctrl-C 结束，和 `ssh -L` 一样 | 只在该命令运行期间存在 | 交互终端里直接 y/N 确认；非交互时返回 `approval_required` |
| **MCP** `cox mcp` | agent（Claude Code 等）按需拉起 | **跟随 agent 会话**，agent 退出它就退出 | 只在该 agent 会话期间存在（加 TTL 兜底） | 见下方“审批的跨进程通道” |

**桌面开着时自动汇合**：`cox desktop` 启动后，在本机随机端口监听，并原子写入 `~/.coxswain/instance.json`（`{addr, token, pid}`，0600，退出时删除）。之后再运行的 `cox <cmd>` / `cox mcp` 检测到它，就**转发给桌面进程执行**，好处有三个：
- 共享连接、锁和隧道（N8）；
- agent 的操作实时显示在桌面里，审批在桌面完成（N6）；
- 审计统一写入一处。

桌面没开时，CLI 和 MCP **在自己的进程内直接执行**，功能不打折，只是看不到 UI。

**跨进程共享的只有磁盘上的状态**：资产清单、凭据（钥匙串 / 加密文件）、策略、审计日志都在 `~/.coxswain/`，SQLite WAL 处理多进程并发。**不存在“必须先启动某个服务”的前提。**

**审批的跨进程通道**（桌面没开时也能工作）：
1. 如果 MCP 客户端支持 elicitation，直接在 agent 界面弹确认；
2. 否则返回 `{"status":"approval_required","approval_id":"ap_x","preview":…}`，审批请求写入 `~/.coxswain/approvals/`；用户在任意终端运行 `cox approve ap_x`（或 `cox approvals` 交互式列表），也可以打开桌面去批；
3. agent 调用 `cox_wait_approval(ap_x)` 等待；审批绑定参数哈希、一次性、带 TTL。

**桌面窗口的关闭流程**（有序）：
1. 如果有正在执行的写操作、待审批项、agent 申请的隧道或进行中的传输，先列出清单请用户确认；
2. 拒绝桌面持有的待审批项，回填“Coxswain 桌面已关闭”；取消运行中的 Action；
3. 关闭隧道、终端、采集器和连接；
4. 刷盘审计日志，删除 `instance.json`。已转发到桌面执行的 CLI/MCP 调用收到明确错误，之后自动回落到进程内执行。

**配套取舍**：
- 资源 owner/TTL 保留，作用于持有它的那个进程的寿命内。
- 指标历史只覆盖有进程在采集的时段，图表把缺口标成“未采集”，不做插值。
- 事件驱动（模式 C）只在桌面开着时生效，不做无人值守巡检。
- 隧道不会“在后台续命”；桌面下次打开时可以一键恢复上次的隧道列表（只恢复配置，不自动连接）。

**分发与升级**：
- **只支持 macOS**（13+）。GitHub Actions 在 macOS runner 上用 GoReleaser 构建 darwin-arm64 / darwin-amd64，合并为 universal 二进制，产出 tar.gz、`checksums.txt`、签名（cosign 或 minisign）。
- 安装渠道（公开仓库，无需 token）：Homebrew tap `hua-ri/homebrew-tap`（主推，GoReleaser 自动更新 formula）、`install.sh`（校验 checksum）、直接下载 Release 资产。
- `cox update`：通过公开的 GitHub Releases API 检查新版本（匿名访问即可），校验签名后原子替换自身；如果是 brew 安装的，提示改用 `brew upgrade`。`cox version --check` 提示新版本。
- 升级后执行 `cox skills install --refresh`，保证各 agent 里的 Skill 与 CLI 版本一致（Skill 中写明所需的最低版本）。
- Release 二进制用 Developer ID 签名并公证，避免 Gatekeeper 拦截；签名身份稳定后，钥匙串的“始终允许”授权不会因升级而失效。

**为什么桌面开着时仍要汇合到一个进程**：N8（多 agent 共享连接和锁）、统一审批与审计、人能看到 AI 在干什么，都需要一个持有状态的进程；这个进程就是用户自己开的桌面窗口，而不是后台服务。

### 4.1 核心抽象：Action

所有能力都是 Action。这是整个方案里最重要的接口，四种对外接口都从它生成：

```go
type Action struct {
    ID          string            // "k8s.pod.logs" —— 也决定 CLI 路径与 MCP 名
    Summary     string            // 给人看
    Description string            // 给 LLM 看：何时用、别做什么
    Input       jsonschema.Schema
    Output      jsonschema.Schema
    Render      RenderHint        // table | log | yaml | metrics | diff —— UI 自动渲染
    Risk        Risk              // Read | Write | Destructive
    Targets     func(in Input) []TargetRef   // 解析出影响对象(host/cluster/ns)，供策略与加锁
    DryRun      bool              // 是否支持预演（apply/批量执行）
    Streaming   bool              // 长输出或长任务：返回 Resource 句柄
    Run         func(ctx Ctx, in Input) (Result, error)
}
```

配套的两个一等对象：

- **Target / Inventory**：`host:prod-web-01`、`k8s:cluster-a/ns-pay`，带 `env=prod`、`team=pay` 等标签以及关联关系（Node IP ↔ Host）。策略、批量执行、跨域跳转都基于它。
- **Resource**：隧道、终端、日志 tail、采集器、后台任务，带 `id / owner(actor) / ttl / status`，可以列出、续期、关闭。agent 会话结束时，它名下 TTL 到期的资源会自动回收。

### 4.2 四种接口如何生成

| 接口 | 形态 | 关键设计 |
|---|---|---|
| **CLI** `cox` | `cox k8s pod logs -c prod -n pay api-7d9 --tail 200 -o json` | 人用，也给“CLI + Skill”模式的 agent 用；`-o json` 是稳定契约 |
| **MCP** | `cox mcp`（stdio）/ `opsd` streamable HTTP | **避免工具数量爆炸**：默认只暴露“常用直达工具（约 15 个）+ `cox_find_actions` / `cox_run_action` 两个元工具”；可按工具集开关，实现渐进披露 |
| **HTTP + WS** | UI 使用 | 输入 schema 自动生成表单，RenderHint 自动渲染结果，**新工具无需写 UI** |
| **Skill** | `cox skills export --to claude,codex,kimi` | 每个工具集自动生成参考 skill，再叠加手写的 runbook skill |

### 4.3 Agent 接入：三种模式

**模式 A：Agent 调工具（M0，立刻可用）**

```bash
claude mcp add cox -- cox mcp --actor claude-code
codex mcp add cox -- cox mcp --actor codex        # 或写到 ~/.codex/config.toml [mcp_servers.cox]
kimi mcp add --transport stdio cox -- cox mcp --actor kimi
```

- `--actor` 加上每次会话的 session id 写入审计，策略也可以按 actor 区分（例如“Codex 只读”）。
- 对于不想走 MCP 的场景，直接用 CLI + Skill，token 开销更低，三家都兼容。

**模式 B：工具调 Agent（M1，核心）**

- opsd 内置 **Agent Runner**，作为 ACP 客户端，统一驱动以下 agent：`kimi acp`、`claude-agent-acp`、`codex-acp`；以后任何支持 ACP 的 agent 都能直接接入。
- UI 上任何对象（主机、Pod、告警、一段终端输出、批量执行的失败项）都有 **“交给 AI ▾（选 agent）”** 入口。点击后：
  1. 生成 **Context Pack**：目标引用、最近输出的 artifact 句柄、相关事件、建议使用的 runbook skill；
  2. 通过 ACP `session/new` 把 cox MCP 注入给该 agent，并附带本次会话的 actor 身份与作用域（例如只允许操作 `ns-pay`）；
  3. agent 的消息、工具调用、审批请求实时流回 UI（ACP 的 `session/update`、`request_permission`），人可以随时打断或接管。
- 降级方案：如果 ACP 适配器有问题，用无头模式替代：`claude -p --output-format stream-json`、`codex exec --json`。
- **协作玩法**（建议 P2 再做，先不过度设计）：A 负责诊断、B 复核变更计划（second opinion）；或者把同一个问题发给多个 agent，对比结论。

**模式 C：事件驱动（P2）**

- 触发源：指标阈值、K8s Warning 事件、定时巡检。
- 流程：触发 → 匹配 runbook → 以**只读作用域**启动 agent → 产出报告（UI / 飞书）→ 报告中附带“建议修复”按钮 → 人审批后才执行。

### 4.3.1 桌面 AI 工作台（核心体验）

**布局**：左侧资源（主机 / 集群 / Argo 应用 / 工作流）｜中间工作区（终端、文件、YAML、日志、图表）｜右侧 **AI 面板**（常驻，可折叠）。

**AI 触点**（每个领域都要有）：

| 位置 | 动作 | 带给 agent 的上下文 |
|---|---|---|
| 终端选中文本 / 报错行 | “分析选区” | 选中内容 + 最近 N 行 + 主机信息 |
| 主机 | “体检一下” / “为什么慢” | 主机、指标快照、`ai_note` |
| Pod / Deployment | “诊断” | `k8s.diagnose` 证据包 |
| Argo CD Application | “为什么 OutOfSync / Degraded” “这次发布改了什么” | 同步状态、健康状态、diff、最近历史 |
| Argo Workflow | “为什么失败” | 失败节点、日志尾部、模板参数 |
| 批量执行的失败项 | “分析失败原因” | 失败主机列表 + 各自输出 |
| 任意多选 | “对比” | 多个对象的快照 |

**交互**（借鉴 Termark，再加上多 agent）：
- 对话开始时选择 **agent**（Claude Code / Codex / Kimi）和 **模式预设**（inspect / diagnose / change / batch，§8）；作用域显示为可编辑的 chips（例如 `@prod-web-01` `@cluster-a/ns-pay`），可以 @ 添加多台主机或多个对象。
- 流式显示 agent 的思考、工具调用（状态机：running / awaiting_approval / reviewing / done / failed）、token 用量。
- 审批卡片内嵌在对话里：Enter 批准、Esc 拒绝、“本会话同一命令不再询问”；窗口在后台时发系统通知。
- 随时打断、改作用域、切换 agent；“让另一个 agent 复核”一键把当前计划和证据发给另一个 agent（second opinion）。
- 会话历史持久化在 `~/.coxswain/sessions/`，可以继续之前的会话，也可以生成复盘报告。

**终端里的 agent**：在 cox 终端里直接运行 `claude` / `codex` / `kimi` 时，自动注入 `COX_*` 环境变量（当前主机、作用域），并识别 OSC 9;4 / 777 通知显示“agent 正在执行”（借鉴 Termark）。

### 4.4 审批与安全

**策略判定（在 opsd 中执行，对所有入口一视同仁）：**

| 输入 | 示例 |
|---|---|
| actor | human / claude-code / codex / kimi / automation |
| Action 风险 | Read / Write / Destructive |
| 目标标签 | `env=prod` / `env=staging` |
| 命令内容 | 用 shell 解析器（`mvdan.cc/sh`）拆出所有命令，与只读白名单（`cat ls df ps top journalctl ss …`）和危险模式（`rm -rf`、`reboot`、`> /etc/…`、`kubectl delete`）比对；**无法解析一律 ask** |

**审批档位（借鉴 Termark，按 环境标签 / 主机 / actor 覆盖）：**

| 档位 | agent 的写操作 | 用途 |
|---|---|---|
| `always` | 每一步都 ask | prod 默认 |
| `balanced` | 只有规则命中（Destructive、危险命令、敏感路径、超出作用域）时 ask，其余直接执行 | 非 prod 默认 |
| `auto_review` | 规则命中时交给独立审查模型：allow 直接执行 / revise 退回改写 / ask_user 或超时转人工 / deny 拒绝 | 想少被打扰的场景 |
| `auto` | 全部放行，只记审计 | 仅限沙箱/测试环境，UI 用警示色标出 |

固定约束（任何档位都生效）：Read 一律放行；自动化 actor 只允许 Read；Destructive 在 prod 下总是 ask，并需要二次确认（Touch ID）；被拒绝时给 agent 回填标准的“不要绕过”文本。

**审批流程的设计要点：**

- 对 agent 而言，需要审批的调用会立即返回 `{"status":"approval_required","approval_id":"ap_x","preview":<diff/命令/目标列表>}`。agent 再调用 `cox_wait_approval(ap_x)` 阻塞等待（或轮询）。**这样不依赖任何 agent 的交互能力**，三家行为一致。如果客户端支持 MCP elicitation，就直接弹出确认。
- 审批入口有三个：UI 卡片 / `cox approve ap_x` / 飞书消息（P2）。
- **审批绑定参数哈希 + 一次性 + TTL**，防止批准的是 A、实际执行的是 B。
- K8s 写操作先做 server-side dry-run，把 diff 放进 preview；批量执行先预演目标清单，再按金丝雀 → 分批执行，失败即停。
- **输出脱敏**：Secret data、`password=`、token 类正则、私钥块，在返回给 agent 之前替换掉（给人看的 UI 可选择显示原文）。
- **目标级写锁**：同一目标同一时刻只允许一个写操作（N8）。
- **急停**：`cox freeze` 或 UI 按钮，冻结所有非 human actor 的写操作，并关闭 agent 名下的隧道。

### 4.5 输出塑形（N5）

- 每个 Action 都有默认的截断上限（例如 8KB / 200 行）。超出部分存为 artifact，返回 `{summary, head, tail, artifact_id, total_lines}`。
- 提供 `cox_artifact_read(id, grep|range)` 按需读取。
- 文件下载**只返回本地路径**，agent 用自己的文件工具去读，**文件内容永远不经过 MCP**。
- 列表类结果统一提供分页和 `fields` 投影。

### 4.6 各领域设计要点

**SSH**

- 按主机复用连接池，同一连接上多路 channel；ProxyJump 链；agent forwarding；known_hosts 严格校验，指纹变化时告警。
- 导入 `~/.ssh/config`，与系统 ssh-agent 互通；终端会话可被 agent“只读旁观”（scrollback 存成 artifact）。

**端口转发**

- L / R / D 三种都由 opsd 托管（应用打开期间），包含健康检查、自动重连、流量统计；应用退出时全部关闭。
- agent 调用 `forward.open(host, remote=10.0.0.5:9090)`，返回 `127.0.0.1:52xxx` 和 resource_id，默认 TTL 为 30 分钟。
- `-R` 默认 ask（会把本机暴露给远端）。

**文件**

- SFTP 分块传输、断点续传、sha256 校验、进度通过 WS 推送；提供目录同步（rsync 语义的简化版）。
- 批量分发：一个文件 → N 台主机，分批执行，失败即停。

**机器指标（远端零安装）**

- 采集脚本结构借鉴 Termark（NUL 分段、`/proc` 双采样、可选 nvidia-smi、`LC_ALL=C`、BusyBox 回退），区别在于外层套一个循环：在已有连接上开一个长驻 exec channel，运行一段约 30 行的 POSIX sh 循环，每 N 秒输出一次 `/proc/stat`、`meminfo`、`loadavg`、`diskstats`、`net/dev`、`df -P` 的快照。由 opsd 计算差值和速率，**远端不安装任何东西**。
- 本地存储：SQLite 时序表 + 降采样（例如 10s 粒度保留 1 天，1m 粒度保留 7 天）。
- 如果目标机器已经部署 node_exporter，就通过隧道抓取，优先使用。
- 给 agent 的接口是**摘要**而不是原始序列：`metrics.snapshot`、`metrics.range(…, agg)`、`metrics.anomalies(host, window)`，返回形如“CPU iowait 在 14:02 后从 2% 升至 38%，磁盘 sda util 95%”。
- 进程 top、端口、磁盘大目录等做成按需的只读 Action（内置 `host-toolbox` 工具集）。

**K8s**

- 基于 client-go：多 kubeconfig、多 context；UI 侧用 informer 缓存；兼容企业 SSO 等 exec 凭据插件。
- 只读（P0）：get / list / describe / events / logs（多容器、previous）/ top / rollout status / RBAC 自查。
- 写（P1）：scale / rollout restart / undo / cordon & drain / delete pod / apply（**必须先 diff**）/ exec / port-forward（托管为 Resource）。
- **组合诊断** `k8s.diagnose(target)`：参照 k8sgpt 的 analyzer，先用规则筛查（CrashLoop、OOMKilled、ImagePull、Pending 原因、探针失败、节点压力），再打包证据（相关事件 + 日志尾部 + describe 片段 + 节点指标），可以**直接跳到对应节点的 SSH 主机**（N9）。

**Argo CD**

- 读：**通过 kube API 读取 `Application` / `AppProject` CRD**（复用 kubeconfig，不需要另配凭据）：同步状态、健康状态、资源树、目标与实际 revision、`status.history`、条件和报错。
- 写（需要审批）：`sync`（可选 prune / dry-run / 指定资源）、`rollback`（到 history 中的某个 id）、`refresh` / hard refresh、`terminate-op`。优先调用 argocd-server API（`cox` 中配置 server 地址 + token，token 存钥匙串）；没有配置时退化为 patch `Application.operation`（这也是官方支持的触发方式），`rollback` 只通过 API 执行。
- `diff`：通过 API 的 managed-resources 获取 live 与 desired 的差异，渲染为 diff，作为审批预览。
- **跨域关联**：Pod/Deployment ↔ Application（argocd 的 tracking label/annotation）↔ Git revision。诊断“发布后出问题”时，agent 能沿着 Pod → Application → 最近一次 sync 的 revision 和 diff 一路查下去。
- AI 诊断 `argocd.diagnose(app)`：OutOfSync 的原因（哪些资源有差异、是否被手动修改）、Degraded 的根因（下钻到 `k8s.diagnose`）、sync 失败的 hook 日志。

**Argo Workflows**

- 读：通过 kube API 读取 `Workflow` / `WorkflowTemplate` / `CronWorkflow` CRD：状态、节点树、失败节点、参数、产物引用；节点日志直接读对应 Pod 的日志（复用 k8s provider）。
- 写（需要审批）：`submit`（从模板提交，参数先预览）、`retry`、`resubmit`、`stop`、`terminate`、`suspend` / `resume`、CronWorkflow 暂停与恢复。优先调用 argo-server API；未配置时，`suspend` / `resume` / `stop` / `terminate` 退化为 patch CRD，`retry` / `resubmit` 只通过 API 执行。
- 本地渲染：给定模板和参数，在本地渲染出最终 Workflow 并做校验（对应已有的 argoflow 本地渲染场景），作为提交前的审批预览。
- AI 诊断 `argowf.diagnose(wf)`：定位第一个失败节点 → 读日志尾部和退出码 → 关联 Pod 事件与节点状态（OOM、驱逐、镜像拉取失败）→ 给出是否可以直接 `retry` 的建议。

**Go 依赖取舍**：不引入 argo-cd / argo-workflows 的完整 Go 模块（依赖树很大）。读操作用 client-go 的 dynamic client + 自定义的少量类型；写操作用薄 HTTP 客户端调用它们的 REST API（只覆盖用到的几个端点）。

### 4.7 事件日志（审计 + 回放 + 复盘）

- 借鉴 DSH 的追加写会话日志：`~/.coxswain/events/YYYY-MM-DD.jsonl`，SQLite 中建索引。
- 事件类型：`action.requested / approved / denied / started / finished`、`resource.opened / closed`、`agent.session.started / message`、`policy.freeze`。
- 每条事件包含：actor、agent session、Action、参数（已脱敏）、目标、审批人、耗时、结果摘要、artifact 引用。
- 在此基础上：`cox timeline --since 2h --target ns-pay` 可以生成复盘时间线，也可以交给 agent 写复盘报告。

---

## 5. 扩展机制：三档插件

### T1：声明式工具集（YAML，分钟级接入，覆盖大部分“未来工具”）

```yaml
# ~/.coxswain/toolsets/nginx.yaml
toolset: nginx
description: Nginx 检查与重载
requires: [ssh]
skills: [./skills/nginx-troubleshoot.md]
actions:
  - id: nginx.config_test
    description: 在目标主机上校验 nginx 配置语法
    risk: read
    target: host
    run: { ssh: "nginx -t 2>&1" }
  - id: nginx.reload
    description: 平滑重载 nginx（会先自动执行 config_test）
    risk: write
    target: host
    pre: [nginx.config_test]
    run: { ssh: "systemctl reload nginx" }
  - id: nginx.top_ips
    risk: read
    target: host
    params:
      log: { type: string, default: /var/log/nginx/access.log }
      n:   { type: integer, default: 20, maximum: 200 }
    run: { ssh: "tail -n 100000 {{log|shellquote}} | awk '{print $1}' | sort | uniq -c | sort -rn | head -{{n}}" }
    output: { parse: lines, render: table }
```

- 执行器类型：`ssh` / `local`（本地 CLI，如内部 IaC、CI 工具）/ `kubectl` / `http`。
- 参数**强制使用 schema 与 `shellquote`**，从源头防止命令注入。
- 同一套机制也可以包装现有的内部 CLI，现有 skills 生态可以平滑并入。

### T2：外部挂载（任意语言 / 现成 MCP 服务）

- opsd 作为 **MCP 聚合代理**：挂载 `kubernetes-mcp-server`、Grafana MCP、内部团队的 MCP 服务，统一加上策略、审计、脱敏。
- 风险等级在挂载配置中按工具声明，未声明的默认 ask。
- 也支持 JSON-RPC 子进程协议，便于用 Python/Shell 写有状态的小工具。

### T3：原生 Provider（Go）

- 用于需要连接池、长连接、流式输出的重能力：ssh / k8s / metrics / forward。
- 接口：`Register(reg *Registry)`，注册 Action、Resource 类型和可选的 UI 面板。

### UI 扩展

- 默认：根据 schema 自动生成表单和结果渲染（table / log / yaml / diff / metrics），**新工具无需写 UI**。
- 可选：在 manifest 中声明自定义面板（例如终端、资源树、指标图），以 Web 微前端（独立 JS 模块 + 约定的挂载接口）加载。

---

## 6. 技术选型

| 部分 | 选型 | 理由 |
|---|---|---|
| 核心 / CLI | **Go 单二进制** `cox`：`desktop` / `mcp` / 各领域子命令；cobra 做命令树（由 Action Registry 生成） | client-go、x/crypto/ssh、pkg/sftp 都很成熟；与团队栈一致；单文件分发 |
| MCP | `modelcontextprotocol/go-sdk` | 官方实现，同时支持 stdio 与 streamable HTTP |
| ACP 客户端 | Go 自行实现（JSON-RPC over stdio，协议本身不大），或参考 `beyond5959/acp-adapter` | Agent Runner 放在 opsd 内 |
| 存储 | SQLite（modernc，纯 Go）+ JSONL 事件日志 | 零部署 |
| 凭据 | 使用 macOS 钥匙串保存数据密钥（可选 Touch ID 解锁），降级为 AES-GCM 文件 + 独立密钥文件（0600）；优先复用系统 ssh-agent | opsd 是独立进程，不受宿主沙箱限制，可以直接用钥匙串 |
| shell 解析 | `mvdan.cc/sh/v3/syntax` | 风险分类 |
| 桌面 | **Wails v3**（Go 原生桌面框架，使用 macOS 系统 WKWebView）+ React + TypeScript + **xterm.js** + Monaco + ECharts；前端资源 `go:embed` 进同一个二进制 | `cox desktop` 直接开窗口，无需另装 App；关窗即退出，天然不常驻。备选：`webview/webview_go`（更轻、能力更少）；WKWebView 下 xterm.js 用 WebGL/Canvas 渲染器，M0 先做兼容性验证 |
| 分发 | GitHub Releases + GoReleaser + Homebrew tap + `install.sh`；cosign/minisign 签名；macOS 签名公证 | 符合“CLI 工具 + GitHub 分发” |
| 测试 | Docker sshd 容器（开启 `AllowTcpForwarding`）；K8s 用 kind；UI 用 Playwright | 全部可在本机跑 |

---

## 7. 目录结构

```
coxswain/
├── cmd/cox/                  # 单二进制入口：desktop | mcp | 各领域子命令
├── internal/
│   ├── core/                 # Action registry, executor, resource manager
│   ├── policy/               # 策略、审批、命令分类、脱敏
│   ├── inventory/            # 主机/集群/标签/拓扑
│   ├── vault/
│   ├── eventlog/
│   ├── shaper/               # 截断、artifact
│   ├── surface/{cli,mcp,http}/
│   ├── agents/               # ACP 客户端 + runner 适配（claude/codex/kimi）
│   └── providers/{ssh,forward,sftp,metrics,k8s,argocd,argowf,local,yaml,mcpproxy}/
├── toolsets/                 # 内置 YAML 工具集 + skills
├── web/                      # React UI（构建产物 go:embed）
├── internal/desktop/         # Wails 窗口、单实例、退出确认、instance.json
├── .goreleaser.yaml          # 多平台构建、签名、Homebrew tap
├── install.sh
└── docs/specs/
```

---

## 8. 从 DSH 借鉴的设计（只借思想，不做插件）

| DSH 的设计 | 本方案怎么用 |
|---|---|
| **AGENT = MODEL + HARNESS**：模型之外的循环、工具、沙箱、会话、UI 才决定可靠性 | 我们不做模型、不做 agent 循环，专心做 harness 里“工具 + 沙箱 + 审批 + 会话记录”这一半，把 agent 当作可替换的“大脑” |
| **一切皆插件，没有特权内核**（Cordis） | opsd 内核只有 Registry / Policy / Executor / EventLog；ssh、k8s、metrics 也都是 Provider，与第三方工具集走同一套注册接口，没有后门 |
| **依赖注入 + 资源随插件生命周期自动回收**（`ctx` / fiber / `inject`） | Provider 通过 `ctx` 声明依赖（例如 k8s 依赖 forward）；Provider 卸载或热重载时，它打开的连接、隧道、goroutine 自动回收；Resource 的 owner/TTL 机制也来自这个思想 |
| **Seam（可替换接缝）**：`subprocess` / `fs` 等底层能力可以整体替换（例如换成远程执行） | 执行器抽象为 seam：`local` / `ssh` / `kubectl exec` 可互换。同一个 YAML 工具集既能在本机跑，也能在远端主机或 Pod 里跑 |
| **分层配置**：bundle 层叠加用户 patch，按 id 覆盖 | 配置分层：内置默认 → 工具集自带 → 用户全局 `~/.coxswain` → 项目 / 环境级；策略同样分层覆盖，`cox config explain` 能说明某个值来自哪一层 |
| **预设（presets）**：standard / plan 等模式打包不同的工具与提示词 | **运维模式预设**：`inspect`（只读巡检）、`diagnose`（只读 + 证据收集）、`change`（允许写但每步审批）、`batch`（批量 + 分批），启动 agent 会话时选择，决定暴露哪些 Action 与策略 |
| **Plan mode** | `change` 模式下 agent 先产出变更计划（目标、步骤、回滚），人批准计划后才解锁写 Action |
| **追加写会话日志**（系统提示、推理、工具调用、子 agent 调度、上下文注入全部记录） | 事件日志（§4.7），并额外记录“注入给 agent 的上下文包”，可以回答“AI 当时看到了什么” |
| **编排子 agent**（DSH 能把 Claude Code / Codex 当子 agent） | Agent Runner（§4.3 模式 B）：opsd 作为编排方，驱动三家 agent |
| **Host / Client 两半分离**：网络和进程只在 Host，Client 只负责渲染 | opsd 持有一切能力，Web UI 只通过 HTTP/WS 调用，**UI 永远拿不到凭据** |
| **对话里生成插件**（agent 写插件、安装、验证） | P2：`cox toolset new` + 一个 “toolset-author” skill，让 Claude Code 等写 YAML 工具集，opsd 先做 schema 校验和 dry-run 再启用 |

**不借鉴的**：DSH 插件的前端运行时（受限的闭包 React、无 fetch、无 xterm.js）。这正是 dsh-ssh 体验差的根源，UI 必须自建。

## 9. 里程碑（每步可验收；工期为粗估）

AI 是核心，所以 **M0 就让三家 agent 能用，M1 就把桌面 AI 工作台做出来**，后面每个领域都带着 AI 触点一起交付。

| 阶段 | 内容 | 验收标准 |
|---|---|---|
| **M0 骨架 + 发布**（约 1 周） | `cox` 二进制、Action Registry、CLI / MCP 接口、事件日志、策略引擎、`cox approve`；SSH：hosts（含 `~/.ssh/config` 导入）、exec；`cox init` 安装 MCP 和 Skill；GoReleaser 发布到 `hua-ri/coxswain`，`brew install hua-ri/tap/coxswain` 可装 | **Claude Code、Codex、Kimi** 都能列出主机并执行只读命令；写命令返回 `approval_required`，`cox approve` 之后才执行；事件日志完整 |
| **M1 桌面 + AI 工作台**（约 2 周） | `cox desktop`（Wails）、单实例与 instance.json 汇合；主机列表 + xterm 终端；Agent Runner（ACP：kimi / claude-agent-acp / codex-acp）；AI 面板（流式消息、工具调用、审批卡片、作用域 chips、模式预设）；“分析选区”“交给 AI” | 在终端里选中一段报错 → 交给 Claude Code → 它通过 cox 查日志和磁盘 → 提出修复 → 在桌面批准后执行；同一问题一键让 Codex 复核 |
| **M2 SSH 完整** | SFTP 传输、托管式 L/R/D 转发（owner/TTL）、批量执行（分批、失败即停）、artifact、`host-toolbox`；各自的 AI 触点 | agent 能申请隧道访问内网 Prometheus；20 台批量执行，失败项一键交给 AI 分析 |
| **M3 K8s** | 只读全集、`k8s.diagnose`、写操作（dry-run diff）、Node→Host 关联、日志与 exec 终端 | CrashLoop 场景下 agent 在 3 步内给出根因与证据；apply 前必有 diff 审批 |
| **M4 Argo CD + Argo Workflows** | CRD 读取、diff、sync / rollback / refresh、workflow retry / resubmit / stop、本地渲染、两个 diagnose；Pod ↔ Application ↔ revision 关联 | “发布后 5xx”场景：agent 从 Pod 追到 Application 的最近一次 sync 和 diff，给出回滚建议，审批后执行 rollback；失败的 workflow 一键诊断 |
| **M5 指标** | 零安装采集、降采样、anomalies 摘要、图表 | 10 台主机持续采集时远端 CPU 开销 < 1%；agent 能用一句话说清异常 |
| **M6 扩展** | YAML 工具集、MCP 聚合代理、`cox toolset new` + toolset-author skill | 新增一个工具 ≤ 30 行 YAML，CLI / MCP / 桌面 / AI 入口自动可用 |
| **M7 事件驱动** | （仅桌面打开期间）阈值 / K8s 事件 / Argo 失败 → 只读 agent 调查 → 报告 → 审批修复 | 一次模拟 workflow 失败，自动生成诊断报告并发系统通知 |

---

## 10. 风险与对策

| 风险 | 对策 |
|---|---|
| ACP 适配器迭代很快（claude-agent-acp、codex-acp 一周一版） | 钉死版本；Runner 抽象出接口，保留无头模式作为降级 |
| 不同 agent 对 MCP 的支持差异（elicitation、结构化输出、工具数量上限） | 审批走“返回 approval_required + wait”的通用路径；默认工具数 ≤ 20 |
| 命令分类有漏网之鱼 | 白名单制（未知一律 ask）；prod 环境对写操作额外确认；审计兜底 |
| 桌面没开时多个 agent 各自持有连接，可能重复连接或同时写同一目标 | 目标级写锁放在磁盘（SQLite 行锁 / flock），跨进程生效；推荐“干活时开着桌面”，以获得共享与可视化 |
| 桌面被强杀或崩溃，留下失效的 `instance.json` | CLI/MCP 连接前校验 pid 存活和 token 握手，失败即删除并回落到进程内执行 |
| Wails + cgo 构建依赖 macOS 工具链 | 只做 macOS，构建统一在 GitHub 的 macOS runner 上完成 |
| 凭据在本机集中存放，被攻破的影响更大 | 明确写清威胁模型；可选钥匙串 / Touch ID；agent 永远拿不到明文 |
| 范围膨胀 | 严格按里程碑推进；M0 就打通三家 agent，先验证最核心的价值 |

---

## 11. 已确认

- 名字 Coxswain，命令 `cox`；GitHub 公开仓库 `hua-ri/coxswain`，Homebrew tap `hua-ri/homebrew-tap`。
- 只做 macOS（CLI 与桌面都是）；面向个人用户，不做团队功能。
- 产品形态：CLI 为本体，`cox desktop` 打开桌面（主流程）；没有常驻进程。
- **AI agent 辅助是核心**：M0 打通三家 agent（模式 A），M1 交付桌面 AI 工作台（模式 B）。
- 领域范围：SSH、K8s、Argo CD、Argo Workflows（“argoflow” 按 Argo Workflows 理解）。

**下一步**：编写 M0 实施计划。
