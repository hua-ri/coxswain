// Argo CD and Argo Workflows instances in the desktop: their shapes (as
// the argo* actions return them), sidebar ids, login state, the add form,
// and helpers for the app and workflow views. Credentials never reach the
// page except as what the user types into a login field.
import { isProdTag, isProdTags } from "./env";
import { parseTagText } from "./hostform";
import { orderPlaces } from "./nav";

export type ArgoKind = "argocd" | "argowf";

export interface LoginStatus {
  logged_in: boolean;
  method?: string;
  identity?: string;
  expires_at?: string;
  renewable?: boolean;
  rejected?: boolean;
  token_sha256_prefix?: string;
  problem?: string;
}

export interface ArgoInstance {
  kind: ArgoKind;
  name: string;
  url: string;
  namespace?: string;
  ca_file?: string;
  insecure_skip_verify?: boolean;
  sso_port?: number;
  tags?: Record<string, string>;
  note?: string;
  login: LoginStatus;
  login_pending?: boolean;
}

export interface MethodInfo {
  method: "browser" | "password" | "token" | "none";
  label: string;
  detail?: string;
}

export interface Methods {
  instance: string;
  url: string;
  methods: MethodInfo[];
  status: LoginStatus;
  problem?: string;
}

// ---- ids ----

const CD = "@argocd:";
const WF = "@argowf:";
const LOGIN = "@argo-login:";
/** The add Argo instance pane. */
export const ADD_ARGO = "@add-argo";

export const argoId = (kind: ArgoKind, name: string) => (kind === "argocd" ? CD : WF) + name;
export const argoLoginId = (kind: ArgoKind, name: string) => `${LOGIN}${kind}:${name}`;

/** argoOf parses an instance id, or returns null. */
export function argoOf(id: string): { kind: ArgoKind; name: string } | null {
  if (id.startsWith(CD)) return { kind: "argocd", name: id.slice(CD.length) };
  if (id.startsWith(WF)) return { kind: "argowf", name: id.slice(WF.length) };
  return null;
}

export function argoLoginOf(id: string): { kind: ArgoKind; name: string } | null {
  if (!id.startsWith(LOGIN)) return null;
  const rest = id.slice(LOGIN.length);
  const i = rest.indexOf(":");
  const kind = rest.slice(0, i);
  return kind === "argocd" || kind === "argowf" ? { kind, name: rest.slice(i + 1) } : null;
}

// ---- sidebar ----

export function envOfInstance(i: ArgoInstance): string {
  const v = (i.tags?.env ?? "").toLowerCase();
  return isProdTag("env", v) ? "prod" : v;
}

export const instanceIsProd = (i: ArgoInstance) => isProdTags(i.tags);

/** sortedInstances: instances of a kind matching the filter, production first. */
export function sortedInstances(list: ArgoInstance[], kind: ArgoKind, filter = ""): ArgoInstance[] {
  const words = filter.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const hay = (i: ArgoInstance) =>
    [i.name, i.url, i.namespace, i.note, ...Object.entries(i.tags ?? {}).map(([k, v]) => `${k}=${v}`)].join(" ").toLowerCase();
  return orderPlaces(list.filter((i) => i.kind === kind && words.every((w) => hay(i).includes(w))), envOfInstance);
}

/** loginNeed is the short sidebar mark for an instance that cannot be used yet, or "". */
export function loginNeed(i: ArgoInstance): string {
  if (i.login_pending) return "waiting";
  if (i.login.logged_in) return "";
  return i.kind === "argocd" ? "log in" : "token";
}

/** loginText describes a login for people: who, how, until when. */
export function loginText(s: LoginStatus, now = Date.now()): string {
  if (s.problem) return s.problem;
  if (!s.method) return "Not logged in";
  if (s.method === "none") return "No credential needed (server auth mode)";
  if (s.rejected) return "The server rejected the credential; log in again";
  const who = s.identity ? `as ${s.identity}` : "";
  const how = { password: "with a password", browser: "in the browser", token: "with a token" }[s.method] ?? "";
  let until = "";
  if (s.expires_at) {
    const t = Date.parse(s.expires_at);
    if (t <= now) until = s.renewable ? "; renews on next use" : "; expired";
    else until = `; expires ${relative(t - now)}${s.renewable ? " (renews itself)" : ""}`;
  } else if (s.method === "token") {
    until = "; the token does not expire";
  }
  return ["Logged in", who, how].filter(Boolean).join(" ") + until;
}

function relative(ms: number): string {
  const m = Math.round(ms / 60000);
  if (m < 60) return `in ${m} min`;
  const h = Math.round(m / 60);
  if (h < 48) return `in ${h} h`;
  return `in ${Math.round(h / 24)} days`;
}

/** reloadsArgo: a finished action changed the instances or a login. */
export function reloadsArgo(action: string | undefined): boolean {
  const a = action ?? "";
  return a.startsWith("argo.") || /^argo(cd|wf)\.(login|logout|cancel_login)$/.test(a);
}

