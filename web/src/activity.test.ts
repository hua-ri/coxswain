import { describe, expect, it } from "vitest";
import { addEvent, clientLabel, filterRows, filterOptions, type Row } from "./activity";
import type { AuditEvent } from "./cox";

const codex = { kind: "agent" as const, name: "codex", session: "a" };
const t = "2026-11-08T09:05:07Z";

function feed(events: AuditEvent[]): Row[] {
  return events.reduce<Row[]>((rows, ev, i) => addEvent(rows, ev, i), []);
}

describe("addEvent", () => {
  it("pairs a request with its result", () => {
    const rows = feed([
      { time: t, type: "action.requested", actor: codex, action: "ssh.exec", risk: "read",
        targets: [{ kind: "host", id: "dev" }], args: { host: "dev", command: "df -h" } },
      { time: t, type: "action.requested", actor: codex, action: "ssh.exec", risk: "destructive",
        targets: [{ kind: "host", id: "prod" }], args: { host: "prod", command: "rm -rf /x" } },
      { time: t, type: "action.finished", actor: codex, action: "ssh.exec", duration_ms: 12 },
      { time: t, type: "action.failed", actor: codex, action: "ssh.exec", duration_ms: 3, error: "boom" },
    ]);
    expect(rows).toHaveLength(2);
    // Newest first; results match the latest pending request.
    expect(rows[0]).toMatchObject({ client: "codex", action: "ssh.exec", hosts: ["prod"], risk: "destructive",
      status: "ok", durationMs: 12, detail: "rm -rf /x" });
    expect(rows[1]).toMatchObject({ hosts: ["dev"], status: "failed", error: "boom", durationMs: 3 });
  });

  it("shows the path and size of file actions, not their content", () => {
    const rows = feed([
      { time: t, type: "action.requested", actor: codex, action: "file.write", risk: "write",
        args: { host: "dev", path: "/etc/app.env", content: { bytes: 17, sha256: "ab" } } },
      { time: t, type: "action.requested", actor: codex, action: "file.download", risk: "read",
        args: { host: "dev", remote_path: "/var/log/app.log" } },
    ]);
    expect(rows[1].detail).toBe("/etc/app.env (17 bytes)");
    expect(rows[0].detail).toBe("/var/log/app.log");
  });

  it("shows where and what of k8s actions", () => {
    const k8s = (action: string, args: Record<string, unknown>) =>
      ({ time: t, type: "action.requested", actor: codex, action, risk: "write",
        targets: [{ kind: "k8s", id: "k8s:prod/shop", tags: { env: "prod" } }], args }) as AuditEvent;
    const rows = feed([
      k8s("k8s.scale", { context: "prod", kind: "deploy", name: "web", replicas: 0 }),
      k8s("k8s.rollout", { context: "prod", kind: "deployment", name: "web", action: "undo" }),
      k8s("k8s.exec", { context: "prod", pod: "web-1", command: ["ls", "-l"] }),
      k8s("k8s.logs", { context: "prod", pod: "web-1", container: "app" }),
      k8s("k8s.apply", { context: "prod", manifest: "a: 1\nb: 2\n", dry_run: true }),
      k8s("k8s.port_forward", { context: "prod", service: "web", remote_port: 80 }),
    ]);
    expect(rows.map((r) => r.detail)).toEqual([
      "service/web :80", "manifest, 2 lines (dry run)", "pod/web-1/app", "pod/web-1 $ ls -l", "deployment/web undo",
      "deploy/web replicas=0",
    ]);
    expect(rows[0].hosts).toEqual(["k8s:prod/shop"]);
  });

  it("records refusals, client and terminal events", () => {
    const rows = feed([
      { time: t, type: "action.refused", actor: codex, action: "ssh.exec", reason: "revoked" },
      { time: t, type: "client.revoked", client: "kimi" },
      { time: t, type: "terminal.opened", actor: codex, terminal: "t_1", targets: [{ kind: "host", id: "dev" }] },
      { time: t, type: "terminal.closed", actor: codex, terminal: "t_1", targets: [{ kind: "host", id: "dev" }],
        reason: "exited", exit_code: 3 },
      { time: t, type: "action.requested", actor: codex, action: "terminal.send", risk: "read",
        args: { id: "t_1", text: "ls" } },
    ]);
    expect(rows.map((r) => [r.client, r.action, r.status, r.terminal ?? ""])).toEqual([
      ["codex", "terminal.send", "running", "t_1"],
      ["codex", "terminal.closed", "info", "t_1"],
      ["codex", "terminal.opened", "info", "t_1"],
      ["kimi", "client.revoked", "info", ""],
      ["codex", "ssh.exec", "refused", ""],
    ]);
    expect(rows[1].detail).toBe("exited (exit code 3)");
    expect(rows[4].error).toBe("revoked");
  });

  it("shows finished transfers but not their progress", () => {
    const detail = { id: "x_1", direction: "download", host: "dev", remote_path: "/var/log/app.log",
      local_path: "/d/app.log", status: "done" };
    const rows = feed([
      { time: t, type: "transfer.started", actor: codex, transfer: "x_1", detail },
      { time: t, type: "transfer.progress", actor: codex, transfer: "x_1", detail },
      { time: t, type: "transfer.finished", actor: codex, transfer: "x_1", detail,
        targets: [{ kind: "host", id: "dev" }] },
      { time: t, type: "transfer.failed", actor: codex, transfer: "x_2", reason: "cancelled by you",
        detail: { ...detail, direction: "upload", status: "cancelled" } },
    ]);
    expect(rows.map((r) => [r.action, r.status, r.detail])).toEqual([
      ["transfer.failed", "info", "upload /d/app.log → dev:/var/log/app.log: cancelled by you"],
      ["transfer.finished", "info", "download dev:/var/log/app.log → /d/app.log"],
    ]);
  });

  it("folds a batch run into its ssh.batch row", () => {
    const actor = { kind: "agent" as const, name: "codex" }; // batch events carry no session
    const rows = feed([
      { time: t, type: "action.requested", actor: codex, action: "ssh.batch", risk: "read",
        targets: [{ kind: "host", id: "a" }, { kind: "host", id: "b" }, { kind: "host", id: "c" }],
        args: { command: "uptime", tag: "role=web" } },
      { time: t, type: "batch.started", actor, batch: "b_1",
        detail: { id: "b_1", command: "uptime", hosts: ["a", "b", "c"], concurrency: 5, canary: true, stop_on_failure: true } },
      { time: t, type: "batch.host_done", actor, batch: "b_1", targets: [{ kind: "host", id: "a" }],
        detail: { host: "a", status: "ok", exit_code: 0, duration_ms: 12 } },
      { time: t, type: "batch.host_done", actor, batch: "b_1", targets: [{ kind: "host", id: "b" }],
        detail: { host: "b", status: "failed", duration_ms: 3, error: "dial tcp: refused" } },
      { time: t, type: "batch.finished", actor, batch: "b_1",
        detail: { id: "b_1", summary: { total: 3, ok: 1, failed: 1, skipped: 1 }, stopped: "b failed", artifact_id: "art_9" } },
      { time: t, type: "action.finished", actor: codex, action: "ssh.batch", duration_ms: 40 },
    ]);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ action: "ssh.batch", status: "ok", detail: "uptime", durationMs: 40 });
    expect(rows[0].batch).toEqual({
      id: "b_1",
      command: "uptime",
      hosts: [
        { host: "a", status: "ok", exitCode: 0, durationMs: 12 },
        { host: "b", status: "failed", durationMs: 3, error: "dial tcp: refused" },
        { host: "c", status: "skipped" },
      ],
      summary: { total: 3, ok: 1, failed: 1, skipped: 1 },
      stopped: "b failed",
      artifactId: "art_9",
      finished: true,
    });
  });

  it("shows a batch without its action row", () => {
    const actor = { kind: "agent" as const, name: "codex" };
    const rows = feed([
      { time: t, type: "batch.started", actor, batch: "b_2", targets: [{ kind: "host", id: "a" }],
        detail: { id: "b_2", command: "false", hosts: ["a"], concurrency: 5, canary: true, stop_on_failure: true } },
      { time: t, type: "batch.host_done", actor, batch: "b_2", detail: { host: "a", status: "failed", exit_code: 1, duration_ms: 2 } },
      { time: t, type: "batch.finished", actor, batch: "b_2", detail: { id: "b_2", summary: { total: 1, ok: 0, failed: 1, skipped: 0 } } },
    ]);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ action: "ssh.batch", status: "failed", detail: "false", hosts: ["a"] });
    expect(rows[0].batch?.hosts).toEqual([{ host: "a", status: "failed", exitCode: 1, durationMs: 2 }]);
  });

  it("shows failed tunnel connections with the error", () => {
    const kimi = { kind: "agent" as const, name: "kimi" };
    const rows = feed([
      { time: t, type: "tunnel.connection_failed", actor: kimi, tunnel: "f_1", error: "ssh: rejected: administratively prohibited",
        targets: [{ kind: "host", id: "dev" }],
        detail: { id: "f_1", type: "local", host: "dev", listen: "127.0.0.1:1", target: "127.0.0.1:8080" } },
    ]);
    expect(rows[0]).toMatchObject({ action: "tunnel.connection_failed", status: "failed",
      error: "127.0.0.1:1 → 127.0.0.1:8080 on dev: ssh: rejected: administratively prohibited" });
  });

  it("hides the desktop's own artifact reads", () => {
    const desktop = { kind: "human" as const, name: "desktop", session: "d" };
    const rows = feed([
      { time: t, type: "action.requested", actor: desktop, action: "artifact.read", risk: "read", args: { id: "art_1" } },
      { time: t, type: "action.finished", actor: desktop, action: "artifact.read", duration_ms: 1 },
      { time: t, type: "action.failed", actor: desktop, action: "artifact.read", error: "not found" },
      { time: t, type: "action.requested", actor: codex, action: "artifact.read", risk: "read", args: { id: "art_1" } },
      { time: t, type: "action.requested", actor: { kind: "human", name: "cli" }, action: "artifact.read", risk: "read" },
    ]);
    expect(rows.map((r) => r.client)).toEqual(["cli", "codex"]);
  });

  it("keeps at most 1000 rows", () => {
    let rows: Row[] = [];
    for (let i = 0; i < 1100; i++) rows = addEvent(rows, { time: t, type: "client.allowed", client: "c" }, i);
    expect(rows).toHaveLength(1000);
    expect(rows[0].key).toBe(1099);
  });
});

