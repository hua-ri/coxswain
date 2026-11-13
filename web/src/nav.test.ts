import { describe, expect, it } from "vitest";
import {
  ACTIVITY, AGENTS, TRANSFERS, afterClose, clusterId, clusterOf, destinationFor, filesHostOf, filesId, footprint, footprintText,
  hostId, hostOf, lastAction, moveIndex, orderPlaces, placeOfWhere, revokeMessage, rowsAt, sessionCounts, visibleKinds, visit,
  ADD_CLUSTER, ADD_HOST, editHostId, editHostOf, importText, removeHostMessage,
} from "./nav";
import type { Row } from "./activity";
import { KINDS } from "./k8s";

const agent = (name: string) => ({ kind: "agent" as const, name });
const me = { kind: "human" as const, name: "desktop" };

describe("ids", () => {
  it("round-trips place and session ids", () => {
    expect(hostOf(hostId("web-1"))).toBe("web-1");
    expect(filesHostOf(filesId("db-primary"))).toBe("db-primary");
    expect(clusterOf(clusterId("prod-hk"))).toBe("prod-hk");
    expect(hostOf(filesId("web-1"))).toBe("");
    expect(filesHostOf(ACTIVITY)).toBe("");
  });
});

describe("destinationFor", () => {
  it("maps ⌘1..⌘4 and nothing else", () => {
    expect(destinationFor({ key: "1", metaKey: true })).toBe(ACTIVITY);
    expect(destinationFor({ key: "2", metaKey: true })).toBe(TRANSFERS);
    expect(destinationFor({ key: "4", metaKey: true })).toBe(AGENTS);
    expect(destinationFor({ key: "5", metaKey: true })).toBe("");
    expect(destinationFor({ key: "1", metaKey: false })).toBe("");
    expect(destinationFor({ key: "1", metaKey: true, shiftKey: true })).toBe("");
  });
});

describe("places", () => {
  it("knows a pod terminal belongs to its cluster", () => {
    expect(placeOfWhere("web-1")).toEqual({ kind: "host", name: "web-1" });
    expect(placeOfWhere("prod-hk/api/api-1/api")).toEqual({ kind: "cluster", name: "prod-hk" });
  });

  it("counts open sessions per place, and whether an agent holds one", () => {
    const c = sessionCounts([
      { id: "a", kind: "terminal", where: "web-1", owner: agent("claude-code") },
      { id: "b", kind: "files", where: "web-1", owner: me },
      { id: "c", kind: "terminal", where: "build-box", owner: me },
      { id: "d", kind: "terminal", where: "build-box", owner: agent("codex"), closed: true },
      { id: "e", kind: "terminal", where: "prod-hk/api/pod", owner: agent("codex") },
    ]);
    expect(c.get("host:web-1")).toEqual({ n: 2, agent: true });
    expect(c.get("host:build-box")).toEqual({ n: 1, agent: false });
    expect(c.get("cluster:prod-hk")).toEqual({ n: 1, agent: true });
    expect(c.has("host:prod-hk")).toBe(false);
  });

  it("orders production first, then environments by name, then untagged, stable inside each", () => {
    const xs = [["a", ""], ["b", "staging"], ["c", "prod"], ["d", "dev"], ["e", "prod"], ["f", ""]];
    expect(orderPlaces(xs, (x) => x[1]).map((x) => x[0])).toEqual(["c", "e", "d", "b", "a", "f"]);
  });
});

describe("moveIndex", () => {
  it("moves without wrapping", () => {
    expect(moveIndex("ArrowDown", 0, 3)).toBe(1);
    expect(moveIndex("ArrowDown", 2, 3)).toBe(2);
    expect(moveIndex("ArrowUp", 0, 3)).toBe(0);
    expect(moveIndex("End", 0, 3)).toBe(2);
    expect(moveIndex("Home", 2, 3)).toBe(0);
    expect(moveIndex("x", 1, 3)).toBe(-1);
    expect(moveIndex("ArrowDown", 0, 0)).toBe(-1);
  });
});

