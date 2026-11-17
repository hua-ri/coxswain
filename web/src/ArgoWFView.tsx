import { useCallback, useEffect, useState } from "react";
import { phaseClass, workflowURL, type ArgoInstance, type Diagnosis as DiagnosisOut, type WFDetail, type WFList, type WFSummary } from "./argo";
import { DiagnosisView, InstanceHead, NeedsLogin, useInvoke } from "./ArgoCDView";
import type { Cox } from "./cox";
import { formatTime } from "./format";
import { activate } from "./keys";
import { Empty, Message } from "./ui";

const POLL_MS = 5000;
const PHASES = ["", "Running", "Failed", "Succeeded", "Pending", "Error"];

type Invoke = <T>(id: string, args: Record<string, unknown>) => Promise<T>;

/** ArgoWFView lists one Argo Workflows instance's runs. Coxswain shows their state and diagnoses failures;
 * the Argo UI is one click away for graphs, logs and templates. */
export function ArgoWFView({ cox, instance, active, onLogin, onEdit, onRemove }: {
  cox: Cox;
  instance: ArgoInstance;
  active: boolean;
  onLogin(): void;
  onEdit(): void;
  onRemove(): void;
}) {
  const [namespace, setNamespace] = useState(instance.namespace ?? "argo");
  const [nsText, setNsText] = useState(namespace);
  const invoke = useInvoke(cox, instance.name, { namespace });
  const [phase, setPhase] = useState("");
  const [list, setList] = useState<WFList | null>(null);
  const [selected, setSelected] = useState("");
  const [error, setError] = useState("");
  const [opened, setOpened] = useState(false);
  const loggedIn = instance.login.logged_in;
  const open = (url: string) => void cox.openURL(url).catch((e) => setError(String(e)));
  const applyNs = () => {
    const ns = nsText.trim();
    if (ns && ns !== namespace) {
      setSelected("");
      setNamespace(ns);
    }
  };

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
    if (opened && loggedIn) void load();
  }, [opened, loggedIn, load]);
  const running = (list?.workflows ?? []).some((w) => w.phase === "Running" || w.phase === "Pending");
  useEffect(() => {
    if (!active || !loggedIn || !running) return;
    const t = setInterval(() => void load(), POLL_MS);
    return () => clearInterval(t);
  }, [active, loggedIn, running, load]);

  const sel = list?.workflows.find((w) => w.name === selected);
  return (
    <div className="pane k8s" style={{ display: active ? "flex" : "none" }}>
      <InstanceHead instance={instance} onLogin={onLogin} onEdit={onEdit} onRemove={onRemove} onOpen={() => open(workflowURL(instance, namespace))} />
      {!loggedIn ? (
        <NeedsLogin instance={instance} onLogin={onLogin} />
      ) : (
        <div className="k8s-body">
          <div className="k8s-main">
            <div className="toolbar">
              <label className="field">Namespace
                <input className="mono ns-input" value={nsText} onChange={(e) => setNsText(e.target.value)} onBlur={applyNs}
                  onKeyDown={(e) => e.key === "Enter" && applyNs()} />
              </label>
              <label className="field">Phase
                <select value={phase} onChange={(e) => setPhase(e.target.value)}>
                  {PHASES.map((p) => <option key={p} value={p}>{p || "any"}{p && list?.counts[p] !== undefined && !phase ? ` (${list.counts[p]})` : ""}</option>)}
                </select>
              </label>
              <span className="spacer" />
              <button className="small" onClick={() => void load()}>Refresh</button>
            </div>
            {error && <Message kind="error" onDismiss={() => setError("")}>{error}</Message>}
            <div className="table-wrap">
              <table className="k8s-table">
                <thead><tr><th>name</th><th>phase</th><th>progress</th><th>template</th><th>started</th><th>duration</th><th>message</th></tr></thead>
                <tbody>
                  {(list?.workflows ?? []).map((w) => (
                    <tr key={w.name} className={w.name === selected ? "clickable selected" : "clickable"} tabIndex={0} aria-selected={w.name === selected}
                      title="Double-click to open in Argo"
                      onClick={() => setSelected(w.name)} onDoubleClick={() => open(workflowURL(instance, namespace, w.name))} onKeyDown={activate(() => setSelected(w.name))}>
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
          </div>
          {sel && (
            <WFPanel key={namespace + "/" + sel.name} invoke={invoke} wf={sel}
              onOpen={() => open(workflowURL(instance, namespace, sel.name))} onClose={() => setSelected("")} />
          )}
        </div>
      )}
    </div>
  );
}

/** WFPanel is a run's state and its failed steps at a glance, and its diagnosis. */
function WFPanel({ invoke, wf, onOpen, onClose }: { invoke: Invoke; wf: WFSummary; onOpen(): void; onClose(): void }) {
  const [d, setD] = useState<WFDetail | null>(null);
  const [diagnosis, setDiagnosis] = useState<DiagnosisOut | null>(null);
  const [diagnosing, setDiagnosing] = useState(false);
  const [error, setError] = useState("");
  const show = (e: unknown) => setError(e instanceof Error ? e.message : String(e));
  useEffect(() => {
    invoke<WFDetail>("argowf.get", { name: wf.name }).then(setD).catch(show);
  }, [invoke, wf.name, wf.phase, wf.progress]);
  const diagnose = () => {
    setDiagnosing(true);
    setDiagnosis(null);
    invoke<DiagnosisOut>("argowf.diagnose", { name: wf.name }).then(setDiagnosis).catch(show).finally(() => setDiagnosing(false));
  };
  const failed = wf.phase === "Failed" || wf.phase === "Error";
  return (
    <aside className="k8s-detail" aria-label={`Workflow ${wf.name}`}>
      <div className="detail-head">
        <div className="titles">
          <div className="detail-kind">Workflow{wf.template ? ` · ${wf.template}` : ""}</div>
          <div className="detail-name">{wf.name}</div>
        </div>
        <button className="ghost icon" onClick={onClose} aria-label="Close details" title="Close details">×</button>
      </div>
      <div className="detail-actions-row">
        <button className="small primary" onClick={onOpen}>Open in Argo ↗</button>
        {failed && <button className="small" onClick={diagnose} disabled={diagnosing}>{diagnosing ? "Diagnosing…" : "Diagnose"}</button>}
      </div>
      {error && <Message kind="error" onDismiss={() => setError("")}>{error}</Message>}
      <div className="detail-body">
        {diagnosis ? (
          <DiagnosisView d={diagnosis} />
        ) : diagnosing ? (
          <div className="loading">Diagnosing — failed steps, exit codes, logs, the pod and its node…</div>
        ) : d ? (
          <>
            <table className="kv"><tbody>
              <tr><th>phase</th><td><span className={"chip " + phaseClass(d.phase)}>{d.phase}</span> <span className="mono muted">{d.progress}</span></td></tr>
              {d.message && <tr><th>message</th><td>{d.message}</td></tr>}
              <tr><th>started</th><td className="mono">{formatTime(d.started_at)}{d.duration ? ` · ${d.duration}` : ""}</td></tr>
              {Object.entries(d.parameters ?? {}).map(([k, v]) => <tr key={k}><th>{k}</th><td className="mono">{v}</td></tr>)}
            </tbody></table>
            {(d.failed_nodes ?? []).length > 0 && (
              <>
                <h4 className="section-head">Failed steps</h4>
                {d.failed_nodes!.map((n) => (
                  <div key={n.id} className="finding bad">
                    <div className="finding-head"><span className="finding-title mono">{n.name}</span><span className="muted">{n.template}</span></div>
                    <div className="finding-detail">{n.message}{n.exit_code && n.exit_code !== "0" && !(n.message ?? "").includes(n.exit_code) ? ` (exit ${n.exit_code})` : ""}</div>
                  </div>
                ))}
              </>
            )}
            <p className="muted hint-line">The graph, every step’s logs and the templates are in the Argo UI.</p>
          </>
        ) : (
          !error && <div className="loading">Loading…</div>
        )}
      </div>
    </aside>
  );
}
