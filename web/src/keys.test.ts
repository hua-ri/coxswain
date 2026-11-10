import { describe, expect, it, vi } from "vitest";
import { activate, stepIndex } from "./keys";

describe("stepIndex", () => {
  it("wraps arrows and jumps with Home/End", () => {
    expect(stepIndex("ArrowRight", 2, 3)).toBe(0);
    expect(stepIndex("ArrowLeft", 0, 3)).toBe(2);
    expect(stepIndex("ArrowDown", 0, 3)).toBe(1);
    expect(stepIndex("Home", 2, 3)).toBe(0);
    expect(stepIndex("End", 0, 3)).toBe(2);
    expect(stepIndex("a", 0, 3)).toBe(-1);
    expect(stepIndex("ArrowRight", 0, 0)).toBe(-1);
  });
});

describe("activate", () => {
  const ev = (key: string, own = true) => {
    const el = {};
    return { key, target: el, currentTarget: own ? el : {}, preventDefault: vi.fn() };
  };
  it("runs on Enter and Space on the element itself", () => {
    const fn = vi.fn();
    const h = activate(fn);
    for (const k of ["Enter", " "]) h(ev(k) as never);
    h(ev("x") as never);
    h(ev("Enter", false) as never);
    expect(fn).toHaveBeenCalledTimes(2);
  });
});
