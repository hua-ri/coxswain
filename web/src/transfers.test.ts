import { describe, expect, it } from "vitest";
import type { AuditEvent } from "./cox";
import { progress, runningCount, transfersReducer, type Transfer, type TransferItem } from "./transfers";

const codex = { kind: "agent" as const, name: "codex" };

function tr(over: Partial<Transfer> = {}): Transfer {
  return {
    id: "x_1", direction: "download", host: "dev", local_path: "/d/app.log", remote_path: "/var/log/app.log",
    owner: codex, status: "running", bytes: 0, total: 1000, started_at: "2026-11-08T09:00:00Z", ...over,
  };
}

function ev(type: string, t: Transfer, time: string, extra: Partial<AuditEvent> = {}): AuditEvent {
  return { time, type, actor: codex, transfer: t.id, detail: t, ...extra };
}

describe("transfersReducer", () => {
  it("follows a transfer's events and measures its speed", () => {
    let s: TransferItem[] = [];
    s = transfersReducer(s, { type: "event", event: ev("transfer.started", tr(), "2026-11-08T09:00:00Z") });
    expect(s).toHaveLength(1);
    expect(s[0].speed).toBeUndefined();
    s = transfersReducer(s, { type: "event", event: ev("transfer.progress", tr({ bytes: 400 }), "2026-11-08T09:00:02Z") });
    expect(s[0].bytes).toBe(400);
    expect(s[0].speed).toBe(200);
    s = transfersReducer(s, { type: "event", event: ev("transfer.progress", tr({ bytes: 600 }), "2026-11-08T09:00:03Z") });
    expect(s[0].speed).toBe(200);
    s = transfersReducer(s, {
      type: "event",
      event: ev("transfer.finished", tr({ bytes: 1000, status: "done", sha256: "ab" }), "2026-11-08T09:00:04Z"),
    });
    expect(s[0]).toMatchObject({ status: "done", bytes: 1000, sha256: "ab" });
    // A late progress event does not bring it back to running.
    s = transfersReducer(s, { type: "event", event: ev("transfer.progress", tr({ bytes: 900 }), "2026-11-08T09:00:05Z") });
    expect(s[0].status).toBe("done");
  });

  it("shows why a transfer failed", () => {
    let s = transfersReducer([], { type: "event", event: ev("transfer.started", tr(), "2026-11-08T09:00:00Z") });
    s = transfersReducer(s, {
      type: "event",
      event: ev("transfer.failed", tr({ status: "cancelled" }), "2026-11-08T09:00:01Z", { reason: "cancelled by you" }),
    });
    expect(s[0]).toMatchObject({ status: "cancelled", error: "cancelled by you" });
  });

  it("ignores other events", () => {
    const s: TransferItem[] = [];
    expect(transfersReducer(s, { type: "event", event: { time: "", type: "terminal.opened" } })).toBe(s);
    expect(transfersReducer(s, { type: "event", event: { time: "", type: "transfer.progress" } })).toBe(s);
  });

  it("syncs with the server's list, newest first, keeping speeds", () => {
    let s = transfersReducer([], { type: "event", event: ev("transfer.started", tr(), "2026-11-08T09:00:00Z") });
    s = transfersReducer(s, { type: "event", event: ev("transfer.progress", tr({ bytes: 100 }), "2026-11-08T09:00:01Z") });
    s = transfersReducer(s, {
      type: "sync",
      list: [
        tr({ bytes: 150 }),
        tr({ id: "x_2", started_at: "2026-11-08T09:10:00Z", status: "done", bytes: 1000 }),
      ],
    });
    expect(s.map((t) => t.id)).toEqual(["x_2", "x_1"]);
    expect(s[1]).toMatchObject({ bytes: 150, speed: 100 });
    expect(runningCount(s)).toBe(1);
  });

  it("computes progress", () => {
    expect(progress(tr({ bytes: 250 }))).toBe(0.25);
    expect(progress(tr({ total: 0 }))).toBe(0);
    expect(progress(tr({ total: 0, status: "done" }))).toBe(1);
  });
});
