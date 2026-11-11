import { describe, expect, it } from "vitest";
import { contextMatches, envOfHost, extraTags, groupHosts, hostMatches, hostTitle, loadFolded, toggleFolded } from "./sidetree";
import type { ContextInfo } from "./k8s";
import type { Host } from "./types";

const hosts: Host[] = [
  { name: "king", hostname: "10.10.151.201", user: "root" },
  { name: "cox-test", hostname: "127.0.0.1", port: 2222, user: "ops", tags: { env: "prod", group: "demo" }, note: "treat as production" },
  { name: "stg-api", hostname: "10.0.3.4", tags: { env: "Staging" } },
  { name: "db-1", hostname: "10.0.2.5", tags: { env: "PRODUCTION" } },
  { name: "cox-test2", hostname: "127.0.0.1", port: 2223, tags: { group: "demo" } },
];

describe("groupHosts", () => {
  it("puts production first, then environments by name, then hosts without env", () => {
    const g = groupHosts(hosts);
    expect(g.map((x) => [x.label, x.hosts.map((h) => h.name)])).toEqual([
      ["Production", ["cox-test", "db-1"]],
      ["Staging", ["stg-api"]],
      ["No environment", ["king", "cox-test2"]],
    ]);
    expect(g[0].prod).toBe(true);
    expect(g[1].prod).toBe(false);
  });

  it("filters by name, address, user, tags and note; every word must match", () => {
    expect(groupHosts(hosts, "demo").flatMap((g) => g.hosts.map((h) => h.name))).toEqual(["cox-test", "cox-test2"]);
    expect(groupHosts(hosts, "group=demo prod").flatMap((g) => g.hosts.map((h) => h.name))).toEqual(["cox-test"]);
    expect(groupHosts(hosts, "ROOT").flatMap((g) => g.hosts.map((h) => h.name))).toEqual(["king"]);
    expect(groupHosts(hosts, "nothing-like-this")).toEqual([]);
  });

  it("drops empty groups", () => {
    expect(groupHosts(hosts, "stg").map((g) => g.key)).toEqual(["staging"]);
  });
});

describe("host helpers", () => {
  it("normalises env", () => {
    expect(envOfHost(hosts[3])).toBe("prod");
    expect(envOfHost(hosts[2])).toBe("staging");
    expect(envOfHost(hosts[0])).toBe("");
  });

  it("lists tags other than env", () => {
    expect(extraTags({ env: "prod", team: "pay", group: "demo" })).toEqual(["group=demo", "team=pay"]);
  });

  it("builds a hover text with tags, jump host and note", () => {
    const t = hostTitle({ ...hosts[1], proxy_jump: "bastion" }, "ops@127.0.0.1:2222");
    expect(t.split("\n")).toEqual([
      "cox-test",
      "ops@127.0.0.1:2222",
      "env=prod  group=demo",
      "via bastion",
      "Note: treat as production",
      "Double-click or Enter: terminal",
    ]);
  });

  it("matches a host with no filter", () => {
    expect(hostMatches(hosts[0], "  ")).toBe(true);
  });
});

describe("contextMatches", () => {
  const c: ContextInfo = { name: "kind-cox-test", cluster: "kind-cox-test", namespace: "default", tags: { env: "dev" } };
  it("matches name, cluster, namespace and tags", () => {
    expect(contextMatches(c, "kind")).toBe(true);
    expect(contextMatches(c, "env=dev")).toBe(true);
    expect(contextMatches(c, "prod")).toBe(false);
  });
});

describe("folded sections", () => {
  function memory() {
    const m = new Map<string, string>();
    return { getItem: (k: string) => m.get(k) ?? null, setItem: (k: string, v: string) => void m.set(k, v) };
  }

  it("persists folding", () => {
    const s = memory();
    let f = loadFolded(s);
    expect(f.size).toBe(0);
    f = toggleFolded(f, "agents", s);
    f = toggleFolded(f, "hosts:", s);
    expect(loadFolded(s)).toEqual(new Set(["agents", "hosts:"]));
    f = toggleFolded(f, "agents", s);
    expect(loadFolded(s)).toEqual(new Set(["hosts:"]));
  });

  it("ignores broken storage", () => {
    expect(loadFolded({ getItem: () => "{not json" }).size).toBe(0);
    expect(loadFolded(undefined).size).toBe(0);
    expect(toggleFolded(new Set(), "x", { setItem: () => { throw new Error("full"); } })).toEqual(new Set(["x"]));
  });
});
