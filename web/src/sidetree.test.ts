import { describe, expect, it } from "vitest";
import { contextMatches, envOfContext, envOfHost, hostMatches, hostTitle, loadFolded, sortedContexts, sortedHosts, toggleFolded } from "./sidetree";
import type { ContextInfo } from "./k8s";
import type { Host } from "./types";

const hosts: Host[] = [
  { name: "king", hostname: "10.10.151.201", user: "root" },
  { name: "cox-test", hostname: "127.0.0.1", port: 2222, user: "ops", tags: { env: "prod", group: "demo" }, note: "treat as production" },
  { name: "stg-api", hostname: "10.0.3.4", tags: { env: "Staging" } },
  { name: "db-1", hostname: "10.0.2.5", tags: { env: "PRODUCTION" } },
  { name: "cox-test2", hostname: "127.0.0.1", port: 2223, tags: { group: "demo" } },
];

describe("sortedHosts", () => {
  const names = (hs: Host[]) => hs.map((h) => h.name);
  it("puts production first, then environments by name, then hosts without env", () => {
    expect(names(sortedHosts(hosts))).toEqual(["cox-test", "db-1", "stg-api", "king", "cox-test2"]);
  });

  it("filters by name, address, user, tags and note; every word must match", () => {
    expect(names(sortedHosts(hosts, "demo"))).toEqual(["cox-test", "cox-test2"]);
    expect(names(sortedHosts(hosts, "group=demo prod"))).toEqual(["cox-test"]);
    expect(names(sortedHosts(hosts, "ROOT"))).toEqual(["king"]);
    expect(sortedHosts(hosts, "nothing-like-this")).toEqual([]);
  });
});

describe("sortedContexts", () => {
  const cs: ContextInfo[] = [
    { name: "kind", cluster: "kind", namespace: "default" },
    { name: "stg", cluster: "gke", namespace: "default", tags: { env: "Staging" } },
    { name: "prod-hk", cluster: "eks", namespace: "api", tags: { env: "production" } },
  ];
  it("orders like hosts and normalises env", () => {
    expect(sortedContexts(cs).map((c) => c.name)).toEqual(["prod-hk", "stg", "kind"]);
    expect(cs.map(envOfContext)).toEqual(["", "staging", "prod"]);
    expect(sortedContexts(cs, "gke").map((c) => c.name)).toEqual(["stg"]);
  });
});

describe("host helpers", () => {
  it("normalises env", () => {
    expect(envOfHost(hosts[3])).toBe("prod");
    expect(envOfHost(hosts[2])).toBe("staging");
    expect(envOfHost(hosts[0])).toBe("");
  });

  it("builds a hover text with tags, jump host and note", () => {
    const t = hostTitle({ ...hosts[1], proxy_jump: "bastion" }, "ops@127.0.0.1:2222");
    expect(t.split("\n")).toEqual([
      "cox-test",
      "ops@127.0.0.1:2222",
      "env=prod  group=demo",
      "via bastion",
      "Note: treat as production",
      "Double-click: terminal",
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
