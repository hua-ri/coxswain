import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import type { Cox } from "./cox";
import {
  ActionError,
  applyArgs,
  conflictsOf,
  takeoverText,
  type Conflict,
  contextBadges,
  diffLines,
  envOf,
  forwardRequest,
  listArgs,
  ALL_NAMESPACES as ALL,
  isProdContext,
  KINDS,
  podContainers,
  rowActions,
  rowObject,
  scaleWarning,
  severityClass,
  statusClass,
  type ApplyOutput,
  type ContextInfo,
  type DescribeOutput,
  type DiagnoseOutput,
  type EventRow,
  type GetOutput,
  type RolloutOutput,
  type RowAction,
} from "./k8s";

/** How often the list refreshes (while the tab shows and auto is on), and logs follow. */
const LIST_POLL_MS = 10000;
const LOGS_POLL_MS = 3000;

export interface PodRef {
  context: string;
  namespace: string;
  pod: string;
  container?: string;
}

interface Props {
  cox: Cox;
  context: ContextInfo;
  active: boolean;
  onOpenTerminal(p: PodRef & { shell?: string }): Promise<void>;
  onForward(body: Record<string, unknown>): Promise<string>; // resolves to where it listens
}

interface Selected {
  name: string;
  namespace: string;
}

type DetailTab = "overview" | "yaml" | "events" | "logs" | "diagnose";

/** Dialog is an in-page modal with a context's env badge. */
function Dialog(props: {
  title: string;
  badge: string;
  children?: ReactNode;
  ok: string;
  danger?: boolean;
  disabled?: boolean;
  onOk(): void;
  onCancel(): void;
}) {
  return (
    <div className="overlay" onClick={props.onCancel}>
      <div className="dialog" onClick={(e) => e.stopPropagation()}>
        <p>
          {props.badge && <span className="badge danger">{props.badge}</span>} {props.title}
        </p>
        {props.children}
        <div className="buttons">
          <button onClick={props.onCancel} autoFocus>
            Cancel
          </button>
          <button className={props.danger ? "danger" : ""} disabled={props.disabled} onClick={props.onOk}>
            {props.ok}
          </button>
        </div>
      </div>
    </div>
  );
}

