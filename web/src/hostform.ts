// The add/edit host form: its fields, validation (the same rules as the
// server's), the requests it sends, and the connection check (scan the
// host key, let the user trust it, log in).
import type { Outcome } from "./cox";
import { isProdTag } from "./env";
import type { Host } from "./types";

export interface HostForm {
  name: string;
  hostname: string;
  port: string; // "" is 22
  user: string;
  keys: string[]; // identity files; none: ssh-agent, then the default keys
  jump: string; // ProxyJump: saved host names or user@host:port, comma-separated
  env: string; // "", prod, staging, dev, or a custom value
  tags: string; // other tags, "role=web, team=core"
  note: string;
}

/** The environments offered; an existing host's custom env is kept as it is. */
export const ENVS = ["prod", "staging", "dev"];

export function blankHost(): HostForm {
  return { name: "", hostname: "", port: "", user: "", keys: [], jump: "", env: "", tags: "", note: "" };
}

export function formFromHost(h: Host): HostForm {
  const tags = { ...(h.tags ?? {}) };
  let env = (tags.env ?? "").toLowerCase();
  if (isProdTag("env", env)) env = "prod";
  delete tags.env;
  return {
    name: h.name,
    hostname: h.hostname,
    port: h.port && h.port !== 22 ? String(h.port) : "",
    user: h.user ?? "",
    keys: [...(h.identity_files ?? [])],
    jump: h.proxy_jump ?? "",
    env,
    tags: Object.entries(tags).sort(([a], [b]) => a.localeCompare(b)).map(([k, v]) => `${k}=${v}`).join(", "),
    note: h.note ?? "",
  };
}

/** parseTagText reads "k=v, k=v"; env belongs in the Environment field. */
export function parseTagText(text: string): { tags: Record<string, string>; error?: string } {
  const tags: Record<string, string> = {};
  for (const part of text.split(",").map((s) => s.trim()).filter(Boolean)) {
    const i = part.indexOf("=");
    const k = i < 0 ? "" : part.slice(0, i).trim();
    if (!k || /\s/.test(k)) return { tags, error: `“${part}” is not key=value` };
    if (k === "env") return { tags, error: "Set env with Environment above" };
    tags[k] = part.slice(i + 1).trim();
  }
  return { tags };
}

const NAME = /^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$/;
const ADDR = /^[A-Za-z0-9_][A-Za-z0-9._:%-]*$/;
const USER = /^[A-Za-z0-9_][A-Za-z0-9._-]*$/;

function portError(p: string): string | undefined {
  if (!p.trim()) return undefined;
  const n = Number(p);
  return /^\d+$/.test(p.trim()) && n >= 1 && n <= 65535 ? undefined : "Port must be 1–65535";
}

function addressError(a: string): string | undefined {
  if (!a.trim()) return "Address is required";
  if (a.trim().startsWith("-")) return "Address must not start with “-”";
  if (!ADDR.test(a.trim())) return "Address has spaces or special characters";
  return undefined;
}

/** jumpError checks one ProxyJump entry that is not a saved host: [user@]host[:port]. */
function jumpError(spec: string): string | undefined {
  const bad = `“${spec}” is not a saved host; use a saved host or user@host:port`;
  let rest = spec;
  const at = rest.lastIndexOf("@");
  if (at >= 0) {
    if (!USER.test(rest.slice(0, at))) return bad;
    rest = rest.slice(at + 1);
  }
  let host = rest;
  const m = /^\[(.+)\]:(\d*)$/.exec(rest) ?? (rest.split(":").length === 2 ? /^(.+):(\d*)$/.exec(rest) : null);
  if (m) {
    host = m[1];
    const n = Number(m[2]);
    if (!/^\d+$/.test(m[2]) || n < 1 || n > 65535) return `Bad port in “${spec}”`;
  }
  if (addressError(host)) return bad;
  if (!/[.:@]/.test(spec) && host !== "localhost") return bad;
  return undefined;
}

export type HostErrors = Partial<Record<keyof HostForm, string>>;

/** formErrors are the problems with the form, by field. editing is the saved name of the host being edited. */
export function formErrors(f: HostForm, hosts: Host[], editing?: string): HostErrors {
  const e: HostErrors = {};
  const name = f.name.trim();
  if (!name) e.name = "Name is required";
  else if (/[\s/\\]/.test(name)) e.name = "No spaces or slashes";
  else if (!NAME.test(name) || name.startsWith(".")) e.name = "Letters, digits, “.”, “_” and “-” (at most 64)";
  else if (name !== editing && hosts.some((h) => h.name === name)) e.name = `A host named ${name} already exists`;
  const addr = addressError(f.hostname);
  if (addr) e.hostname = addr;
  const port = portError(f.port);
  if (port) e.port = port;
  if (f.user.trim() && !USER.test(f.user.trim())) e.user = "Letters, digits, “.”, “_” and “-”";
  const saved = new Set(hosts.map((h) => h.name).filter((n) => n !== editing));
  for (const spec of f.jump.split(",").map((s) => s.trim()).filter(Boolean)) {
    if (spec === name) e.jump = "A host cannot be its own jump host";
    else if (!saved.has(spec)) e.jump = jumpError(spec);
    if (e.jump) break;
  }
  const tags = parseTagText(f.tags);
  if (tags.error) e.tags = tags.error;
  return e;
}

/** tagList is the form's tags as key=value, env first. */
function tagList(f: HostForm): string[] {
  const out = f.env.trim() ? [`env=${f.env.trim()}`] : [];
  return out.concat(Object.entries(parseTagText(f.tags).tags).map(([k, v]) => `${k}=${v}`));
}

