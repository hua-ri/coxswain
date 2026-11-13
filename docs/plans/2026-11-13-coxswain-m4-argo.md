# Coxswain M4 — Argo CD 与 Argo Workflows 设计与实施计划

> 日期：2026-11-13（v3：官方 REST API + 多实例；登录方式作为能力，人和 agent 都可查询并选择；v1 的 CRD patch 方案作废）。前置：M3 + 新 shell。
> **For agentic workers:** TDD per task, one commit per task; after every commit `gofmt -l .` empty, `go vet ./...`,
> `go test -race ./...`, web typecheck/test/build green; `-race -count=20` on new packages. No tag/release.
> **RED LINE:** never read, write or switch the user's kubeconfig (`~/.kube/*`, `$KUBECONFIG`) or touch any non-cox
> cluster. Real-cluster tests only on kind clusters named `cox-*` with kubeconfigs under `~/.coxswain-test/kind/`,
> always with explicit `--kubeconfig`. Unit tests use httptest fakes of argocd-server / argo-server / OIDC.
> Never call the user's real Argo instances from tests; never store or log a credential the user pasted in chat.

## 原则

- **只走官方 API**：Argo CD 走 argocd-server REST API（`/api/v1/...`），Argo Workflows 走 argo-server REST API。
  不 patch CRD，不调用 `argocd` / `argo` CLI，不依赖集群访问。
- **不 import argo-cd 模块**：`github.com/argoproj/argo-cd/v3` 的 go.mod 有 37 条 replace，作为库引入时这些
  replace 不生效，必须整份抄进我们的 go.mod，并拖入 gitops-engine 和整套 k8s 依赖。改为使用它登录所用的**同一批
  官方库**（`golang.org/x/oauth2`，自带 PKCE；`github.com/coreos/go-oidc/v3`），按
  `cmd/argocd/commands/login.go`（v3.2）逐步移植登录流程（约 150 行），行为与 CLI 一致。API 的精简类型自己定义。
- **多实例**：Argo CD 与 Argo Workflows 各自是「实例」列表，一个实例 = 一个 URL。实例像主机 / 集群一样有标签
  （`env=prod`）和给 agent 的备注。
- **只做能力，方式交给用户选**：每个实例先查询支持哪些登录方式，人（桌面 / CLI）和 agent（MCP）看到同一份列表、
  调用同一组 action。Coxswain 不内置任何公司的 SSO，也不替用户决定用哪种方式。
- **凭据：能写不能读**。agent 是用户自己选的，Coxswain 不限制它添加实例、登录、填密码 / token；限制只在输出：
  任何 action 的结果、审计、事件、错误信息里，凭据一律脱敏（只显示「已配置」、类型、身份、到期时间、
  sha256 前 8 位）。所以 agent 能添加凭据，但读不到任何凭据，包括用户手动添加的。没有可读回凭据的 action。
  存储：macOS 钥匙串（服务 `coxswain`，账号 `argocd/<instance>` / `argowf/<instance>`，cgo Security
  framework）；`~/.coxswain/argo.json` 只存非敏感配置；密码只用于换 token，永不保存。
- **可分享**：`argo.export` 输出实例配置（不含凭据），同事 `argo.import` 后各自登录。
- 风险标签：读 read；登录 / 添加实例 / sync / refresh / suspend / resume / retry / resubmit / submit write；
  sync prune、rollback、terminate、stop、delete、移除实例 destructive。目标 `argocd:<instance>/<app>`、
  `argowf:<instance>/<ns>`，带实例标签。
- 给 agent 的输出精简：资源树摘要、失败节点、日志尾部 + artifact。

## 1. Argo CD 登录

`argocd.login_methods {instance}` → `GET /api/v1/settings`（无需认证），返回该实例支持的方式：

| method | 何时可用 | 流程（与 `argocd login` 相同） |
|---|---|---|
| `password` | 总是（本地账号可能被禁用，失败时如实报错） | `POST /api/v1/session {username, password}` → JWT |
| `browser` | settings 中有 `dexConfig.connectors` 或 `oidcConfig` | OIDC 授权码 + PKCE：client id 用 `oidcConfig.cliClientID`（无则 `clientID`），Dex 用 `argo-cd-cli`；scopes 同 CLI；`open` **系统浏览器**，本机回调 `http://localhost:<sso_port 默认 8085>/auth/callback`；得到 id_token（Bearer）与 refresh_token |
| `token` | 总是 | 用户提供的 API token（本地账号 token、project role token） |

