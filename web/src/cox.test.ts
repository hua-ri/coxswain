import { describe, expect, it, vi } from "vitest";
import { createCox, fromBase64, toBase64, type Bindings, type Payload } from "./cox";

// fakeBindings answers coxApi like Go: a call id now, the result on "reply"
// (pushed through target.__cox) shortly after.
function fakeBindings(
  overrides: Partial<Bindings> = {},
  target: { __cox?: { emit(id: string, p: Payload): void } } = {},
): Bindings & { calls: unknown[][]; target: typeof target } {
  const calls: unknown[][] = [];
  let next = 0;
  let call = 0;
  return {
    calls,
    target,
    coxApi: async (...args) => {
      calls.push(["api", ...args]);
      const id = "c" + ++call;
      const fail = args[1] === "/fail";
      setTimeout(() =>
        target.__cox!.emit(
          "reply",
          fail ? { type: "reply", call: id, ok: false, error: "boom" } : { type: "reply", call: id, ok: true, value: { status: "ok", result: [1, 2] } },
        ),
      );
      return id;
    },
    coxSubscribe: async (...args) => {
      calls.push(["subscribe", ...args]);
      return "s" + ++next;
    },
    coxUnsubscribe: async (id) => {
      calls.push(["unsubscribe", id]);
    },
    coxInput: async (...args) => {
      calls.push(["input", ...args]);
    },
    coxResize: async (...args) => {
      calls.push(["resize", ...args]);
    },
    coxStatus: async () => ({ connected: true, addr: "127.0.0.1:1", version: "v", pid: 1 }),
    ...overrides,
  };
}

describe("base64", () => {
  it("round-trips UTF-8", () => {
    expect(new TextDecoder().decode(fromBase64(toBase64("héllo 世界\r")))).toBe("héllo 世界\r");
    expect(toBase64("echo hi\r")).toBe(btoa("echo hi\r"));
    expect(fromBase64("")).toEqual(new Uint8Array());
  });
});

describe("cox", () => {
  it("installs window.__cox and routes messages to subscribers", async () => {
    const target: { __cox?: { emit(id: string, p: Payload): void } } = {};
    const cox = createCox(fakeBindings(), target);
    expect(target.__cox).toBe(cox);
    const got: Payload[] = [];
    const stop = await cox.subscribe("terminal", "t_1", (p) => got.push(p));
    target.__cox!.emit("s1", { type: "data", data: "aGk=", backlog: true });
    target.__cox!.emit("s2", { type: "gap" });
    expect(got).toEqual([{ type: "data", data: "aGk=", backlog: true }]);
    stop();
    target.__cox!.emit("s1", { type: "gap" });
    expect(got).toHaveLength(1);
  });

  it("keeps messages that arrive before subscribe resolves", async () => {
    let cox!: ReturnType<typeof createCox>;
    const b = fakeBindings({
      coxSubscribe: async () => {
        cox.emit("s9", { type: "data", data: "YQ==", backlog: true });
        cox.emit("s9", { type: "exit", code: 0 });
        return "s9";
      },
    });
    cox = createCox(b, {});
    const got: Payload[] = [];
    await cox.subscribe("terminal", "t_1", (p) => got.push(p));
    expect(got.map((p) => p.type)).toEqual(["data", "exit"]);
  });

  it("unsubscribes on the Go side once", async () => {
    const b = fakeBindings();
    const cox = createCox(b, {});
    const stop = await cox.subscribe("events", "", () => {});
    stop();
    stop();
    expect(b.calls.filter((c) => c[0] === "unsubscribe")).toEqual([["unsubscribe", "s1"]]);
  });

  it("reports connection changes", () => {
    const cox = createCox(fakeBindings(), {});
    const seen = vi.fn();
    const off = cox.onStatus(seen);
    cox.emit("bridge", { type: "disconnected" });
    off();
    cox.emit("bridge", { type: "connected", addr: "a", version: "v", pid: 2 });
    expect(seen).toHaveBeenCalledTimes(1);
    expect(seen).toHaveBeenCalledWith({ type: "disconnected" });
  });

  it("delivers API results that arrive before the call id", async () => {
    const target: { __cox?: { emit(id: string, p: Payload): void } } = {};
    const b = fakeBindings({
      coxApi: async () => {
        target.__cox!.emit("reply", { type: "reply", call: "c7", ok: true, value: 42 });
        return "c7";
      },
    }, target);
    const cox = createCox(b, target);
    expect(await cox.api("GET", "/api/v1/clients")).toBe(42);
  });

  it("rejects failed API calls", async () => {
    const b = fakeBindings();
    const cox = createCox(b, b.target);
    await expect(cox.api("GET", "/fail")).rejects.toThrow("boom");
  });

  it("encodes input and invokes actions", async () => {
    const b = fakeBindings();
    const cox = createCox(b, b.target);
    await cox.input("t_1", "ls\r");
    await cox.resize("t_1", 100, 30);
    const out = await cox.invoke("ssh.hosts.list", {});
    expect(out.result).toEqual([1, 2]);
    expect(b.calls).toEqual([
      ["input", "t_1", btoa("ls\r")],
      ["resize", "t_1", 100, 30],
      ["api", "POST", "/api/v1/actions/ssh.hosts.list", {}],
    ]);
  });
});
