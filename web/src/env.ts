// Where things run, and whether that is production: the UI marks
// production hosts, contexts, terminals and activity the same way.
import type { ContextInfo } from "./k8s";
import type { Host } from "./types";

/** isProdTag: the tag is env=prod (or env=production). */
export function isProdTag(key: string, value: string | undefined): boolean {
  const v = (value ?? "").toLowerCase();
  return key === "env" && (v === "prod" || v === "production");
}

/** isProdTags: the tags include env=prod. */
export function isProdTags(tags: Record<string, string> | undefined): boolean {
  return Object.entries(tags ?? {}).some(([k, v]) => isProdTag(k, v));
}

/** prodWhere: a terminal's place (a host, or context/namespace/pod[/container]) is in production. */
export function prodWhere(where: string, hosts: Host[], contexts: ContextInfo[]): boolean {
  const host = hosts.find((h) => h.name === where);
  if (host) return isProdTags(host.tags);
  const ctx = where.split("/")[0];
  const c = where.includes("/") ? contexts.find((x) => x.name === ctx) : undefined;
  return c ? isProdTags(c.tags) : false;
}
