import { describe, expect, it, vi } from "vitest";
import { activate } from "./keys";

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
