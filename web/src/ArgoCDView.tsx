import { useCallback, useEffect, useState, type ReactNode } from "react";
import {
  appIssue, healthClass, instanceIsProd, loginText, resourceRef, syncArgs, syncClass,
  type AppDetail, type AppsOutput, type AppSummary, type ArgoInstance, type DiffOutput, type Diagnosis as DiagnosisOut, type TreeNode,
} from "./argo";
import type { Cox } from "./cox";
import { formatTime } from "./format";
import { Icon } from "./icons";
import { diffLines } from "./k8s";
import { activate } from "./keys";
import { Empty, HazardBand, Message, Modal, Tags } from "./ui";

const POLL_MS = 10000;

type Invoke = <T>(id: string, args: Record<string, unknown>) => Promise<T>;

function useInvoke(cox: Cox, instance: string): Invoke {
  return useCallback(
    async <T,>(id: string, args: Record<string, unknown>): Promise<T> => {
      const out = await cox.invoke<T>(id, { instance, ...args });
      if (out.status !== "ok") throw new Error([out.error, out.hint].filter(Boolean).join(" — "));
      return out.result as T;
    },
    [cox, instance],
  );
}

/** InstanceHead is the pane header of an Argo instance: name, URL, tags, login, and actions. */
export function InstanceHead({ instance, onLogin, onRemove, children }: { instance: ArgoInstance; onLogin(): void; onRemove(): void; children?: ReactNode }) {
  const prod = instanceIsProd(instance);
  const tags = Object.fromEntries(Object.entries(instance.tags ?? {}).filter(([k]) => !(prod && k === "env")));
  return (
    <>
      {prod && <HazardBand env="env=prod" where={instance.name} note={instance.note} />}
      <header className="pane-head">
        <Icon name={instance.kind} />
        <h1 className="pane-title">{instance.name}</h1>
        <span className="pane-meta mono" title={instance.url}>{instance.url}</span>
        <Tags tags={tags} />
        {instance.note && !prod && <span className="pane-note" title={instance.note}>{instance.note}</span>}
        <span className="spacer" />
        {children}
        <button className={instance.login.logged_in ? "small ghost" : "small"} onClick={onLogin} title={loginText(instance.login)}>
          {instance.login.logged_in ? (instance.login.identity ?? "Logged in") : instance.login_pending ? "Waiting for login…" : "Log in…"}
        </button>
        <button className="small ghost danger-text remove" onClick={onRemove}>Remove…</button>
      </header>
    </>
  );
}

/** NeedsLogin replaces a view's content until the instance is logged in. */
export function NeedsLogin({ instance, onLogin }: { instance: ArgoInstance; onLogin(): void }) {
  return (
    <div className="pane-scroll">
      <Empty title={instance.login.rejected ? "The server rejected the login" : "Not logged in"}>
        <p>{loginText(instance.login)}. Log in to see {instance.kind === "argocd" ? "applications" : "workflows"}; agents use the same login.</p>
        <p><button className="primary" onClick={onLogin}>Log in…</button></p>
      </Empty>
    </div>
  );
}

type DetailTab = "overview" | "resources" | "diff" | "history" | "diagnose";

