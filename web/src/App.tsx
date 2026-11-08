import { useCallback, useEffect, useState } from "react";
import { ActivityView } from "./ActivityView";
import { addEvent, type Row } from "./activity";
import { cox } from "./bridge";
import type { Cox, Status } from "./cox";
import { Sidebar } from "./Sidebar";
import type { ClientInfo, Host } from "./types";

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

  const report = useCallback((e: unknown) => setError(e instanceof Error ? e.message : String(e)), []);

  const loadHosts = useCallback(async () => {
    const out = await cox.invoke<Host[]>("ssh.hosts.list");
    if (out.status !== "ok") throw new Error(out.error);
    setHosts(out.result ?? []);
  }, [cox]);
  const loadClients = useCallback(async () => {
    setClients((await cox.api<ClientInfo[]>("GET", "/api/v1/clients")) ?? []);
  }, [cox]);
  const loadAll = useCallback(() => {
    loadHosts().catch(report);
    loadClients().catch(report);
  }, [loadHosts, loadClients, report]);

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

  const openTerminal = useCallback(
    (host: string) => {
      cox.api("POST", "/api/v1/terminals", { host, cols: 80, rows: 24 }).catch(report);
    },
    [cox, report],
  );
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
            <button className="tab active">Activity</button>
          </nav>
          <div className="tab-body">
            <ActivityView rows={rows} onOpenTerminal={() => {}} />
          </div>
        </section>
      </div>
      <footer className="statusbar">
        <span>{status.connected ? `Coxswain ${status.version ?? ""} · ${status.addr ?? ""}` : "disconnected"}</span>
        <span>
          {agents} agent{agents === 1 ? "" : "s"} allowed
        </span>
      </footer>
    </div>
  );
}
