// The tunnels panel's model, kept in sync with the server's list and its
// tunnel.* events.
import type { AuditEvent } from "./cox";

export interface Tunnel {
  id: string;
  type: "local" | "remote" | "socks" | "k8s";
  host: string; // SSH tunnels
  // k8s tunnels: pod is where connections go now (a service's resolved pod).
  context?: string;
  namespace?: string;
  pod?: string;
  service?: string;
  remote_port?: number;
  listen: string; // 127.0.0.1:port here (local, socks) or on the host (remote)
  target?: string;
  owner: { kind: "human" | "agent"; name: string };
  created_at: string;
  status: "open" | "reconnecting";
  connections: number;
  total_connections: number;
  bytes_out: number;
  bytes_in: number;
  idle_minutes: number;
  error?: string; // why it is reconnecting
  failed_connections?: number; // connections that could not reach the target
  last_error?: string;
  last_error_at?: string;
}

export type TunnelAction = { type: "sync"; list: Tunnel[] } | { type: "event"; event: AuditEvent };

export function tunnelsReducer(list: Tunnel[], a: TunnelAction): Tunnel[] {
  if (a.type === "sync") return a.list.slice().sort(newestFirst);
  const ev = a.event;
  if (!ev.type.startsWith("tunnel.") || !ev.detail) return list;
  const t = ev.detail as Tunnel;
  const i = list.findIndex((x) => x.id === t.id);
  if (ev.type === "tunnel.closed") return i < 0 ? list : list.filter((x) => x.id !== t.id);
  if (i < 0) return ev.type === "tunnel.opened" ? [t, ...list].sort(newestFirst) : list;
  const out = list.slice();
  out[i] = t;
  return out;
}

function newestFirst(a: Tunnel, b: Tunnel): number {
  return a.created_at < b.created_at ? 1 : a.created_at > b.created_at ? -1 : 0;
}

/** tunnelRoute says where connections enter and where they go. */
export function tunnelRoute(t: Tunnel): string {
  switch (t.type) {
    case "k8s": {
      const ns = `${t.context ?? ""}/${t.namespace ?? ""}`;
      if (t.service) return `${t.listen} → ${ns}/service ${t.service}:${t.remote_port ?? ""} (pod ${t.target ?? ""})`;
      return `${t.listen} → ${ns}/${t.target ?? ""}`;
    }
    case "local":
      return `${t.listen} → ${t.target ?? ""} on ${t.host}`;
    case "remote":
      return `${t.listen} on ${t.host} → ${t.target ?? ""} here`;
    default:
      return `SOCKS5 ${t.listen} → via ${t.host}`;
  }
}

/** tunnelPlace is what a tunnel goes through: its host, or context/namespace. */
export function tunnelPlace(t: Tunnel): string {
  return t.type === "k8s" ? `${t.context ?? ""}/${t.namespace ?? ""}` : t.host;
}

/** TunnelForm is the "New tunnel" form, as typed. */
export interface TunnelForm {
  host: string;
  type: Exclude<Tunnel["type"], "k8s">;
  localPort: string;
  remoteHost: string;
  remotePort: string;
}

/** tunnelRequest turns the form into a POST /api/v1/tunnels body, or says what is wrong. */
export function tunnelRequest(f: TunnelForm): { body: Record<string, unknown> } | { error: string } {
  if (!f.host) return { error: "Choose a host." };
  const port = (s: string): number | null | undefined => {
    const v = s.trim();
    if (v === "") return undefined;
    if (!/^\d+$/.test(v)) return null;
    const n = Number(v);
    return n >= 1 && n <= 65535 ? n : null;
  };
  const local = port(f.localPort);
  const remote = f.type === "socks" ? undefined : port(f.remotePort);
  if (local === null || remote === null) return { error: "Ports are numbers from 1 to 65535." };
  const body: Record<string, unknown> = { host: f.host, type: f.type };
  if (local !== undefined) body.local_port = local;
  switch (f.type) {
    case "local":
      if (remote === undefined) return { error: "A local tunnel needs the remote port." };
      if (f.remoteHost.trim()) body.remote_host = f.remoteHost.trim();
      body.remote_port = remote;
      break;
    case "remote":
      if (local === undefined) return { error: "A remote tunnel needs the local port it leads to." };
      if (remote !== undefined) body.remote_port = remote;
      break;
  }
  return { body };
}

/** failureText summarizes a tunnel's failed connections, or "" if none. */
export function failureText(t: Tunnel): string {
  const n = t.failed_connections ?? 0;
  if (n === 0) return "";
  return `${n} failed connection${n === 1 ? "" : "s"}; last: ${t.last_error ?? "unknown error"}`;
}
