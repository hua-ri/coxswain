import { describe, expect, it } from "vitest";
import {
  ActionError,
  actionError,
  applyArgs,
  conflictsOf,
  takeoverText,
  contextBadges,
  contextFromTab,
  diffLines,
  envOf,
  forwardRequest,
  listArgs,
  terminalRequest,
  isProdContext,
  k8sTab,
  KINDS,
  podContainers,
  rowActions,
  rowObject,
  scaleWarning,
  severityClass,
  statusClass,
  type ContextInfo,
  type DescribeOutput,
} from "./k8s";

const prod: ContextInfo = { name: "prod", cluster: "c", namespace: "shop", tags: { env: "prod", team: "web" }, note: "ask first" };

describe("contexts", () => {
  it("mark production and inherited tags", () => {
    expect(isProdContext(prod)).toBe(true);
    expect(isProdContext({ ...prod, tags: { env: "Production" } })).toBe(true);
    expect(isProdContext({ ...prod, tags: { env: "dev" } })).toBe(false);
    expect(envOf(prod)).toBe("env=prod");
    expect(envOf({ ...prod, tags: {} })).toBe("");
    expect(contextBadges(prod)).toEqual([
      { text: "env=prod", danger: true, inherited: false },
      { text: "team=web", danger: false, inherited: false },
    ]);
    const alias: ContextInfo = { ...prod, name: "alias", inherited_from: ["prod"], own_tags: { team: "web" } };
    expect(contextBadges(alias)).toEqual([
      { text: "env=prod", danger: true, inherited: true },
      { text: "team=web", danger: false, inherited: false },
    ]);
    // No own tags at all: everything is inherited.
    expect(contextBadges({ ...alias, own_tags: undefined }).every((b) => b.inherited)).toBe(true);
  });

  it("have one tab each", () => {
    expect(k8sTab("kind-test")).toBe("@k8s:kind-test");
    expect(contextFromTab("@k8s:kind-test")).toBe("kind-test");
    expect(contextFromTab("@files:x")).toBe("");
  });
});

describe("tables", () => {
  it("color statuses", () => {
    expect(statusClass("status", "Running")).toBe("ok");
    expect(statusClass("status", "Ready")).toBe("ok");
    expect(statusClass("status", "Completed")).toBe("ok");
    expect(statusClass("status", "Pending")).toBe("warn");
    expect(statusClass("status", "Init:0/1")).toBe("warn");
    expect(statusClass("status", "CrashLoopBackOff")).toBe("bad");
    expect(statusClass("status", "ImagePullBackOff")).toBe("bad");
    expect(statusClass("status", "NotReady,SchedulingDisabled")).toBe("bad");
    expect(statusClass("status", "OOMKilled")).toBe("bad");
    expect(statusClass("ready", "1/1")).toBe("ok");
    expect(statusClass("ready", "0/3")).toBe("bad");
    expect(statusClass("ready", "2/3")).toBe("warn");
    expect(statusClass("name", "CrashLoopBackOff")).toBe("");
    expect(statusClass("status", "Ready=True Synced=False")).toBe("warn");
  });

  it("find a row's object", () => {
    expect(rowObject(["name", "ready"], ["web-1", "1/1"], "shop")).toEqual({ name: "web-1", namespace: "shop" });
    expect(rowObject(["namespace", "name"], ["kube-system", "dns"], "")).toEqual({ name: "dns", namespace: "kube-system" });
  });

  it("offer actions by kind", () => {
    expect(rowActions("Pod")).toEqual(["logs", "terminal", "forward", "delete"]);
    expect(rowActions("Deployment")).toEqual(["restart", "scale", "rollback"]);
    expect(rowActions("StatefulSet")).toEqual(["restart", "scale"]);
    expect(rowActions("Service")).toEqual([]);
    expect(KINDS.map((k) => k.kind)).toContain("cronjobs");
    expect(KINDS[0]).toEqual({ label: "Pods", kind: "pods" });
  });
});