/** hostArgs are ssh.hosts.add's arguments. */
export function hostArgs(f: HostForm): Record<string, unknown> {
  const a: Record<string, unknown> = { name: f.name.trim(), hostname: f.hostname.trim() };
  if (f.port.trim()) a.port = Number(f.port);
  if (f.user.trim()) a.user = f.user.trim();
  a.identity_files = [...f.keys];
  if (f.jump.trim()) a.proxy_jump = f.jump.trim();
  a.tags = tagList(f);
  if (f.note.trim()) a.note = f.note.trim();
  return a;
}

/** updateArgs are ssh.hosts.update's arguments: every field, so a cleared field clears. */
export function updateArgs(f: HostForm, original: Host): Record<string, unknown> {
  const a: Record<string, unknown> = { name: original.name };
  if (f.name.trim() !== original.name) a.new_name = f.name.trim();
  return {
    ...a, hostname: f.hostname.trim(), port: f.port.trim() ? Number(f.port) : 22, user: f.user.trim(),
    identity_files: [...f.keys], proxy_jump: f.jump.trim(), tags: tagList(f), note: f.note.trim(),
  };
}

/** connArgs describe the host as the form has it, for scan_key / trust_key / test. */
export function connArgs(f: HostForm): Record<string, unknown> {
  return {
    name: f.name.trim(), hostname: f.hostname.trim(), port: f.port.trim() ? Number(f.port) : 22, user: f.user.trim(),
    identity_files: [...f.keys], proxy_jump: f.jump.trim(),
  };
}

/** connKey changes when a field that affects connecting changes (then the check starts over). */
export function connKey(f: HostForm): string {
  return JSON.stringify(connArgs(f));
}

/** keygenCommand removes a user's own known_hosts entry (Coxswain never edits that file). */
export function keygenCommand(address: string): string {
  const m = /^\[?(.*?)\]?:(\d+)$/.exec(address);
  if (!m) return `ssh-keygen -R ${address}`;
  return m[2] === "22" ? `ssh-keygen -R ${m[1]}` : `ssh-keygen -R '[${m[1]}]:${m[2]}'`;
}

/** HostKey is ssh.hosts.scan_key's result. */
export interface HostKey {
  host: string; // the hop: the host, or a jump host on the way
  address: string;
  type: string;
  fingerprint: string;
  status: "trusted" | "unknown" | "changed";
  via?: string[];
  known?: { file: string; line: number; type: string; fingerprint: string; coxswain: boolean }[];
}

/** Check is where the connection check is. */
export type Check =
  | { step: "idle" }
  | { step: "scanning" }
  | { step: "confirm"; key: HostKey } // an unknown key: the user compares and trusts it
  | { step: "changed"; key: HostKey } // a different key is recorded: never trusted automatically
  | { step: "trusting"; key: HostKey }
  | { step: "forgetting"; key: HostKey }
  | { step: "login" }
  | { step: "ok"; os: string; ms: number }
  | { step: "failed"; error: string };

export function afterScan(key: HostKey): Check {
  if (key.status === "trusted") return { step: "login" };
  if (key.status === "unknown") return { step: "confirm", key };
  return { step: "changed", key };
}

type Invoke = <T>(id: string, args?: Record<string, unknown>) => Promise<Outcome<T>>;
type Set = (c: Check) => void;

function failed(o: Outcome): Check {
  return { step: "failed", error: [o.error, o.hint].filter(Boolean).join(" — ") || "failed" };
}

async function login(invoke: Invoke, conn: Record<string, unknown>, set: Set) {
  set({ step: "login" });
  const out = await invoke<{ os: string; duration_ms: number }>("ssh.hosts.test", conn);
  set(out.status === "ok" && out.result ? { step: "ok", os: out.result.os, ms: out.result.duration_ms } : failed(out));
}

/** runCheck scans the host's key and, if it is trusted, logs in; otherwise it stops for the user. */
export async function runCheck(invoke: Invoke, conn: Record<string, unknown>, set: Set) {
  try {
    set({ step: "scanning" });
    const out = await invoke<HostKey>("ssh.hosts.scan_key", conn);
    if (out.status !== "ok" || !out.result) return set(failed(out));
    const next = afterScan(out.result);
    if (next.step !== "login") return set(next);
    await login(invoke, conn, set);
  } catch (e) {
    set({ step: "failed", error: e instanceof Error ? e.message : String(e) });
  }
}

/** trustAndContinue trusts the key the user confirmed, then checks again (a jump host's key may be next). */
export async function trustAndContinue(invoke: Invoke, conn: Record<string, unknown>, key: HostKey, set: Set) {
  try {
    set({ step: "trusting", key });
    const out = await invoke<HostKey>("ssh.hosts.trust_key", { ...conn, fingerprint: key.fingerprint });
    if (out.status !== "ok") return set(failed(out));
    await runCheck(invoke, conn, set);
  } catch (e) {
    set({ step: "failed", error: e instanceof Error ? e.message : String(e) });
  }
}

/** forgetAndRescan removes Coxswain's recorded key for the hop, then scans again. */
export async function forgetAndRescan(invoke: Invoke, conn: Record<string, unknown>, key: HostKey, set: Set) {
  try {
    set({ step: "forgetting", key });
    const out = await invoke("ssh.hosts.forget_key", { ...conn, address: key.address });
    if (out.status !== "ok") return set(failed(out));
    await runCheck(invoke, conn, set);
  } catch (e) {
    set({ step: "failed", error: e instanceof Error ? e.message : String(e) });
  }
}
