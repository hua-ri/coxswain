// The activity table's model: audit events folded into rows.
import type { AuditEvent } from "./cox";

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
}

export const MAX_ROWS = 1000;

/** clientLabel shows the user's own clients (cli, desktop) as "you". */
export function clientLabel(name: string): string {
  return name === "cli" || name === "desktop" ? "you" : name;
}

/** addEvent returns rows (newest first) with ev folded in. */
export function addEvent(rows: Row[], ev: AuditEvent, key: number): Row[] {
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
      const detail = [args.command, args.text, args.host].find((v) => typeof v === "string") as string | undefined;
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
    default:
      return prepend(rows, { ...base, action: ev.type });
  }
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
