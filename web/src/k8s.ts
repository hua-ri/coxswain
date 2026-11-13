// The Kubernetes panel's model: shapes of the k8s.* actions' results and
// the small decisions the views make about them.
import type { Outcome } from "./cox";

export interface ContextInfo {
  name: string;
  cluster: string;
  server?: string;
  user?: string;
  namespace: string;
  current?: boolean;
  tags?: Record<string, string>; // what applies, including inherited ones
  note?: string;
  inherited_from?: string[];
  own_tags?: Record<string, string>; // present (if any) when inherited_from is
  source?: "kubeconfig" | "coxswain"; // the user's kubeconfig (read-only) or added in Coxswain
}

export interface ContextsOutput {
  kubeconfig: string[];
  current?: string;
  contexts: ContextInfo[];
}

export interface Shaped {
  text: string;
  truncated?: boolean;
  total_lines: number;
  artifact_id?: string;
}

export interface GetOutput {
  context: string;
  kind: string;
  namespace?: string;
  all_namespaces?: boolean;
  columns?: string[];
  rows?: string[][];
  total: number;
  more?: boolean;
  truncated?: boolean;
  name?: string;
  yaml?: Shaped;
}

export interface EventRow {
  namespace?: string;
  type: string;
  reason: string;
  object: string;
  message: string;
  count?: number;
  age: string;
}

export interface Condition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
}

export interface ContainerInfo {
  name: string;
  image: string;
  init?: boolean;
  ready: boolean;
  restarts: number;
  state?: string;
  last_state?: string;
}

export interface DescribeOutput {
  kind: string;
  namespace?: string;
  name: string;
  summary: Record<string, string>;
  labels?: Record<string, string>;
  owners?: string[];
  conditions?: Condition[];
  containers?: ContainerInfo[];
  keys?: string[];
  events: EventRow[];
}

export interface HostCandidate {
  name: string;
  matched_by: string;
  address?: string;
  tags?: Record<string, string>;
  note?: string;
}

export interface Finding {
  severity: "critical" | "warning" | "info";
  title: string;
  detail?: string;
  evidence_refs?: string[];
}

export interface DiagnoseOutput {
  kind: string;
  name: string;
  findings: Finding[];
  evidence: {
    workload?: Record<string, string>;
    pods?: { name: string; status: string; node?: string; containers: ContainerInfo[]; previous_logs?: Record<string, Shaped> }[];
    nodes?: { name: string; status: string; problems?: string[]; ssh_host?: string; ssh_candidates?: HostCandidate[] }[];
    events?: EventRow[];
  };
  next_steps: string[];
}

export interface ApplyObject {
  kind: string;
  namespace?: string;
  name: string;
  operation: "create" | "configure" | "unchanged";
  diff?: Shaped;
  applied?: boolean;
}

export interface ApplyOutput {
  dry_run: boolean;
  objects: ApplyObject[];
}

export interface Revision {
  revision: number;
  replicaset: string;
  images: string[];
  age: string;
  current?: boolean;
}

export interface RolloutOutput {
  done?: boolean;
  message: string;
  revisions?: Revision[];
}

/** The resource kinds listed in the K8s tab; others can be typed. */
export const KINDS: { label: string; kind: string }[] = [
  { label: "Pods", kind: "pods" },
  { label: "Deployments", kind: "deployments" },
  { label: "StatefulSets", kind: "statefulsets" },
  { label: "DaemonSets", kind: "daemonsets" },
  { label: "Jobs", kind: "jobs" },
  { label: "CronJobs", kind: "cronjobs" },
  { label: "Services", kind: "services" },
  { label: "Ingresses", kind: "ingresses" },
  { label: "ConfigMaps", kind: "configmaps" },
  { label: "Secrets", kind: "secrets" },
  { label: "Nodes", kind: "nodes" },
  { label: "Events", kind: "events" },
  { label: "Namespaces", kind: "namespaces" },
];

const K8S_TAB = "@k8s:";

/** k8sTab is the tab id of a context's K8s tab. */
export function k8sTab(context: string): string {
  return K8S_TAB + context;
}

/** contextFromTab is the context of a K8s tab id, or "". */
export function contextFromTab(tab: string): string {
  return tab.startsWith(K8S_TAB) ? tab.slice(K8S_TAB.length) : "";
}

function prodValue(v: string | undefined): boolean {
  const s = (v ?? "").toLowerCase();
  return s === "prod" || s === "production";
}

/** isProdContext: the context (or one on the same cluster) is tagged env=prod. */
export function isProdContext(c: ContextInfo): boolean {
  return prodValue(c.tags?.env);
}

/** envOf is the context's env tag as "env=…", or "". */
export function envOf(c: ContextInfo | undefined): string {
  return c?.tags?.env ? `env=${c.tags.env}` : "";
}

export interface Badge {
  text: string;
  danger: boolean;
  inherited: boolean; // from another context on the same cluster
}

/** contextBadges are a context's tags, production in red, inherited ones marked. */
export function contextBadges(c: ContextInfo): Badge[] {
  const own = c.own_tags ?? {};
  const inheriting = (c.inherited_from ?? []).length > 0;
  return Object.entries(c.tags ?? {})
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([k, v]) => ({
      text: `${k}=${v}`,
      danger: k === "env" && prodValue(v),
      inherited: inheriting && own[k] !== v,
    }));
}

const GOOD = /^(Running|Ready|Completed?|Succeeded|Active|Bound|Available|True)$/;
const WAITING = /^(Pending|ContainerCreating|PodInitializing|Terminating|Init:|Unknown|Suspended)/;

