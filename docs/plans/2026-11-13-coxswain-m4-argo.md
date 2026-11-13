# Coxswain M4 — Argo CD 与 Argo Workflows 设计与实施计划

> 日期：2026-11-13。前置：M3（Kubernetes）+ 新 shell（one source list）。
> **For agentic workers:** TDD per task, one commit per task; after every commit `gofmt -l .` empty, `go vet ./...`,
> `go test -race ./...`, web typecheck/test/build green; `-race -count=20` on new packages. No push/tag/release.
> **RED LINE:** never read, write or switch the user's kubeconfig (`~/.kube/*`, `$KUBECONFIG`) or touch any non-cox
> cluster. Real-cluster tests only on kind clusters named `cox-*` with kubeconfigs under `~/.coxswain-test/kind/`,
> always with explicit `--kubeconfig`. Unit tests use fakes/httptest under `k8stest.Main`.

## 原则（沿用）

- 读 Argo 资源走 kube API（复用 M3 的 cluster client，无需额外凭据）：Argo CD `Application`/`AppProject`，Argo
  Workflows `Workflow`/`WorkflowTemplate`/`ClusterWorkflowTemplate`/`CronWorkflow`。
- 写操作优先 patch CRD 的官方触发字段（Argo CD `operation`、Workflow `spec.suspend`/`spec.shutdown`），
  只有确实需要 API server 的操作（Workflow retry/resubmit、Argo CD rollback 的部分场景）才调用 argocd-server /
  argo-server 的 REST API（地址与 token 由用户在 Coxswain 中配置，token 存 macOS 钥匙串或 `~/.coxswain` 0600，
  human-only 配置）。没有配置时，这些操作明确报错并说明需要配置什么。
- 风险标签：读 read；sync / refresh / suspend / resume write；sync with prune、rollback、terminate、stop、
  delete destructive；目标 `k8s:<ctx>/<ns>` 带 context 标签（env=prod）。
- 给 agent 的输出精简：资源树摘要、失败节点、日志尾部 + artifact。

## 1. Argo CD

| Action | 注解 | 说明 |
|---|---|---|
| `argocd.apps` | read-only | `{context, namespace?=argocd, project?, selector?}` → name、project、repo/path/revision、目标集群/namespace、sync status、health、最近 sync 时间与结果 |
| `argocd.app` | read-only | 单个 Application：source(s)、destination、sync policy、conditions、operationState（phase、message、resources 结果）、`status.history`（最近 10 次：id、revision、deployedAt） |
| `argocd.diff` | read-only | 每个 managed resource 的 OutOfSync 原因：kind/ns/name、status、health、requiresPruning；有 argocd-server 配置时取 live/target diff（unified，Secret 值脱敏），否则说明需要配置 |
| `argocd.sync` | write（prune → destructive） | patch `operation.sync {revision?, prune?, dryRun?, resources?[]}`；若已有进行中的 operation 则拒绝并说明；返回 operation 已提交，可用 `argocd.app` 轮询 |
| `argocd.refresh` | write | annotation `argocd.argoproj.io/refresh: normal|hard` |
| `argocd.rollback` | destructive | 需要 argocd-server（`/api/v1/applications/{name}/rollback`，id 来自 history）；auto-sync 开启时拒绝并提示 |
| `argocd.terminate` | destructive | 终止进行中的 operation（argocd-server API；否则清空 `operation` 字段并说明效果） |
| `argocd.diagnose` | read-only | 规则：OutOfSync 原因（手动改动 / 新 revision 未同步 / prune 待处理）、Degraded → 下钻到不健康资源（Deployment/Pod 复用 `k8s.diagnose`）、sync 失败 → hook/资源错误信息与相关事件、ComparisonError/InvalidSpecError 条件；输出 findings + evidence + next_steps |

- 关联：Pod / Deployment → Application（`app.kubernetes.io/instance` 或 `argocd.argoproj.io/tracking-id` 注解）。
  `k8s.diagnose` 结果增加 `argocd_app`（若能关联），提示「发布后出问题」时看 app 最近一次 sync。

## 2. Argo Workflows

| Action | 注解 | 说明 |
|---|---|---|
| `argowf.list` | read-only | `{context, namespace, phase?, label_selector?, limit=50}` → name、phase、progress、started/finished、duration、message |
| `argowf.get` | read-only | 节点树（精简：id、displayName、type、phase、message、startedAt/finishedAt、exitCode、podName、templateName），失败节点优先 |
| `argowf.logs` | read-only | 节点 → Pod（`podName` / 节点 id 推导）→ `main` 容器日志尾部（复用 `k8s.logs`，shaper） |
| `argowf.templates` | read-only | WorkflowTemplate / ClusterWorkflowTemplate / CronWorkflow 列表与参数定义 |
| `argowf.submit` | write | 从 WorkflowTemplate 提交：`{template, parameters{}, generate_name?}`；先校验参数（必填 / enum），返回预览 YAML，`dry_run=true` 只返回预览（服务端 dry-run create） |
| `argowf.suspend` / `argowf.resume` | write | patch `spec.suspend` |
| `argowf.stop` / `argowf.terminate` | destructive | patch `spec.shutdown: Stop|Terminate` |
| `argowf.retry` / `argowf.resubmit` | write | 需要 argo-server API；未配置时说明 |
| `argowf.cron` | write | CronWorkflow 暂停 / 恢复（`spec.suspend`） |
| `argowf.diagnose` | read-only | 第一个失败节点 → 退出码与日志尾部 → Pod 事件 / 节点状况（OOM、驱逐、拉镜像失败）→ 是否适合直接 retry 的建议 |

