import { useCallback, useEffect, useReducer, useRef, useState } from "react";
import { ActivityView } from "./ActivityView";
import { addEvent, type Row } from "./activity";
import { fetchHostOutput, type BatchInfo } from "./batch";
import { cox } from "./bridge";
import type { Cox, Status } from "./cox";
import { Confirm } from "./Confirm";
import { isProdTags, prodWhere } from "./env";
import { AgentsView } from "./AgentsView";
import { HostView } from "./HostView";
import { HostEditor } from "./HostEditor";
import { SyncView } from "./SyncView";
import { syncSummary, type ImportOutput } from "./sync";
import { ClusterEditor } from "./ClusterEditor";
import {
  ACTIVITY, ADD_CLUSTER, ADD_HOST, AGENTS, TRANSFERS, TUNNELS, afterClose, clusterOf, destinationFor, editHostId, editHostOf,
  filesHostOf, filesId, footprint, hostId, hostOf, placeOfWhere, SYNC_HOSTS, removeHostMessage, revokeMessage, rowsAt, visit,
} from "./nav";
import { reloadsContexts, reloadsHosts } from "./sidetree";
import { HazardBand, Message, PaneHeader, type MenuItem } from "./ui";
import { FileBrowser } from "./FileBrowser";
import { actionError, isProdContext, k8sTab, terminalRequest, type ContextInfo, type ContextsOutput } from "./k8s";
import { K8sView, type PodRef } from "./K8sView";
import { Icon } from "./icons";
import { Sidebar, type SideSession } from "./Sidebar";
import { TerminalView } from "./TerminalView";
import { needsConfirm, sessionWhere, tabsReducer, type Tab } from "./terminals";
import { runningCount, transfersReducer, type Transfer, type TransferItem } from "./transfers";
import { TransfersView } from "./TransfersView";
import { tunnelPlace, tunnelsReducer, type Tunnel } from "./tunnels";
import { TunnelsView } from "./TunnelsView";
import type { ClientInfo, Host, TermSession } from "./types";

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
  const [active, setActive] = useState(ACTIVITY); // what the main area shows; see nav.ts
  const [closing, setClosing] = useState<Tab | null>(null);
  const [transfers, dispatchTransfer] = useReducer(transfersReducer, []);
  const [cancelling, setCancelling] = useState<TransferItem | null>(null);
  const [browsers, setBrowsers] = useState<string[]>([]); // hosts with a Files tab
  const [tunnels, dispatchTunnel] = useReducer(tunnelsReducer, []);
  const [closingTunnel, setClosingTunnel] = useState<Tunnel | null>(null);
  const [now, setNow] = useState(Date.now());
  const [contexts, setContexts] = useState<ContextInfo[]>([]);
  const [contextsError, setContextsError] = useState("");
  const [kubeconfigFiles, setKubeconfigFiles] = useState<string[]>([]);
  const [notice, setNotice] = useState("");
  const [removingHost, setRemovingHost] = useState<Host | null>(null);
  const [removingCluster, setRemovingCluster] = useState<ContextInfo | null>(null);
  const [clusters, setClusters] = useState<string[]>([]); // contexts opened in this window (their browsers stay mounted)
  const [kinds, setKinds] = useState<Record<string, string>>({}); // each cluster's listed kind
  const [revoking, setRevoking] = useState("");
  const history = useRef<string[]>([]);
  const seen = useRef(new Map<string, number>()); // when each session first showed, for the Open list's order

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
    setKubeconfigFiles(out.result?.kubeconfig ?? []);
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
        if (ev.type === "action.finished" && reloadsHosts(ev.action)) loadHosts().catch(report);
        if (ev.type === "action.finished" && reloadsContexts(ev.action)) loadContexts().catch(report);
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

  // Selection history: closing what is shown goes back to the previous selection.
  useEffect(() => {
    history.current = visit(history.current, active);
  }, [active]);
  const exists = useRef<(id: string) => boolean>(() => true);
  exists.current = (id: string) => {
    if ([ACTIVITY, TRANSFERS, TUNNELS, AGENTS].includes(id)) return true;
    if (id === ADD_HOST || id === ADD_CLUSTER || id === SYNC_HOSTS || editHostOf(id)) return false; // forms are not returned to
    if (hostOf(id)) return hosts.some((h) => h.name === hostOf(id));
    if (filesHostOf(id)) return browsers.includes(filesHostOf(id));
    if (clusterOf(id)) return clusters.includes(clusterOf(id));
    return tabs.some((t) => t.id === id);
  };
  const leave = (id: string) => setActive((a) => (a === id ? afterClose(history.current, id, exists.current) : a));

  // ⌘1–⌘4 select the destinations, wherever the focus is (the terminal included).
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const id = destinationFor(e);
      if (!id) return;
      e.preventDefault();
      e.stopPropagation();
      setActive(id);
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, []);

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
  const chooseKind = (ctx: string, kind: string) => {
    setKinds((k) => ({ ...k, [ctx]: kind }));
    openCluster(ctx);
  };
  const select = (id: string) => (clusterOf(id) ? openCluster(clusterOf(id)) : setActive(id));
  const showTerminal = (id: string) => {
    if (tabs.some((t) => t.id === id)) setActive(id);
  };
  const removeTab = (id: string) => {
    leave(id);
    dispatch({ type: "remove", id });
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
    setActive(filesId(host));
  };
  const closeFiles = (host: string) => {
    leave(filesId(host));
    setBrowsers((b) => b.filter((h) => h !== host));
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

  const hostSaved = async (name: string) => {
    await loadHosts().catch(report);
    setActive(hostId(name));
  };
  const clusterSaved = async (name: string) => {
    await loadContexts().catch(report);
    openCluster(name);
  };
  const synced = async (r: ImportOutput) => {
    setNotice(syncSummary(r));
    await loadHosts().catch(report);
    leave(SYNC_HOSTS);
  };
  const removeHost = async (h: Host) => {
    setRemovingHost(null);
    const out = await cox.invoke("ssh.hosts.remove", { name: h.name }).catch((e) => ({ status: "error" as const, error: String(e) }));
    if (out.status !== "ok") return report(new Error(out.error));
    leave(hostId(h.name));
    leave(editHostId(h.name));
    await loadHosts().catch(report);
  };
  const removeCluster = async (c: ContextInfo) => {
    setRemovingCluster(null);
    const out = await cox.invoke("k8s.clusters.remove", { context: c.name }).catch((e) => ({ status: "error" as const, error: String(e) }));
    if (out.status !== "ok") return report(new Error(out.error));
    leave(k8sTab(c.name));
    setClusters((list) => list.filter((x) => x !== c.name));
    await loadContexts().catch(report);
  };
  const hostMenu = (h: Host): MenuItem[] => [
    { label: "Open terminal", onSelect: () => openTerminal(h.name) },
    { label: "Files", onSelect: () => openFiles(h.name) },
    { label: "Edit…", onSelect: () => setActive(editHostId(h.name)), separatorBefore: true },
    { label: "Remove…", onSelect: () => setRemovingHost(h), danger: true },
  ];
  const clusterMenu = (c: ContextInfo): MenuItem[] => [
    { label: "Open", onSelect: () => openCluster(c.name) },
    c.source === "coxswain"
      ? { label: "Remove…", onSelect: () => setRemovingCluster(c), danger: true, separatorBefore: true }
      : { label: "From your kubeconfig (read-only)", onSelect: () => {}, disabled: true, separatorBefore: true },
  ];

  const agents = clients.filter((c) => !c.revoked).length;
  const hostProd = (h: string) => isProdTags(hosts.find((x) => x.name === h)?.tags);
  const ctxProd = (c: string) => {
    const info = contexts.find((x) => x.name === c);
    return info ? isProdContext(info) : false;
  };
  const running = runningCount(transfers);
  const typingTab = tabs.find((t) => (t.typingUntil ?? 0) > now && t.owner.kind === "agent");

  // The Open list: terminals and file browsers, in the order they first showed.
  const order = (id: string) => {
    if (!seen.current.has(id)) seen.current.set(id, seen.current.size);
    return seen.current.get(id)!;
  };
  const sessions: SideSession[] = [
    ...tabs.map((t): SideSession => ({
      id: t.id, kind: "terminal", where: t.host, owner: t.owner, closed: t.closed,
      prod: prodWhere(t.host, hosts, contexts), typing: (t.typingUntil ?? 0) > now,
      title: `Terminal on ${t.host}, opened by ${t.owner.kind === "agent" ? t.owner.name : "you"}${t.closed ? " (ended)" : ""}`,
    })),
    ...browsers.map((h): SideSession => ({
      id: filesId(h), kind: "files", where: h, owner: { kind: "human", name: "you" },
      prod: hostProd(h), typing: false, title: `Files on ${h}`,
    })),
  ].sort((a, b) => order(a.id) - order(b.id));
  const closeSession = (id: string) => {
    const t = tabs.find((x) => x.id === id);
    if (t) return closeTab(t);
    if (filesHostOf(id)) closeFiles(filesHostOf(id));
  };
  const selectedHost = hosts.find((h) => h.name === hostOf(active));
  const editedHost = hosts.find((h) => h.name === editHostOf(active));
  const openOn = (name: string) => ({
    sessions: sessions.filter((x) => !x.closed && placeOfWhere(x.where).kind === "host" && x.where === name).length,
    tunnels: tunnels.filter((t) => t.type !== "k8s" && t.host === name).length,
  });
  const footprintOf = (name: string) => footprint(name, tabs, tunnels, transfers);

  return (
    <div className="app">
      {!status.connected && (
        <div className="banner down" role="alert">
          <span className="pulse running" aria-hidden="true" />
          <span className="banner-text">The Coxswain server stopped. Start it with cox server; reconnecting…</span>
        </div>
      )}
      {error && (
        <div className="banner error" role="alert">
          <span className="banner-text">{error}</span>
          <button className="ghost small" onClick={() => setError("")}>
            Dismiss
          </button>
        </div>
      )}
      {notice && <Message kind="notice" onDismiss={() => setNotice("")}>{notice}</Message>}
      <div className="main">
        <Sidebar
          active={active}
          onSelect={select}
          hosts={hosts}
          contexts={contexts}
          contextsError={contextsError}
          sessions={sessions}
          running={running}
          tunnels={tunnels.length}
          agents={{ allowed: agents, typing: !!typingTab }}
          kindOf={(c) => kinds[c] ?? "pods"}
          onKind={chooseKind}
          onOpenTerminal={openTerminal}
          onCloseSession={closeSession}
          onAddHost={() => setActive(ADD_HOST)}
          onImportHosts={() => setActive(SYNC_HOSTS)}
          onAddCluster={() => setActive(ADD_CLUSTER)}
          hostMenu={hostMenu}
          clusterMenu={clusterMenu}
        />
        <section className="content">
          <div className="pane" style={{ display: active === ACTIVITY ? "flex" : "none" }}>
            <ActivityView rows={rows} onOpenTerminal={showTerminal} loadOutput={loadOutput} />
          </div>
          <div className="pane" style={{ display: active === TRANSFERS ? "flex" : "none" }}>
            <PaneHeader icon={<Icon name="@transfers" />} title="Transfers" meta={running > 0 ? `${running} running` : undefined} />
            <div className="pane-scroll">
              <TransfersView transfers={transfers} onCancel={askCancel} />
            </div>
          </div>
          <div className="pane" style={{ display: active === TUNNELS ? "flex" : "none" }}>
            <div className="pane-scroll">
              <TunnelsView tunnels={tunnels} hosts={hosts} onClose={askCloseTunnel} onCreate={createTunnel} />
            </div>
          </div>
          <AgentsView active={active === AGENTS} clients={clients} rows={rows} footprintOf={footprintOf}
            typing={(n) => tabs.some((t) => t.owner.name === n && (t.typingUntil ?? 0) > now)}
            onAllow={decide("PUT")} onRevoke={setRevoking} onSelect={select} />
          {selectedHost && (
            <HostView key={selectedHost.name} host={selectedHost} prod={hostProd(selectedHost.name)} active
              sessions={sessions.filter((x) => placeOfWhere(x.where).kind === "host" && x.where === selectedHost.name)}
              tunnels={tunnels.filter((t) => t.type !== "k8s" && t.host === selectedHost.name)}
              rows={rowsAt(rows, selectedHost.name, 12)}
              onTerminal={() => openTerminal(selectedHost.name)} onFiles={() => openFiles(selectedHost.name)}
              onSelect={select} onTunnels={() => setActive(TUNNELS)}
              onEdit={() => setActive(editHostId(selectedHost.name))} onRemove={() => setRemovingHost(selectedHost)} />
          )}
          {active === ADD_HOST && (
            <HostEditor cox={cox} hosts={hosts} onSaved={(n) => void hostSaved(n)} onCancel={() => leave(ADD_HOST)} />
          )}
          {editedHost && (
            <HostEditor key={editedHost.name} cox={cox} hosts={hosts} original={editedHost} onSaved={(n) => void hostSaved(n)}
              onCancel={() => setActive(hostId(editedHost.name))} onRemove={() => setRemovingHost(editedHost)} />
          )}
          {active === SYNC_HOSTS && <SyncView cox={cox} onDone={(r) => void synced(r)} onCancel={() => leave(SYNC_HOSTS)} />}
          {active === ADD_CLUSTER && (
            <ClusterEditor cox={cox} contexts={contexts} kubeconfig={kubeconfigFiles} onSaved={(n) => void clusterSaved(n)}
              onCancel={() => leave(ADD_CLUSTER)} />
          )}
          {browsers.map((h) => (
            <FileBrowser key={h} cox={cox} host={h} prod={hostProd(h)} note={hosts.find((x) => x.name === h)?.note} active={active === filesId(h)}
              onClose={() => closeFiles(h)} />
          ))}
          {clusters.map((c) => {
            const info = contexts.find((x) => x.name === c) ?? { name: c, cluster: "", namespace: "default" };
            return (
              <K8sView key={c} cox={cox} context={info} active={active === k8sTab(c)} kind={kinds[c] ?? "pods"}
                onOpenTerminal={openPodTerminal} onForward={forward}
                onRemove={info.source === "coxswain" ? () => setRemovingCluster(info) : undefined} />
            );
          })}
          {tabs.map((t) => (
            <TerminalView
              key={t.id}
              cox={cox}
              tab={t}
              prod={prodWhere(t.host, hosts, contexts)}
              typing={(t.typingUntil ?? 0) > now}
              active={active === t.id}
              onExit={(code) => dispatch({ type: "closed", id: t.id, code: code ?? undefined })}
              onClose={() => closeTab(t)}
            />
          ))}
        </section>
      </div>
      {closing && (
        <Confirm
          message={`${closing.owner.name} opened this terminal on ${closing.host}. Close it? Its shell ends.`}
          confirm="Close terminal"
          band={band(prodWhere(closing.host, hosts, contexts), closing.host)}
          onYes={() => endTerminal(closing)}
          onNo={() => setClosing(null)}
        />
      )}
      {cancelling && (
        <Confirm
          message={`${cancelling.owner.name} started this ${cancelling.direction} (${cancelling.host}:${cancelling.remote_path}). Cancel it?`}
          confirm="Cancel transfer"
          band={band(hostProd(cancelling.host), cancelling.host)}
          onYes={() => cancelTransfer(cancelling)}
          onNo={() => setCancelling(null)}
        />
      )}
      {revoking && (
        <Confirm
          message={revokeMessage(revoking, footprintOf(revoking))}
          confirm="Revoke"
          onYes={() => {
            decide("DELETE")(revoking);
            setRevoking("");
          }}
          onNo={() => setRevoking("")}
        />
      )}
      {removingHost && (
        <Confirm
          message={removeHostMessage(removingHost.name, openOn(removingHost.name).sessions, openOn(removingHost.name).tunnels)}
          confirm="Remove host"
          band={band(isProdTags(removingHost.tags), removingHost.name)}
          onYes={() => void removeHost(removingHost)}
          onNo={() => setRemovingHost(null)}
        />
      )}
      {removingCluster && (
        <Confirm
          message={`Remove ${removingCluster.name} from Coxswain? Its stored kubeconfig, tags and note are deleted. Your own kubeconfig is not touched.`}
          confirm="Remove cluster"
          band={band(isProdContext(removingCluster), removingCluster.name)}
          onYes={() => void removeCluster(removingCluster)}
          onNo={() => setRemovingCluster(null)}
        />
      )}
      {closingTunnel && (
        <Confirm
          message={`${closingTunnel.owner.name} opened this tunnel (${tunnelPlace(closingTunnel)}, ${closingTunnel.listen}). Close it? Its connections end.`}
          confirm="Close tunnel"
          band={band(closingTunnel.type === "k8s" ? ctxProd(closingTunnel.context ?? "") : hostProd(closingTunnel.host), tunnelPlace(closingTunnel))}
          onYes={() => closeTunnel(closingTunnel)}
          onNo={() => setClosingTunnel(null)}
        />
      )}
      <footer className="statusbar">
        <span className="conn">
          <span className={status.connected ? "dot" : "dot off"} aria-hidden="true" />
          {status.connected ? (
            <>
              Coxswain {status.version ?? ""} <span className="mono">{status.addr ?? ""}</span>
            </>
          ) : (
            "disconnected"
          )}
        </span>
        <span>
          {typingTab ? (
            <>
              <span className="crew-text">{typingTab.owner.name}</span> is typing in <span className="mono">{typingTab.host}</span>
            </>
          ) : (
            <>
              {agents} agent{agents === 1 ? "" : "s"} allowed
            </>
          )}
        </span>
      </footer>
    </div>
  );
}

/** band is the production band of a confirmation, or nothing. */
function band(prod: boolean, where: string) {
  return prod ? <HazardBand env="env=prod" where={where} /> : undefined;
}
