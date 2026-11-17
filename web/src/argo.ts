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
  k8s_context?: string;
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
const EDIT = "@argo-edit:";
export const argoEditId = (kind: ArgoKind, name: string) => `${EDIT}${kind}:${name}`;

export function argoEditOf(id: string): { kind: ArgoKind; name: string } | null {
  return id.startsWith(EDIT) ? argoLoginOf(LOGIN + id.slice(EDIT.length)) : null;
}

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
  k8sContext: string;
  env: string;
  tags: string;
  note: string;
}

export function blankArgo(kind: ArgoKind = "argocd"): ArgoForm {
  return { kind, name: "", url: "", namespace: kind === "argowf" ? "argo" : "", insecure: false, caFile: "", ssoPort: "", k8sContext: "", env: "", tags: "", note: "" };
}

const NAME = /^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$/;

/** formFromInstance fills the form to edit a saved instance. */
export function formFromInstance(i: ArgoInstance): ArgoForm {
  const tags = { ...(i.tags ?? {}) };
  const env = tags.env ?? "";
  delete tags.env;
  return {
    kind: i.kind, name: i.name, url: i.url, namespace: i.namespace ?? "", insecure: !!i.insecure_skip_verify, caFile: i.ca_file ?? "",
    ssoPort: i.sso_port ? String(i.sso_port) : "", k8sContext: i.k8s_context ?? "", env,
    tags: Object.entries(tags).sort(([a], [b]) => a.localeCompare(b)).map(([k, v]) => `${k}=${v}`).join(", "), note: i.note ?? "",
  };
}

/** updateArgoArgs are argo.update's arguments: every field, so clearing one clears it. */
export function updateArgoArgs(f: ArgoForm, original: ArgoInstance): Record<string, unknown> {
  const a = addArgoArgs(f);
  a.name = original.name;
  if (f.name.trim() !== original.name) a.new_name = f.name.trim();
  a.note = f.note.trim();
  a.k8s_context = f.k8sContext;
  a.ca_file = f.caFile.trim();
  a.insecure_skip_verify = f.insecure;
  if (f.kind === "argocd") a.sso_port = f.ssoPort.trim() ? Number(f.ssoPort) : 0;
  return a;
}

export type ArgoErrors = Partial<Record<"name" | "url" | "namespace" | "ssoPort" | "tags", string>>;

export function argoErrors(f: ArgoForm, list: ArgoInstance[], editing?: string): ArgoErrors {
  const e: ArgoErrors = {};
  const name = f.name.trim();
  if (!name) e.name = "Name is required";
  else if (!NAME.test(name)) e.name = "Letters, digits, “.”, “_” and “-”";
  else if (name !== editing && list.some((i) => i.kind === f.kind && i.name === name)) e.name = `${name} already exists`;
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
  if (f.k8sContext) a.k8s_context = f.k8sContext;
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

export interface Shaped {
  text: string;
  truncated?: boolean;
  artifact_id?: string;
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

// ---- links into the Argo UIs ----

const seg = encodeURIComponent;

/** appURL is an application's page in the Argo CD UI. */
export function appURL(i: { url: string }, app: { name: string; namespace?: string }): string {
  return app.namespace ? `${i.url}/applications/${seg(app.namespace)}/${seg(app.name)}` : `${i.url}/applications/${seg(app.name)}`;
}

/** workflowURL is a workflow's page in the Argo Workflows UI; without a name, the namespace's list. */
export function workflowURL(i: { url: string }, namespace: string, name?: string): string {
  return `${i.url}/workflows/${seg(namespace)}${name ? "/" + seg(name) : ""}`;
}
