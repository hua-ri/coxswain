// The sidebar's structure: hosts grouped by environment (production first),
// a filter over hosts and clusters, and which sections are folded.
import { isProdTag, isProdTags } from "./env";
import type { ContextInfo } from "./k8s";
import type { Host } from "./types";

export interface HostGroup {
  key: string; // "prod", an env value, or "" for hosts without env
  label: string;
  prod: boolean;
  hosts: Host[];
}

/** envOfHost is the host's env tag, lower-cased; production spellings become "prod". */
export function envOfHost(h: Host): string {
  const v = (h.tags?.env ?? "").toLowerCase();
  return isProdTag("env", v) ? "prod" : v;
}

/** matches reports whether a filter text matches any of the given fields. */
function matches(filter: string, fields: (string | undefined)[]): boolean {
  const f = filter.trim().toLowerCase();
  if (!f) return true;
  return f.split(/\s+/).every((word) => fields.some((x) => (x ?? "").toLowerCase().includes(word)));
}

function tagText(tags: Record<string, string> | undefined): string[] {
  return Object.entries(tags ?? {}).map(([k, v]) => `${k}=${v}`);
}

/** hostMatches: the filter matches the host's name, address, user, tags or note. */
export function hostMatches(h: Host, filter: string): boolean {
  return matches(filter, [h.name, h.hostname, h.user, h.note, ...tagText(h.tags)]);
}

/** contextMatches: the filter matches the context's name, cluster, namespace, tags or note. */
export function contextMatches(c: ContextInfo, filter: string): boolean {
  return matches(filter, [c.name, c.cluster, c.namespace, c.server, c.note, ...tagText(c.tags)]);
}

/**
 * groupHosts groups the hosts that match the filter by environment:
 * production first, then other environments by name, then hosts with no
 * env tag. Hosts keep their order within a group.
 */
export function groupHosts(hosts: Host[], filter = ""): HostGroup[] {
  const groups = new Map<string, Host[]>();
  for (const h of hosts) {
    if (!hostMatches(h, filter)) continue;
    const env = envOfHost(h);
    groups.set(env, [...(groups.get(env) ?? []), h]);
  }
  const keys = [...groups.keys()].sort((a, b) => rank(a) - rank(b) || a.localeCompare(b));
  return keys.map((key) => ({
    key,
    label: key === "prod" ? "Production" : key === "" ? "No environment" : key.charAt(0).toUpperCase() + key.slice(1),
    prod: key === "prod",
    hosts: groups.get(key)!,
  }));
}

function rank(env: string): number {
  return env === "prod" ? 0 : env === "" ? 2 : 1;
}

/** extraTags are a host's tags other than env, as k=v, for its tooltip line. */
export function extraTags(tags: Record<string, string> | undefined): string[] {
  return tagText(tags)
    .filter((t) => !t.startsWith("env="))
    .sort();
}

/** hostTitle is the hover text for a host row. */
export function hostTitle(h: Host, address: string): string {
  const lines = [h.name, address];
  const tags = tagText(h.tags).sort();
  if (tags.length) lines.push(tags.join("  "));
  if (h.proxy_jump) lines.push(`via ${h.proxy_jump}`);
  if (h.note) lines.push(`Note: ${h.note}`);
  lines.push("Double-click or Enter: terminal");
  return lines.join("\n");
}

/** contextIsProd: the context's effective tags say production. */
export function contextIsProd(c: ContextInfo): boolean {
  return isProdTags(c.tags);
}

const FOLD_KEY = "cox.sidebar.folded";

/** loadFolded reads which sections are folded. */
export function loadFolded(storage: Pick<Storage, "getItem"> | undefined): Set<string> {
  try {
    const raw = storage?.getItem(FOLD_KEY);
    const list = raw ? (JSON.parse(raw) as unknown) : [];
    return new Set(Array.isArray(list) ? list.filter((x): x is string => typeof x === "string") : []);
  } catch {
    return new Set();
  }
}

/** toggleFolded flips a section and saves the result. */
export function toggleFolded(folded: Set<string>, key: string, storage: Pick<Storage, "setItem"> | undefined): Set<string> {
  const next = new Set(folded);
  if (next.has(key)) next.delete(key);
  else next.add(key);
  try {
    storage?.setItem(FOLD_KEY, JSON.stringify([...next].sort()));
  } catch {
    // storage full or unavailable: folding still works for this window
  }
  return next;
}
