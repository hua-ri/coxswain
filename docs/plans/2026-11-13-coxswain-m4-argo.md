# Coxswain M4 — Argo CD 与 Argo Workflows 设计与实施计划

> 日期：2026-11-13（v2：改为官方 REST API + 多实例；v1 的 CRD patch 方案作废）。前置：M3 + 新 shell。
> **For agentic workers:** TDD per task, one commit per task; after every commit `gofmt -l .` empty, `go vet ./...`,
> `go test -race ./...`, web typecheck/test/build green; `-race -count=20` on new packages. No tag/release.
> **RED LINE:** never read, write or switch the user's kubeconfig (`~/.kube/*`, `$KUBECONFIG`) or touch any non-cox
> cluster. Real-cluster tests only on kind clusters named `cox-*` with kubeconfigs under `~/.coxswain-test/kind/`,
> always with explicit `--kubeconfig`. Unit tests use httptest fakes of argocd-server / argo-server / OIDC.
> Never call the user's real Argo instances from tests; never store or log a credential the user pasted in chat.

## 原则

- **只走官方 API**：Argo CD 走 argocd-server REST API（`/api/v1/...`），Argo Workflows 走 argo-server REST API。
  不 patch CRD，不依赖集群访问，不引入 argo 的 Go 依赖（精简类型自己定义，JSON 解析）。
- **多实例**：Argo CD 与 Argo Workflows 各自是「实例」列表，一个实例 = 一个 URL。实例像主机 / 集群一样有标签
  （`env=prod`）和给 agent 的备注。
- **认证跟着各自的官方做法走，Coxswain 不内置任何公司的 SSO**：
  - Argo CD：模仿 `argocd login`（见 §1）。
  - Argo Workflows：配置 URL + namespace + token（见 §2），与 gops-iac `apis/argo-server-api` 一致。
- **凭据只给人管**：添加 / 修改 / 删除实例、登录 / 登出、填 token 都是 human-only；agent 只能列出实例和使用。
  凭据存 macOS 钥匙串（服务 `coxswain`，账号 `argocd/<instance>` / `argowf/<instance>`，cgo Security
  framework，不经命令行参数传递）；`~/.coxswain/argo.json` 只存非敏感配置。审计只记 instance 与 token
  sha256 前缀，密码永不进入审计、日志和 agent 输出。
- **可分享**：`cox argo export` 输出实例配置（不含凭据），同事 `cox argo import` 后各自登录。
- 风险标签：读 read；sync / refresh / suspend / resume / retry / resubmit / submit write；sync prune、rollback、
  terminate、stop、delete destructive。目标 `argocd:<instance>/<app>`、`argowf:<instance>/<ns>`，带实例标签。
- 给 agent 的输出精简：资源树摘要、失败节点、日志尾部 + artifact。

## 1. Argo CD 登录（模仿 argocd CLI）

`cox server` 执行登录（它是长驻进程，持有 token）；桌面的「Log in」按钮和终端里的 `cox argocd login <instance>`
都只是触发它。

1. `GET /api/v1/settings`（无需认证）→ 得知实例是否配置了 SSO（`dexConfig` / `oidcConfig`）。
2. **SSO**（与 `argocd login --sso` 相同）：OIDC 授权码 + PKCE；client id 用 settings 返回的
   `oidcConfig.cliClientID`，Dex 用 `argo-cd-cli`；Coxswain 调用 `open` 打开**系统浏览器**，本机回调
   `http://localhost:8085/auth/callback`（端口可按实例改，同 CLI 的 `--sso-port`）。因为 client id 与回调地址都与
   官方 CLI 一致，**凡是 `argocd login --sso` 能用的实例，Coxswain 就能用**，不需要公司额外配置。得到 id_token
   作为 Bearer，refresh_token 存钥匙串，过期前自动续期（同 CLI）。
3. **账号密码**（本地账号，如 admin；同 `argocd login --username`）：桌面表单 / 终端无回显输入 →
   `POST /api/v1/session` → JWT。密码不保存（CLI 也不保存），token 到期（默认 24h）后提示重新登录。
4. **粘贴 token**（兜底：本地账号 API token、project role token）。
5. 任一请求 401 → 实例状态变为「需要登录」，桌面在实例行上显示，agent 收到明确错误：
   `argocd instance "x" needs login; ask the user to log in in Coxswain`。
6. TLS：默认校验；实例可配 `ca_file` 或 `insecure_skip_verify`（显示警告）。

## 2. Argo Workflows 配置

