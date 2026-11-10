import { describe, expect, it } from "vitest";
import { terminalTheme } from "./theme";

describe("terminalTheme", () => {
  it("has a light and a dark variant with a full palette", () => {
    const light = terminalTheme(false);
    const dark = terminalTheme(true);
    expect(light.background).toBe("#fbfbfc"); // --hull
    expect(dark.background).toBe("#101114"); // --well
    for (const t of [light, dark]) {
      for (const k of ["foreground", "cursor", "cursorAccent", "selectionBackground", "black", "red", "green",
        "yellow", "blue", "magenta", "cyan", "white", "brightBlack", "brightWhite"] as const) {
        expect(t[k], k).toMatch(/^#[0-9a-f]{6}$/);
      }
      expect(t.foreground).not.toBe(t.background);
      expect(t.cursorAccent).toBe(t.background);
    }
  });
});
