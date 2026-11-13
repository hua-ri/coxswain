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
import { activate } from "./keys";
import { APPLY } from "./nav";
import { Icon } from "./icons";
import { Empty, HazardBand, Message, Modal, Tag, Tags } from "./ui";

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
  kind: string; // the kind listed, or APPLY for the apply panel; chosen in the sidebar
  onRemove?(): void; // set for clusters added in Coxswain (the user's kubeconfig is read-only)
}

interface Selected {
  name: string;
  namespace: string;
}

type DetailTab = "overview" | "yaml" | "events" | "logs" | "diagnose";

/** Dialog is an in-page modal; a production context's dialogs open with its hazard band. */
function Dialog(props: {
  title: string;
  badge: string;
  ctx?: string;
  children?: ReactNode;
  ok: string;
  danger?: boolean;
  disabled?: boolean;
  onOk(): void;
  onCancel(): void;
}) {
  return (
    <Modal title={props.title} band={props.badge ? <HazardBand env={props.badge} where={props.ctx ?? ""} /> : undefined}
      ok={props.ok} danger={props.danger} disabled={props.disabled} onOk={props.onOk} onCancel={props.onCancel}>
      {props.children}
    </Modal>
  );
}

function Events({ events }: { events: EventRow[] }) {
  if (events.length === 0) return <p className="muted">No events.</p>;
  return (
    <table className="compact">
      <tbody>
        {events.map((e, i) => (
          <tr key={i}>
            <td className="meta mono">{e.age}</td>
            <td className={e.type === "Warning" ? "event-reason warn" : "event-reason"}>
              {e.reason}
              {e.count && e.count > 1 ? <span className="muted"> ×{e.count}</span> : null}
            </td>
            <td>
              {e.message}
              <div className="muted mono">{e.object}</div>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

/** K8sView browses one context: lists, details, logs, diagnosis, changes. */
export function K8sView({ cox, context, active, onOpenTerminal, onForward, kind: chosen, onRemove }: Props) {
  const ctx = context.name;
  const [namespace, setNamespace] = useState(context.namespace || "default");
  const [namespaces, setNamespaces] = useState<string[]>([]);
  const applying = chosen === APPLY;
  const kind = applying ? "pods" : chosen;
  const [list, setList] = useState<GetOutput | null>(null);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [auto, setAuto] = useState(true);
  const [selected, setSelected] = useState<Selected | null>(null);
  const [tab, setTab] = useState<DetailTab>("overview");
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

  // A new kind from the sidebar starts with a fresh list and no selection.
  useEffect(() => {
    setSelected(null);
    setList(null);
  }, [kind]);

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
        setDialog(<TerminalDialog badge={badge} ctx={ctx} pod={obj} invoke={invoke}
          onOpen={(container, shell) => {
            setDialog(null);
            onOpenTerminal({ context: ctx, namespace: obj.namespace, pod: obj.name, container, shell }).catch(fail);
          }}
          onCancel={() => setDialog(null)} />);
        return;
      case "forward":
        setDialog(<ForwardDialog badge={badge} ctx={ctx} where={where}
          onOk={(remote, local) =>
            onForward(forwardRequest(ctx, obj.namespace, obj.name, remote, local))
              .then((listen) => done(`Forwarding ${listen} → ${obj.name}:${remote} (see Tunnels)`))
              .catch(fail)
          }
          onCancel={() => setDialog(null)} />);
        return;
      case "delete":
        setDialog(<Dialog badge={badge} ctx={ctx} title={`Delete ${where}?`} ok="Delete" danger
          onOk={() => invoke("k8s.delete", ref).then(() => done(`Deleted ${objKind} ${obj.name}`)).catch(fail)}
          onCancel={() => setDialog(null)} />);
        return;
      case "restart":
        setDialog(<Dialog badge={badge} ctx={ctx} title={`Restart ${where}? Its pods are replaced one by one.`} ok="Restart" danger
          onOk={() => invoke("k8s.rollout", { ...ref, action: "restart" }).then(() => done(`Restarting ${obj.name}`)).catch(fail)}
          onCancel={() => setDialog(null)} />);
        return;
      case "scale":
        setDialog(<ScaleDialog badge={badge} ctx={ctx} where={where}
          onOk={(n) => invoke("k8s.scale", { ...ref, replicas: n }).then(() => done(`Scaled ${obj.name} to ${n}`)).catch(fail)}
          onCancel={() => setDialog(null)} />);
        return;
      case "rollback":
        invoke<RolloutOutput>("k8s.rollout", { ...ref, action: "status" })
          .then((st) =>
            setDialog(<RollbackDialog badge={badge} ctx={ctx} where={where} status={st}
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
  const select = (obj: Selected) => {
    setSelected(obj);
    if (tab === "logs" && list?.kind !== "Pod") setTab("overview");
  };
  const cell = (column: string, value: string, i: number) => {
    if (column === "name") return <td key={i} className="name" title={value}>{value}</td>;
    if (column === "ready") {
      const cls = statusClass(column, value);
      return <td key={i} className={`mono ready-${cls || "ok"}`}>{value}</td>;
    }
    const cls = statusClass(column, value);
    if (cls) return <td key={i}><span className={"chip " + cls}>{value}</span></td>;
    if (column === "age") return <td key={i} className="mono age">{value}</td>;
    if (column === "restarts") return <td key={i} className={/^0\b/.test(value) ? "mono dim" : "mono"}>{value}</td>;
    return <td key={i} title={value} className={column === "node" || column === "namespace" ? "dim" : undefined}>{value}</td>;
  };
  return (
    <div className="pane k8s" style={{ display: active ? "flex" : "none" }}>
      {badge && <HazardBand env={badge} where={ctx} note={context.note} />}
      <header className="pane-head">
        <Icon name="cluster" />
        <h1 className="pane-title">{ctx}</h1>
        <span className="pane-meta mono" title={context.server}>{context.cluster}</span>
        {contextBadges(context).filter((b) => !b.text.startsWith("env=") || !badge).map((b) => {
          const [k, ...v] = b.text.split("=");
          return <Tag key={b.text} k={k} v={v.join("=")} inherited={b.inherited} from={context.inherited_from?.join(", ")} />;
        })}
        {context.note && !badge && <span className="pane-note" title={context.note}>{context.note}</span>}
        <span className="spacer" />
        {onRemove ? (
          <>
            <span className="pane-meta">Added in Coxswain</span>
            <button className="small ghost danger-text remove" onClick={onRemove}>Remove…</button>
          </>
        ) : (
          context.source === "kubeconfig" && <span className="pane-meta" title="Coxswain only reads your kubeconfig; change this context with kubectl">From your kubeconfig (read-only)</span>
        )}
      </header>
      <div className="k8s-body">
      <div className="k8s-main">
        {applying ? (
          <ApplyPanel invoke={invoke} badge={badge} ctx={ctx} />
        ) : (
          <>
            <div className="toolbar">
              <label className="field">
                Namespace
                <select value={namespace} onChange={(e) => { setNamespace(e.target.value); setSelected(null); }}>
                  <option value={ALL}>all namespaces</option>
                  {[...new Set([namespace, ...namespaces])].filter((n) => n !== ALL).map((n) => (
                    <option key={n} value={n}>{n}</option>
                  ))}
                </select>
              </label>
              {list && <span className="meta">{list.rows?.length ?? 0} {list.kind}{(list.rows?.length ?? 0) === 1 ? "" : "s"}</span>}
              <span className="spacer" />
              <label className="check">
                <input type="checkbox" checked={auto} onChange={(e) => setAuto(e.target.checked)} /> Auto-refresh
              </label>
              <button className="small" onClick={() => void load()}>Refresh</button>
            </div>
            {notice && <Message kind="notice" onDismiss={() => setNotice("")}>{notice}</Message>}
            {error && <Message kind="error" onDismiss={() => setError("")}>{error}</Message>}
            {(list?.more || list?.truncated) && (
              <Message kind="note">
                Showing {list.rows?.length ?? 0} of {list.more ? "more than " : ""}{list.total} — narrow it with a namespace.
              </Message>
            )}
            <div className="table-wrap">
              <table className="k8s-table">
                <thead>
                  <tr>
                    {columns.map((c) => <th key={c}>{c}</th>)}
                    {actions.length > 0 && <th><span className="visually-hidden">Actions</span></th>}
                  </tr>
                </thead>
                <tbody>
                  {(list?.rows ?? []).map((row) => {
                    const obj = rowObject(columns, row, list?.namespace || namespace);
                    const sel = selected?.name === obj.name && selected.namespace === obj.namespace;
                    return (
                      <tr key={obj.namespace + "/" + obj.name} className={sel ? "clickable selected" : "clickable"} tabIndex={0}
                        aria-selected={sel} onClick={() => select(obj)} onKeyDown={activate(() => select(obj))}>
                        {row.map((v, i) => cell(columns[i], v, i))}
                        {actions.length > 0 && (
                          <td className="row-actions" onClick={(e) => e.stopPropagation()}>
                            {actions.map((a) => (
                              <button key={a} className={a === "delete" ? "ghost small danger-text" : "ghost small"} onClick={() => act(a, obj)}>{ACTION_LABEL[a]}</button>
                            ))}
                          </td>
                        )}
                      </tr>
                    );
                  })}
                </tbody>
              </table>
              {list && (list.rows ?? []).length === 0 && (
                <Empty title={`No ${list.kind} here`}>{namespace === ALL ? "None in any namespace." : `Nothing in namespace ${namespace}. Try all namespaces.`}</Empty>
              )}
              {!list && !error && <div className="loading">Loading {kind}…</div>}
            </div>
          </>
        )}
      </div>
      {selected && !applying && list && (
        <Detail key={`${list.kind}/${selected.namespace}/${selected.name}`} invoke={invoke} kind={list.kind} obj={selected}
          tab={tab} setTab={setTab} active={active} onClose={() => setSelected(null)} />
      )}
      </div>
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
  const label = (t: DetailTab) => t[0].toUpperCase() + t.slice(1);
  return (
    <aside className="k8s-detail" aria-label={`${kind} ${obj.name}`}>
      <div className="detail-head">
        <div className="titles">
          <div className="detail-kind">{kind} · {obj.namespace}</div>
          <div className="detail-name">{obj.name}</div>
        </div>
        <button className="ghost icon" onClick={props.onClose} aria-label="Close details" title="Close details">×</button>
      </div>
      <div className="segmented" role="tablist" aria-label="Details">
        {tabs.map((t) => (
          <button key={t} role="tab" aria-selected={tab === t} onClick={() => setTab(t)}>{label(t)}</button>
        ))}
        {DIAGNOSABLE[kind] && <button role="tab" aria-selected={tab === "diagnose"} onClick={diagnose}>Diagnose</button>}
      </div>
      {error && <Message kind="error" onDismiss={() => setError("")}>{error}</Message>}
      <div className="detail-body" role="tabpanel">
        {tab === "overview" && (describe ? <Overview d={describe} /> : !error && <div className="loading">Loading…</div>)}
        {tab === "yaml" && (yaml ? <pre className="yaml">{yaml}</pre> : !error && <div className="loading">Loading YAML…</div>)}
        {tab === "events" && describe && <div className="diagnosis"><Events events={describe.events} /></div>}
        {tab === "logs" && kind === "Pod" && <Logs invoke={invoke} obj={obj} containers={podContainers(describe)} active={active} />}
        {tab === "diagnose" && (diagnosis ? <Diagnosis d={diagnosis} /> : !error && <div className="loading">Diagnosing — reading status, events, nodes and previous logs…</div>)}
      </div>
    </aside>
  );
}

function Overview({ d }: { d: DescribeOutput }) {
  return (
    <div>
      <h4 className="section-head">Summary</h4>
      <table className="kv">
        <tbody>
          {Object.entries(d.summary).map(([k, v]) => {
            const cls = statusClass(k, v);
            return <tr key={k}><th>{k}</th><td>{cls ? <span className={"chip " + cls}>{v}</span> : v}</td></tr>;
          })}
          {d.owners && <tr><th>owners</th><td className="mono">{d.owners.join(", ")}</td></tr>}
          {d.keys && <tr><th>keys</th><td className="mono">{d.keys.join(" ")}</td></tr>}
        </tbody>
      </table>
      {d.conditions && (
        <>
          <h4 className="section-head">Conditions</h4>
          <table className="compact"><tbody>
            {d.conditions.map((c) => (
              <tr key={c.type}>
                <td>{c.type}</td>
                <td>{c.status === "True" ? <span className="chip ok">True</span> : <span className="chip warn">{c.status}</span>}</td>
                <td>{c.reason}{c.message && <div className="muted">{c.message}</div>}</td>
              </tr>
            ))}
          </tbody></table>
        </>
      )}
      {d.containers && (
        <>
          <h4 className="section-head">Containers</h4>
          <table className="compact"><tbody>
            {d.containers.map((c) => (
              <tr key={c.name}>
                <td>
                  <div className="cell-line"><strong>{c.name}</strong>{c.init && <span className="chip">init</span>}</div>
                  <div className="mono muted">{c.image}</div>
                </td>
                <td>
                  {c.ready ? <span className="chip ok">ready</span> : <span className="chip warn">not ready</span>}
                  <div className={c.restarts > 0 ? "warn-text" : "muted"}>{c.restarts} restart{c.restarts === 1 ? "" : "s"}</div>
                </td>
                <td>{c.state}{c.last_state && <div className="muted">last: {c.last_state}</div>}</td>
              </tr>
            ))}
          </tbody></table>
        </>
      )}
      <h4 className="section-head">Events</h4>
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
      <div className="log-controls">
        {containers.length > 1 && (
          <select aria-label="Container" value={chosen} onChange={(e) => setContainer(e.target.value)}>
            {containers.map((c) => <option key={c}>{c}</option>)}
          </select>
        )}
        <label className="field">Tail <input type="number" min={1} max={10000} value={tail} onChange={(e) => setTail(Number(e.target.value) || 200)} /></label>
        <label className="check"><input type="checkbox" checked={previous} onChange={(e) => setPrevious(e.target.checked)} /> Previous</label>
        <label className="check"><input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> Follow</label>
        <button className="small" onClick={load}>Refresh</button>
      </div>
      {error && <Message kind="error">{error}</Message>}
      <pre ref={pre} className="log-text">{text || (containers.length === 0 ? "Loading…" : "No log lines.")}</pre>
    </div>
  );
}

function Diagnosis({ d }: { d: DiagnoseOutput }) {
  return (
    <div className="diagnosis">
      {d.findings.length === 0 && <p className="muted">No problems found.</p>}
      {d.findings.map((f, i) => (
        <div key={i} className={"finding " + severityClass(f.severity)}>
          <div className="finding-head">
            <span className={"chip " + (severityClass(f.severity) || "info")}>{f.severity}</span>
            <span className="finding-title">{f.title}</span>
          </div>
          {f.detail && <div className="finding-detail">{f.detail}</div>}
        </div>
      ))}
      {d.next_steps.length > 0 && (
        <>
          <h4 className="section-head">Next steps</h4>
          <ol className="next-steps">{d.next_steps.map((s, i) => <li key={i}>{s}</li>)}</ol>
        </>
      )}
      {(d.evidence.nodes ?? []).map((n) => (
        <div key={n.name}>
          <h4 className="section-head">Node</h4>
          <div className="cell-line">
            <span className="mono">{n.name}</span>
            <span className={n.status === "Ready" ? "chip ok" : "chip bad"}>{n.status}</span>
            {n.problems?.map((p) => <span key={p} className="chip warn">{p}</span>)}
          </div>
          {n.ssh_host && <div>SSH host: <strong>{n.ssh_host}</strong></div>}
          {(n.ssh_candidates ?? []).length > 0 && (
            <ul className="candidates">
              {n.ssh_candidates!.map((c) => (
                <li key={c.name}>
                  <strong>{c.name}</strong> <span className="muted">({c.matched_by}{c.address ? ` ${c.address}` : ""})</span>
                  <Tags tags={c.tags} />
                </li>
              ))}
            </ul>
          )}
        </div>
      ))}
      {(d.evidence.pods ?? []).map((p) =>
        Object.entries(p.previous_logs ?? {}).map(([c, l]) => (
          <div key={p.name + c}>
            <h4 className="section-head">Previous logs · {c}</h4>
            <pre className="log-text">{l.text}</pre>
          </div>
        )),
      )}
      {(d.evidence.events ?? []).length > 0 && (
        <>
          <h4 className="section-head">Warning events</h4>
          <Events events={d.evidence.events ?? []} />
        </>
      )}
    </div>
  );
}

function TerminalDialog(props: { badge: string; ctx: string; pod: Selected; invoke: Invoke; onOpen(container: string | undefined, shell: string): void; onCancel(): void }) {
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
    <Dialog badge={props.badge} ctx={props.ctx} title={`Open a terminal in pod ${props.pod.name}`} ok="Open"
      onOk={() => props.onOpen(container || containers[0], shell)} onCancel={props.onCancel}>
      <div className="fields">
        {containers.length > 1 && (
          <label className="field">Container
            <select value={container || containers[0]} onChange={(e) => setContainer(e.target.value)}>
              {containers.map((c) => <option key={c}>{c}</option>)}
            </select>
          </label>
        )}
        <label className="field">Shell <input value={shell} onChange={(e) => setShell(e.target.value)} /></label>
      </div>
    </Dialog>
  );
}

function ForwardDialog(props: { badge: string; ctx: string; where: string; onOk(remote: number, local: number): void; onCancel(): void }) {
  const [remote, setRemote] = useState("");
  const [local, setLocal] = useState("");
  const port = (s: string) => (/^\d+$/.test(s) && Number(s) >= 1 && Number(s) <= 65535 ? Number(s) : 0);
  return (
    <Dialog badge={props.badge} ctx={props.ctx} title={`Forward a local port to ${props.where}`} ok="Forward" disabled={!port(remote) || (local !== "" && !port(local))}
      onOk={() => props.onOk(port(remote), port(local))} onCancel={props.onCancel}>
      <div className="fields">
        <label className="field">Pod port <input value={remote} onChange={(e) => setRemote(e.target.value)} placeholder="8080" inputMode="numeric" autoFocus /></label>
        <label className="field">Local port <input value={local} onChange={(e) => setLocal(e.target.value)} placeholder="free" inputMode="numeric" /></label>
      </div>
    </Dialog>
  );
}

function ScaleDialog(props: { badge: string; ctx: string; where: string; onOk(n: number): void; onCancel(): void }) {
  const [n, setN] = useState("");
  const valid = /^\d+$/.test(n);
  const warning = valid ? scaleWarning(Number(n)) : "";
  return (
    <Dialog badge={props.badge} ctx={props.ctx} title={`Scale ${props.where}`} ok="Scale" danger disabled={!valid}
      onOk={() => props.onOk(Number(n))} onCancel={props.onCancel}>
      <div className="fields">
        <label className="field">Replicas <input value={n} onChange={(e) => setN(e.target.value)} inputMode="numeric" autoFocus /></label>
      </div>
      {warning && <p className="warn-text">{warning}</p>}
    </Dialog>
  );
}

function RollbackDialog(props: { badge: string; ctx: string; where: string; status: RolloutOutput; onOk(rev: number): void; onCancel(): void }) {
  const older = (props.status.revisions ?? []).filter((r) => !r.current);
  const [rev, setRev] = useState(older[0]?.revision ?? 0);
  return (
    <Dialog badge={props.badge} ctx={props.ctx} title={`Roll back ${props.where}`} ok="Roll back" danger disabled={!rev}
      onOk={() => props.onOk(rev)} onCancel={props.onCancel}>
      <p className="muted">{props.status.message}</p>
      {older.length === 0 ? (
        <p>No earlier revision.</p>
      ) : (
        <select aria-label="Revision" value={rev} onChange={(e) => setRev(Number(e.target.value))}>
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
      <textarea value={yaml} placeholder="Paste YAML (several documents allowed)…" aria-label="YAML to apply" spellCheck={false}
        onChange={(e) => { setYaml(e.target.value); setPreview(null); setResult(null); setConflicts([]); setForce(null); }} />
      <div className="toolbar">
        <button disabled={!yaml.trim() || busy} onClick={() => void run(true, null)}>Preview diff</button>
        <button className="danger" disabled={!preview || busy} onClick={() => setConfirming(true)} title={preview ? undefined : "Preview the diff first"}>Apply</button>
        {busy && <span className="meta">Working…</span>}
        {!preview && !busy && !result && <span className="meta">Preview first: Apply is enabled once you have seen the diff.</span>}
        {force && preview && <span className="warn-text">Taking over fields of other managers</span>}
      </div>
      {conflicts.length > 0 ? (
        <div className="conflicts" role="alert">
          <strong>These fields were set by other field managers; applying would change them:</strong>
          <table className="compact">
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
          <button className="danger small" disabled={busy} onClick={() => void run(true, conflicts)}>Take over these fields</button>
        </div>
      ) : (
        error && <Message kind="error">{error}</Message>
      )}
      {shown && (
        <div className="apply-result">
          {result && <Message kind="notice">Applied to {ctx}.</Message>}
          {shown.objects.map((o) => (
            <div key={`${o.kind}/${o.namespace}/${o.name}`}>
              <h4 className="apply-object-head">
                <span>{o.kind}</span>
                <span className="mono">{o.namespace ? `${o.namespace}/` : ""}{o.name}</span>
                <span className={o.operation === "unchanged" ? "chip" : o.operation === "create" ? "chip ok" : "chip warn"}>{o.operation}{o.applied ? "d" : ""}</span>
              </h4>
              {o.diff && (
                <pre className="diff">
                  {diffLines(o.diff.text).map((l, i) => <div key={i} className={l.cls}>{l.text || " "}</div>)}
                </pre>
              )}
            </div>
          ))}
        </div>
      )}
      {confirming && preview && (
        <Dialog badge={badge} ctx={ctx} title={`Apply ${preview.objects.length} object(s) to ${ctx}${badge ? " — a production cluster" : ""}?`}
          ok={force ? "Take over and apply" : "Apply"} danger onOk={() => void run(false, force)} onCancel={() => setConfirming(false)}>
          {force && <p className="warn-text">{takeoverText(force)}</p>}
        </Dialog>
      )}
    </div>
  );
}
