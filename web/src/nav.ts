// The shell's navigation model: one source list, one selection. The
// selected id names what the main area shows; see the "Shell and
// navigation" section of .interface-design/system.md.
import { contextFromTab, k8sTab } from "./k8s";
import type { Row } from "./activity";

/** Ids of the fixed destinations, and the prefixes of place and session ids. */
export const ACTIVITY = "@activity";
export const TRANSFERS = "@transfers";
export const TUNNELS = "@tunnels";
export const AGENTS = "@agents";
const HOST = "@host:";
const FILES = "@files:";
const EDIT_HOST = "@edit-host:";
/** The add host and add cluster panes. */
export const ADD_HOST = "@add-host";
export const ADD_CLUSTER = "@add-cluster";
/** The sync from ~/.ssh/config pane. */
export const SYNC_HOSTS = "@sync-hosts";

export interface Destination {
  id: string;
  label: string;
  key: string; // ⌘ + key selects it
}

export const DESTINATIONS: Destination[] = [
  { id: ACTIVITY, label: "Activity", key: "1" },
  { id: TRANSFERS, label: "Transfers", key: "2" },
  { id: TUNNELS, label: "Tunnels", key: "3" },
  { id: AGENTS, label: "Agents", key: "4" },
];

/** The pseudo-kind of a cluster's Apply YAML panel. */
export const APPLY = "@apply";

export const hostId = (name: string) => HOST + name;
export const filesId = (host: string) => FILES + host;
export const clusterId = k8sTab;
export const hostOf = (id: string) => (id.startsWith(HOST) ? id.slice(HOST.length) : "");
export const filesHostOf = (id: string) => (id.startsWith(FILES) ? id.slice(FILES.length) : "");
export const clusterOf = contextFromTab;
export const editHostId = (name: string) => EDIT_HOST + name;
export const editHostOf = (id: string) => (id.startsWith(EDIT_HOST) ? id.slice(EDIT_HOST.length) : "");

/** destinationFor is the destination a ⌘-digit selects, or "". */
export function destinationFor(e: { key: string; metaKey: boolean; ctrlKey?: boolean; altKey?: boolean; shiftKey?: boolean }): string {
  if (!e.metaKey || e.ctrlKey || e.altKey || e.shiftKey) return "";
  return DESTINATIONS.find((d) => d.key === e.key)?.id ?? "";
}

/** A session: a terminal (ssh or pod) or a file browser. */
export interface Session {
  id: string;
  kind: "terminal" | "files";
  where: string; // host name, or context/namespace/pod[/container]
  owner: { kind: "human" | "agent"; name: string };
  closed?: boolean;
}

/** A place a session belongs to. */
export interface Place {
  kind: "host" | "cluster";
  name: string;
}

/** placeOfWhere: a pod terminal's where starts with its context; anything else is a host. */
export function placeOfWhere(where: string): Place {
  const i = where.indexOf("/");
  return i < 0 ? { kind: "host", name: where } : { kind: "cluster", name: where.slice(0, i) };
}

/** sessionCounts counts open sessions per place ("host:web-1", "cluster:prod-hk") and whether an agent holds one. */
export function sessionCounts(sessions: Session[]): Map<string, { n: number; agent: boolean }> {
  const out = new Map<string, { n: number; agent: boolean }>();
  for (const s of sessions) {
    if (s.closed) continue;
    const p = placeOfWhere(s.where);
    const k = `${p.kind}:${p.name}`;
    const c = out.get(k) ?? { n: 0, agent: false };
    out.set(k, { n: c.n + 1, agent: c.agent || s.owner.kind === "agent" });
  }
  return out;
}

/** envRank orders environments: production first, then by name, then none. */
function envRank(env: string): number {
  return env === "prod" ? 0 : env === "" ? 2 : 1;
}

/** orderPlaces sorts places production first, then other environments by name, then untagged; stable within one. */
export function orderPlaces<T>(items: T[], envOf: (x: T) => string): T[] {
  return items
    .map((x, i) => ({ x, i, env: envOf(x) }))
    .sort((a, b) => envRank(a.env) - envRank(b.env) || a.env.localeCompare(b.env) || a.i - b.i)
    .map((e) => e.x);
}

