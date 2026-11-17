import { useCallback, useEffect, useState, type ReactNode } from "react";
import {
  appIssue, appURL, healthClass, instanceIsProd, loginText, syncClass,
  type AppDetail, type AppsOutput, type AppSummary, type ArgoInstance, type Diagnosis as DiagnosisOut,
} from "./argo";
import type { Cox } from "./cox";
import { formatTime } from "./format";
import { Icon } from "./icons";
import { activate } from "./keys";
import { Empty, HazardBand, Message, Tags } from "./ui";

const POLL_MS = 10000;

type Invoke = <T>(id: string, args: Record<string, unknown>) => Promise<T>;

export function useInvoke(cox: Cox, instance: string, extra: Record<string, unknown> = {}): Invoke {
  const key = JSON.stringify(extra);
  return useCallback(
    async <T,>(id: string, args: Record<string, unknown>): Promise<T> => {
      const out = await cox.invoke<T>(id, { instance, ...JSON.parse(key), ...args });
      if (out.status !== "ok") throw new Error([out.error, out.hint].filter(Boolean).join(" — "));
      return out.result as T;
    },
    [cox, instance, key],
  );
}

/** InstanceHead is the pane header of an Argo instance: name, URL, tags, login, and actions. */
export function InstanceHead({ instance, onLogin, onEdit, onRemove, onOpen }: { instance: ArgoInstance; onLogin(): void; onEdit(): void; onRemove(): void; onOpen(): void }) {
  const prod = instanceIsProd(instance);
  const tags = Object.fromEntries(Object.entries(instance.tags ?? {}).filter(([k]) => !(prod && k === "env")));
  const ui = instance.kind === "argocd" ? "Argo CD" : "Argo Workflows";
  return (
    <>
      {prod && <HazardBand env="env=prod" where={instance.name} note={instance.note} />}
      <header className="pane-head">
        <Icon name={instance.kind} />
        <h1 className="pane-title">{instance.name}</h1>
        <span className="pane-meta mono" title={instance.url}>{instance.url}</span>
        <Tags tags={tags} />
        {instance.k8s_context && <span className="pane-meta" title="The Coxswain cluster it runs in: diagnoses follow pods there">in {instance.k8s_context}</span>}
        {instance.note && !prod && <span className="pane-note" title={instance.note}>{instance.note}</span>}
        <span className="spacer" />
        <button className="small" onClick={onOpen} title={`Open ${instance.url} in your browser`}>Open {ui} ↗</button>
        <button className={instance.login.logged_in ? "small ghost" : "small"} onClick={onLogin} title={loginText(instance.login)}>
          {instance.login.logged_in ? (instance.login.identity ?? "Logged in") : instance.login_pending ? "Waiting for login…" : "Log in…"}
        </button>
        <button className="small ghost" onClick={onEdit}>Edit…</button>
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

/** ArgoCDView lists one Argo CD instance's applications. Coxswain shows their state and diagnoses them;
 * the Argo CD UI is one click away for everything else. */
export function ArgoCDView({ cox, instance, active, onLogin, onEdit, onRemove }: {
  cox: Cox;
  instance: ArgoInstance;
  active: boolean;
  onLogin(): void;
  onEdit(): void;
  onRemove(): void;
}) {
  const invoke = useInvoke(cox, instance.name);
  const [apps, setApps] = useState<AppsOutput | null>(null);
  const [onlyIssues, setOnlyIssues] = useState(false);
  const [error, setError] = useState("");
  const [selected, setSelected] = useState("");
  const [opened, setOpened] = useState(false);
  const loggedIn = instance.login.logged_in;
  const open = (url: string) => void cox.openURL(url).catch((e) => setError(String(e)));

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
    if (!active || !loggedIn) return;
    const t = setInterval(() => void load(), POLL_MS);
    return () => clearInterval(t);
  }, [active, loggedIn, load]);

  const list = (apps?.apps ?? []).filter((a) => !onlyIssues || appIssue(a));
  const issues = (apps?.apps ?? []).filter(appIssue).length;
  const sel = apps?.apps.find((a) => a.name === selected);
  return (
    <div className="pane k8s" style={{ display: active ? "flex" : "none" }}>
      <InstanceHead instance={instance} onLogin={onLogin} onEdit={onEdit} onRemove={onRemove} onOpen={() => open(instance.url)} />
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
              <button className="small" onClick={() => void load()}>Refresh</button>
            </div>
            {error && <Message kind="error" onDismiss={() => setError("")}>{error}</Message>}
            <div className="table-wrap">
              <table className="k8s-table">
                <thead>
                  <tr><th>name</th><th>sync</th><th>health</th><th>project</th><th>revision</th><th>last sync</th><th>destination</th></tr>
                </thead>
                <tbody>
                  {list.map((a) => (
                    <tr key={a.name} className={a.name === selected ? "clickable selected" : "clickable"} tabIndex={0} aria-selected={a.name === selected}
                      title="Double-click to open in Argo CD"
                      onClick={() => setSelected(a.name)} onDoubleClick={() => open(appURL(instance, a))} onKeyDown={activate(() => setSelected(a.name))}>
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
            <AppPanel key={sel.name} invoke={invoke} app={sel} onOpen={() => open(appURL(instance, sel))} onClose={() => setSelected("")} />
          )}
        </div>
      )}
    </div>
  );
}

