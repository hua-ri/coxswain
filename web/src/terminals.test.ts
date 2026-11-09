import { describe, expect, it } from "vitest";
import { needsConfirm, sessionWhere, tabTitle, tabsReducer, type Tab } from "./terminals";
import type { TermSession } from "./types";

const codex = { kind: "agent" as const, name: "codex" };
const desktop = { kind: "human" as const, name: "desktop" };

function session(id: string, owner: TermSession["owner"] = desktop, closed = false): TermSession {
  return { id, host: "dev", owner, created_at: "", cols: 80, rows: 24, closed };
}

describe("tabsReducer", () => {
  it("adds terminals once and keeps their order", () => {
    let tabs: Tab[] = [];
    tabs = tabsReducer(tabs, { type: "opened", id: "t_1", host: "dev", owner: codex });
    tabs = tabsReducer(tabs, { type: "opened", id: "t_2", host: "prod", owner: desktop });
    tabs = tabsReducer(tabs, { type: "opened", id: "t_1", host: "dev", owner: codex });
    expect(tabs.map((t) => t.id)).toEqual(["t_1", "t_2"]);
  });

  it("syncs with the server's list", () => {
    let tabs: Tab[] = [{ id: "t_old", host: "dev", owner: desktop, closed: false }];
    tabs = tabsReducer(tabs, { type: "sync", sessions: [session("t_1", codex), session("t_gone", desktop, true)] });
    // Terminals the server no longer has are ended; ended ones are not reopened.
    expect(tabs).toEqual([
      { id: "t_old", host: "dev", owner: desktop, closed: true },
      { id: "t_1", host: "dev", owner: codex, closed: false },
    ]);
  });

  it("marks ended terminals, typing agents, and removes tabs", () => {
    let tabs: Tab[] = tabsReducer([], { type: "opened", id: "t_1", host: "dev", owner: codex });
    tabs = tabsReducer(tabs, { type: "typing", id: "t_1", until: 5000 });
    expect(tabs[0].typingUntil).toBe(5000);
    tabs = tabsReducer(tabs, { type: "typing", id: "t_nope", until: 5000 });
    tabs = tabsReducer(tabs, { type: "closed", id: "t_1", code: 3 });
    expect(tabs[0]).toMatchObject({ closed: true, exitCode: 3 });
    tabs = tabsReducer(tabs, { type: "remove", id: "t_1" });
    expect(tabs).toEqual([]);
  });
});

describe("tabs", () => {
  it("are titled host · opener", () => {
    expect(tabTitle({ id: "t", host: "dev", owner: codex, closed: false })).toBe("dev · codex");
    expect(tabTitle({ id: "t", host: "dev", owner: desktop, closed: false })).toBe("dev · you");
  });
  it("ask before closing an agent's open terminal", () => {
    expect(needsConfirm({ id: "t", host: "dev", owner: codex, closed: false })).toBe(true);
    expect(needsConfirm({ id: "t", host: "dev", owner: codex, closed: true })).toBe(false);
    expect(needsConfirm({ id: "t", host: "dev", owner: desktop, closed: false })).toBe(false);
  });
});

describe("pod terminals", () => {
  const pod: TermSession = {
    id: "t_p", kind: "pod", host: "", context: "kind-test", namespace: "shop", pod: "web-1", container: "app",
    owner: codex, created_at: "", cols: 80, rows: 24, closed: false,
  };

  it("are named context/namespace/pod[/container]", () => {
    expect(sessionWhere(pod)).toBe("kind-test/shop/web-1/app");
    expect(sessionWhere({ ...pod, container: undefined })).toBe("kind-test/shop/web-1");
    expect(sessionWhere(session("t_1"))).toBe("dev");
  });

  it("get tabs titled with where they run and who opened them", () => {
    const tabs = tabsReducer([], { type: "sync", sessions: [pod] });
    expect(tabTitle(tabs[0])).toBe("kind-test/shop/web-1/app · codex");
  });
});