describe("history", () => {
  it("returns to the previous selection that still exists", () => {
    let h: string[] = [];
    for (const id of [ACTIVITY, "t1", hostId("web-1"), "t2", "t1"]) h = visit(h, id);
    expect(h).toEqual([ACTIVITY, hostId("web-1"), "t2", "t1"]);
    expect(afterClose(h, "t1", () => true)).toBe("t2");
    expect(afterClose(h, "t1", (id) => id !== "t2")).toBe(hostId("web-1"));
    expect(afterClose([], "t1", () => true)).toBe(ACTIVITY);
  });

  it("keeps a bounded history", () => {
    let h: string[] = [];
    for (let i = 0; i < 100; i++) h = visit(h, "t" + i);
    expect(h.length).toBe(30);
    expect(h[h.length - 1]).toBe("t99");
  });
});

describe("agents", () => {
  const terminals = [
    { owner: agent("codex"), closed: false },
    { owner: agent("codex"), closed: true },
    { owner: me, closed: false },
  ];
  const tunnels = [{ owner: agent("codex") }, { owner: agent("codex") }, { owner: agent("claude-code") }];
  const transfers = [{ owner: agent("codex"), status: "running" }, { owner: agent("codex"), status: "done" }];

  it("counts what an agent holds open", () => {
    const f = footprint("codex", terminals, tunnels, transfers);
    expect(f).toEqual({ terminals: 1, tunnels: 2, transfers: 1 });
    expect(footprintText(f)).toBe("1 terminal, 2 tunnels, 1 running transfer");
    expect(footprintText(footprint("kimi", terminals, tunnels, transfers))).toBe("");
  });

  it("says what revoking leaves open", () => {
    expect(revokeMessage("kimi", { terminals: 0, tunnels: 0, transfers: 0 })).toBe(
      "Revoke kimi? Its calls are refused from now on and it is disconnected.",
    );
    expect(revokeMessage("codex", { terminals: 1, tunnels: 0, transfers: 0 })).toContain("It still holds 1 terminal; they stay open");
  });

  it("finds a client's newest action and a host's recent rows", () => {
    const row = (key: number, client: string, hosts: string[]) => ({ key, client, hosts }) as unknown as Row;
    const rows = [row(3, "codex", ["web-1"]), row(2, "claude-code", ["web-2"]), row(1, "codex", ["web-1", "web-2"])];
    expect(lastAction(rows, "codex")?.key).toBe(3);
    expect(lastAction(rows, "kimi")).toBeUndefined();
    expect(rowsAt(rows, "web-2", 5).map((r) => r.key)).toEqual([2, 1]);
    expect(rowsAt(rows, "web-1", 1).map((r) => r.key)).toEqual([3]);
  });
});

describe("visibleKinds", () => {
  it("shows the common kinds and the current one until expanded", () => {
    const v = visibleKinds(KINDS, false, "secrets").map((k) => k.kind);
    expect(v).toContain("pods");
    expect(v).toContain("events");
    expect(v).toContain("secrets");
    expect(v).not.toContain("configmaps");
    expect(visibleKinds(KINDS, true, "pods")).toHaveLength(KINDS.length);
  });
});

describe("editor panes", () => {
  it("have their own ids", () => {
    expect(editHostId("web-1")).toBe("@edit-host:web-1");
    expect(editHostOf(editHostId("web-1"))).toBe("web-1");
    expect(editHostOf(hostId("web-1"))).toBe("");
    expect(hostOf(editHostId("web-1"))).toBe("");
    expect([ADD_HOST, ADD_CLUSTER].every((id) => id.startsWith("@"))).toBe(true);
  });

  it("say what removing a host leaves open", () => {
    expect(removeHostMessage("web-1", 0, 0)).toBe("Remove web-1 from Coxswain? Its tags and note go with it.");
    expect(removeHostMessage("web-1", 2, 1)).toBe(
      "Remove web-1 from Coxswain? Its tags and note go with it. 2 sessions and 1 tunnel on it stay open until you close them.",
    );
  });

  it("summarise an import", () => {
    expect(importText({ path: "/Users/me/.ssh/config", added: 2, updated: 1 })).toBe("Imported /Users/me/.ssh/config: 2 hosts added, 1 updated.");
    expect(importText({ path: "~/.ssh/config", added: 0, updated: 0 })).toBe("Imported ~/.ssh/config: no hosts in it.");
  });
});
