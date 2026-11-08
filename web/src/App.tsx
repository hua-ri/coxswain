import { useCallback, useEffect, useReducer, useState } from "react";
import { ActivityView } from "./ActivityView";
import { addEvent, type Row } from "./activity";
import { cox } from "./bridge";
import type { Cox, Status } from "./cox";
import { Confirm } from "./Confirm";
import { Sidebar } from "./Sidebar";
import { TerminalView } from "./TerminalView";
import { needsConfirm, tabTitle, tabsReducer, type Tab } from "./terminals";
import type { ClientInfo, Host, TermSession } from "./types";

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
  const [active, setActive] = useState(""); // "" is the activity tab
  const [closing, setClosing] = useState<Tab | null>(null);
  const [now, setNow] = useState(Date.now());

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
  const loadAll = useCallback(() => {
    loadHosts().catch(report);
    loadClients().catch(report);
    loadTerminals().catch(report);
  }, [loadHosts, loadClients, loadTerminals, report]);

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
        if (ev.type.startsWith("client.")) loadClients().catch(report);
        if (ev.type === "terminal.opened" && ev.terminal && ev.actor) {
          const { kind, name } = ev.actor;
          dispatch({ type: "opened", id: ev.terminal, host: ev.targets?.[0]?.id ?? "", owner: { kind, name } });
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
      })
      .then((s) => (cancelled ? s() : (stop = s)), report);
    return () => {
      cancelled = true;
      stop?.();
    };
  }, [cox, loadClients, loadHosts, report]);

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
          dispatch({ type: "opened", id: s.id, host: s.host, owner: s.owner });
          setActive(s.id);
        })
        .catch(report);
    },
    [cox, report],
  );
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
          onOpenTerminal={openTerminal}
          onAllow={decide("PUT")}
          onRevoke={decide("DELETE")}
        />
        <section className="content">
          <nav className="tabs">
            <button className={active === "" ? "tab active" : "tab"} onClick={() => setActive("")}>
              Activity
            </button>
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
              <ActivityView rows={rows} onOpenTerminal={showTerminal} />
            </div>
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
      <footer className="statusbar">
        <span>{status.connected ? `Coxswain ${status.version ?? ""} · ${status.addr ?? ""}` : "disconnected"}</span>
        <span>
          {agents} agent{agents === 1 ? "" : "s"} allowed
        </span>
      </footer>
    </div>
  );
}