- 实例：`{name, url, namespace（默认）, token, ca_file?, insecure_skip_verify?, tags, note}`。
- token 原样存钥匙串；用户粘贴带不带 `Bearer ` 前缀都可以（自动规范化），请求头 `Authorization: Bearer <token>`。
  这同时适用于 argo-server 的 client 模式（K8s token）和 sso 模式（从 argo UI 的 user-info 页复制的
  `v2:...`）；server 模式可以不填 token。
- 「Test」：`GET /api/v1/workflows/{ns}?listOptions.limit=1`，报告成功或具体错误（401 / 403 / TLS）。
- 若 token 是 JWT，解码（不校验）显示其身份与到期时间，例如「acts as `system:serviceaccount:kube-system:admin`，
  never expires」，让用户看清权限；到期前 / 401 时提示更新 token。

## 3. Actions

Argo CD：

| Action | 注解 | API |
|---|---|---|
| `argocd.instances` | read-only | 实例列表：name、url、tags、note、登录状态（不含凭据） |
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

实例管理（human-only，agent 调用时拒绝并提示问用户）：`argo.add` / `argo.update` / `argo.remove` /
`argocd.login` / `argocd.logout` / `argowf.set_token`；CLI `cox argocd add|login|logout|remove`、
`cox argowf add|token|remove`、`cox argo export|import`。

## 4. 桌面

- 侧边栏新增两个区：「Argo CD」与「Workflows」，行样式同 Hosts / Clusters（名字、env 标记）；需要登录或 token
  失效的实例在行尾显示 caution 色的 `log in` / `token`。「+」打开添加实例的编辑面板（同 Add host 的面板规范）：
  URL → 自动 `GET /api/v1/settings` → 显示「Log in with SSO」（若有）与账号密码表单 / 粘贴 token；Workflows
  面板是 URL、namespace、token、Test。
- Argo CD 实例：Applications 表（name、project、sync 徽章、health、revision、last sync）；详情：概览、资源树
  （按 kind 分组，非 Synced / 非 Healthy 突出）、历史、Diff、Events、Diagnose；操作：Sync（prune、dry run、
  指定资源）、Refresh / Hard refresh、Rollback（历史选择）、Terminate。prod 沿用 hazard band 与确认。
- Workflows 实例：子行 Workflows / Templates / Cron；Workflows 表（name、phase、progress、duration、started）；
  详情：节点树（缩进树，不画 DAG）、失败节点日志、参数、Diagnose；操作：Suspend / Resume、Stop、Terminate、
  Retry、Resubmit、Delete；Templates 详情：参数表单 → Submit（预览 → 确认）。
- Activity：argo 动作显示 `app/<name> sync prune=true`、`wf/<name> stop` 等。

## 5. 实施任务

1. **实例存储与凭据**：`internal/argo/instances`（argo.json、校验、标签）；钥匙串封装（cgo Security
   framework，测试用内存实现）；human-only 管理 actions + CLI；export / import（不含凭据）。
2. **Argo CD 客户端与登录**：HTTP 客户端（TLS 选项、401 → 需要登录状态）；`/api/v1/settings`；账号密码
   `/api/v1/session`；粘贴 token；SSO：PKCE、`open` 系统浏览器、本机回调（端口可配、占用时明确报错、5 分钟
   超时）、id_token / refresh_token 续期。测试：httptest 假 argocd-server + 假 OIDC provider（Dex 与外部
   OIDC 两种 client id 路径）、浏览器步骤由测试直接请求回调地址模拟。
3. **Argo CD 读操作**：instances、apps、app、tree、diff（Secret 脱敏）、events、logs。
4. **Argo CD 写操作**：refresh、sync（进行中 operation 冲突）、rollback（auto-sync 拒绝）、terminate；风险标签。
5. **Argo Workflows**：客户端（token 规范化、JWT 身份 / 到期解析）、Test；全部读写 actions；submit 参数校验与
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

1. 桌面添加 cox-test 的 Argo CD 实例：一次用 admin 账号密码登录，一次「Log in with SSO」弹出系统浏览器经
   Dex 登录；Applications 列表状态正确。
2. 添加 Workflows 实例（URL + namespace + token），Test 通过，显示 token 的身份与到期时间。
3. 让 Claude 诊断 Degraded 的 app：给出根因与最近一次 sync 信息。
4. 让 Codex 对 OutOfSync 的 app 先 dry-run sync，再真正 sync，桌面看到状态变化。
5. 让 Kimi 诊断失败的 workflow，读到失败节点日志，并给出是否 retry 的建议。
6. 用户在桌面从 WorkflowTemplate 填参数提交 workflow，看到节点树推进。
7. agent 试图添加实例 / 登录 / 设置 token → 被拒绝并提示问用户；`cox argo export` 输出不含任何凭据。
8. （可选，由你决定）在你们公司的真实 Argo CD 上用 SSO 登录并只做读操作，确认公司 SSO 可用。