function Events({ events }: { events: EventRow[] }) {
  if (events.length === 0) return <p className="muted">No events.</p>;
  return (
    <table>
      <tbody>
        {events.map((e, i) => (
          <tr key={i}>
            <td className="muted">{e.age}</td>
            <td className={e.type === "Warning" ? "warn-text" : ""}>{e.reason}</td>
            <td>{e.object}</td>
            <td>{e.message}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

/** K8sView browses one context: lists, details, logs, diagnosis, changes. */
export function K8sView({ cox, context, active, onOpenTerminal, onForward }: Props) {
  const ctx = context.name;
  const [namespace, setNamespace] = useState(context.namespace || "default");
  const [namespaces, setNamespaces] = useState<string[]>([]);
  const [kind, setKind] = useState("pods");
  const [typedKind, setTypedKind] = useState("");
  const [list, setList] = useState<GetOutput | null>(null);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [auto, setAuto] = useState(true);
  const [selected, setSelected] = useState<Selected | null>(null);
  const [tab, setTab] = useState<DetailTab>("overview");
  const [applying, setApplying] = useState(false);
  const [dialog, setDialog] = useState<ReactNode>(null);
  const [opened, setOpened] = useState(false);
  const badge = isProdContext(context) ? envOf(context) : "";

  const invoke = useCallback(
    async <T,>(id: string, args: Record<string, unknown>): Promise<T> => {
      const out = await cox.invoke<T>(id, { context: ctx, ...args });
      if (out.status !== "ok") throw new ActionError(out);
      return out.result as T;
    },
    [cox, ctx],
  );
  const load = useCallback(async () => {
    try {
      const out = await invoke<GetOutput>("k8s.get", listArgs(kind, namespace));
      setList(out);
      setError("");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, [invoke, kind, namespace]);

  useEffect(() => {
    if (active) setOpened(true);
  }, [active]);
  useEffect(() => {
    if (!opened) return;
    invoke<GetOutput>("k8s.get", { kind: "namespaces" })
      .then((o) => setNamespaces((o.rows ?? []).map((r) => r[0])))
      .catch(() => {});
  }, [opened, invoke]);
  useEffect(() => {
    if (opened) void load();
  }, [opened, load]);
  useEffect(() => {
    if (!active || !auto) return;
    const timer = setInterval(() => void load(), LIST_POLL_MS);
    return () => clearInterval(timer);
  }, [active, auto, load]);

  const choose = (k: string) => {
    setKind(k);
    setSelected(null);
    setList(null);
  };

  const done = (msg: string) => {
    setDialog(null);
    setNotice(msg);
    void load();
  };
  const fail = (e: unknown) => {
    setDialog(null);
    setError(e instanceof Error ? e.message : String(e));
  };

  const act = (a: RowAction, obj: Selected) => {
    const objKind = list?.kind ?? kind;
    const ref = { kind: objKind, name: obj.name, namespace: obj.namespace };
    const where = `${objKind} ${obj.name} in ${ctx}/${obj.namespace}`;
    switch (a) {
      case "logs":
        setSelected(obj);
        setTab("logs");
        return;
      case "terminal":
        setDialog(<TerminalDialog badge={badge} pod={obj} invoke={invoke}
          onOpen={(container, shell) => {
            setDialog(null);
            onOpenTerminal({ context: ctx, namespace: obj.namespace, pod: obj.name, container, shell }).catch(fail);
          }}
          onCancel={() => setDialog(null)} />);
        return;
      case "forward":
        setDialog(<ForwardDialog badge={badge} where={where}
          onOk={(remote, local) =>
            onForward(forwardRequest(ctx, obj.namespace, obj.name, remote, local))
              .then((listen) => done(`Forwarding ${listen} → ${obj.name}:${remote} (see Tunnels)`))
              .catch(fail)
          }
          onCancel={() => setDialog(null)} />);
        return;
      case "delete":
        setDialog(<Dialog badge={badge} title={`Delete ${where}?`} ok="Delete" danger
          onOk={() => invoke("k8s.delete", ref).then(() => done(`Deleted ${objKind} ${obj.name}`)).catch(fail)}
          onCancel={() => setDialog(null)} />);
        return;
      case "restart":
        setDialog(<Dialog badge={badge} title={`Restart ${where}? Its pods are replaced one by one.`} ok="Restart" danger
          onOk={() => invoke("k8s.rollout", { ...ref, action: "restart" }).then(() => done(`Restarting ${obj.name}`)).catch(fail)}
          onCancel={() => setDialog(null)} />);
        return;
      case "scale":
        setDialog(<ScaleDialog badge={badge} where={where}
          onOk={(n) => invoke("k8s.scale", { ...ref, replicas: n }).then(() => done(`Scaled ${obj.name} to ${n}`)).catch(fail)}
          onCancel={() => setDialog(null)} />);
        return;
      case "rollback":
        invoke<RolloutOutput>("k8s.rollout", { ...ref, action: "status" })
          .then((st) =>
            setDialog(<RollbackDialog badge={badge} where={where} status={st}
              onOk={(rev) =>
                invoke("k8s.rollout", { ...ref, action: "undo", to_revision: rev }).then(() => done(`Rolled ${obj.name} back to revision ${rev}`)).catch(fail)
              }
              onCancel={() => setDialog(null)} />),
          )
          .catch(fail);
        return;
    }
  };

  const columns = list?.columns ?? [];
  const actions = rowActions(list?.kind ?? "");
  return (
    <div className="pane k8s" style={{ display: active ? "flex" : "none" }}>
      <div className="k8s-kinds">
        <div className="k8s-context">
          <strong>{ctx}</strong>
          <div className="tags">
            {contextBadges(context).map((b) => (
              <span key={b.text} className={b.danger ? "badge danger" : "badge"} title={b.inherited ? `from ${context.inherited_from?.join(", ")}` : undefined}>
                {b.text}
                {b.inherited && " ↑"}
              </span>
            ))}
          </div>
          {context.note && <div className="muted note-text">{context.note}</div>}
        </div>
        {KINDS.map((k) => (
          <button key={k.kind} className={kind === k.kind && !applying ? "kind active" : "kind"} onClick={() => { setApplying(false); choose(k.kind); }}>
            {k.label}
          </button>
        ))}
        <form onSubmit={(e) => { e.preventDefault(); if (typedKind.trim()) { setApplying(false); choose(typedKind.trim()); } }}>
          <input placeholder="other kind…" value={typedKind} onChange={(e) => setTypedKind(e.target.value)} />
        </form>
        <button className={applying ? "kind active" : "kind"} onClick={() => setApplying(true)}>Apply YAML…</button>
      </div>
      <div className="k8s-main">
        {applying ? (
          <ApplyPanel invoke={invoke} badge={badge} ctx={ctx} />
        ) : (
          <>
            <div className="filters">
              <label>
                Namespace{" "}
                <select value={namespace} onChange={(e) => { setNamespace(e.target.value); setSelected(null); }}>
                  <option value={ALL}>all</option>
                  {[...new Set([namespace, ...namespaces])].filter((n) => n !== ALL).map((n) => (
                    <option key={n} value={n}>{n}</option>
                  ))}
                </select>
              </label>
              <label>
                <input type="checkbox" checked={auto} onChange={(e) => setAuto(e.target.checked)} /> auto-refresh
              </label>
              <button className="small" onClick={() => void load()}>Refresh</button>
              {notice && <span className="muted" onClick={() => setNotice("")}>{notice}</span>}
            </div>
            {error && <div className="error-box">{error}</div>}
            {(list?.more || list?.truncated) && (
              <div className="muted notice-line">
                Showing {list.rows?.length ?? 0} of {list.more ? "more than " : ""}{list.total} — narrow it with a namespace.
              </div>
            )}
            <div className="table-wrap">
              <table className="k8s-table">
                <thead>
                  <tr>
                    {columns.map((c) => <th key={c}>{c}</th>)}
                    {actions.length > 0 && <th />}
                  </tr>
                </thead>
                <tbody>
                  {(list?.rows ?? []).map((row) => {
                    const obj = rowObject(columns, row, list?.namespace || namespace);
                    const sel = selected?.name === obj.name && selected.namespace === obj.namespace;
                    return (
                      <tr key={obj.namespace + "/" + obj.name} className={sel ? "selected" : ""} onClick={() => { setSelected(obj); if (tab === "logs" && list?.kind !== "Pod") setTab("overview"); }}>
                        {row.map((cell, i) => {
                          const cls = statusClass(columns[i], cell);
                          return <td key={i}>{cls ? <span className={"status " + cls}>{cell}</span> : cell}</td>;
                        })}
                        {actions.length > 0 && (
                          <td className="row-actions" onClick={(e) => e.stopPropagation()}>
                            {actions.map((a) => (
                              <button key={a} className="small" onClick={() => act(a, obj)}>{ACTION_LABEL[a]}</button>
                            ))}
                          </td>
                        )}
                      </tr>
                    );
                  })}
                </tbody>
              </table>
              {list && (list.rows ?? []).length === 0 && <p className="muted">No {list.kind} here.</p>}
            </div>
          </>
        )}
      </div>
      {selected && !applying && list && (
        <Detail key={`${list.kind}/${selected.namespace}/${selected.name}`} invoke={invoke} kind={list.kind} obj={selected}
          tab={tab} setTab={setTab} active={active} onClose={() => setSelected(null)} />
      )}
      {dialog}
    </div>
  );
}

const ACTION_LABEL: Record<RowAction, string> = {
  logs: "Logs", terminal: "Terminal", forward: "Forward", delete: "Delete", restart: "Restart", scale: "Scale", rollback: "Rollback",
};

type Invoke = <T>(id: string, args: Record<string, unknown>) => Promise<T>;

const DIAGNOSABLE: Record<string, string> = { Pod: "pod", Deployment: "deployment", StatefulSet: "statefulset", DaemonSet: "daemonset", Job: "job" };

function Detail(props: { invoke: Invoke; kind: string; obj: Selected; tab: DetailTab; setTab(t: DetailTab): void; active: boolean; onClose(): void }) {
  const { invoke, kind, obj, tab, setTab, active } = props;
  const ref = { kind, name: obj.name, namespace: obj.namespace };
  const [describe, setDescribe] = useState<DescribeOutput | null>(null);
  const [yaml, setYaml] = useState("");
  const [diagnosis, setDiagnosis] = useState<DiagnoseOutput | null>(null);
  const [error, setError] = useState("");
  const show = (e: unknown) => setError(e instanceof Error ? e.message : String(e));

  useEffect(() => {
    invoke<DescribeOutput>("k8s.describe", ref).then(setDescribe).catch(show);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  useEffect(() => {
    if (tab === "yaml" && !yaml) invoke<GetOutput>("k8s.get", ref).then((o) => setYaml(o.yaml?.text ?? "")).catch(show);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tab]);
  const diagnose = () => {
    setTab("diagnose");
    setDiagnosis(null);
    invoke<DiagnoseOutput>("k8s.diagnose", { ...ref, kind: DIAGNOSABLE[kind] }).then(setDiagnosis).catch(show);
  };

  const tabs: DetailTab[] = ["overview", "yaml", "events", ...(kind === "Pod" ? (["logs"] as DetailTab[]) : [])];
  return (
    <div className="k8s-detail">
      <div className="detail-head">
        <strong>{kind} {obj.name}</strong>
        <span className="close" onClick={props.onClose} title="Close">×</span>
      </div>
      <div className="detail-tabs">
        {tabs.map((t) => (
          <button key={t} className={tab === t ? "small active" : "small"} onClick={() => setTab(t)}>{t}</button>
        ))}
        {DIAGNOSABLE[kind] && <button className={tab === "diagnose" ? "small active" : "small"} onClick={diagnose}>Diagnose</button>}
      </div>
      {error && <div className="error-box">{error}</div>}
      <div className="detail-body">
        {tab === "overview" && describe && <Overview d={describe} />}
        {tab === "yaml" && <pre className="mono yaml">{yaml || "…"}</pre>}
        {tab === "events" && describe && <Events events={describe.events} />}
        {tab === "logs" && kind === "Pod" && <Logs invoke={invoke} obj={obj} containers={podContainers(describe)} active={active} />}
        {tab === "diagnose" && (diagnosis ? <Diagnosis d={diagnosis} /> : <p className="muted">Diagnosing…</p>)}
      </div>
    </div>
  );
}

function Overview({ d }: { d: DescribeOutput }) {
  return (
    <div>
      <table>
        <tbody>
          {Object.entries(d.summary).map(([k, v]) => {
            const cls = statusClass(k, v);
            return <tr key={k}><th>{k}</th><td>{cls ? <span className={"status " + cls}>{v}</span> : v}</td></tr>;
          })}
          {d.owners && <tr><th>owners</th><td>{d.owners.join(", ")}</td></tr>}
          {d.keys && <tr><th>keys</th><td className="mono">{d.keys.join(" ")}</td></tr>}
        </tbody>
      </table>
      {d.conditions && (
        <>
          <h4>Conditions</h4>
          <table><tbody>
            {d.conditions.map((c) => (
              <tr key={c.type}><td>{c.type}</td><td className={c.status === "True" ? "" : "warn-text"}>{c.status}</td><td>{c.reason}</td><td>{c.message}</td></tr>
            ))}
          </tbody></table>
        </>
      )}
      {d.containers && (
        <>
          <h4>Containers</h4>
          <table><tbody>
            {d.containers.map((c) => (
              <tr key={c.name}>
                <td>{c.name}{c.init && <span className="badge">init</span>}</td>
                <td className="mono">{c.image}</td>
                <td>{c.ready ? "ready" : <span className="warn-text">not ready</span>}, {c.restarts} restarts</td>
                <td>{c.state}{c.last_state && <div className="muted">last: {c.last_state}</div>}</td>
              </tr>
            ))}
          </tbody></table>
        </>
      )}
      <h4>Events</h4>
      <Events events={d.events} />
    </div>
  );
}

function Logs({ invoke, obj, containers, active }: { invoke: Invoke; obj: Selected; containers: string[]; active: boolean }) {
  const [container, setContainer] = useState("");
  const [previous, setPrevious] = useState(false);
  const [tail, setTail] = useState(200);
  const [follow, setFollow] = useState(false);
  const [text, setText] = useState("");
  const [error, setError] = useState("");
  const pre = useRef<HTMLPreElement>(null);
  const chosen = container || containers[0] || "";

  const load = useCallback(() => {
    invoke<{ text: { text: string } }>("k8s.logs", { namespace: obj.namespace, pod: obj.name, container: chosen || undefined, previous, tail })
      .then((o) => {
        setText(o.text.text);
        setError("");
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, [invoke, obj, chosen, previous, tail]);
  useEffect(() => {
    if (containers.length > 0) load();
  }, [load, containers.length]);
  useEffect(() => {
    if (!follow || !active) return;
    const timer = setInterval(load, LOGS_POLL_MS);
    return () => clearInterval(timer);
  }, [follow, active, load]);
  useEffect(() => {
    if (follow && pre.current) pre.current.scrollTop = pre.current.scrollHeight;
  }, [text, follow]);

  return (
    <div className="logs">
      <div className="filters">
        {containers.length > 1 && (
          <select value={chosen} onChange={(e) => setContainer(e.target.value)}>
            {containers.map((c) => <option key={c}>{c}</option>)}
          </select>
        )}
        <label><input type="checkbox" checked={previous} onChange={(e) => setPrevious(e.target.checked)} /> previous</label>
        <label>tail <input type="number" min={1} max={10000} value={tail} onChange={(e) => setTail(Number(e.target.value) || 200)} style={{ width: 70 }} /></label>
        <button className="small" onClick={load}>Refresh</button>
        <label><input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> follow</label>
      </div>
      {error && <div className="error-box">{error}</div>}
      <pre ref={pre} className="mono log-text">{text}</pre>
    </div>
  );
}

function Diagnosis({ d }: { d: DiagnoseOutput }) {
  return (
    <div className="diagnosis">
      {d.findings.map((f, i) => (
        <div key={i} className={"finding " + severityClass(f.severity)}>
          <span className={"status " + (severityClass(f.severity) || "info")}>{f.severity}</span> <strong>{f.title}</strong>
          {f.detail && <div>{f.detail}</div>}
        </div>
      ))}
      {d.next_steps.length > 0 && (
        <>
          <h4>Next steps</h4>
          <ul>{d.next_steps.map((s, i) => <li key={i}>{s}</li>)}</ul>
        </>
      )}
      {(d.evidence.nodes ?? []).map((n) => (
        <div key={n.name}>
          <h4>Node {n.name}: {n.status}{n.problems?.length ? ` (${n.problems.join(", ")})` : ""}</h4>
          {n.ssh_host && <div>SSH host: <strong>{n.ssh_host}</strong></div>}
          {(n.ssh_candidates ?? []).length > 0 && (
            <ul>
              {n.ssh_candidates!.map((c) => (
                <li key={c.name}>
                  {c.name} <span className="muted">({c.matched_by}{c.address ? ` ${c.address}` : ""})</span>
                  {Object.entries(c.tags ?? {}).map(([k, v]) => (
                    <span key={k} className={k === "env" && v === "prod" ? "badge danger" : "badge"}>{k}={v}</span>
                  ))}
                </li>
              ))}
            </ul>
          )}
        </div>
      ))}
      {(d.evidence.pods ?? []).map((p) =>
        Object.entries(p.previous_logs ?? {}).map(([c, l]) => (
          <div key={p.name + c}>
            <h4>Previous logs of {p.name}/{c}</h4>
            <pre className="mono log-text">{l.text}</pre>
          </div>
        )),
      )}
      {(d.evidence.events ?? []).length > 0 && (
        <>
          <h4>Warning events</h4>
          <Events events={d.evidence.events ?? []} />
        </>
      )}
    </div>
  );
}

function TerminalDialog(props: { badge: string; pod: Selected; invoke: Invoke; onOpen(container: string | undefined, shell: string): void; onCancel(): void }) {
  const [containers, setContainers] = useState<string[]>([]);
  const [container, setContainer] = useState("");
  const [shell, setShell] = useState("sh");
  useEffect(() => {
    props.invoke<DescribeOutput>("k8s.describe", { kind: "Pod", name: props.pod.name, namespace: props.pod.namespace })
      .then((d) => setContainers(podContainers(d)))
      .catch(() => {});
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  return (
    <Dialog badge={props.badge} title={`Open a terminal in pod ${props.pod.name}`} ok="Open"
      onOk={() => props.onOpen(container || containers[0], shell)} onCancel={props.onCancel}>
      {containers.length > 1 && (
        <label>Container{" "}
          <select value={container || containers[0]} onChange={(e) => setContainer(e.target.value)}>
            {containers.map((c) => <option key={c}>{c}</option>)}
          </select>
        </label>
      )}
      <label> Shell <input value={shell} onChange={(e) => setShell(e.target.value)} style={{ width: 80 }} /></label>
    </Dialog>
  );
}

function ForwardDialog(props: { badge: string; where: string; onOk(remote: number, local: number): void; onCancel(): void }) {
  const [remote, setRemote] = useState("");
  const [local, setLocal] = useState("");
  const port = (s: string) => (/^\d+$/.test(s) && Number(s) >= 1 && Number(s) <= 65535 ? Number(s) : 0);
  return (
    <Dialog badge={props.badge} title={`Forward a local port to ${props.where}`} ok="Forward" disabled={!port(remote) || (local !== "" && !port(local))}
      onOk={() => props.onOk(port(remote), port(local))} onCancel={props.onCancel}>
      <label>Pod port <input value={remote} onChange={(e) => setRemote(e.target.value)} placeholder="8080" style={{ width: 80 }} autoFocus /></label>{" "}
      <label>Local port <input value={local} onChange={(e) => setLocal(e.target.value)} placeholder="free" style={{ width: 80 }} /></label>
    </Dialog>
  );
}

function ScaleDialog(props: { badge: string; where: string; onOk(n: number): void; onCancel(): void }) {
  const [n, setN] = useState("");
  const valid = /^\d+$/.test(n);
  const warning = valid ? scaleWarning(Number(n)) : "";
  return (
    <Dialog badge={props.badge} title={`Scale ${props.where}`} ok="Scale" danger disabled={!valid}
      onOk={() => props.onOk(Number(n))} onCancel={props.onCancel}>
      <label>Replicas <input value={n} onChange={(e) => setN(e.target.value)} style={{ width: 70 }} autoFocus /></label>
      {warning && <p className="warn-text">{warning}</p>}
    </Dialog>
  );
}

function RollbackDialog(props: { badge: string; where: string; status: RolloutOutput; onOk(rev: number): void; onCancel(): void }) {
  const older = (props.status.revisions ?? []).filter((r) => !r.current);
  const [rev, setRev] = useState(older[0]?.revision ?? 0);
  return (
    <Dialog badge={props.badge} title={`Roll back ${props.where}`} ok="Roll back" danger disabled={!rev}
      onOk={() => props.onOk(rev)} onCancel={props.onCancel}>
      <p className="muted">{props.status.message}</p>
      {older.length === 0 ? (
        <p>No earlier revision.</p>
      ) : (
        <select value={rev} onChange={(e) => setRev(Number(e.target.value))}>
          {older.map((r) => (
            <option key={r.revision} value={r.revision}>
              {r.revision}: {r.images.join(", ")} ({r.age} ago)
            </option>
          ))}
        </select>
      )}
    </Dialog>
  );
}

function ApplyPanel({ invoke, badge, ctx }: { invoke: Invoke; badge: string; ctx: string }) {
  const [yaml, setYaml] = useState("");
  const [preview, setPreview] = useState<ApplyOutput | null>(null);
  const [result, setResult] = useState<ApplyOutput | null>(null);
  const [conflicts, setConflicts] = useState<Conflict[]>([]);
  const [force, setForce] = useState<Conflict[] | null>(null); // taking these over
  const [confirming, setConfirming] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const run = async (dry: boolean, takeover: Conflict[] | null) => {
    setBusy(true);
    setError("");
    setConflicts([]);
    try {
      const out = await invoke<ApplyOutput>("k8s.apply", applyArgs(yaml, dry, takeover !== null));
      setForce(takeover);
      if (dry) {
        setPreview(out);
        setResult(null);
      } else {
        setResult(out);
        setPreview(null);
        setForce(null);
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setConflicts(conflictsOf(e));
      setPreview(null);
    } finally {
      setBusy(false);
      setConfirming(false);
    }
  };
  const shown = result ?? preview;
  return (
    <div className="apply">
      <textarea className="mono" value={yaml} placeholder="Paste YAML (several documents allowed)…"
        onChange={(e) => { setYaml(e.target.value); setPreview(null); setResult(null); setConflicts([]); setForce(null); }} />
      <div className="filters">
        <button disabled={!yaml.trim() || busy} onClick={() => void run(true, null)}>Preview diff</button>
        <button className="danger" disabled={!preview || busy} onClick={() => setConfirming(true)}>Apply</button>
        {busy && <span className="muted">Working…</span>}
        {force && preview && <span className="warn-text">Taking over fields of other managers</span>}
      </div>
      {conflicts.length > 0 ? (
        <div className="conflicts">
          <p>These fields were set by other field managers; applying would change them:</p>
          <table>
            <tbody>
              {conflicts.map((c, i) => (
                <tr key={i}>
                  <td>{c.object}</td>
                  <td className="mono">{c.field}</td>
                  <td>{c.manager} <span className="muted">({c.operation.toLowerCase()})</span></td>
                  <td className="muted">{c.hint ? `usually changed with ${c.hint}` : ""}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <p className="muted">Leave these fields out of the YAML, change them with the matching action, or take them over.</p>
          <button className="danger" disabled={busy} onClick={() => void run(true, conflicts)}>Take over these fields</button>
        </div>
      ) : (
        error && <div className="error-box">{error}</div>
      )}
      {shown && (
        <div className="apply-result">
          {result && <p>Applied to {ctx}.</p>}
          {shown.objects.map((o) => (
            <div key={`${o.kind}/${o.namespace}/${o.name}`}>
              <h4>
                {o.kind} {o.namespace ? `${o.namespace}/` : ""}{o.name} <span className="badge">{o.operation}{o.applied ? "d" : ""}</span>
              </h4>
              {o.diff && (
                <pre className="mono diff">
                  {diffLines(o.diff.text).map((l, i) => <div key={i} className={l.cls}>{l.text || " "}</div>)}
                </pre>
              )}
            </div>
          ))}
        </div>
      )}
      {confirming && preview && (
        <Dialog badge={badge} title={`Apply ${preview.objects.length} object(s) to ${ctx}${badge ? " — a production cluster" : ""}?`}
          ok={force ? "Take over and apply" : "Apply"} danger onOk={() => void run(false, force)} onCancel={() => setConfirming(false)}>
          {force && <p className="warn-text">{takeoverText(force)}</p>}
        </Dialog>
      )}
    </div>
  );
}