/** statusClass colors a status cell: ok, warn, bad, or "" (not a status). */
export function statusClass(column: string, value: string): string {
  if (column === "ready") {
    const m = /^(\d+)\/(\d+)$/.exec(value);
    if (!m) return "";
    const [ready, want] = [Number(m[1]), Number(m[2])];
    return ready >= want ? "ok" : ready === 0 ? "bad" : "warn";
  }
  if (column !== "status" || value === "-" || value === "") return "";
  if (GOOD.test(value)) return "ok";
  if (WAITING.test(value)) return "warn";
  if (/=/.test(value)) return /=False/.test(value) ? "warn" : "ok"; // conditions
  return "bad";
}

/** rowObject is the name and namespace of a table row. */
export function rowObject(columns: string[], row: string[], namespace: string): { name: string; namespace: string } {
  const at = (c: string) => {
    const i = columns.indexOf(c);
    return i < 0 ? "" : row[i];
  };
  return { name: at("name"), namespace: at("namespace") || namespace };
}

export type RowAction = "logs" | "terminal" | "forward" | "delete" | "restart" | "scale" | "rollback";

/** rowActions are the buttons of a row of kind. */
export function rowActions(kind: string): RowAction[] {
  switch (kind) {
    case "Pod":
      return ["logs", "terminal", "forward", "delete"];
    case "Deployment":
      return ["restart", "scale", "rollback"];
    case "StatefulSet":
      return ["restart", "scale"];
  }
  return [];
}

/** podContainers are the containers logs can be read from. */
export function podContainers(d: DescribeOutput | null): string[] {
  return (d?.containers ?? []).filter((c) => !c.init).map((c) => c.name);
}

/** severityClass colors a diagnose finding. */
export function severityClass(s: string): string {
  return s === "critical" ? "bad" : s === "warning" ? "warn" : "";
}

export interface DiffLine {
  text: string;
  cls: "" | "add" | "del" | "hunk" | "meta";
}

/** diffLines splits a unified diff into lines to color. */
export function diffLines(diff: string): DiffLine[] {
  if (!diff) return [];
  return diff
    .replace(/\n$/, "")
    .split("\n")
    .map((text) => {
      if (text.startsWith("---") || text.startsWith("+++")) return { text, cls: "meta" };
      if (text.startsWith("@@")) return { text, cls: "hunk" };
      if (text.startsWith("+")) return { text, cls: "add" };
      if (text.startsWith("-")) return { text, cls: "del" };
      return { text, cls: "" };
    });
}

/** scaleWarning is said before scaling to n replicas. */
export function scaleWarning(n: number): string {
  return n === 0 ? "Scaling to 0 stops the workload: all its pods are deleted." : "";
}

/** actionError is an action's error with its hint. */
export function actionError(o: Outcome): string {
  const e = o.error || "failed";
  return o.hint ? `${e} (${o.hint})` : e;
}

/** A namespace value meaning every namespace. */
export const ALL_NAMESPACES = "*";

/** listArgs are k8s.get's arguments for a kind in a namespace (or all). */
export function listArgs(kind: string, namespace: string): Record<string, unknown> {
  return namespace === ALL_NAMESPACES ? { kind, all_namespaces: true } : { kind, namespace };
}

/** terminalRequest is the POST /api/v1/terminals body of a pod terminal. */
export function terminalRequest(context: string, namespace: string, pod: string, container?: string, shell?: string) {
  const spec: Record<string, string> = { context, namespace, pod };
  if (container) spec.container = container;
  if (shell && shell.trim()) spec.shell = shell.trim();
  return { pod: spec, cols: 80, rows: 24 };
}

/** forwardRequest is the POST /api/v1/tunnels body of a port-forward to a pod. */
export function forwardRequest(context: string, namespace: string, pod: string, remote: number, local?: number) {
  const body: Record<string, unknown> = { type: "k8s", context, namespace, pod, remote_port: remote };
  if (local) body.local_port = local;
  return body;
}

/** ActionError is a failed action, with its outcome (details included). */
export class ActionError extends Error {
  outcome: Outcome;
  constructor(outcome: Outcome) {
    super(actionError(outcome));
    this.outcome = outcome;
  }
}

/** Conflict is a field another field manager set, which an apply would change. */
export interface Conflict {
  object: string;
  field: string;
  manager: string;
  operation: string; // Update or Apply
  hint?: string;
}

/** conflictsOf are the apply conflicts an error reports, if any. */
export function conflictsOf(e: unknown): Conflict[] {
  if (!(e instanceof ActionError)) return [];
  const d = e.outcome.details as { conflicts?: Conflict[] } | undefined;
  return d?.conflicts ?? [];
}

/** applyArgs are k8s.apply's arguments; force takes conflicting fields over. */
export function applyArgs(manifest: string, dryRun: boolean, force: boolean): Record<string, unknown> {
  const args: Record<string, unknown> = { manifest, dry_run: dryRun };
  if (force) args.force_conflicts = true;
  return args;
}

/** takeoverText says whose fields a forced apply overrides. */
export function takeoverText(conflicts: Conflict[]): string {
  const byManager = new Map<string, Map<string, string[]>>();
  for (const c of conflicts) {
    const objects = byManager.get(c.manager) ?? new Map<string, string[]>();
    objects.set(c.object, [...(objects.get(c.object) ?? []), c.field]);
    byManager.set(c.manager, objects);
  }
  const parts = [...byManager].map(([m, objects]) => `${m} (${[...objects].map(([o, f]) => `${o} ${f.join(", ")}`).join("; ")})`);
  return `This overrides fields managed by: ${parts.join("; ")}.`;
}
