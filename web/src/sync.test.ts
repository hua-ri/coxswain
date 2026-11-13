import { describe, expect, it } from "vitest";
import { conflictHosts, hasWork, syncArgs, syncSummary, type ImportOutput } from "./sync";

const plan: ImportOutput = {
  path: "/Users/king/.ssh/config",
  added: [{ name: "new-1", address: "ops@10.0.0.8:22" }, { name: "new-2", address: "10.0.0.9:22" }],
  updated: [{ name: "king", changes: [{ field: "hostname", from: "10.0.0.1", to: "10.0.0.2" }] }],
  unchanged: ["a", "b", "c"],
  conflicts: [
    { name: "bastion", field: "hostname", from: "10.9.9.9", to: "10.0.0.7" },
    { name: "bastion", field: "user", from: "ops", to: "" },
  ],
};

describe("sync from ~/.ssh/config", () => {
  it("groups conflicts per host", () => {
    expect(conflictHosts(plan.conflicts)).toEqual([{ name: "bastion", changes: [{ field: "hostname", from: "10.9.9.9", to: "10.0.0.7" }, { field: "user", from: "ops", to: "" }] }]);
  });

  it("knows when there is nothing to do", () => {
    const none = { ...plan, added: [], updated: [] };
    expect(hasWork(plan, [])).toBe(true);
    expect(hasWork(none, [])).toBe(false);
    expect(hasWork(none, ["bastion"])).toBe(true);
  });

  it("asks to overwrite only the chosen hosts", () => {
    expect(syncArgs([])).toEqual({});
    expect(syncArgs(["bastion"])).toEqual({ overwrite: ["bastion"] });
  });

  it("says plainly what happened", () => {
    expect(syncSummary({ ...plan, conflicts: [] })).toBe("Added 2 hosts, updated 1 (king: hostname 10.0.0.1 → 10.0.0.2). 3 unchanged.");
    expect(syncSummary(plan)).toBe(
      "Added 2 hosts, updated 1 (king: hostname 10.0.0.1 → 10.0.0.2). 3 unchanged. Left bastion as edited in Coxswain.",
    );
    expect(syncSummary({ ...plan, added: [], updated: [], conflicts: [] })).toBe("Already up to date: 3 hosts unchanged.");
    expect(syncSummary({ ...plan, added: [plan.added[0]], updated: [], unchanged: [], conflicts: [] })).toBe("Added 1 host.");
    expect(syncSummary({ ...plan, added: [], unchanged: [], conflicts: [],
      updated: [{ name: "w", changes: [{ field: "port", from: "22", to: "2222" }, { field: "user", from: "", to: "ops" }] }] })).toBe(
      "Updated 1 (w: port 22 → 2222, user (none) → ops).",
    );
  });
});
