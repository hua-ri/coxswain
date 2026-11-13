import { describe, expect, it } from "vitest";
import { addClusterArgs, blankCluster, clusterErrors, pickContext, sourceArgs, testClusterArgs, type Candidate } from "./clusterform";
import type { ContextInfo } from "./k8s";

const cands: Candidate[] = [
  { name: "staging", cluster: "c1", server: "https://staging:6443", auth: "token", current: true },
  { name: "prod", cluster: "c2", server: "https://prod:6443", auth: "gcp", problem: "auth provider \"gcp\" is not supported" },
  { name: "dev", cluster: "c3", auth: "exec", exists: "kubeconfig" },
];
const contexts: ContextInfo[] = [
  { name: "dev", cluster: "c", namespace: "default", source: "kubeconfig" },
  { name: "lab", cluster: "c", namespace: "default", source: "coxswain" },
];

describe("cluster form", () => {
  it("sends the pasted YAML or the picked file, not both", () => {
    expect(sourceArgs({ ...blankCluster(), yaml: "  " })).toBeNull();
    expect(sourceArgs({ ...blankCluster(), yaml: "apiVersion: v1\n" })).toEqual({ kubeconfig: "apiVersion: v1\n" });
    expect(sourceArgs({ ...blankCluster(), source: "file", yaml: "x", path: "/Users/me/kc.yaml" })).toEqual({ path: "/Users/me/kc.yaml" });
    expect(sourceArgs({ ...blankCluster(), source: "file" })).toBeNull();
  });

  it("picks a context: the only one, the current one, or the first usable", () => {
    expect(pickContext([cands[2]])).toBe("dev");
    expect(pickContext(cands)).toBe("staging");
    expect(pickContext([cands[1], cands[2]])).toBe("dev");
    expect(pickContext([])).toBe("");
  });

  it("validates: a usable context, and a name free in the kubeconfig and in Coxswain", () => {
    const f = { ...blankCluster(), yaml: "x", context: "staging", name: "staging" };
    expect(clusterErrors(f, cands, contexts)).toEqual({});
    expect(clusterErrors({ ...f, yaml: "" }, cands, contexts).source).toBeDefined();
    expect(clusterErrors({ ...f, context: "" }, cands, contexts).context).toMatch(/Choose/);
    expect(clusterErrors({ ...f, context: "prod", name: "prod" }, cands, contexts).context).toMatch(/gcp/);
    expect(clusterErrors({ ...f, context: "dev", name: "dev" }, cands, contexts).name).toMatch(/your kubeconfig.*Rename/);
    expect(clusterErrors({ ...f, name: "lab" }, cands, contexts).name).toMatch(/already added/);
    expect(clusterErrors({ ...f, name: "a b" }, cands, contexts).name).toMatch(/spaces/);
    expect(clusterErrors({ ...f, name: "" }, cands, contexts).name).toMatch(/required/);
    expect(clusterErrors({ ...f, tags: "env=prod" }, cands, contexts).tags).toBeDefined();
  });

  it("builds the requests", () => {
    const f = { ...blankCluster(), yaml: "Y", context: "dev", name: "dev-copy", env: "staging", tags: "team=core", note: " shared " };
    expect(addClusterArgs(f)).toEqual({ kubeconfig: "Y", context: "dev", name: "dev-copy", tags: ["env=staging", "team=core"], note: "shared" });
    expect(addClusterArgs({ ...f, name: "dev", env: "", tags: "", note: "" })).toEqual({ kubeconfig: "Y", context: "dev", tags: [] });
    expect(testClusterArgs({ ...f, source: "file", path: "/k" })).toEqual({ path: "/k", context: "dev" });
  });
});
