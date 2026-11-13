import { useCallback, useEffect, useState, type ReactNode } from "react";
import {
  instanceIsProd, missingParams, paramDefaults, phaseClass, wfOpsFor,
  type ArgoInstance, type Diagnosis as DiagnosisOut, type NodeView, type Shaped, type TemplateView, type WFDetail, type WFList, type WFSummary,
} from "./argo";
import { DiagnosisView, InstanceHead, NeedsLogin } from "./ArgoCDView";
import type { Cox } from "./cox";
import { formatTime } from "./format";
import { activate } from "./keys";
import { Empty, HazardBand, Message, Modal } from "./ui";

const POLL_MS = 5000;
const PHASES = ["", "Running", "Failed", "Succeeded", "Pending", "Error"];

type Invoke = <T>(id: string, args: Record<string, unknown>) => Promise<T>;
type Tab = "workflows" | "templates";

/** ArgoWFView shows one Argo Workflows instance: runs, their steps and logs, and templates to submit. */
export function ArgoWFView({ cox, instance, active, onLogin, onRemove }: {
  cox: Cox;
  instance: ArgoInstance;
  active: boolean;
  onLogin(): void;
  onRemove(): void;
}) {
  const [namespace, setNamespace] = useState(instance.namespace ?? "argo");
  const invoke: Invoke = useCallback(
    async <T,>(id: string, args: Record<string, unknown>): Promise<T> => {
      const out = await cox.invoke<T>(id, { instance: instance.name, namespace, ...args });
      if (out.status !== "ok") throw new Error([out.error, out.hint].filter(Boolean).join(" — "));
      return out.result as T;
    },
    [cox, instance.name, namespace],
  );
  const [tab, setTab] = useState<Tab>("workflows");
  const [phase, setPhase] = useState("");
  const [list, setList] = useState<WFList | null>(null);
  const [selected, setSelected] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [opened, setOpened] = useState(false);
  const loggedIn = instance.login.logged_in;
  const prod = instanceIsProd(instance);

  const load = useCallback(async () => {
    try {
      setList(await invoke<WFList>("argowf.list", phase ? { phase } : {}));
      setError("");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, [invoke, phase]);
  useEffect(() => {
    if (active) setOpened(true);
  }, [active]);
  useEffect(() => {
    if (opened && loggedIn && tab === "workflows") void load();
  }, [opened, loggedIn, tab, load]);
  const running = (list?.workflows ?? []).some((w) => w.phase === "Running" || w.phase === "Pending");
  useEffect(() => {
    if (!active || !loggedIn || tab !== "workflows" || !running) return;
    const t = setInterval(() => void load(), POLL_MS);
    return () => clearInterval(t);
  }, [active, loggedIn, tab, running, load]);

  const submitted = (name: string) => {
    setNotice(`Submitted ${name}`);
    setTab("workflows");
    setPhase("");
    setSelected(name);
  };
  return (
    <div className="pane k8s" style={{ display: active ? "flex" : "none" }}>
      <InstanceHead instance={instance} onLogin={onLogin} onRemove={onRemove} />
      {!loggedIn ? (
        <NeedsLogin instance={instance} onLogin={onLogin} />
      ) : (
        <div className="k8s-body">
          <div className="k8s-main">
            <div className="toolbar">
              <div className="segmented inline" role="tablist" aria-label="View">
                <button role="tab" aria-selected={tab === "workflows"} onClick={() => setTab("workflows")}>Workflows</button>
                <button role="tab" aria-selected={tab === "templates"} onClick={() => setTab("templates")}>Templates</button>
              </div>
              <label className="field">Namespace
                <input className="mono ns-input" value={namespace} onChange={(e) => setNamespace(e.target.value)} onBlur={() => setSelected("")} />
              </label>
              {tab === "workflows" && (
                <label className="field">Phase
                  <select value={phase} onChange={(e) => setPhase(e.target.value)}>
                    {PHASES.map((p) => <option key={p} value={p}>{p || "any"}{p && list?.counts[p] !== undefined && !phase ? ` (${list.counts[p]})` : ""}</option>)}
                  </select>
                </label>
              )}
              <span className="spacer" />
              {tab === "workflows" && <button className="small" onClick={() => void load()}>Refresh</button>}
            </div>
            {notice && <Message kind="notice" onDismiss={() => setNotice("")}>{notice}</Message>}
            {error && <Message kind="error" onDismiss={() => setError("")}>{error}</Message>}
            {tab === "workflows" ? (
              <div className="table-wrap">
                <table className="k8s-table">
                  <thead><tr><th>name</th><th>phase</th><th>progress</th><th>template</th><th>started</th><th>duration</th><th>message</th></tr></thead>
                  <tbody>
                    {(list?.workflows ?? []).map((w) => (
                      <tr key={w.name} className={w.name === selected ? "clickable selected" : "clickable"} tabIndex={0} aria-selected={w.name === selected}
                        onClick={() => setSelected(w.name)} onKeyDown={activate(() => setSelected(w.name))}>
                        <td className="name">{w.name}</td>
                        <td><span className={"chip " + phaseClass(w.phase)}>{w.suspended ? "Suspended" : w.phase}</span></td>
                        <td className="mono">{w.progress}</td>
                        <td className="dim">{w.template}</td>
                        <td className="mono age">{formatTime(w.started_at)}</td>
                        <td className="mono">{w.duration}</td>
                        <td className="dim" title={w.message}>{w.message}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                {list && list.workflows.length === 0 && <Empty title="No workflows">{phase ? `None ${phase.toLowerCase()} in ${namespace}.` : `Nothing in namespace ${namespace}.`}</Empty>}
                {list?.more && <Message kind="note">Showing the newest {list.workflows.length}.</Message>}
                {!list && !error && <div className="loading">Loading workflows…</div>}
              </div>
            ) : (
              <Templates invoke={invoke} namespace={namespace} prod={prod} where={instance.name} onSubmitted={submitted} onError={setError} />
            )}
          </div>
          {tab === "workflows" && selected && (
            <WFDetailPanel key={namespace + "/" + selected} invoke={invoke} name={selected} prod={prod} where={instance.name}
              summary={list?.workflows.find((w) => w.name === selected)}
              onClose={() => setSelected("")} onDone={(msg) => { setNotice(msg); void load(); }} />
          )}
        </div>
      )}
    </div>
  );
}

type DetailTab = "steps" | "logs" | "diagnose";

function WFDetailPanel(props: { invoke: Invoke; name: string; summary?: WFSummary; prod: boolean; where: string; onClose(): void; onDone(msg: string): void }) {
  const { invoke, name } = props;
  const [d, setD] = useState<WFDetail | null>(null);
  const [tab, setTab] = useState<DetailTab>("steps");
  const [node, setNode] = useState<NodeView | null>(null);
  const [diagnosis, setDiagnosis] = useState<DiagnosisOut | null>(null);
  const [dialog, setDialog] = useState<ReactNode>(null);
  const [error, setError] = useState("");
  const show = (e: unknown) => setError(e instanceof Error ? e.message : String(e));
  const load = useCallback(() => invoke<WFDetail>("argowf.get", { name }).then(setD).catch(show), [invoke, name]);
  useEffect(() => void load(), [load, props.summary?.phase, props.summary?.progress]);
  useEffect(() => {
    if (tab === "diagnose" && !diagnosis) invoke<DiagnosisOut>("argowf.diagnose", { name }).then(setDiagnosis).catch(show);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tab]);
  const band = props.prod ? <HazardBand env="env=prod" where={props.where} /> : undefined;
  const op = (o: string) => {
    const go = () => {
      setDialog(null);
      invoke<WFSummary>(`argowf.${o}`, { name }).then(() => { props.onDone(`${o} ${name}`); void load(); setDiagnosis(null); }).catch(show);
    };
    if (o === "stop" || o === "terminate") {
      return setDialog(
        <Modal title={`${o === "stop" ? "Stop" : "Terminate"} ${name}?`} band={band} ok={o === "stop" ? "Stop" : "Terminate"} danger onOk={go} onCancel={() => setDialog(null)}>
          <p className="muted">{o === "stop" ? "Running steps stop, then exit handlers run." : "Running steps are killed; exit handlers do not run."} The run cannot be continued.</p>
        </Modal>,
      );
    }
    if (o === "retry") {
      return setDialog(
        <Modal title={`Retry ${name} from its failed steps?`} band={band} ok="Retry" onOk={go} onCancel={() => setDialog(null)}>
          <p className="muted">Diagnose first: a failure caused by the step itself fails again.</p>
        </Modal>,
      );
    }
    go();
  };
  const logsOf = (n: NodeView) => {
    setNode(n);
    setTab("logs");
  };
  const ops = props.summary ? wfOpsFor(props.summary) : d ? wfOpsFor(d) : [];
  return (
    <aside className="k8s-detail" aria-label={`Workflow ${name}`}>
      <div className="detail-head">
        <div className="titles">
          <div className="detail-kind">Workflow{d?.template ? ` · ${d.template}` : ""}</div>
          <div className="detail-name">{name}</div>
        </div>
        <button className="ghost icon" onClick={props.onClose} aria-label="Close details" title="Close details">×</button>
      </div>
      <div className="detail-actions-row">
        {ops.map((o) => (
          <button key={o} className={o === "stop" || o === "terminate" ? "small danger-text ghost" : "small"} onClick={() => op(o)}>{o[0].toUpperCase() + o.slice(1)}</button>
        ))}
      </div>
      <div className="segmented" role="tablist" aria-label="Details">
        <button role="tab" aria-selected={tab === "steps"} onClick={() => setTab("steps")}>Steps</button>
        <button role="tab" aria-selected={tab === "logs"} onClick={() => setTab("logs")}>Logs</button>
        <button role="tab" aria-selected={tab === "diagnose"} onClick={() => setTab("diagnose")}>Diagnose</button>
      </div>
      {error && <Message kind="error" onDismiss={() => setError("")}>{error}</Message>}
      <div className="detail-body" role="tabpanel">
        {tab === "steps" && (d ? <Steps d={d} onLogs={logsOf} /> : !error && <div className="loading">Loading…</div>)}
        {tab === "logs" && d && <StepLogs invoke={invoke} d={d} node={node} setNode={setNode} />}
        {tab === "diagnose" && (diagnosis ? <DiagnosisView d={diagnosis} /> : !error && <div className="loading">Diagnosing — failed steps, exit codes, logs…</div>)}
      </div>
      {dialog}
    </aside>
  );
}

function Steps({ d, onLogs }: { d: WFDetail; onLogs(n: NodeView): void }) {
  return (
    <div>
      <table className="kv"><tbody>
        <tr><th>phase</th><td><span className={"chip " + phaseClass(d.phase)}>{d.phase}</span> <span className="mono muted">{d.progress}</span></td></tr>
        {d.message && <tr><th>message</th><td>{d.message}</td></tr>}
        <tr><th>started</th><td className="mono">{formatTime(d.started_at)}{d.duration ? ` · ${d.duration}` : ""}</td></tr>
        {Object.entries(d.parameters ?? {}).map(([k, v]) => <tr key={k}><th>{k}</th><td className="mono">{v}</td></tr>)}
      </tbody></table>
      <h4 className="section-head">Steps</h4>
      <table className="compact steps"><tbody>
        {d.nodes.map((n) => (
          <tr key={n.id} className={n.type === "Pod" ? "clickable" : undefined} onClick={n.type === "Pod" ? () => onLogs(n) : undefined}
            tabIndex={n.type === "Pod" ? 0 : undefined} onKeyDown={n.type === "Pod" ? activate(() => onLogs(n)) : undefined}>
            <td style={{ paddingLeft: 8 + n.depth * 14 }}>
              <span className={n.type === "Pod" ? "mono" : "mono muted"}>{n.name}</span>
              {n.template && n.type === "Pod" && <span className="muted"> · {n.template}</span>}
              {n.message && <div className={n.phase === "Failed" || n.phase === "Error" ? "bad-text" : "muted"}>{n.message}</div>}
            </td>
            <td>{n.phase && <span className={"chip " + phaseClass(n.phase)}>{n.phase}</span>}</td>
            <td className="mono muted">{n.exit_code && n.exit_code !== "0" ? `exit ${n.exit_code}` : n.duration}</td>
          </tr>
        ))}
      </tbody></table>
      {!!d.nodes_omitted && <p className="muted">{d.nodes_omitted} more steps not shown.</p>}
    </div>
  );
}

function StepLogs({ invoke, d, node, setNode }: { invoke: Invoke; d: WFDetail; node: NodeView | null; setNode(n: NodeView): void }) {
  const pods = d.nodes.filter((n) => n.type === "Pod");
  const chosen = node ?? d.failed_nodes?.[0] ?? pods[pods.length - 1] ?? null;
  const [logs, setLogs] = useState<Shaped | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    if (!chosen) return;
    setLogs(null);
    invoke<{ logs: Shaped }>("argowf.logs", { name: d.name, node: chosen.id, tail: 500 })
      .then((o) => { setLogs(o.logs); setError(""); })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, [invoke, d.name, chosen?.id]); // eslint-disable-line react-hooks/exhaustive-deps
  if (!chosen) return <p className="muted">This workflow has no pod steps yet.</p>;
  return (
    <div className="logs">
      <div className="log-controls">
        <select aria-label="Step" value={chosen.id} onChange={(e) => setNode(pods.find((p) => p.id === e.target.value)!)}>
          {pods.map((p) => <option key={p.id} value={p.id}>{p.name} ({p.phase})</option>)}
        </select>
      </div>
      {error && <Message kind="error">{error}</Message>}
      <pre className="log-text">{logs ? logs.text || "No log lines (the pod may be gone)." : !error && "Loading…"}</pre>
    </div>
  );
}

function Templates({ invoke, namespace, prod, where, onSubmitted, onError }: {
  invoke: Invoke; namespace: string; prod: boolean; where: string; onSubmitted(name: string): void; onError(e: string): void;
}) {
  const [list, setList] = useState<TemplateView[] | null>(null);
  const [problems, setProblems] = useState<string[]>([]);
  const [submitting, setSubmitting] = useState<TemplateView | null>(null);
  const load = useCallback(() => {
    invoke<{ templates: TemplateView[]; problems?: string[] }>("argowf.templates", {})
      .then((o) => { setList(o.templates); setProblems(o.problems ?? []); })
      .catch((e) => onError(e instanceof Error ? e.message : String(e)));
  }, [invoke, onError]);
  useEffect(() => load(), [load]);
  const cron = (t: TemplateView) =>
    invoke("argowf.cron", { name: t.name, suspend: !t.suspended }).then(load).catch((e) => onError(String(e)));
  return (
    <div className="table-wrap">
      {problems.map((p) => <Message key={p} kind="note">{p}</Message>)}
      <table className="k8s-table">
        <thead><tr><th>name</th><th>kind</th><th>parameters / schedule</th><th></th></tr></thead>
        <tbody>
          {(list ?? []).map((t) => (
            <tr key={t.kind + t.name}>
              <td className="name" title={t.description}>{t.name}</td>
              <td className="dim">{t.kind === "ClusterWorkflowTemplate" ? "cluster template" : t.kind === "CronWorkflow" ? "cron" : "template"}</td>
              <td className="mono dim">
                {t.kind === "CronWorkflow"
                  ? `${t.schedule ?? ""}${t.suspended ? " · suspended" : ""}${t.last_run ? ` · last ${formatTime(t.last_run)}` : ""}`
                  : (t.parameters ?? []).map((p) => p.name).join(", ")}
              </td>
              <td className="row-actions">
                {t.kind === "CronWorkflow"
                  ? <button className="ghost small" onClick={() => void cron(t)}>{t.suspended ? "Resume" : "Suspend"}</button>
                  : <button className="ghost small" onClick={() => setSubmitting(t)}>Submit…</button>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {list && list.length === 0 && <Empty title="No templates">No workflow templates or cron workflows in {namespace}.</Empty>}
      {!list && <div className="loading">Loading templates…</div>}
      {submitting && (
        <SubmitDialog t={submitting} band={prod ? <HazardBand env="env=prod" where={where} /> : undefined}
          onOk={(params, dry) =>
            invoke<{ workflow: string }>("argowf.submit", { template: submitting.name, cluster_scope: submitting.kind === "ClusterWorkflowTemplate" || undefined, parameters: params, dry_run: dry || undefined })
              .then((o) => { setSubmitting(null); if (!dry) onSubmitted(o.workflow); else onError(""); })
              .catch((e) => { setSubmitting(null); onError(e instanceof Error ? e.message : String(e)); })
          }
          onCancel={() => setSubmitting(null)} />
      )}
    </div>
  );
}

function SubmitDialog({ t, band, onOk, onCancel }: { t: TemplateView; band?: ReactNode; onOk(params: Record<string, string>, dry: boolean): void; onCancel(): void }) {
  const [values, setValues] = useState(() => paramDefaults(t.parameters));
  const [dry, setDry] = useState(false);
  const missing = missingParams(t.parameters, values);
  return (
    <Modal title={`Submit ${t.name}`} band={band} ok={dry ? "Validate" : "Submit"} disabled={missing.length > 0} onOk={() => onOk(values, dry)} onCancel={onCancel}>
      {t.description && <p className="muted">{t.description}</p>}
      <div className="fields">
        {(t.parameters ?? []).map((p) => (
          <label key={p.name} className="field" title={p.description}>
            {p.name}
            {p.enum?.length ? (
              <select value={values[p.name]} onChange={(e) => setValues((v) => ({ ...v, [p.name]: e.target.value }))}>
                {p.enum.map((x) => <option key={x}>{x}</option>)}
              </select>
            ) : (
              <input className="mono" value={values[p.name] ?? ""} placeholder={p.value === undefined && p.default === undefined ? "required" : ""}
                onChange={(e) => setValues((v) => ({ ...v, [p.name]: e.target.value }))} />
            )}
          </label>
        ))}
        <label className="check"><input type="checkbox" checked={dry} onChange={(e) => setDry(e.target.checked)} /> Validate on the server only (dry run)</label>
      </div>
      {(t.parameters ?? []).length === 0 && <p className="muted">This template takes no parameters.</p>}
    </Modal>
  );
}