返回内容还包括 SSO 提供方名称（如 Dex connector 名）和当前登录状态（谁、何时到期）。

- `argocd.login {instance, method, username?, password?, token?, sso_port?}`：
  - `browser`：**由 `cox server` 打开浏览器并监听回调**，不论是谁发起（桌面、CLI 或 agent），最后都由屏幕前的人在
    浏览器里完成登录；action 一直等到回调或 5 分钟超时，期间桌面显示「正在等待浏览器登录 · 取消」。端口占用时明确报错。
  - 因为 client id 与回调地址都与官方 CLI 一致，凡是 `argocd login --sso` 能用的实例，Coxswain 都能用，
    公司不需要额外配置。
- `argocd.logout {instance}`：删除钥匙串中的 token（同 CLI 不调用服务端）。
- 续期：有 refresh_token 时在到期前自动续期（同 CLI 的 `RefreshToken` 逻辑）；没有则到期后标为「需要登录」。
- 任一请求 401 → 实例状态变为「需要登录」，桌面在实例行上显示；agent 收到
  `argocd instance "x" needs login; methods: password, browser, token`。
- TLS：默认校验；实例可配 `ca_file` 或 `insecure_skip_verify`（显示警告）。

## 2. Argo Workflows 登录

argo-server 自己没有账号，`argowf.login_methods {instance}` 返回：

| method | 说明 |
|---|---|
| `token` | 用户手填。client 模式填 K8s token（如 `kubectl create token <sa>` 的短期 token，或 SA 的长期 token）；sso 模式填 argo UI user-info 页复制的 `v2:...`。带不带 `Bearer ` 前缀都可以 |
| `none` | argo-server 为 server 模式时无需凭据 |

以后若有其他取 token 的方式（例如用 Coxswain 里某个集群的身份签发短期 token），只需加一个 method，不影响上层。

- 探测：`GET /api/v1/info` 与无凭据的 `GET /api/v1/workflows/{ns}?listOptions.limit=1`，据此提示「需要 token」或
  「无需凭据」；探测结果只是提示，用户仍可自由选择。
- `argowf.login {instance, method, token?}`：保存前先用该 token 调一次列表接口验证，报告成功或具体错误
  （401 / 403 / TLS）。`argowf.logout` 删除 token。
- 若 token 是 JWT，解码（不校验）显示身份与到期时间，例如「acts as `system:serviceaccount:kube-system:admin`，
  never expires」；到期前 / 401 时标为「需要 token」。

## 实例管理

`argo.add {kind: argocd|argowf, name, url, namespace?, ca_file?, insecure_skip_verify?, tags?, note?}`、
`argo.update`、`argo.remove`、`argo.export`、`argo.import`；人与 agent 都可调用（风险标签见原则，
审批交给 agent 自己的权限提示）。CLI：`cox argo add|update|remove|export|import`、
`cox argocd login <instance> [--method …]`（密码在终端无回显输入）、`cox argowf login <instance>`
（token 从 stdin 或无回显输入，不接受命令行参数，避免进入 shell 历史和 `ps`）。

## 3. Actions

Argo CD：