/** moveIndex is where ↑/↓/Home/End moves in a list of n (no wrapping, like a Mac list), or -1. */
export function moveIndex(key: string, i: number, n: number): number {
  if (n === 0) return -1;
  switch (key) {
    case "ArrowDown":
      return Math.min(i + 1, n - 1);
    case "ArrowUp":
      return Math.max(i - 1, 0);
    case "Home":
      return 0;
    case "End":
      return n - 1;
  }
  return -1;
}

const HISTORY = 30;

/** visit records a selection, most recent last, without repeats. */
export function visit(history: string[], id: string): string[] {
  const next = history.filter((x) => x !== id);
  next.push(id);
  return next.slice(-HISTORY);
}

/** afterClose is where the selection goes when id closes: the most recent other selection that still exists. */
export function afterClose(history: string[], id: string, exists: (id: string) => boolean): string {
  for (let i = history.length - 1; i >= 0; i--) {
    const h = history[i];
    if (h !== id && exists(h)) return h;
  }
  return ACTIVITY;
}

/** What an agent holds open. */
export interface Footprint {
  terminals: number;
  tunnels: number;
  transfers: number; // running
}

type Owned = { owner: { kind: string; name: string } };

/** footprint counts the open terminals, tunnels and running transfers an agent owns. */
export function footprint(
  name: string,
  terminals: (Owned & { closed?: boolean })[],
  tunnels: Owned[],
  transfers: (Owned & { status: string })[],
): Footprint {
  const mine = (o: Owned) => o.owner.kind === "agent" && o.owner.name === name;
  return {
    terminals: terminals.filter((t) => mine(t) && !t.closed).length,
    tunnels: tunnels.filter(mine).length,
    transfers: transfers.filter((t) => mine(t) && t.status === "running").length,
  };
}

const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"}`;

/** footprintText is "1 terminal, 2 tunnels", or "" when nothing is open. */
export function footprintText(f: Footprint): string {
  const parts: string[] = [];
  if (f.terminals) parts.push(plural(f.terminals, "terminal"));
  if (f.tunnels) parts.push(plural(f.tunnels, "tunnel"));
  if (f.transfers) parts.push(plural(f.transfers, "running transfer"));
  return parts.join(", ");
}

/** revokeMessage is the confirmation text for revoking an agent. */
export function revokeMessage(name: string, f: Footprint): string {
  const held = footprintText(f);
  const first = `Revoke ${name}? Its calls are refused from now on and it is disconnected.`;
  return held ? `${first} It still holds ${held}; they stay open until you close them.` : first;
}

/** lastAction is the newest activity row of a client (rows are newest first), if any. */
export function lastAction(rows: Row[], client: string): Row | undefined {
  return rows.find((r) => r.client === client);
}

/** rowsAt are the activity rows that touched a host, newest first, at most n. */
export function rowsAt(rows: Row[], host: string, n: number): Row[] {
  const out: Row[] = [];
  for (const r of rows) {
    if (r.hosts.includes(host)) out.push(r);
    if (out.length >= n) break;
  }
  return out;
}

/** The kinds a cluster shows before "More kinds". */
const COMMON_KINDS = ["pods", "deployments", "statefulsets", "daemonsets", "jobs", "services", "ingresses", "events"];

/** visibleKinds: the common kinds, plus the current one; all of them when expanded. */
export function visibleKinds<T extends { kind: string }>(all: T[], expanded: boolean, current: string): T[] {
  if (expanded) return all;
  return all.filter((k) => COMMON_KINDS.includes(k.kind) || k.kind === current);
}

/** removeHostMessage is the confirmation text for removing a host; what is open on it stays. */
export function removeHostMessage(name: string, sessions: number, tunnels: number): string {
  const first = `Remove ${name} from Coxswain? Its tags and note go with it.`;
  const open = [sessions && plural(sessions, "session"), tunnels && plural(tunnels, "tunnel")].filter(Boolean).join(" and ");
  return open ? `${first} ${open} on it stay open until you close them.` : first;
}
