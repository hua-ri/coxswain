# Coxswain M3 — Kubernetes 设计与实施计划

> 日期：2026-11-09。前置：v0.4.0。
> **For agentic workers:** TDD per task (failing test first), one commit per task; after every commit `gofmt -l .`
> empty, `go vet ./...`, `go test -race ./...`, `npm --prefix web run typecheck && npm --prefix web test && npm
> --prefix web run build` green; `-race -count=20` on new packages. No push/tag/remotes. **Never touch the user's
> real kubeconfig or clusters**: tests use client-go fake clients or a kubeconfig in a temp dir; the user's current
> context `pops` is a real cluster. No real windows/dialogs in tests. Tests must not depend on timing.

## 原则（沿用）

- 能力是 Action（CLI / MCP / 桌面同时可用），MCP 注解如实；风险标签 + 审计 + 首次授权；审批交给 agent。
- 有状态的东西（port-forward、exec 终端、日志跟随）在 server 里，带 owner，桌面可见可停，server 停时关闭，撤销 agent 时关闭它的。
- 给 agent 的输出要小：列表是精简表格（不是完整 YAML），日志尾部 + artifact，YAML 按需取。
- 不碰真实集群做测试；用户自己的 kubeconfig 只读。

## 1. 集群接入

- kubeconfig 来源：`$KUBECONFIG`（多文件合并）或 `~/.kube/config`，与 kubectl 一致；server 启动时加载，文件变化时重新加载（mtime 轮询 5s）。
- 每个 context 是一个「集群目标」：`k8s:<context>`。目标标签存在 `~/.coxswain/clusters.json`（`{context: {tags, note}}`），与主机标签、备注同样的用法（`env=prod`、给 agent 的备注）。`cox k8s contexts tag <ctx> --set env=prod`。
- 认证：client-go 原生支持 exec 凭据插件（企业 SSO 等）、token、证书；插件需要交互登录时报错并提示用户在终端里先登录。
- 每个 context 一个 client（rest.Config QPS 20 / Burst 40，超时 30s），懒加载、缓存；discovery 结果缓存 10 分钟。

## 2. Actions

| id | 注解 | 说明 |
|---|---|---|
| `k8s.contexts` | read-only | context 列表：名字、集群、默认 namespace、当前与否、标签、备注 |
| `k8s.get` | read-only | `{context, kind, namespace?, name?, all_namespaces?, label_selector?, field_selector?, limit=200}`；kind 支持简称 / 复数 / `group/version/kind`（discovery 解析）；返回精简列表：name、namespace、age、状态摘要（按资源类型：Pod 的 phase/ready/restarts/node，Deployment 的 ready/updated/available，Node 的 ready/roles/version/IP，Service 的 type/clusterIP/ports，其余取 `status.conditions` 摘要）；带 `name` 时返回单个对象的 YAML（经 shaper 截断） |
| `k8s.describe` | read-only | 对象摘要 + 相关 events（按 involvedObject）+ Pod 的容器状态 / 上次终止原因 |
| `k8s.events` | read-only | `{context, namespace?, all_namespaces?, type?=Warning, since?=1h, limit=200}`，按时间倒序 |
| `k8s.logs` | read-only | `{context, namespace, pod, container?, previous?, tail=200, since?}`；输出走 shaper（截断 + artifact）；多容器未指定时报错并列出容器名 |
| `k8s.top` | read-only | Pod / Node 资源使用（metrics.k8s.io 不存在时给出明确提示） |
| `k8s.diagnose` | read-only | 见 §3 |
| `k8s.apply` | destructive, open-world | `{context, namespace?, manifest (YAML, 可多文档), dry_run?}`；**总是先 server-side dry-run**，返回每个对象的 diff（当前 vs dry-run 结果，忽略 managedFields / resourceVersion / generation / status）；`dry_run=true` 只返回 diff；实际应用用 server-side apply（fieldManager `cox`，force=false） |
| `k8s.delete` | destructive | `{context, kind, namespace, name}`；单对象，不支持 selector 批量删除；Namespace / Node / PV / CRD 删除风险标签 destructive 且结果提示 |
| `k8s.scale` | destructive | Deployment / StatefulSet / ReplicaSet 副本数；风险：缩到 0 为 destructive，其余 write |
| `k8s.rollout` | destructive | `{context, namespace, kind, name, action: status|restart|undo|pause|resume, to_revision?}`；status 视为 read |
| `k8s.exec` | destructive, open-world | 一次性命令（非交互）：`{context, namespace, pod, container?, command: [..] 或 string(sh -c), timeout_sec}`；风险标签用 `ClassifyCommand`；输出走 shaper |
| `k8s.port_forward` | destructive | 打开到 Pod / Service（解析到一个就绪 Pod）的转发，本机 `127.0.0.1:<port>`；作为隧道出现在 Tunnels 面板（type `k8s`），owner / 闲置关闭 / 撤销规则与 SSH 隧道一致 |
| `k8s.node_host` | read-only | Node → SSH 主机映射：按 Node 的 InternalIP / Hostname 匹配已保存主机的 HostName；无匹配时说明 |

- Secret：`get`/`describe` 只返回 keys 和每个值的长度，永不返回值；`apply` 的 diff 中 Secret 的 `data`/`stringData` 值替换为 `<redacted N bytes>`；审计中 apply 的 manifest 经 Redact：Secret 值同样替换。
- 风险标签：读为 read；写按上表；目标为 `k8s:<context>/<namespace>` 带 context 标签（env=prod 由用户在 clusters.json 标注）。