function Chip({ cls, children }: { cls: string; children: ReactNode }) {
  return <span className={"chip " + cls}>{children}</span>;
}

/** AppPanel is an app's state at a glance and its diagnosis; details and changes are in Argo CD (or for agents). */
function AppPanel({ invoke, app, onOpen, onClose }: { invoke: Invoke; app: AppSummary; onOpen(): void; onClose(): void }) {
  const [detail, setDetail] = useState<AppDetail | null>(null);
  const [diagnosis, setDiagnosis] = useState<DiagnosisOut | null>(null);
  const [diagnosing, setDiagnosing] = useState(false);
  const [error, setError] = useState("");
  const show = (e: unknown) => setError(e instanceof Error ? e.message : String(e));
  const ref = { app: app.name, ...(app.namespace && app.namespace !== "argocd" ? { app_namespace: app.namespace } : {}) };
  useEffect(() => {
    invoke<AppDetail>("argocd.app", ref).then(setDetail).catch(show);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [invoke, app.name, app.sync, app.health, app.last_sync]);
  const diagnose = () => {
    setDiagnosing(true);
    setDiagnosis(null);
    invoke<DiagnosisOut>("argocd.diagnose", ref).then(setDiagnosis).catch(show).finally(() => setDiagnosing(false));
  };
  const op = detail?.last_operation;
  return (
    <aside className="k8s-detail" aria-label={`Application ${app.name}`}>
      <div className="detail-head">
        <div className="titles">
          <div className="detail-kind">Application · {app.project}</div>
          <div className="detail-name">{app.name}</div>
        </div>
        <button className="ghost icon" onClick={onClose} aria-label="Close details" title="Close details">×</button>
      </div>
      <div className="detail-actions-row">
        <button className="small primary" onClick={onOpen}>Open in Argo CD ↗</button>
        <button className="small" onClick={diagnose} disabled={diagnosing}>{diagnosing ? "Diagnosing…" : "Diagnose"}</button>
      </div>
      {error && <Message kind="error" onDismiss={() => setError("")}>{error}</Message>}
      <div className="detail-body">
        {diagnosis ? (
          <DiagnosisView d={diagnosis} />
        ) : diagnosing ? (
          <div className="loading">Diagnosing — conditions, last sync, unhealthy pods, their nodes and SSH hosts…</div>
        ) : detail ? (
          <>
            <table className="kv"><tbody>
              <tr><th>sync</th><td><Chip cls={syncClass(detail.sync)}>{detail.sync}</Chip> <span className="mono muted">{detail.revision}</span></td></tr>
              <tr><th>health</th><td><Chip cls={healthClass(detail.health)}>{detail.health}</Chip>{detail.health_message && <div className="muted">{detail.health_message}</div>}</td></tr>
              <tr><th>source</th><td className="mono">{detail.source}</td></tr>
              <tr><th>destination</th><td className="mono">{detail.destination}</td></tr>
              <tr><th>sync policy</th><td>{detail.auto_sync ? `automatic${detail.self_heal ? ", self-heal" : ""}${detail.auto_prune ? ", prune" : ""}` : "manual"}</td></tr>
              {op && <tr><th>last sync</th><td><Chip cls={op.phase === "Succeeded" ? "ok" : op.phase === "Running" ? "warn" : "bad"}>{op.phase}</Chip> <span className="mono muted">{formatTime(op.finished_at ?? op.started_at)}</span>{op.phase !== "Succeeded" && op.message && <div className="muted">{op.message}</div>}</td></tr>}
            </tbody></table>
            {(detail.conditions ?? []).map((c, i) => (
              <div key={i} className={"finding " + (c.type.endsWith("Error") ? "bad" : "warn")}>
                <div className="finding-head"><span className="finding-title">{c.type}</span></div>
                <div className="finding-detail">{c.message}</div>
              </div>
            ))}
            {appIssue(app) && <p className="muted hint-line">Diagnose looks for the cause; resources, diffs and history are in Argo CD.</p>}
          </>
        ) : (
          !error && <div className="loading">Loading…</div>
        )}
      </div>
    </aside>
  );
}

/** DiagnosisView shows an Argo diagnosis: findings, next steps, and log tails. */
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
        const e = (v ?? {}) as Record<string, unknown>;
        const logs = typeof e.logs_tail === "string" ? e.logs_tail : "";
        const events = Array.isArray(e.events) ? (e.events as unknown[]).map((x) => (typeof x === "string" ? x : `${(x as { reason?: string }).reason ?? ""}: ${(x as { message?: string }).message ?? ""}`)) : [];
        const prev = Array.isArray(e.pods) ? (e.pods as { previous_logs?: Record<string, { text: string }> }[]).flatMap((p) => Object.values(p.previous_logs ?? {}).map((l) => l.text)) : [];
        if (!logs && !events.length && !prev.length) return null;
        return (
          <div key={k}>
            <h4 className="section-head">{k}{typeof e.logs_of === "string" ? ` · ${e.logs_of}` : ""}</h4>
            {events.slice(0, 5).map((x, i) => <div key={i} className="muted">{x}</div>)}
            {[logs, ...prev].filter(Boolean).map((l, i) => <pre key={i} className="log-text">{l}</pre>)}
          </div>
        );
      })}
    </div>
  );
}
