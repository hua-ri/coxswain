import { describe, expect, it } from "vitest";
import { addEvent, filterRows, filterOptions, type Row } from "./activity";
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
