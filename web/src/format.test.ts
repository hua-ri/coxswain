import { describe, expect, it } from "vitest";
import { formatBytes, formatSpeed, ownerLabel } from "./format";

describe("format", () => {
  it("formats sizes", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(1023)).toBe("1023 B");
    expect(formatBytes(1536)).toBe("1.5 KB");
    expect(formatBytes(10 * 1024 * 1024)).toBe("10.0 MB");
    expect(formatBytes(3 * 1024 ** 3)).toBe("3.0 GB");
  });

  it("formats speeds", () => {
    expect(formatSpeed(undefined)).toBe("");
    expect(formatSpeed(2048)).toBe("2.0 KB/s");
  });

  it("names owners", () => {
    expect(ownerLabel({ kind: "human", name: "desktop" })).toBe("you");
    expect(ownerLabel({ kind: "agent", name: "codex" })).toBe("codex");
  });
});
