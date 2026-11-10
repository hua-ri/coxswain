import { describe, expect, it } from "vitest";
import { isProdTag, isProdTags, prodWhere } from "./env";

describe("isProdTag", () => {
  it("matches env=prod and env=production only", () => {
    expect(isProdTag("env", "prod")).toBe(true);
    expect(isProdTag("env", "Production")).toBe(true);
    expect(isProdTag("env", "staging")).toBe(false);
    expect(isProdTag("tier", "prod")).toBe(false);
    expect(isProdTag("env", undefined)).toBe(false);
  });
  it("looks through a tag set", () => {
    expect(isProdTags({ role: "db", env: "prod" })).toBe(true);
    expect(isProdTags({ role: "db" })).toBe(false);
    expect(isProdTags(undefined)).toBe(false);
  });
});

describe("prodWhere", () => {
  const hosts = [
    { name: "web-1", hostname: "a", tags: { env: "prod" } },
    { name: "dev", hostname: "b", tags: { env: "dev" } },
  ];
  const contexts = [
    { name: "prod-hk", cluster: "c", namespace: "api", tags: { env: "prod" } },
    { name: "kind", cluster: "k", namespace: "default" },
  ];
  it("knows hosts and pod places", () => {
    expect(prodWhere("web-1", hosts, contexts)).toBe(true);
    expect(prodWhere("dev", hosts, contexts)).toBe(false);
    expect(prodWhere("prod-hk/api/api-1/api", hosts, contexts)).toBe(true);
    expect(prodWhere("kind/default/x", hosts, contexts)).toBe(false);
    expect(prodWhere("unknown", hosts, contexts)).toBe(false);
    expect(prodWhere("prod-hk", hosts, contexts)).toBe(false); // a bare name is a host
  });
});
