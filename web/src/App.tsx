import { useCallback, useEffect, useReducer, useState } from "react";
import { ActivityView } from "./ActivityView";
import { addEvent, type Row } from "./activity";
import { fetchHostOutput, type BatchInfo } from "./batch";
import { cox } from "./bridge";
import type { Cox, Status } from "./cox";
import { Confirm } from "./Confirm";
import { FileBrowser } from "./FileBrowser";
import { actionError, contextFromTab, isProdContext, k8sTab, terminalRequest, type ContextInfo, type ContextsOutput } from "./k8s";
import { K8sView, type PodRef } from "./K8sView";
import { Sidebar } from "./Sidebar";
import { TerminalView } from "./TerminalView";
import { needsConfirm, sessionWhere, tabTitle, tabsReducer, type Tab } from "./terminals";
import { runningCount, transfersReducer, type Transfer, type TransferItem } from "./transfers";
import { TransfersView } from "./TransfersView";
import { tunnelPlace, tunnelsReducer, type Tunnel } from "./tunnels";
import { TunnelsView } from "./TunnelsView";
import type { ClientInfo, Host, TermSession } from "./types";

/** Tab ids of the fixed panels and the prefix of Files tabs. */
const TRANSFERS = "@transfers";
const FILES = "@files:";
const TUNNELS = "@tunnels";

/** How often the open Tunnels tab refreshes its traffic counters. */
const TUNNEL_POLL_MS = 3000;

/** How long the "agent is typing" dot stays after a terminal_send. */
const TYPING_MS = 3000;

export function App() {
  if (!cox) {
    return <div className="notice">Open this page with <code>cox desktop</code>.</div>;
  }
  return <Desktop cox={cox} />;
}