/** ArgoCDView lists one Argo CD instance's applications, with details and actions. */
export function ArgoCDView({ cox, instance, active, onLogin, onRemove }: {
  cox: Cox;
  instance: ArgoInstance;
  active: boolean;
  onLogin(): void;
  onRemove(): void;
}) {
  const invoke = useInvoke(cox, instance.name);
  const [apps, setApps] = useState<AppsOutput | null>(null);
  const [onlyIssues, setOnlyIssues] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [selected, setSelected] = useState("");
  const [tab, setTab] = useState<DetailTab>("overview");
  const [opened, setOpened] = useState(false);
  const [auto, setAuto] = useState(true);
  const loggedIn = instance.login.logged_in;
  const prod = instanceIsProd(instance);

  const load = useCallback(async () => {
    try {
      setApps(await invoke<AppsOutput>("argocd.apps", {}));
      setError("");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, [invoke]);
  useEffect(() => {
    if (active) setOpened(true);
  }, [active]);
  useEffect(() => {
    if (opened && loggedIn) void load();
  }, [opened, loggedIn, load]);
  useEffect(() => {
    if (!active || !auto || !loggedIn) return;
    const t = setInterval(() => void load(), POLL_MS);
    return () => clearInterval(t);
  }, [active, auto, loggedIn, load]);

  const list = (apps?.apps ?? []).filter((a) => !onlyIssues || appIssue(a));
  const issues = (apps?.apps ?? []).filter(appIssue).length;
  const sel = apps?.apps.find((a) => a.name === selected);
  return (
    <div className="pane k8s" style={{ display: active ? "flex" : "none" }}>
      <InstanceHead instance={instance} onLogin={onLogin} onRemove={onRemove} />
      {!loggedIn ? (
        <NeedsLogin instance={instance} onLogin={onLogin} />
      ) : (
        <div className="k8s-body">
          <div className="k8s-main">
            <div className="toolbar">
              <div className="segmented inline" role="radiogroup" aria-label="Show">
                <button role="radio" aria-checked={!onlyIssues} aria-selected={!onlyIssues} onClick={() => setOnlyIssues(false)}>All {apps ? apps.apps.length : ""}</button>
                <button role="radio" aria-checked={onlyIssues} aria-selected={onlyIssues} onClick={() => setOnlyIssues(true)}>Needs attention {apps ? issues : ""}</button>
              </div>
              <span className="spacer" />
              <label className="check"><input type="checkbox" checked={auto} onChange={(e) => setAuto(e.target.checked)} /> Auto-refresh</label>
              <button className="small" onClick={() => void load()}>Refresh</button>
            </div>
            {notice && <Message kind="notice" onDismiss={() => setNotice("")}>{notice}</Message>}
            {error && <Message kind="error" onDismiss={() => setError("")}>{error}</Message>}
            <div className="table-wrap">
              <table className="k8s-table">
                <thead>
                  <tr><th>name</th><th>sync</th><th>health</th><th>project</th><th>revision</th><th>last sync</th><th>destination</th></tr>
                </thead>
                <tbody>
                  {list.map((a) => (
                    <tr key={a.name} className={a.name === selected ? "clickable selected" : "clickable"} tabIndex={0} aria-selected={a.name === selected}
                      onClick={() => setSelected(a.name)} onKeyDown={activate(() => setSelected(a.name))}>
                      <td className="name" title={a.problem ?? a.name}>{a.name}{a.auto_sync && <span className="chip info" title="auto-sync">auto</span>}</td>
                      <td><span className={"chip " + syncClass(a.sync)}>{a.sync || "Unknown"}</span></td>
                      <td><span className={"chip " + healthClass(a.health)}>{a.health || "Unknown"}</span></td>
                      <td className="dim">{a.project}</td>
                      <td className="mono">{a.revision}</td>
                      <td className="mono age" title={a.last_result}>{a.last_sync ? formatTime(a.last_sync) : ""}{a.last_result && a.last_result !== "Succeeded" ? ` · ${a.last_result}` : ""}</td>
                      <td className="dim" title={a.source}>{a.destination}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {apps && list.length === 0 && (
                <Empty title={onlyIssues ? "Every application is Synced and Healthy" : "No applications"}>
                  {onlyIssues ? null : "This instance has no applications you may see."}
                </Empty>
              )}
              {!apps && !error && <div className="loading">Loading applications…</div>}
            </div>
          </div>
          {sel && (
            <AppDetailPanel key={sel.name} invoke={invoke} app={sel} tab={tab} setTab={setTab} prod={prod} where={instance.name}
              onClose={() => setSelected("")}
              onDone={(msg) => { setNotice(msg); void load(); }} />
          )}
        </div>
      )}
    </div>
  );
}

function Chip({ cls, children }: { cls: string; children: ReactNode }) {
  return <span className={"chip " + cls}>{children}</span>;
}

function AppDetailPanel(props: {
  invoke: Invoke;
  app: AppSummary;
  tab: DetailTab;
  setTab(t: DetailTab): void;
  prod: boolean;
  where: string;
  onClose(): void;
  onDone(msg: string): void;
}) {
  const { invoke, app, tab, setTab } = props;
  const [detail, setDetail] = useState<AppDetail | null>(null);
  const [tree, setTree] = useState<TreeNode[] | null>(null);
  const [diff, setDiff] = useState<DiffOutput | null>(null);
  const [diagnosis, setDiagnosis] = useState<DiagnosisOut | null>(null);
  const [dialog, setDialog] = useState<ReactNode>(null);
  const [error, setError] = useState("");
  const show = (e: unknown) => setError(e instanceof Error ? e.message : String(e));
  const ref = { app: app.name, ...(app.namespace && app.namespace !== "argocd" ? { app_namespace: app.namespace } : {}) };

  const loadDetail = useCallback(() => invoke<AppDetail>("argocd.app", ref).then(setDetail).catch(show),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [invoke, app.name]);
  useEffect(() => void loadDetail(), [loadDetail, app.sync, app.health, app.last_sync]);
  useEffect(() => {
    if (tab === "resources" && !tree) invoke<{ nodes: TreeNode[] }>("argocd.tree", ref).then((o) => setTree(o.nodes)).catch(show);
    if (tab === "diff" && !diff) invoke<DiffOutput>("argocd.diff", ref).then(setDiff).catch(show);
    if (tab === "diagnose" && !diagnosis) invoke<DiagnosisOut>("argocd.diagnose", ref).then(setDiagnosis).catch(show);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tab]);

  const band = props.prod ? <HazardBand env="env=prod" where={props.where} /> : undefined;
  const run = (id: string, args: Record<string, unknown>, msg: string) => {
    setDialog(null);
    invoke(id, { ...ref, ...args }).then(() => { props.onDone(msg); void loadDetail(); setTree(null); setDiff(null); setDiagnosis(null); }).catch(show);
  };
  const outOfSync = (detail?.unhealthy_or_out_of_sync ?? []).filter((r) => r.sync === "OutOfSync" || r.requires_pruning);
  const openSync = () => setDialog(
    <SyncDialog band={band} app={app.name} resources={outOfSync.map(resourceRef)} running={!!detail?.operation_running}
      onOk={(o) => run("argocd.sync", syncArgs(app.name, o), o.dryRun ? `Dry-run sync of ${app.name} started; see Overview` : `Sync of ${app.name} started`)}
      onCancel={() => setDialog(null)} />,
  );
  const openRollback = () => setDialog(
    <RollbackDialog band={band} detail={detail} onOk={(id) => run("argocd.rollback", { id }, `Rolling ${app.name} back to deployment ${id}`)} onCancel={() => setDialog(null)} />,
  );
  const tabs: DetailTab[] = ["overview", "resources", "diff", "history"];
  return (
    <aside className="k8s-detail" aria-label={`Application ${app.name}`}>
      <div className="detail-head">
        <div className="titles">
          <div className="detail-kind">Application · {app.project}</div>
          <div className="detail-name">{app.name}</div>
        </div>
        <button className="ghost icon" onClick={props.onClose} aria-label="Close details" title="Close details">×</button>
      </div>
      <div className="detail-actions-row">
        <button className="small primary" onClick={openSync}>Sync…</button>
        <button className="small" onClick={() => run("argocd.refresh", {}, `Refreshed ${app.name}`)}>Refresh</button>
        <button className="small" onClick={() => run("argocd.refresh", { hard: true }, `Hard-refreshed ${app.name}`)} title="Regenerate manifests from Git">Hard refresh</button>
        <button className="small" onClick={openRollback} disabled={!detail?.history?.length}>Rollback…</button>
        {detail?.operation_running && (
          <button className="small danger" onClick={() => setDialog(
            <Modal title={`Stop the running sync of ${app.name}?`} band={band} ok="Terminate" danger
              onOk={() => run("argocd.terminate", {}, `Terminated the operation on ${app.name}`)} onCancel={() => setDialog(null)}>
              <p className="muted">Resources already applied stay as they are.</p>
            </Modal>)}>Terminate</button>
        )}
      </div>
      <div className="segmented" role="tablist" aria-label="Details">
        {tabs.map((t) => <button key={t} role="tab" aria-selected={tab === t} onClick={() => setTab(t)}>{t[0].toUpperCase() + t.slice(1)}</button>)}
        <button role="tab" aria-selected={tab === "diagnose"} onClick={() => { setDiagnosis(null); setTab("diagnose"); if (tab === "diagnose") invoke<DiagnosisOut>("argocd.diagnose", ref).then(setDiagnosis).catch(show); }}>Diagnose</button>
      </div>
      {error && <Message kind="error" onDismiss={() => setError("")}>{error}</Message>}
      <div className="detail-body" role="tabpanel">
        {tab === "overview" && (detail ? <AppOverview d={detail} /> : !error && <div className="loading">Loading…</div>)}
        {tab === "resources" && (tree ? <Tree nodes={tree} /> : !error && <div className="loading">Loading resources…</div>)}
        {tab === "diff" && (diff ? <Diff d={diff} /> : !error && <div className="loading">Comparing with Git…</div>)}
        {tab === "history" && detail && <History d={detail} />}
        {tab === "diagnose" && (diagnosis ? <DiagnosisView d={diagnosis} /> : !error && <div className="loading">Diagnosing — conditions, last sync, unhealthy pods with events and logs…</div>)}
      </div>
      {dialog}
    </aside>
  );
}

function AppOverview({ d }: { d: AppDetail }) {
  const op = d.last_operation;
  return (
    <div>
      <h4 className="section-head">Status</h4>
      <table className="kv"><tbody>
        <tr><th>sync</th><td><Chip cls={syncClass(d.sync)}>{d.sync}</Chip> <span className="mono muted">{d.revision}</span></td></tr>
        <tr><th>health</th><td><Chip cls={healthClass(d.health)}>{d.health}</Chip>{d.health_message && <div className="muted">{d.health_message}</div>}</td></tr>
        <tr><th>source</th><td className="mono">{d.source}</td></tr>
        <tr><th>destination</th><td className="mono">{d.destination}</td></tr>
        <tr><th>sync policy</th><td>{d.auto_sync ? `automatic${d.self_heal ? ", self-heal" : ""}${d.auto_prune ? ", prune" : ""}` : "manual"}</td></tr>
        <tr><th>resources</th><td className="mono">{Object.entries(d.resources).map(([k, n]) => `${n} ${k.replace(/\/$/, "").replace("/", " · ")}`).join(", ")}</td></tr>
      </tbody></table>
      {(d.conditions ?? []).length > 0 && (
        <>
          <h4 className="section-head">Conditions</h4>
          {d.conditions!.map((c, i) => (
            <div key={i} className={"finding " + (c.type.endsWith("Error") ? "bad" : "warn")}>
              <div className="finding-head"><span className="finding-title">{c.type}</span></div>
              <div className="finding-detail">{c.message}</div>
            </div>
          ))}
        </>
      )}
      {op && (
        <>
          <h4 className="section-head">Last operation{d.operation_running ? " (running)" : ""}</h4>
          <table className="kv"><tbody>
            <tr><th>phase</th><td><Chip cls={op.phase === "Succeeded" ? "ok" : op.phase === "Running" ? "warn" : "bad"}>{op.phase}</Chip> <span className="mono muted">{op.revision}</span></td></tr>
            {op.message && <tr><th>message</th><td>{op.message}</td></tr>}
            <tr><th>finished</th><td className="mono">{formatTime(op.finished_at ?? op.started_at)}</td></tr>
          </tbody></table>
          {(op.failed_resources ?? []).map((f) => <div key={f} className="finding bad"><div className="finding-detail mono">{f}</div></div>)}
        </>
      )}
      {(d.unhealthy_or_out_of_sync ?? []).length > 0 && (
        <>
          <h4 className="section-head">Not synced or not healthy</h4>
          <table className="compact"><tbody>
            {d.unhealthy_or_out_of_sync!.map((r) => (
              <tr key={`${r.kind}/${r.namespace}/${r.name}`}>
                <td><span className="mono">{r.kind}/{r.name}</span>{r.namespace && <div className="muted mono">{r.namespace}</div>}</td>
                <td>{r.sync && r.sync !== "Synced" && <Chip cls="warn">{r.sync}</Chip>}{r.requires_pruning && <Chip cls="bad">prune</Chip>}</td>
                <td>{r.health && <Chip cls={healthClass(r.health)}>{r.health}</Chip>}{r.message && <div className="muted">{r.message}</div>}</td>
              </tr>
            ))}
          </tbody></table>
        </>
      )}
    </div>
  );
}

function Tree({ nodes }: { nodes: TreeNode[] }) {
  if (nodes.length === 0) return <p className="muted">No live resources.</p>;
  return (
    <table className="compact"><tbody>
      {nodes.map((n) => (
        <tr key={`${n.kind}/${n.namespace}/${n.name}`}>
          <td><span className="mono">{n.kind}/{n.name}</span>{n.parent && <div className="muted mono">← {n.parent}</div>}</td>
          <td>{n.health && <Chip cls={healthClass(n.health)}>{n.health}</Chip>}</td>
          <td className="muted">{[n.message, n.info].filter(Boolean).join(" · ")}</td>
        </tr>
      ))}
    </tbody></table>
  );
}

function Diff({ d }: { d: DiffOutput }) {
  if (d.diffs.length === 0) return <p className="muted">Everything matches Git ({d.in_sync} resources).</p>;
  return (
    <div>
      {d.diffs.map((x) => (
        <div key={x.object}>
          <h4 className="apply-object-head"><span className="mono">{x.object}</span><Chip cls={x.change === "removed" ? "bad" : x.change === "added" ? "ok" : "warn"}>{x.change}</Chip></h4>
          <pre className="diff">{diffLines(x.diff.text).map((l, i) => <div key={i} className={l.cls}>{l.text || " "}</div>)}</pre>
        </div>
      ))}
      {d.in_sync > 0 && <p className="muted">{d.in_sync} other resources match Git.</p>}
    </div>
  );
}

function History({ d }: { d: AppDetail }) {
  if (!d.history?.length) return <p className="muted">No deployments yet.</p>;
  return (
    <table className="compact"><tbody>
      {d.history.map((h, i) => (
        <tr key={h.id}>
          <td className="mono">{h.id}</td>
          <td className="mono">{h.revision}{i === 0 && <span className="chip info">current</span>}</td>
          <td className="mono muted">{formatTime(h.deployed_at)}</td>
          <td className="muted">{h.by}</td>
        </tr>
      ))}
    </tbody></table>
  );
}

export function DiagnosisView({ d }: { d: DiagnosisOut }) {
  const sev = (s: string) => (s === "error" ? "bad" : s === "warning" ? "warn" : "");
  return (
    <div className="diagnosis">
      <p className="muted">{d.summary}</p>
      {d.findings.map((f, i) => (
        <div key={i} className={"finding " + sev(f.severity)}>
          <div className="finding-head"><span className={"chip " + (sev(f.severity) || "info")}>{f.severity}</span><span className="finding-title">{f.title}</span></div>
          {f.detail && <div className="finding-detail">{f.detail}</div>}
        </div>
      ))}
      {(d.next_steps ?? []).length > 0 && (
        <>
          <h4 className="section-head">Next steps</h4>
          <ol className="next-steps">{d.next_steps!.map((s, i) => <li key={i}>{s}</li>)}</ol>
        </>
      )}
      {Object.entries(d.evidence ?? {}).map(([k, v]) => {
        const e = v as Record<string, unknown>;
        const logs = typeof e?.logs_tail === "string" ? e.logs_tail : "";
        const events = Array.isArray(e?.events) ? (e.events as string[]) : [];
        if (!logs && !events.length) return null;
        return (
          <div key={k}>
            <h4 className="section-head">{k}{typeof e.logs_of === "string" ? ` · ${e.logs_of}` : ""}</h4>
            {events.map((x, i) => <div key={i} className="muted">{x}</div>)}
            {logs && <pre className="log-text">{logs}</pre>}
          </div>
        );
      })}
    </div>
  );
}

function SyncDialog(props: { band?: ReactNode; app: string; resources: string[]; running: boolean; onOk(o: { prune: boolean; dryRun: boolean; resources: string[]; revision: string }): void; onCancel(): void }) {
  const [prune, setPrune] = useState(false);
  const [dryRun, setDryRun] = useState(false);
  const [only, setOnly] = useState<string[]>([]);
  const [revision, setRevision] = useState("");
  return (
    <Modal title={`Sync ${props.app}`} band={props.band} ok={dryRun ? "Dry run" : "Sync"} danger={prune && !dryRun} disabled={props.running}
      onOk={() => props.onOk({ prune, dryRun, resources: only, revision })} onCancel={props.onCancel}>
      {props.running && <p className="warn-text">An operation is running; terminate it or wait.</p>}
      <div className="fields">
        <label className="check"><input type="checkbox" checked={dryRun} onChange={(e) => setDryRun(e.target.checked)} /> Dry run (check only)</label>
        <label className="check"><input type="checkbox" checked={prune} onChange={(e) => setPrune(e.target.checked)} /> Prune: delete resources no longer in Git</label>
        <label className="field">Revision <input className="mono" value={revision} placeholder="target revision" onChange={(e) => setRevision(e.target.value)} /></label>
      </div>
      {props.resources.length > 0 && (
        <>
          <p className="muted">Only these (none checked: everything):</p>
          <div className="fields">
            {props.resources.map((r) => (
              <label key={r} className="check mono"><input type="checkbox" checked={only.includes(r)}
                onChange={(e) => setOnly((o) => (e.target.checked ? [...o, r] : o.filter((x) => x !== r)))} /> {r}</label>
            ))}
          </div>
        </>
      )}
      {prune && !dryRun && <p className="warn-text">Resources removed from Git will be deleted from the cluster.</p>}
    </Modal>
  );
}

function RollbackDialog(props: { band?: ReactNode; detail: AppDetail | null; onOk(id: number): void; onCancel(): void }) {
  const older = (props.detail?.history ?? []).slice(1);
  const [id, setId] = useState(older[0]?.id ?? 0);
  const auto = !!props.detail?.auto_sync;
  return (
    <Modal title={`Roll back ${props.detail?.name ?? ""}`} band={props.band} ok="Roll back" danger disabled={!id || auto}
      onOk={() => props.onOk(id)} onCancel={props.onCancel}>
      {auto ? (
        <p className="warn-text">Auto-sync is on and would undo a rollback. Turn it off in Argo CD first.</p>
      ) : older.length === 0 ? (
        <p>No earlier deployment.</p>
      ) : (
        <select aria-label="Deployment" value={id} onChange={(e) => setId(Number(e.target.value))}>
          {older.map((h) => <option key={h.id} value={h.id}>{h.id}: {h.revision} ({formatTime(h.deployed_at)}{h.by ? `, ${h.by}` : ""})</option>)}
        </select>
      )}
      <p className="muted">The app stays OutOfSync with Git until the next sync.</p>
    </Modal>
  );
}
