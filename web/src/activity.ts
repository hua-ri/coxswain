// The activity table's model: audit events folded into rows.
import type { AuditEvent } from "./cox";
import { finishBatch, hostDone, startBatch, type BatchEnd, type BatchHostDone, type BatchInfo, type BatchStart } from "./batch";
import { describeTransfer, type Transfer } from "./transfers";
import { tunnelRoute, type Tunnel } from "./tunnels";

export interface Row {
  key: number;
  time: string;
  client: string; // client or actor name
  who: string; // client as shown: "you" for the user's own clients
  agent: boolean;
  action: string; // action id, or the event type for client.* / terminal.*
  hosts: string[];
  risk?: string;
  status: "running" | "ok" | "failed" | "refused" | "info";
  durationMs?: number;
  error?: string;
  detail?: string; // command line, typed text, or why a terminal closed
  terminal?: string; // terminal id, for jumping to its tab
  session?: string;
  batch?: BatchInfo; // ssh.batch rows: per-host progress
}

export const MAX_ROWS = 1000;

/** clientLabel shows the user's own clients (cli, desktop) as "you". */
export function clientLabel(name: string): string {
  return name === "cli" || name === "desktop" ? "you" : name;
}

/** addEvent returns rows (newest first) with ev folded in. */
export function addEvent(rows: Row[], ev: AuditEvent, key: number): Row[] {
  if (internal(ev)) return rows;
  const actor = ev.actor;
  const client = actor?.name ?? ev.client ?? "";
  const base: Row = {
    key,
    time: ev.time,
    client,
    who: actor?.kind === "human" ? "you" : clientLabel(client),
    agent: actor ? actor.kind === "agent" : ev.client !== undefined,
    action: ev.action ?? ev.type,
    hosts: (ev.targets ?? []).map((t) => t.id),
    status: "info",
    session: actor?.session,
  };
  switch (ev.type) {
    case "action.requested": {
      const args = ev.args ?? {};
      const terminal = ev.action?.startsWith("terminal.") && typeof args.id === "string" ? args.id : undefined;
      const detail = requestDetail(args);
      return prepend(rows, { ...base, risk: ev.risk, status: "running", detail, terminal });
    }
    case "action.finished":
    case "action.failed": {
      const i = rows.findIndex(
        (r) => r.status === "running" && r.action === ev.action && r.client === base.client && r.session === base.session,
      );
      const result: Partial<Row> = {
        status: ev.type === "action.finished" ? "ok" : "failed",
        durationMs: ev.duration_ms ?? 0,
        error: ev.error,
      };
      if (i < 0) return prepend(rows, { ...base, ...result });
      const out = rows.slice();
      out[i] = { ...rows[i], ...result };
      return out;
    }
    case "action.refused":
      return prepend(rows, { ...base, status: "refused", error: ev.reason });
    case "terminal.opened":
    case "terminal.closed": {
      let detail = ev.reason;
      if (ev.exit_code !== undefined) detail = `${detail ?? "exited"} (exit code ${ev.exit_code})`;
      return prepend(rows, { ...base, action: ev.type, terminal: ev.terminal, detail });
    }
    case "transfer.started":
    case "transfer.progress":
      return rows; // the transfers panel shows these
    case "transfer.finished":
    case "transfer.failed": {
      const t = ev.detail as Transfer | undefined;
      let detail = t ? describeTransfer(t) : undefined;
      const why = ev.reason ?? ev.error ?? t?.error;
      if (ev.type === "transfer.failed" && why) detail = `${detail ?? ""}: ${why}`;
      return prepend(rows, { ...base, action: ev.type, hosts: base.hosts.length ? base.hosts : t ? [t.host] : [], detail });
    }
    case "batch.started": {
      const d = ev.detail as BatchStart | undefined;
      if (!d) return rows;
      const batch = startBatch(d);
      const i = rows.findIndex((r) => r.status === "running" && r.action === "ssh.batch" && !r.batch && r.client === base.client);
      if (i < 0) {
        const hosts = base.hosts.length ? base.hosts : batch.hosts.map((h) => h.host);
        return prepend(rows, { ...base, action: "ssh.batch", hosts, status: "running", detail: d.command, batch });
      }
      return replace(rows, i, { ...rows[i], batch });
    }
    case "batch.host_done":
    case "batch.finished": {
      const i = rows.findIndex((r) => r.batch?.id === ev.batch);
      if (i < 0 || !ev.detail) return rows;
      const r = rows[i];
      if (ev.type === "batch.host_done") return replace(rows, i, { ...r, batch: hostDone(r.batch!, ev.detail as BatchHostDone) });
      const batch = finishBatch(r.batch!, ev.detail as BatchEnd);
      const row: Row = { ...r, batch };
      if (r.risk === undefined && r.status === "running") {
        // No action row to finish it (the batch row stands alone).
        row.status = ev.error || (batch.summary?.failed ?? 0) > 0 ? "failed" : "ok";
        if (ev.error) row.error = ev.error;
        else if (batch.summary?.failed) row.error = `${batch.summary.failed} of ${batch.summary.total} hosts failed`;
      }
      return replace(rows, i, row);
    }
    case "tunnel.opened":
    case "tunnel.reconnecting":
    case "tunnel.reconnected":
    case "tunnel.closed":
    case "tunnel.connection_failed": {
      const t = ev.detail as Tunnel | undefined;
      let detail = t ? tunnelRoute(t) : undefined;
      const why = ev.reason ?? ev.error;
      if (why) detail = `${detail ?? ""}: ${why}`;
      const hosts = base.hosts.length ? base.hosts : t ? [t.host] : [];
      if (ev.type === "tunnel.connection_failed") {
        return prepend(rows, { ...base, action: ev.type, hosts, status: "failed", error: detail });
      }
      return prepend(rows, { ...base, action: ev.type, hosts, detail });
    }
    default:
      return prepend(rows, { ...base, action: ev.type });
  }
}