| Action | 注解 | API |
|---|---|---|
| `argocd.instances` | read-only | 实例列表：name、url、tags、note、登录状态（不含凭据） |
| `argocd.login_methods` / `argocd.login` / `argocd.logout` | read-only / write / write | 见 §1 |
| `argocd.apps` | read-only | `GET /api/v1/applications?projects=&selector=` → name、project、repo/path/revision、destination、sync、health、最近 sync |
| `argocd.app` | read-only | `GET /api/v1/applications/{name}`：sources、sync policy、conditions、operationState、history（最近 10） |
| `argocd.tree` | read-only | `GET …/resource-tree`：非 Healthy / 非 Synced 资源优先的摘要 |
| `argocd.diff` | read-only | `GET …/managed-resources`：live vs target 的 unified diff，Secret 值脱敏 |
| `argocd.events` / `argocd.logs` | read-only | `GET …/events`、`GET …/logs?podName=&container=&tailLines=`（shaper） |
| `argocd.refresh` | write | `GET …/{name}?refresh=normal\|hard` |
| `argocd.sync` | write（prune → destructive） | `POST …/sync {revision?, prune?, dryRun?, resources?[]}`；有进行中的 operation 时拒绝并说明 |
| `argocd.rollback` | destructive | `POST …/rollback {id, prune?, dryRun?}`；auto-sync 开启时拒绝并提示 |
| `argocd.terminate` | destructive | `DELETE …/operation` |
| `argocd.diagnose` | read-only | conditions（ComparisonError 等）、OutOfSync 原因、Degraded → 不健康资源的 events 与日志尾部（全部经 REST）、sync 失败的 hook / 资源错误；若目标集群也在 Coxswain 里（server URL 匹配），next_steps 建议 `k8s.diagnose` |

Argo Workflows（`namespace` 缺省用实例默认值）：

| Action | 注解 | API |
|---|---|---|
| `argowf.instances` | read-only | 实例列表（不含 token） |
| `argowf.login_methods` / `argowf.login` / `argowf.logout` | read-only / write / write | 见 §2 |
| `argowf.list` | read-only | `GET /api/v1/workflows/{ns}?listOptions.labelSelector=&fields=…` → name、phase、progress、started/finished、duration、message |
| `argowf.get` | read-only | `GET /api/v1/workflows/{ns}/{name}`：精简节点树，失败节点优先 |
| `argowf.logs` | read-only | `GET …/{name}/log?podName=&logOptions.container=main&logOptions.tailLines=`（老版本退回 `nodeId=`）；行 JSON `result.content`，shaper |
| `argowf.templates` | read-only | `GET /api/v1/workflow-templates/{ns}`、`/api/v1/cluster-workflow-templates`、`/api/v1/cron-workflows/{ns}`，含参数定义 |
| `argowf.submit` | write | `POST /api/v1/workflows/{ns}/submit {resourceKind, resourceName, submitOptions{parameters, generateName, serverDryRun}}`；先校验参数（必填 / enum），`dry_run=true` 只返回预览 |
| `argowf.suspend` / `argowf.resume` | write | `PUT …/{name}/suspend\|resume` |
| `argowf.stop` / `argowf.terminate` | destructive | `PUT …/{name}/stop\|terminate` |
| `argowf.retry` / `argowf.resubmit` | write | `PUT …/{name}/retry {restartSuccessful?, nodeFieldSelector?}`、`PUT …/{name}/resubmit` |
| `argowf.delete` | destructive | `DELETE …/{name}` |
| `argowf.cron` | write | `PUT /api/v1/cron-workflows/{ns}/{name}/suspend\|resume` |
| `argowf.diagnose` | read-only | 第一个失败节点 → 退出码 / message / 日志尾部 → OOM、超时、拉镜像失败等分类 → 是否适合直接 retry 的建议 |

## 4. 桌面

- 侧边栏新增两个区：「Argo CD」与「Workflows」，行样式同 Hosts / Clusters（名字、env 标记）；需要登录或 token
  失效的实例在行尾显示 caution 色的 `log in` / `token`。「+」打开添加实例的编辑面板（同 Add host 的面板规范）：
  填 URL → 自动查询登录方式 → 列出该实例支持的方式（分段选择：Browser（显示 SSO 提供方名称）· Password ·
  Token），用户选一种登录；Workflows 面板是 URL、namespace、方式（Token · None）、Test。实例的 Log in…
  随时可再选方式。agent 发起的浏览器登录在桌面显示等待条（crew 标记 + 取消）。
- Argo CD 实例：Applications 表（name、project、sync 徽章、health、revision、last sync）；详情：概览、资源树
  （按 kind 分组，非 Synced / 非 Healthy 突出）、历史、Diff、Events、Diagnose；操作：Sync（prune、dry run、
  指定资源）、Refresh / Hard refresh、Rollback（历史选择）、Terminate。prod 沿用 hazard band 与确认。
