// Batch runs (ssh.batch) in the activity table: per-host progress from
// batch.* events, and each host's output read on demand from the run's
// artifact. The artifact holds every host's output in order, each under
// "=== <host>: <result> ===", stderr after "--- stderr ---".
import type { Outcome } from "./cox";

export interface BatchHost {
  host: string;
  status: "pending" | "ok" | "failed" | "skipped";
  exitCode?: number;
  durationMs?: number;
  error?: string;
}

export interface BatchSummary {
  total: number;
  ok: number;
  failed: number;
  skipped: number;
}

export interface BatchInfo {
  id: string;
  command: string;
  hosts: BatchHost[];
  summary?: BatchSummary;
  stopped?: string;
  artifactId?: string;
  finished: boolean;
}

/** Event details, as the server sends them. */
export interface BatchStart {
  id: string;
  command: string;
  hosts: string[];
}
export interface BatchHostDone {
  host: string;
  status: "ok" | "failed" | "skipped";
  exit_code?: number;
  duration_ms: number;
  error?: string;
}
export interface BatchEnd {
  id: string;
  summary: BatchSummary;
  stopped?: string;
  artifact_id?: string;
}

export function startBatch(d: BatchStart): BatchInfo {
  return { id: d.id, command: d.command, hosts: (d.hosts ?? []).map((host) => ({ host, status: "pending" })), finished: false };
}

export function hostDone(b: BatchInfo, d: BatchHostDone): BatchInfo {
  const h: BatchHost = { host: d.host, status: d.status };
  if (d.exit_code !== undefined) h.exitCode = d.exit_code;
  if (d.duration_ms !== undefined) h.durationMs = d.duration_ms;
  if (d.error) h.error = d.error;
  const i = b.hosts.findIndex((x) => x.host === d.host);
  const hosts = b.hosts.slice();
  if (i < 0) hosts.push(h);
  else hosts[i] = h;
  return { ...b, hosts };
}

export function finishBatch(b: BatchInfo, d: BatchEnd): BatchInfo {
  const out: BatchInfo = {
    ...b,
    hosts: b.hosts.map((h) => (h.status === "pending" ? { host: h.host, status: "skipped" } : h)),
    summary: d.summary,
    finished: true,
  };
  if (d.stopped) out.stopped = d.stopped;
  if (d.artifact_id) out.artifactId = d.artifact_id;
  return out;
}

/** One line of artifact.read's answer ("N: text"). */
export interface Line {
  n: number;
  text: string;
}

export function parseNumbered(s: string): Line[] {
  const out: Line[] = [];
  for (const raw of s.split("\n")) {
    const m = /^(\d+): ?(.*)$/.exec(raw);
    if (m) out.push({ n: Number(m[1]), text: m[2] });
  }
  return out;
}

export interface Section {
  start: number; // first output line
  end: number; // last output line; 0 = to the end
  header: string; // the host's result, as the header says it
}

// A host's header: "=== web1: exit 0, 12ms ===", "=== web2: error after
// 3ms: <error> ===", "=== web3: skipped ===".
const HEADER = /^=== (.+?): (skipped|ok|failed|exit -?\d+, \d+ms|error after \d+ms: .*) ===$/;

/** locateSections finds each host's lines from the header lines, taking the hosts in order. */
export function locateSections(headers: Line[], hosts: string[]): Map<string, Section> {
  const found: { host: string; line: number; header: string }[] = [];
  let from = 0;
  for (const host of hosts) {
    for (let i = from; i < headers.length; i++) {
      const m = HEADER.exec(headers[i].text);
      if (m && m[1] === host) {
        found.push({ host, line: headers[i].n, header: m[2] });
        from = i + 1;
        break;
      }
    }
  }
  const out = new Map<string, Section>();
  found.forEach((f, i) => {
    out.set(f.host, { start: f.line + 1, end: i + 1 < found.length ? found[i + 1].line - 1 : 0, header: f.header });
  });
  return out;
}

export function splitOutput(lines: string[]): { stdout: string; stderr: string } {
  const i = lines.indexOf("--- stderr ---");
  if (i < 0) return { stdout: lines.join("\n"), stderr: "" };
  return { stdout: lines.slice(0, i).join("\n"), stderr: lines.slice(i + 1).join("\n") };
}

export interface HostOutput {
  stdout: string;
  stderr: string;
  truncated: boolean; // longer than one artifact.read (500 lines)
}

type Invoke = (id: string, args: Record<string, unknown>) => Promise<Outcome<unknown>>;

/** READ_LIMIT is how many lines one artifact.read returns at most. */
const READ_LIMIT = 500;

async function read(invoke: Invoke, args: Record<string, unknown>): Promise<Line[]> {
  const out = await invoke("artifact.read", args);
  if (out.status !== "ok") throw new Error(out.error ?? "cannot read the output");
  return parseNumbered(typeof out.result === "string" ? out.result : "");
}

/** fetchHostOutput reads one host's output from a batch artifact. */
export async function fetchHostOutput(invoke: Invoke, artifactId: string, hosts: string[], host: string): Promise<HostOutput> {
  const sections = locateSections(await read(invoke, { id: artifactId, grep: "^=== " }), hosts);
  const s = sections.get(host);
  if (!s) throw new Error(`no output for ${host}`);
  if (s.end !== 0 && s.end < s.start) return { stdout: "", stderr: "", truncated: false };
  const lines = await read(invoke, { id: artifactId, start: s.start, end: s.end });
  const want = s.end === 0 ? Infinity : s.end - s.start + 1;
  const truncated = lines.length >= READ_LIMIT && want > READ_LIMIT;
  return { ...splitOutput(lines.map((l) => l.text)), truncated };
}

/** batchProgress is the result column of a batch row. */
export function batchProgress(b: BatchInfo): string {
  if (b.finished && b.summary) {
    const s = b.summary;
    return `${s.total} hosts: ${s.ok} ok, ${s.failed} failed, ${s.skipped} skipped`;
  }
  const done = b.hosts.filter((h) => h.status !== "pending").length;
  return `${done}/${b.hosts.length} hosts done`;
}
