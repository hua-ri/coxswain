// The sidebar's places: hosts and clusters ordered production first, a
// filter over them, and which sections are folded.
import { isProdTag, isProdTags } from "./env";
import type { ContextInfo } from "./k8s";
import { orderPlaces } from "./nav";
import type { Host } from "./types";

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

/** sortedHosts are the hosts that match the filter, production first (see orderPlaces). */
export function sortedHosts(hosts: Host[], filter = ""): Host[] {
  return orderPlaces(hosts.filter((h) => hostMatches(h, filter)), envOfHost);
}

/** envOfContext is the context's env, lower-cased; production spellings become "prod". */
export function envOfContext(c: ContextInfo): string {
  return contextIsProd(c) ? "prod" : (c.tags?.env ?? "").toLowerCase();
}

/** sortedContexts are the contexts that match the filter, production first. */
export function sortedContexts(contexts: ContextInfo[], filter = ""): ContextInfo[] {
  return orderPlaces(contexts.filter((c) => contextMatches(c, filter)), envOfContext);
}

/** hostTitle is the hover text for a host row. */
export function hostTitle(h: Host, address: string): string {
  const lines = [h.name, address];
  const tags = tagText(h.tags).sort();
  if (tags.length) lines.push(tags.join("  "));
  if (h.proxy_jump) lines.push(`via ${h.proxy_jump}`);
  if (h.note) lines.push(`Note: ${h.note}`);
  lines.push("Double-click: terminal");
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

const HOST_CHANGES = ["ssh.hosts.add", "ssh.hosts.update", "ssh.hosts.remove", "ssh.hosts.import", "ssh.hosts.tag"];
const CONTEXT_CHANGES = ["k8s.clusters.add", "k8s.clusters.remove", "k8s.contexts.tag"];

/** reloadsHosts: a finished action changed the saved hosts (from any client), so the Hosts section reloads. */
export function reloadsHosts(action: string | undefined): boolean {
  return HOST_CHANGES.includes(action ?? "");
}

/** reloadsContexts: a finished action changed the cluster list or its tags. */
export function reloadsContexts(action: string | undefined): boolean {
  return CONTEXT_CHANGES.includes(action ?? "");
}
