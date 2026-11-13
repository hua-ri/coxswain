// The add cluster form: where the kubeconfig comes from (pasted or a
// file), which of its contexts to add and under what name, and the
// requests it sends. The user's own kubeconfig is never written: the
// server stores the context in Coxswain's own directory.
import { parseTagText } from "./hostform";
import type { ContextInfo } from "./k8s";

export interface ClusterForm {
  source: "paste" | "file";
  yaml: string;
  path: string;
  context: string; // the context in the kubeconfig
  name: string; // its name in Coxswain
  env: string;
  tags: string;
  note: string;
}

/** Candidate is a context of the kubeconfig being added (k8s.clusters.inspect). */
export interface Candidate {
  name: string;
  cluster: string;
  server?: string;
  user?: string;
  namespace?: string;
  auth: string;
  current?: boolean;
  problem?: string;
  exists?: "kubeconfig" | "coxswain";
}

export function blankCluster(): ClusterForm {
  return { source: "paste", yaml: "", path: "", context: "", name: "", env: "", tags: "", note: "" };
}

/** sourceArgs are the kubeconfig arguments, or null while there is none. */
export function sourceArgs(f: ClusterForm): { kubeconfig: string } | { path: string } | null {
  if (f.source === "file") return f.path ? { path: f.path } : null;
  return f.yaml.trim() ? { kubeconfig: f.yaml } : null;
}

/** pickContext is the context to preselect: the only one, the current one, or the first usable. */
export function pickContext(cands: Candidate[]): string {
  if (cands.length === 1) return cands[0].name;
  const usable = cands.filter((c) => !c.problem);
  return (usable.find((c) => c.current) ?? usable[0])?.name ?? "";
}

export type ClusterErrors = Partial<Record<"source" | "context" | "name" | "tags", string>>;

export function clusterErrors(f: ClusterForm, cands: Candidate[], contexts: ContextInfo[]): ClusterErrors {
  const e: ClusterErrors = {};
  if (!sourceArgs(f)) e.source = f.source === "file" ? "Choose a kubeconfig file" : "Paste a kubeconfig";
  const c = cands.find((x) => x.name === f.context);
  if (!f.context || !c) e.context = "Choose a context";
  else if (c.problem) e.context = c.problem;
  const name = f.name.trim();
  const taken = contexts.find((x) => x.name === name);
  if (!name) e.name = "Name is required";
  else if (/\s/.test(name)) e.name = "No spaces";
  else if (taken?.source === "coxswain") e.name = `${name} was already added in Coxswain`;
  else if (taken) e.name = `${name} is already in your kubeconfig. Rename it here; only Coxswain's copy gets the new name`;
  const tags = parseTagText(f.tags);
  if (tags.error) e.tags = tags.error;
  return e;
}

/** addClusterArgs are k8s.clusters.add's arguments. */
export function addClusterArgs(f: ClusterForm): Record<string, unknown> {
  const a: Record<string, unknown> = { ...sourceArgs(f), context: f.context };
  if (f.name.trim() && f.name.trim() !== f.context) a.name = f.name.trim();
  const tags = f.env.trim() ? [`env=${f.env.trim()}`] : [];
  a.tags = tags.concat(Object.entries(parseTagText(f.tags).tags).map(([k, v]) => `${k}=${v}`));
  if (f.note.trim()) a.note = f.note.trim();
  return a;
}

/** testClusterArgs are k8s.clusters.test's arguments. */
export function testClusterArgs(f: ClusterForm): Record<string, unknown> {
  return { ...sourceArgs(f), context: f.context };
}