function Desktop({ cox }: { cox: Cox }) {
  const [status, setStatus] = useState<Status>({ connected: true });
  const [hosts, setHosts] = useState<Host[]>([]);
  const [clients, setClients] = useState<ClientInfo[]>([]);
  const [rows, setRows] = useState<Row[]>([]);
  const [error, setError] = useState("");
  const [tabs, dispatch] = useReducer(tabsReducer, []);
  const [active, setActive] = useState(""); // "" activity, TRANSFERS, files:<host>, or a terminal id
  const [closing, setClosing] = useState<Tab | null>(null);
  const [transfers, dispatchTransfer] = useReducer(transfersReducer, []);
  const [cancelling, setCancelling] = useState<TransferItem | null>(null);
  const [browsers, setBrowsers] = useState<string[]>([]); // hosts with a Files tab
  const [tunnels, dispatchTunnel] = useReducer(tunnelsReducer, []);
  const [closingTunnel, setClosingTunnel] = useState<Tunnel | null>(null);
  const [now, setNow] = useState(Date.now());
  const [contexts, setContexts] = useState<ContextInfo[]>([]);
  const [contextsError, setContextsError] = useState("");
  const [clusters, setClusters] = useState<string[]>([]); // contexts with a K8s tab

  const report = useCallback((e: unknown) => setError(e instanceof Error ? e.message : String(e)), []);

  const loadHosts = useCallback(async () => {
    const out = await cox.invoke<Host[]>("ssh.hosts.list");
    if (out.status !== "ok") throw new Error(out.error);
    setHosts(out.result ?? []);
  }, [cox]);
  const loadClients = useCallback(async () => {
    setClients((await cox.api<ClientInfo[]>("GET", "/api/v1/clients")) ?? []);
  }, [cox]);
  const loadTerminals = useCallback(async () => {
    const sessions = (await cox.api<TermSession[]>("GET", "/api/v1/terminals")) ?? [];
    dispatch({ type: "sync", sessions });
  }, [cox]);
  const loadTransfers = useCallback(async () => {
    const list = (await cox.api<Transfer[]>("GET", "/api/v1/transfers")) ?? [];
    dispatchTransfer({ type: "sync", list });
  }, [cox]);
  const loadTunnels = useCallback(async () => {
    const list = (await cox.api<Tunnel[]>("GET", "/api/v1/tunnels")) ?? [];
    dispatchTunnel({ type: "sync", list });
  }, [cox]);
  const loadContexts = useCallback(async () => {
    const out = await cox.invoke<ContextsOutput>("k8s.contexts");
    if (out.status !== "ok") {
      setContextsError(actionError(out));
      return;
    }
    setContextsError("");
    setContexts(out.result?.contexts ?? []);
  }, [cox]);
  const loadAll = useCallback(() => {
    loadContexts().catch(report);
    loadHosts().catch(report);
    loadClients().catch(report);
    loadTerminals().catch(report);
    loadTransfers().catch(report);
    loadTunnels().catch(report);
  }, [loadContexts, loadHosts, loadClients, loadTerminals, loadTransfers, loadTunnels, report]);

  useEffect(() => {
    void cox.status().then(setStatus);
    loadAll();
    return cox.onStatus((p) => {
      if (p.type === "connected") {
        setStatus({ connected: true, addr: p.addr, version: p.version, pid: p.pid });
        setError("");
        loadAll();
      } else {
        setStatus({ connected: false });
      }
    });
  }, [cox, loadAll]);

  useEffect(() => {
    let key = 0;
    let stop: (() => void) | undefined;
    let cancelled = false;
    void cox
      .subscribe("events", "", (p) => {
        if (p.type !== "event") return;
        const ev = p.event;
        setRows((rows) => addEvent(rows, ev, key++));
        if (ev.type.startsWith("transfer.")) dispatchTransfer({ type: "event", event: ev });
        if (ev.type.startsWith("tunnel.")) dispatchTunnel({ type: "event", event: ev });
        if (ev.type.startsWith("client.")) loadClients().catch(report);
        if (ev.type === "terminal.opened" && ev.terminal && ev.actor) {
          const { kind, name } = ev.actor;
          dispatch({
            type: "opened", id: ev.terminal, owner: { kind, name },
            host: ev.detail ? sessionWhere(ev.detail as TermSession) : (ev.targets?.[0]?.id ?? ""),
          });
        }
        if (ev.type === "terminal.closed" && ev.terminal) {
          dispatch({ type: "closed", id: ev.terminal, code: ev.exit_code });
        }
        if (ev.type === "action.requested" && ev.action === "terminal.send" && ev.actor?.kind === "agent") {
          const id = ev.args?.id;
          if (typeof id === "string") dispatch({ type: "typing", id, until: Date.now() + TYPING_MS });
        }
        if (ev.type === "action.finished" && ev.action?.startsWith("ssh.hosts.") && ev.action !== "ssh.hosts.list") {
          loadHosts().catch(report);
        }
        if (ev.type === "action.finished" && ev.action === "k8s.contexts.tag") loadContexts().catch(report);
      })
      .then((s) => (cancelled ? s() : (stop = s)), report);
    return () => {
      cancelled = true;
      stop?.();
    };
  }, [cox, loadClients, loadHosts, loadContexts, report]);

  // Traffic counters change without events: refresh while the tab shows.
  useEffect(() => {
    if (active !== TUNNELS || !status.connected) return;
    const timer = setInterval(() => loadTunnels().catch(() => {}), TUNNEL_POLL_MS);
    return () => clearInterval(timer);
  }, [active, status.connected, loadTunnels]);

  // Redraw while an "agent is typing" dot is showing.
  const typing = tabs.some((t) => (t.typingUntil ?? 0) > now);
  useEffect(() => {
    if (!typing && !tabs.some((t) => (t.typingUntil ?? 0) > Date.now())) return;
    const timer = setInterval(() => setNow(Date.now()), 500);
    return () => clearInterval(timer);
  }, [typing, tabs]);

  const openTerminal = useCallback(
    (host: string) => {
      cox
        .api<TermSession>("POST", "/api/v1/terminals", { host, cols: 80, rows: 24 })
        .then((s) => {
          dispatch({ type: "opened", id: s.id, host: sessionWhere(s), owner: s.owner });
          setActive(s.id);
        })
        .catch(report);
    },
    [cox, report],
  );
  const openPodTerminal = async (p: PodRef & { shell?: string }) => {
    const s = await cox.api<TermSession>("POST", "/api/v1/terminals", terminalRequest(p.context, p.namespace, p.pod, p.container, p.shell));
    dispatch({ type: "opened", id: s.id, host: sessionWhere(s), owner: s.owner });
    setActive(s.id);
  };
  const forward = async (body: Record<string, unknown>) => {
    const t = await cox.api<Tunnel>("POST", "/api/v1/tunnels", body);
    dispatchTunnel({ type: "event", event: { time: "", type: "tunnel.opened", detail: t } });
    return t.listen;
  };
  const openCluster = (ctx: string) => {
    setClusters((c) => (c.includes(ctx) ? c : [...c, ctx]));
    setActive(k8sTab(ctx));
  };
  const closeCluster = (ctx: string) => {
    setClusters((c) => c.filter((x) => x !== ctx));
    setActive((a) => (a === k8sTab(ctx) ? "" : a));
  };
  const showTerminal = (id: string) => {
    if (tabs.some((t) => t.id === id)) setActive(id);
  };
  const removeTab = (id: string) => {
    dispatch({ type: "remove", id });
    setActive((a) => (a === id ? "" : a));
  };
  const closeTab = (t: Tab) => {
    if (t.closed) return removeTab(t.id);
    if (needsConfirm(t)) return setClosing(t);
    endTerminal(t);
  };
  const endTerminal = (t: Tab) => {
    setClosing(null);
    cox
      .api("DELETE", "/api/v1/terminals/" + encodeURIComponent(t.id))
      .catch((e) => {
        if (!String(e).includes("(404)")) report(e);
      })
      .finally(() => removeTab(t.id));
  };
  const openFiles = (host: string) => {
    setBrowsers((b) => (b.includes(host) ? b : [...b, host]));
    setActive(FILES + host);
  };
  const closeFiles = (host: string) => {
    setBrowsers((b) => b.filter((h) => h !== host));
    setActive((a) => (a === FILES + host ? "" : a));
  };
  const askCancel = (t: TransferItem) => (t.owner.kind === "agent" ? setCancelling(t) : cancelTransfer(t));
  const cancelTransfer = (t: TransferItem) => {
    setCancelling(null);
    cox.api("DELETE", "/api/v1/transfers/" + encodeURIComponent(t.id)).catch((e) => {
      if (!/\((404|409)\)/.test(String(e))) report(e);
    });
  };
  const askCloseTunnel = (t: Tunnel) => (t.owner.kind === "agent" ? setClosingTunnel(t) : closeTunnel(t));
  const closeTunnel = (t: Tunnel) => {
    setClosingTunnel(null);
    cox
      .api("DELETE", "/api/v1/tunnels/" + encodeURIComponent(t.id))
      .catch((e) => {
        if (!String(e).includes("(404)")) report(e);
      })
      .finally(() => loadTunnels().catch(report));
  };
  const createTunnel = async (body: Record<string, unknown>) => {
    const t = await cox.api<Tunnel>("POST", "/api/v1/tunnels", body);
    dispatchTunnel({ type: "event", event: { time: "", type: "tunnel.opened", detail: t } });
  };
  const loadOutput = (b: BatchInfo, host: string) =>
    fetchHostOutput(cox.invoke, b.artifactId ?? "", b.hosts.map((h) => h.host), host);
  const decide = (method: "PUT" | "DELETE") => (name: string) => {
    cox
      .api(method, "/api/v1/clients/" + encodeURIComponent(name))
      .then(loadClients)
      .catch(report);
  };

  const agents = clients.filter((c) => !c.revoked).length;
  return (
    <div className="app">
      {!status.connected && (
        <div className="banner">The Coxswain server stopped. Start it with cox server; reconnecting…</div>
      )}
      {error && (
        <div className="banner error" onClick={() => setError("")} title="Dismiss">
          {error}
        </div>
      )}
      <div className="main">
        <Sidebar
          hosts={hosts}
          clients={clients}
          contexts={contexts}
          contextsError={contextsError}
          onOpenCluster={openCluster}
          onOpenTerminal={openTerminal}
          onOpenFiles={openFiles}
          onAllow={decide("PUT")}
          onRevoke={decide("DELETE")}
        />
        <section className="content">
          <nav className="tabs">
            <button className={active === "" ? "tab active" : "tab"} onClick={() => setActive("")}>
              Activity
            </button>
            <button className={active === TRANSFERS ? "tab active" : "tab"} onClick={() => setActive(TRANSFERS)}>
              Transfers
              {runningCount(transfers) > 0 && <span className="count">{runningCount(transfers)}</span>}
            </button>
            <button className={active === TUNNELS ? "tab active" : "tab"} onClick={() => setActive(TUNNELS)}>
              Tunnels
              {tunnels.length > 0 && <span className="count">{tunnels.length}</span>}
            </button>
            {browsers.map((h) => (
              <div key={h} className={active === FILES + h ? "tab active" : "tab"} onClick={() => setActive(FILES + h)}>
                <span>{h} · files</span>
                <span
                  className="close"
                  title="Close"
                  onClick={(e) => {
                    e.stopPropagation();
                    closeFiles(h);
                  }}
                >
                  ×
                </span>
              </div>
            ))}
            {clusters.map((c) => {
              const info = contexts.find((x) => x.name === c);
              return (
                <div key={c} className={active === k8sTab(c) ? "tab active" : "tab"} onClick={() => setActive(k8sTab(c))}>
                  {info && isProdContext(info) && <span className="badge danger">env={info.tags?.env}</span>}
                  <span>{c} · k8s</span>
                  <span
                    className="close"
                    title="Close"
                    onClick={(e) => {
                      e.stopPropagation();
                      closeCluster(c);
                    }}
                  >
                    ×
                  </span>
                </div>
              );
            })}
            {tabs.map((t) => (
              <div key={t.id} className={active === t.id ? "tab active" : "tab"} onClick={() => setActive(t.id)}>
                {t.owner.kind === "agent" && <span className="badge agent">agent</span>}
                <span className={t.closed ? "muted" : ""}>{tabTitle(t)}</span>
                {(t.typingUntil ?? 0) > now && <span className="typing" title={`${t.owner.name} is typing`} />}
                <span
                  className="close"
                  title="Close terminal"
                  onClick={(e) => {
                    e.stopPropagation();
                    closeTab(t);
                  }}
                >
                  ×
                </span>
              </div>
            ))}
          </nav>
          <div className="tab-body">
            <div className="pane" style={{ display: active === "" ? "block" : "none" }}>
              <ActivityView rows={rows} onOpenTerminal={showTerminal} loadOutput={loadOutput} />
            </div>
            <div className="pane scroll" style={{ display: active === TRANSFERS ? "block" : "none" }}>
              <TransfersView transfers={transfers} onCancel={askCancel} />
            </div>
            <div className="pane scroll" style={{ display: active === TUNNELS ? "block" : "none" }}>
              <TunnelsView tunnels={tunnels} hosts={hosts} onClose={askCloseTunnel} onCreate={createTunnel} />
            </div>
            {browsers.map((h) => (
              <FileBrowser key={h} cox={cox} host={h} active={active === FILES + h} />
            ))}
            {clusters.map((c) => {
              const info = contexts.find((x) => x.name === c) ?? { name: c, cluster: "", namespace: "default" };
              return (
                <K8sView key={c} cox={cox} context={info} active={contextFromTab(active) === c}
                  onOpenTerminal={openPodTerminal} onForward={forward} />
              );
            })}
            {tabs.map((t) => (
              <TerminalView
                key={t.id}
                cox={cox}
                id={t.id}
                active={active === t.id}
                closed={t.closed}
                onExit={(code) => dispatch({ type: "closed", id: t.id, code: code ?? undefined })}
              />
            ))}
          </div>
        </section>
      </div>
      {closing && (
        <Confirm
          message={`${closing.owner.name} opened this terminal on ${closing.host}. Close it? Its shell ends.`}
          confirm="Close terminal"
          onYes={() => endTerminal(closing)}
          onNo={() => setClosing(null)}
        />
      )}
      {cancelling && (
        <Confirm
          message={`${cancelling.owner.name} started this ${cancelling.direction} (${cancelling.host}:${cancelling.remote_path}). Cancel it?`}
          confirm="Cancel transfer"
          onYes={() => cancelTransfer(cancelling)}
          onNo={() => setCancelling(null)}
        />
      )}
      {closingTunnel && (
        <Confirm
          message={`${closingTunnel.owner.name} opened this tunnel (${tunnelPlace(closingTunnel)}, ${closingTunnel.listen}). Close it? Its connections end.`}
          confirm="Close tunnel"
          onYes={() => closeTunnel(closingTunnel)}
          onNo={() => setClosingTunnel(null)}
        />
      )}
      <footer className="statusbar">
        <span>{status.connected ? `Coxswain ${status.version ?? ""} · ${status.addr ?? ""}` : "disconnected"}</span>
        <span>
          {agents} agent{agents === 1 ? "" : "s"} allowed
        </span>
      </footer>
    </div>
  );
}