/** internal: the desktop's own plumbing (reading batch output), kept out of the table but not the audit log. */
function internal(ev: AuditEvent): boolean {
  return ev.action === "artifact.read" && ev.actor?.kind === "human" && ev.actor.name === "desktop";
}

function replace(rows: Row[], i: number, row: Row): Row[] {
  const out = rows.slice();
  out[i] = row;
  return out;
}

function prepend(rows: Row[], row: Row): Row[] {
  return [row, ...rows.slice(0, MAX_ROWS - 1)];
}

export interface Filter {
  client: string; // "" = all
  host: string;
}

export function filterRows(rows: Row[], f: Filter): Row[] {
  return rows.filter((r) => (!f.client || r.client === f.client) && (!f.host || r.hosts.includes(f.host)));
}

/** filterOptions lists the clients and hosts that appear in rows. */
export function filterOptions(rows: Row[]): { clients: string[]; hosts: string[] } {
  const clients = new Set<string>();
  const hosts = new Set<string>();
  for (const r of rows) {
    if (r.client) clients.add(r.client);
    r.hosts.forEach((h) => hosts.add(h));
  }
  return { clients: [...clients].sort(), hosts: [...hosts].sort() };
}

/** requestDetail summarises a request's arguments: the command, the text typed, the file (with the size
 * written; file contents are not in the activity stream) or the host. */
function requestDetail(args: Record<string, unknown>): string | undefined {
  const path = [args.path, args.remote_path].find((v) => typeof v === "string") as string | undefined;
  if (path !== undefined) {
    const content = args.content as { bytes?: unknown } | undefined;
    return typeof content?.bytes === "number" ? `${path} (${content.bytes} bytes)` : path;
  }
  return [args.command, args.text, args.host].find((v) => typeof v === "string") as string | undefined;
}