## 3. 桌面

- Clusters 下每个 context 的 kinds 列表增加分组「Argo CD」（Applications、Projects）与「Workflows」
  （Workflows、Templates、Cron），只在集群里存在对应 CRD 时显示（discovery）。
- Applications 表：name、project、sync（Synced / OutOfSync 徽章）、health（Healthy / Progressing / Degraded /
  Missing）、revision、last sync；详情：概览、资源树（按 kind 分组，非 Synced / 非 Healthy 突出）、历史、
  Diff、Diagnose；操作：Sync（选项：prune、dry run、指定资源）、Refresh / Hard refresh、Rollback（历史选择）、
  Terminate。prod 确认沿用 HazardBand。
- Workflows 表：name、phase、progress、duration、started；详情：节点树（DAG/steps 用缩进树，不做图形 DAG）、
  失败节点日志、参数、Diagnose；操作：Suspend/Resume、Stop、Terminate、Retry、Resubmit。Templates 详情：参数
  表单 → Submit（预览 YAML → 确认）。
- Activity：argo 动作的 Details 显示 `app/<name> sync prune=true`、`wf/<name> stop` 等。

## 4. 配置（argocd-server / argo-server）

- `cox k8s argo configure {context, argocd_url?, argocd_token?, argo_url?, argo_token?, insecure_skip_verify?}`：
  human-only；token 存 macOS 钥匙串（服务名 `coxswain`，账号 `<context>/argocd`），失败时退回
  `~/.coxswain/secrets/` 0600；审计只记录 url 与 token sha256 前缀。也可选择「通过 port-forward 访问」：
  配置 service 名（如 `argocd/argocd-server:443`），Coxswain 用 k8s port-forward 隧道临时连接。
- 桌面集群页「Argo」设置区：URL / 通过 port-forward / token（只写不读，显示 已配置 / 未配置）、Test。

## 5. 实施任务

1. **CRD 读取与资源发现**：`internal/argo`（Application / Workflow 等精简类型，从 unstructured 解析，不引入 argo
   依赖）；discovery 判断是否安装；`argocd.apps`、`argocd.app`、`argowf.list`、`argowf.get`、`argowf.templates`；
   fake dynamic client 测试。
2. **CRD 写操作**：`argocd.sync`、`argocd.refresh`、`argowf.suspend/resume/stop/terminate`、`argowf.cron`、
   `argowf.submit`（服务端 dry-run 预览）；风险标签；进行中 operation 冲突。
3. **API server 访问层**：配置存储（钥匙串 + 0600 回退）、human-only configure、port-forward 访问、
   `argocd.diff`、`argocd.rollback`、`argocd.terminate`、`argowf.retry/resubmit`；httptest 模拟 REST API。
4. **诊断与关联**：`argocd.diagnose`、`argowf.diagnose`、`argowf.logs`；`k8s.diagnose` 关联 argocd app。
5. **桌面**：§3 全部界面。
6. **kind 集成验证与文档**：脚本 `scripts/kind-argo.sh`（只操作 `cox-test` 集群，`--kubeconfig
   ~/.coxswain-test/kind/kubeconfig`）：安装 Argo CD 与 Argo Workflows（镜像先本地拉取后 `ctr import`，避免集群内
   拉取失败），部署示例：一个 Synced app、一个 OutOfSync app（手动改动）、一个 Degraded app（CrashLoop）、一个
   成功 workflow、一个失败 workflow（exit 1）、一个 OOM workflow、一个带参数的 WorkflowTemplate；带 build tag
   `kind` 的 e2e 测试。README / SKILL.md 更新。

## 6. 验收（与用户一起，在 cox-test 集群）

1. 桌面 Clusters → kind-cox-test 出现 Argo CD / Workflows 分组，Applications 与 Workflows 列表状态正确。
2. 让 Claude 诊断 Degraded 的 app：下钻到 CrashLoop 的 Pod，给出根因与「最近一次 sync」信息。
3. 让 Codex 对 OutOfSync 的 app 先 dry-run sync，再真正 sync，桌面看到状态变化。
4. 让 Kimi 诊断失败的 workflow，读到失败节点日志，并给出是否 retry 的建议。
5. 用户在桌面从 WorkflowTemplate 填参数提交一个 workflow，看到节点树推进。
6. agent 试图配置 argocd token → 被拒绝并提示问用户。