// ---- the add form ----

export interface ArgoForm {
  kind: ArgoKind;
  name: string;
  url: string;
  namespace: string;
  insecure: boolean;
  caFile: string;
  ssoPort: string;
  env: string;
  tags: string;
  note: string;
}

export function blankArgo(kind: ArgoKind = "argocd"): ArgoForm {
  return { kind, name: "", url: "", namespace: kind === "argowf" ? "argo" : "", insecure: false, caFile: "", ssoPort: "", env: "", tags: "", note: "" };
}

const NAME = /^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$/;

export type ArgoErrors = Partial<Record<"name" | "url" | "namespace" | "ssoPort" | "tags", string>>;

export function argoErrors(f: ArgoForm, list: ArgoInstance[]): ArgoErrors {
  const e: ArgoErrors = {};
  const name = f.name.trim();
  if (!name) e.name = "Name is required";
  else if (!NAME.test(name)) e.name = "Letters, digits, “.”, “_” and “-”";
  else if (list.some((i) => i.kind === f.kind && i.name === name)) e.name = `${name} already exists`;
  const url = f.url.trim();
  if (!url) e.url = "URL is required";
  else if (!/^(https?:\/\/)?[^\s/?#@]+(:\d+)?(\/[^\s?#]*)?$/.test(url)) e.url = "Give http(s)://host[:port][/path]";
  if (f.kind === "argowf" && !f.namespace.trim()) e.namespace = "The namespace used when none is given";
  if (f.ssoPort.trim()) {
    const n = Number(f.ssoPort);
    if (!/^\d+$/.test(f.ssoPort.trim()) || n < 1024 || n > 65535) e.ssoPort = "1024–65535";
  }
  const tags = parseTagText(f.tags);
  if (tags.error) e.tags = tags.error;
  return e;
}

/** guessName suggests a name from a URL: the first label of its host. */
export function guessName(url: string): string {
  const host = url.trim().replace(/^https?:\/\//, "").split(/[/:]/)[0];
  const first = host.split(".")[0] ?? "";
  return /^\d+$/.test(first) || first === "localhost" ? "" : first.replace(/[^A-Za-z0-9._-]/g, "");
}

export function addArgoArgs(f: ArgoForm): Record<string, unknown> {
  const a: Record<string, unknown> = { kind: f.kind, name: f.name.trim(), url: f.url.trim() };
  if (f.kind === "argowf") a.namespace = f.namespace.trim();
  if (f.insecure) a.insecure_skip_verify = true;
  if (f.caFile.trim()) a.ca_file = f.caFile.trim();
  if (f.kind === "argocd" && f.ssoPort.trim()) a.sso_port = Number(f.ssoPort);
  const tags = f.env ? [`env=${f.env}`] : [];
  a.tags = tags.concat(Object.entries(parseTagText(f.tags).tags).map(([k, v]) => `${k}=${v}`));
  if (f.note.trim()) a.note = f.note.trim();
  return a;
}

// ---- Argo CD ----

export interface AppSummary {
  name: string;
  namespace?: string;
  project: string;
  source: string;
  destination: string;
  sync: string;
  health: string;
  auto_sync?: boolean;
  revision?: string;
  last_sync?: string;
  last_result?: string;
  problem?: string;
}

export interface AppsOutput {
  instance: string;
  apps: AppSummary[];
  counts: Record<string, number>;
}

export interface AppDetail extends AppSummary {
  health_message?: string;
  conditions?: { type: string; message: string }[];
  self_heal?: boolean;
  auto_prune?: boolean;
  last_operation?: { phase: string; message?: string; revision?: string; started_at?: string; finished_at?: string; failed_resources?: string[] };
  operation_running?: boolean;
  history?: { id: number; revision: string; deployed_at?: string; by?: string }[];
  resources: Record<string, number>;
  unhealthy_or_out_of_sync?: { kind: string; namespace?: string; name: string; sync?: string; health?: string; message?: string; requires_pruning?: boolean }[];
}

export interface TreeNode {
  kind: string;
  namespace?: string;
  name: string;
  parent?: string;
  health?: string;
  message?: string;
  info?: string;
}

export interface Shaped {
  text: string;
  truncated?: boolean;
  artifact_id?: string;
}

export interface DiffOutput {
  app: string;
  diffs: { object: string; change: string; diff: Shaped }[];
  in_sync: number;
}

export interface Finding {
  severity: string;
  title: string;
  detail?: string;
}

export interface Diagnosis {
  target: string;
  summary: string;
  findings: Finding[];
  evidence?: Record<string, unknown>;
  next_steps?: string[];
}

/** syncClass and healthClass map Argo states to chip classes. */
export function syncClass(s: string): string {
  return s === "Synced" ? "ok" : s === "OutOfSync" ? "warn" : "";
}

export function healthClass(h: string): string {
  switch (h) {
    case "Healthy":
      return "ok";
    case "Degraded":
    case "Missing":
      return "bad";
    case "Progressing":
    case "Suspended":
      return "warn";
  }
  return "";
}

export function phaseClass(p: string): string {
  switch (p) {
    case "Succeeded":
      return "ok";
    case "Failed":
    case "Error":
      return "bad";
    case "Running":
    case "Pending":
      return "warn";
  }
  return "";
}

/** appIssue: the app needs attention (not Synced and Healthy, or has a condition). */
export const appIssue = (a: AppSummary) => a.sync !== "Synced" || a.health !== "Healthy" || !!a.problem;

/** syncArgs are argocd.sync's arguments. */
export function syncArgs(app: string, o: { prune: boolean; dryRun: boolean; resources: string[]; revision: string }): Record<string, unknown> {
  const a: Record<string, unknown> = { app };
  if (o.prune) a.prune = true;
  if (o.dryRun) a.dry_run = true;
  if (o.resources.length) a.resources = o.resources;
  if (o.revision.trim()) a.revision = o.revision.trim();
  return a;
}

/** resourceRef is how argocd.sync names a resource: Kind:namespace/name. */
export function resourceRef(r: { kind: string; namespace?: string; name: string }): string {
  return `${r.kind}:${r.namespace ? r.namespace + "/" : ""}${r.name}`;
}

// ---- Argo Workflows ----

export interface WFSummary {
  name: string;
  phase: string;
  progress?: string;
  template?: string;
  started_at?: string;
  finished_at?: string;
  duration?: string;
  message?: string;
  suspended?: boolean;
}

export interface WFList {
  namespace: string;
  workflows: WFSummary[];
  counts: Record<string, number>;
  more?: boolean;
}

export interface NodeView {
  id: string;
  name: string;
  depth: number;
  type: string;
  template?: string;
  phase?: string;
  message?: string;
  exit_code?: string;
  pod?: string;
  duration?: string;
}

export interface WFDetail extends WFSummary {
  namespace: string;
  parameters?: Record<string, string>;
  failed_nodes?: NodeView[];
  nodes: NodeView[];
  nodes_omitted?: number;
}

export interface Parameter {
  name: string;
  value?: string;
  default?: string;
  enum?: string[];
  description?: string;
}

export interface TemplateView {
  kind: "WorkflowTemplate" | "ClusterWorkflowTemplate" | "CronWorkflow";
  name: string;
  entrypoint?: string;
  description?: string;
  parameters?: Parameter[];
  schedule?: string;
  suspended?: boolean;
  last_run?: string;
  active?: number;
}

/** paramDefaults: the value each parameter starts with in the submit form. */
export function paramDefaults(ps: Parameter[] | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  for (const p of ps ?? []) out[p.name] = p.value ?? p.default ?? (p.enum?.[0] ?? "");
  return out;
}

/** missingParams: parameters without a value or default that are still empty. */
export function missingParams(ps: Parameter[] | undefined, values: Record<string, string>): string[] {
  return (ps ?? []).filter((p) => p.value === undefined && p.default === undefined && !(values[p.name] ?? "").trim()).map((p) => p.name);
}

/** wfOpsFor: the operations a workflow in this phase allows. */
export function wfOpsFor(w: WFSummary): ("suspend" | "resume" | "stop" | "terminate" | "retry" | "resubmit")[] {
  if (w.phase === "Running" || w.phase === "Pending") return w.suspended ? ["resume", "stop", "terminate"] : ["suspend", "stop", "terminate"];
  if (w.phase === "Failed" || w.phase === "Error") return ["retry", "resubmit"];
  return ["resubmit"];
}

/** activityDetail summarises an argo action's arguments for the Activity list. */
export function argoDetail(action: string, args: Record<string, unknown>): string | undefined {
  const s = (k: string) => (typeof args[k] === "string" ? (args[k] as string) : "");
  if (action.startsWith("argocd.") && s("app")) {
    const parts = [`app/${s("app")}`, action.slice("argocd.".length)];
    if (args.prune) parts.push("prune=true");
    if (args.dry_run) parts.push("dry_run=true");
    if (args.hard) parts.push("hard");
    if (typeof args.id === "number") parts.push(`to ${args.id}`);
    return parts.join(" ");
  }
  if (action.startsWith("argowf.")) {
    const op = action.slice("argowf.".length);
    if (s("template")) return `submit ${s("template")}${args.dry_run ? " (dry run)" : ""}`;
    if (s("name")) return `${op === "cron" ? "cron" : "wf"}/${s("name")} ${op === "cron" ? (args.suspend ? "suspend" : "resume") : op}`;
  }
  if (/^argo(cd|wf)\.login$/.test(action) && s("method")) return `login with ${s("method")}${s("username") ? " as " + s("username") : ""}`;
  if (action.startsWith("argo.") && s("name")) return `${s("kind") || ""} ${s("name")}`.trim();
  return undefined;
}