## 3. 诊断 `k8s.diagnose`

`{context, namespace, kind: pod|deployment|statefulset|daemonset|job, name}` → 规则检查 + 证据包（参考 k8sgpt analyzer 思路）：

- Pod：phase、各容器 state / lastState（CrashLoopBackOff、OOMKilled + 退出码、ImagePullBackOff + 错误信息、CreateContainerConfigError）、未就绪的 readiness / liveness 探针失败事件、Pending 原因（调度失败事件：资源不足 / 节点选择器 / 污点 / PVC 未绑定）、重启次数、所在节点及节点状况（MemoryPressure / DiskPressure / NotReady）。
- 控制器：期望 vs 就绪副本、最近 rollout 状态、选中的异常 Pod（最多 3 个）逐个诊断。
- 证据：相关 Warning events（最近 20 条）、异常容器 previous 日志尾部 50 行（经 shaper）、节点 → SSH 主机映射（有的话，提示可用 ssh_exec 看 dmesg / 磁盘）。
- 输出：`{findings: [{severity, title, detail, evidence_refs}], evidence: {...}, next_steps: [...]}`，文字简短。

## 4. 共享终端：Pod exec

- 扩展 `internal/term`：会话后端抽象（SSH shell / Pod exec with TTY），Pod 终端在桌面同样以标签页出现（标题 `ctx/ns/pod[/container] · owner`），agent 可 `terminal_open {pod: {context, namespace, pod, container?, shell?=sh}}`，`terminal_send/read/close` 不变。
- exec 使用 SPDY / WebSocket（client-go remotecommand），resize 透传。

## 5. 桌面：Kubernetes 面板

- 侧边栏新增「Clusters」区：context 列表（标签徽章，env=prod 红色，备注提示），点进入 K8s 标签页。
- K8s 标签页：顶部 context / namespace 选择；左侧资源种类（Pods、Deployments、StatefulSets、DaemonSets、Jobs、CronJobs、Services、Ingresses、ConfigMaps、Secrets、Nodes、Events、Namespaces，可输入任意 kind）；主区列表（精简列 + 状态徽章，10s 自动刷新可关闭）；选中对象右侧详情：概览 / YAML（只读，Secret 不显示值）/ Events / Logs（Pod，容器选择、previous、tail、跟随刷新）/ 诊断按钮。
- Pod 行操作：日志、终端（共享终端）、端口转发（端口选择）、删除（确认）。Deployment 行操作：重启、扩缩容（确认）、回滚（选择版本，确认）。
- Apply：编辑框粘贴 YAML → 「预览差异」显示 dry-run diff → 确认应用。

## 6. 实施任务

1. **集群接入与读操作**：client-go 依赖；`internal/k8s`（kubeconfig 加载 / 重载、context 目标与 clusters.json、client 缓存、discovery 解析 kind）；`k8s.contexts`、`k8s.get`（精简摘要）、`k8s.describe`、`k8s.events`、Secret 脱敏；测试用 client-go fake（`k8s.io/client-go/dynamic/fake`、`kubernetes/fake`、fake discovery）与临时 kubeconfig。CLI：`cox k8s contexts`、`cox k8s contexts tag`。
2. **日志、top、exec**：`k8s.logs`（fake clientset 的日志 + shaper）、`k8s.top`（metrics 缺失时提示）、`k8s.exec`（remotecommand 抽象接口，测试用假执行器）。
3. **写操作**：`k8s.apply`（dry-run diff、SSA、多文档、Secret 脱敏、Redact）、`k8s.delete`、`k8s.scale`、`k8s.rollout`；风险标签。diff 逻辑可单元测试；SSA 用 fake dynamic client 的 Apply（或抽象接口）。
4. **诊断**：`k8s.diagnose` 规则 + 证据，表驱动测试覆盖 CrashLoop、OOMKilled、ImagePull、Pending（资源 / 污点 / PVC）、探针失败、节点压力；`k8s.node_host`。
5. **port-forward 与 Pod 终端**：隧道管理器支持 k8s 类型（client-go portforward），term 后端抽象 + Pod exec TTY；owner / 撤销 / 关闭规则复用；测试用抽象接口与假实现。
6. **桌面 K8s 面板**（§5）。
7. **集成测试（可选，本地手动）与文档**：`scripts/kind-e2e.sh` 在**独立 kubeconfig**（`$TMPDIR/cox-kind/kubeconfig`）里创建 kind 集群 `cox-test`（镜像 `kindest/node:v1.34.0`，已在本机），部署示例工作负载（正常 / CrashLoop / OOM / ImagePull / Pending），运行带 build tag `kind` 的 e2e 测试；CI 不跑。README 能力表 + K8s 节、SKILL.md（先 diagnose 再看日志；apply 先 dry_run；不要读 Secret 值；prod context 先确认）。snapshot 构建通过后停止。

## 7. 验收（与用户一起，使用 kind 集群 `cox-test`）

1. 用户以 `KUBECONFIG=<kind kubeconfig> cox server` 启动，桌面 Clusters 区出现 kind-cox-test，浏览 Pods / Deployments。
2. 让 Claude 诊断 CrashLoop 的 Deployment：给出根因（退出码 / 日志证据），不读 Secret 值。
3. 让 Codex 把某个 Deployment 扩到 3：先给出 dry-run / 变更说明，agent 自己的确认后执行，桌面看到变化。
4. 让 Kimi 对一个 Pod 开 port-forward，Tunnels 面板出现，用户关闭。
5. 用户在桌面打开 Pod 终端；agent 读到其中输出。