describe("filters", () => {
  const rows = feed([
    { time: t, type: "action.requested", actor: codex, action: "ssh.exec", targets: [{ kind: "host", id: "dev" }] },
    { time: t, type: "action.requested", actor: { kind: "human", name: "cli" }, action: "ssh.exec",
      targets: [{ kind: "host", id: "prod" }] },
  ]);
  it("filters by client and host", () => {
    expect(filterRows(rows, { client: "codex", host: "" })).toHaveLength(1);
    expect(filterRows(rows, { client: "", host: "prod" })[0].client).toBe("cli");
    expect(filterRows(rows, { client: "codex", host: "prod" })).toHaveLength(0);
    expect(filterRows(rows, { client: "", host: "" })).toHaveLength(2);
  });
  it("offers the clients and hosts seen", () => {
    expect(filterOptions(rows)).toEqual({ clients: ["cli", "codex"], hosts: ["dev", "prod"] });
  });
});

describe("clientLabel", () => {
  it("calls the user's own clients you", () => {
    expect(clientLabel("desktop")).toBe("you");
    expect(clientLabel("cli")).toBe("you");
    expect(clientLabel("codex")).toBe("codex");
  });
  it("labels rows", () => {
    const rows = feed([
      { time: t, type: "action.requested", actor: { kind: "human", name: "desktop" }, action: "ssh.exec" },
      { time: t, type: "action.requested", actor: codex, action: "ssh.exec" },
      { time: t, type: "client.revoked", client: "kimi" },
    ]);
    expect(rows.map((r) => [r.who, r.client])).toEqual([
      ["kimi", "kimi"],
      ["codex", "codex"],
      ["you", "desktop"],
    ]);
  });
});
