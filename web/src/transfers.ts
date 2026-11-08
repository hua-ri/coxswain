// The transfers panel's model: uploads and downloads, kept in sync with
// the server's list and its transfer.* events.
import type { AuditEvent } from "./cox";

export interface Transfer {
  id: string;
  direction: "upload" | "download";
  host: string;
  local_path: string;
  remote_path: string;
  owner: { kind: "human" | "agent"; name: string };
  status: "running" | "done" | "failed" | "cancelled";
  bytes: number;
  total: number;
  resumed_from?: number;
  sha256?: string;
  verified?: boolean;
  started_at: string;
  finished_at?: string;
  error?: string;
}

/** TransferItem is a transfer with its measured speed. */
export interface TransferItem extends Transfer {
  speed?: number; // bytes per second, from the last two progress events
  sampleBytes?: number;
  sampleAt?: number; // ms
}

export type TransferAction = { type: "sync"; list: Transfer[] } | { type: "event"; event: AuditEvent };

export function transfersReducer(items: TransferItem[], a: TransferAction): TransferItem[] {
  if (a.type === "sync") {
    const old = new Map(items.map((t) => [t.id, t]));
    return a.list
      .map((t) => {
        const prev = old.get(t.id);
        return prev && t.status === "running"
          ? { ...t, speed: prev.speed, sampleBytes: prev.sampleBytes, sampleAt: prev.sampleAt }
          : { ...t };
      })
      .sort(newestFirst);
  }
  const ev = a.event;
  if (!ev.type.startsWith("transfer.") || !ev.detail) return items;
  const t = ev.detail as Transfer;
  const at = Date.parse(ev.time);
  const i = items.findIndex((x) => x.id === t.id);
  const prev = i >= 0 ? items[i] : undefined;
  if (prev && prev.status !== "running" && t.status === "running") return items; // late progress
  const next: TransferItem = { ...t, sampleBytes: t.bytes, sampleAt: at };
  if (ev.type === "transfer.failed" && !next.error && ev.reason) next.error = ev.reason;
  if (prev && t.status === "running") {
    const dt = (at - (prev.sampleAt ?? at)) / 1000;
    next.speed = dt > 0 ? (t.bytes - (prev.sampleBytes ?? 0)) / dt : prev.speed;
  }
  if (!prev) return [next, ...items].sort(newestFirst);
  const out = items.slice();
  out[i] = next;
  return out;
}

function newestFirst(a: Transfer, b: Transfer): number {
  return a.started_at < b.started_at ? 1 : a.started_at > b.started_at ? -1 : 0;
}

/** progress is the done fraction, 0..1. */
export function progress(t: Transfer): number {
  if (t.total > 0) return Math.min(1, t.bytes / t.total);
  return t.status === "done" ? 1 : 0;
}

export function runningCount(items: Transfer[]): number {
  return items.filter((t) => t.status === "running").length;
}

/** describe is "upload local → host:remote" (or the other way round). */
export function describeTransfer(t: Pick<Transfer, "direction" | "host" | "local_path" | "remote_path">): string {
  const remote = `${t.host}:${t.remote_path}`;
  return t.direction === "upload" ? `upload ${t.local_path} → ${remote}` : `download ${remote} → ${t.local_path}`;
}
