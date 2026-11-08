import { describe, expect, it } from "vitest";
import { batchProgress, fetchHostOutput, locateSections, parseNumbered, splitOutput } from "./batch";

describe("artifact parsing", () => {
  it("reads numbered lines", () => {
    expect(parseNumbered("1: === a: exit 0, 5ms ===\n2: hello\n3: \n4: x: y")).toEqual([
      { n: 1, text: "=== a: exit 0, 5ms ===" },
      { n: 2, text: "hello" },
      { n: 3, text: "" },
      { n: 4, text: "x: y" },
    ]);
    expect(parseNumbered("")).toEqual([]);
  });

  it("finds each host's section in order, skipping look-alike output", () => {
    const headers = parseNumbered(
      [
        "1: === web1: exit 0, 12ms ===",
        "3: === web2: fake header printed by web1 ===",
        "5: === web2: error after 3ms: dial tcp: refused ===",
        "6: === web3: skipped ===",
      ].join("\n"),
    );
    expect(locateSections(headers, ["web1", "web2", "web3"])).toEqual(
      new Map([
        ["web1", { start: 2, end: 4, header: "exit 0, 12ms" }],
        ["web2", { start: 6, end: 5, header: "error after 3ms: dial tcp: refused" }],
        ["web3", { start: 7, end: 0, header: "skipped" }],
      ]),
    );
  });

  it("splits stdout from stderr", () => {
    expect(splitOutput(["out1", "out2", "--- stderr ---", "err"])).toEqual({ stdout: "out1\nout2", stderr: "err" });
    expect(splitOutput(["only"])).toEqual({ stdout: "only", stderr: "" });
  });
});

describe("fetchHostOutput", () => {
  it("reads only the host's lines", async () => {
    const calls: Record<string, unknown>[] = [];
    const invoke = async (_id: string, args: Record<string, unknown>) => {
      calls.push(args);
      if (args.grep) return { status: "ok" as const, result: "1: === a: exit 0, 1ms ===\n4: === b: exit 1, 2ms ===" };
      return { status: "ok" as const, result: "5: oops\n6: --- stderr ---\n7: bad" };
    };
    const out = await fetchHostOutput(invoke, "art_1", ["a", "b"], "b");
    expect(out).toEqual({ stdout: "oops", stderr: "bad", truncated: false });
    expect(calls).toEqual([
      { id: "art_1", grep: "^=== " },
      { id: "art_1", start: 5, end: 0 },
    ]);
  });

  it("handles an empty section and a missing host", async () => {
    const invoke = async (_id: string, args: Record<string, unknown>) => {
      if (args.grep) return { status: "ok" as const, result: "1: === a: skipped ===\n2: === b: exit 0, 1ms ===" };
      throw new Error("should not read");
    };
    expect(await fetchHostOutput(invoke, "art_1", ["a", "b"], "a")).toEqual({ stdout: "", stderr: "", truncated: false });
    await expect(fetchHostOutput(invoke, "art_1", ["a", "b"], "zz")).rejects.toThrow("no output for zz");
  });

  it("reports when the section is longer than one read", async () => {
    const lines = Array.from({ length: 500 }, (_, i) => `${i + 2}: l${i}`).join("\n");
    const invoke = async (_id: string, args: Record<string, unknown>) =>
      args.grep ? { status: "ok" as const, result: "1: === a: exit 0, 1ms ===" } : { status: "ok" as const, result: lines };
    const out = await fetchHostOutput(invoke, "art_1", ["a"], "a");
    expect(out.truncated).toBe(true);
  });

  it("passes errors on", async () => {
    const invoke = async () => ({ status: "error" as const, error: "artifact art_1: not found" });
    await expect(fetchHostOutput(invoke, "art_1", ["a"], "a")).rejects.toThrow("not found");
  });
});

describe("batchProgress", () => {
  it("counts hosts while running, then summarizes", () => {
    const b = { id: "b", command: "x", finished: false, hosts: [
      { host: "a", status: "ok" as const }, { host: "b", status: "pending" as const }] };
    expect(batchProgress(b)).toBe("1/2 hosts done");
    expect(batchProgress({ ...b, finished: true, summary: { total: 2, ok: 1, failed: 0, skipped: 1 } })).toBe(
      "2 hosts: 1 ok, 0 failed, 1 skipped",
    );
  });
});