- Workflows 实例：子行 Workflows / Templates / Cron；Workflows 表（name、phase、progress、duration、started）；
  详情：节点树（缩进树，不画 DAG）、失败节点日志、参数、Diagnose；操作：Suspend / Resume、Stop、Terminate、
  Retry、Resubmit、Delete；Templates 详情：参数表单 → Submit（预览 → 确认）。
- Activity：argo 动作显示 `app/<name> sync prune=true`、`wf/<name> stop` 等。

## 5. 实施任务

1. **实例存储与凭据**：`internal/argo/instances`（argo.json、校验、标签）；钥匙串封装（cgo Security
   framework，测试用内存实现）；实例管理 actions + CLI；export / import（不含凭据）；password / token 参数 Redact。
2. **Argo CD 客户端与登录**：HTTP 客户端（TLS 选项、401 → 需要登录状态）；`login_methods`（settings 解析）；
   password（`/api/v1/session`）；token；browser：按 argo-cd v3.2 `login.go` 移植（x/oauth2 PKCE、go-oidc、
   `open` 系统浏览器、本机回调、端口可配、占用时明确报错、5 分钟超时、可取消）、refresh_token 续期。测试：httptest 假 argocd-server + 假 OIDC provider（Dex 与外部
   OIDC 两种 client id 路径）、浏览器步骤由测试直接请求回调地址模拟。
3. **Argo CD 读操作**：instances、apps、app、tree、diff（Secret 脱敏）、events、logs。
4. **Argo CD 写操作**：refresh、sync（进行中 operation 冲突）、rollback（auto-sync 拒绝）、terminate；风险标签。
5. **Argo Workflows**：客户端（token 规范化、JWT 身份 / 到期解析）、login_methods 探测、login 验证；全部读写 actions；submit 参数校验与
   server dry-run。
6. **诊断**：`argocd.diagnose`、`argowf.diagnose`，表驱动测试。
7. **桌面**：§4 全部界面；Playwright 截图检查（私有 harness）。
8. **kind 集成验证与文档**：`scripts/kind-argo.sh`（只操作 `cox-test`，显式 `--kubeconfig
   ~/.coxswain-test/kind/kubeconfig`）：安装 Argo CD 与 Argo Workflows（argo-server `--auth-mode=client`），
   用 port-forward 暴露；独立部署一个 Dex（静态用户）作为 Argo CD 的 OIDC，用来验证浏览器 SSO 登录；为 Workflows
   建一个最小权限 ServiceAccount 并 `kubectl create token` 取短期 token；示例：Synced / OutOfSync / Degraded
   app，成功 / 失败（exit 1）/ OOM workflow，带参数的 WorkflowTemplate。带 build tag `kind` 的 e2e 测试。
   README / SKILL.md 更新。

## 6. 验收（与用户一起，在 cox-test 集群）

1. 桌面添加 cox-test 的 Argo CD 实例：看到 Browser / Password / Token 三种方式；一次用 admin 账号密码登录，
   一次 Browser 弹出系统浏览器经 Dex 登录；Applications 列表状态正确。
2. 添加 Workflows 实例（URL + namespace + 手填 token），Test 通过，显示 token 的身份与到期时间。
3. 让 Claude 诊断 Degraded 的 app：给出根因与最近一次 sync 信息。
4. 让 Codex 对 OutOfSync 的 app 先 dry-run sync，再真正 sync，桌面看到状态变化。
5. 让 Kimi 诊断失败的 workflow，读到失败节点日志，并给出是否 retry 的建议。
6. 用户在桌面从 WorkflowTemplate 填参数提交 workflow，看到节点树推进。
7. 让 Claude 查询 cox-test Argo CD 的登录方式并发起浏览器登录：系统浏览器弹出，你完成 Dex 登录后
   Claude 继续工作；审计里看不到任何密码 / token；`cox argo export` 输出不含任何凭据。
8. （可选，由你决定）在你们公司的真实 Argo CD 上用 SSO 登录并只做读操作，确认公司 SSO 可用。