describe("details", () => {
  it("list a pod's containers, not its init containers", () => {
    const d = {
      containers: [
        { name: "setup", image: "a", init: true, ready: false, restarts: 0 },
        { name: "app", image: "b", ready: true, restarts: 1 },
        { name: "sidecar", image: "c", ready: true, restarts: 0 },
      ],
    } as DescribeOutput;
    expect(podContainers(d)).toEqual(["app", "sidecar"]);
  });

  it("color diagnose findings", () => {
    expect(severityClass("critical")).toBe("bad");
    expect(severityClass("warning")).toBe("warn");
    expect(severityClass("info")).toBe("");
  });

  it("render a unified diff by line", () => {
    const d = "--- a (live)\n+++ a (after apply)\n@@ -1,2 +1,2 @@\n x\n-image: 1\n+image: 2\n";
    expect(diffLines(d)).toEqual([
      { text: "--- a (live)", cls: "meta" },
      { text: "+++ a (after apply)", cls: "meta" },
      { text: "@@ -1,2 +1,2 @@", cls: "hunk" },
      { text: " x", cls: "" },
      { text: "-image: 1", cls: "del" },
      { text: "+image: 2", cls: "add" },
    ]);
    expect(diffLines("")).toEqual([]);
  });

  it("warn before scaling to 0", () => {
    expect(scaleWarning(0)).toContain("stops");
    expect(scaleWarning(2)).toBe("");
  });

  it("show an action's error with its hint", () => {
    expect(actionError({ status: "error", error: "boom", hint: "do x" })).toBe("boom (do x)");
    expect(actionError({ status: "error" })).toBe("failed");
  });
});

describe("requests", () => {
  it("list a namespace or all of them", () => {
    expect(listArgs("pods", "shop")).toEqual({ kind: "pods", namespace: "shop" });
    expect(listArgs("pods", "*")).toEqual({ kind: "pods", all_namespaces: true });
  });

  it("open pod terminals and port-forwards", () => {
    expect(terminalRequest("kind", "shop", "web-1", "app", " bash ")).toEqual({
      pod: { context: "kind", namespace: "shop", pod: "web-1", container: "app", shell: "bash" }, cols: 80, rows: 24,
    });
    expect(terminalRequest("kind", "shop", "web-1")).toEqual({ pod: { context: "kind", namespace: "shop", pod: "web-1" }, cols: 80, rows: 24 });
    expect(forwardRequest("kind", "shop", "web-1", 8080)).toEqual({ type: "k8s", context: "kind", namespace: "shop", pod: "web-1", remote_port: 8080 });
    expect(forwardRequest("kind", "shop", "web-1", 8080, 5000).local_port).toBe(5000);
  });
});

describe("apply conflicts", () => {
  const conflicts = [
    { object: "Deployment/web", field: ".spec.replicas", manager: "cox-ops", operation: "Update", hint: "k8s_scale" },
    { object: "Deployment/web", field: ".spec.paused", manager: "cox-ops", operation: "Update" },
    { object: "Deployment/api", field: ".spec.replicas", manager: "helm", operation: "Apply" },
  ];

  it("come with the action's error", () => {
    const e = new ActionError({ status: "error", error: "Applying … conflicts", details: { conflicts } });
    expect(e.message).toBe("Applying … conflicts");
    expect(conflictsOf(e)).toEqual(conflicts);
    expect(conflictsOf(new ActionError({ status: "error", error: "boom" }))).toEqual([]);
    expect(conflictsOf(new Error("x"))).toEqual([]);
  });

  it("are taken over only when asked", () => {
    expect(applyArgs("a: 1", true, false)).toEqual({ manifest: "a: 1", dry_run: true });
    expect(applyArgs("a: 1", false, true)).toEqual({ manifest: "a: 1", dry_run: false, force_conflicts: true });
  });

  it("say whose fields are overridden", () => {
    expect(takeoverText(conflicts)).toBe(
      "This overrides fields managed by: cox-ops (Deployment/web .spec.replicas, .spec.paused); helm (Deployment/api .spec.replicas).",
    );
  });
});
