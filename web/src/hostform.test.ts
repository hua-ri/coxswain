import { describe, expect, it } from "vitest";
import {
  afterScan, blankHost, connArgs, connKey, formErrors, formFromHost, hostArgs, keygenCommand, parseTagText, runCheck,
  trustAndContinue, updateArgs, type Check, type HostKey,
} from "./hostform";
import type { Outcome } from "./cox";
import type { Host } from "./types";

const hosts: Host[] = [
  { name: "bastion", hostname: "10.0.0.1" },
  { name: "web-1", hostname: "10.0.1.1", user: "deploy", port: 2222, proxy_jump: "bastion", identity_files: ["/k/id_web"], tags: { env: "prod", role: "web" }, note: "careful" },
];

describe("host form", () => {
  it("fills from a host, splitting env from the other tags", () => {
    const f = formFromHost(hosts[1]);
    expect(f).toMatchObject({ name: "web-1", hostname: "10.0.1.1", port: "2222", user: "deploy", jump: "bastion", keys: ["/k/id_web"], env: "prod", tags: "role=web", note: "careful" });
    expect(formFromHost({ name: "x", hostname: "x", tags: { env: "Production" } }).env).toBe("prod");
    expect(formFromHost({ name: "x", hostname: "x", port: 22 }).port).toBe("");
  });

  it("parses extra tags", () => {
    expect(parseTagText("role=web, team = core")).toEqual({ tags: { role: "web", team: "core" } });
    expect(parseTagText("  ")).toEqual({ tags: {} });
    expect(parseTagText("role").error).toMatch(/key=value/);
    expect(parseTagText("env=prod").error).toMatch(/Environment/);
  });

  it("validates like the server", () => {
    const ok = { ...blankHost(), name: "db-1", hostname: "db.example.com" };
    expect(formErrors(ok, hosts)).toEqual({});
    expect(formErrors({ ...ok, name: "" }, hosts).name).toMatch(/required/);
    expect(formErrors({ ...ok, name: "a b" }, hosts).name).toMatch(/spaces/);
    expect(formErrors({ ...ok, name: "a/b" }, hosts).name).toMatch(/slashes/);
    expect(formErrors({ ...ok, name: "web-1" }, hosts).name).toMatch(/already/);
    expect(formErrors({ ...ok, name: "web-1" }, hosts, "web-1").name).toBeUndefined();
    expect(formErrors({ ...ok, hostname: "" }, hosts).hostname).toMatch(/required/);
    expect(formErrors({ ...ok, hostname: "-oProxyCommand=x" }, hosts).hostname).toBeDefined();
    expect(formErrors({ ...ok, port: "70000" }, hosts).port).toMatch(/1–65535/);
    expect(formErrors({ ...ok, port: "22x" }, hosts).port).toBeDefined();
    expect(formErrors({ ...ok, user: "a b" }, hosts).user).toBeDefined();
    expect(formErrors({ ...ok, jump: "bastion" }, hosts)).toEqual({});
    expect(formErrors({ ...ok, jump: "ops@jump.example.com:2200" }, hosts)).toEqual({});
    expect(formErrors({ ...ok, jump: "bastoin" }, hosts).jump).toMatch(/not a saved host/);
    expect(formErrors({ ...ok, jump: "j.example.com:0" }, hosts).jump).toMatch(/port/);
    expect(formErrors({ ...ok, name: "db-1", jump: "db-1" }, hosts).jump).toMatch(/own jump/);
    expect(formErrors({ ...ok, tags: "nope" }, hosts).tags).toBeDefined();
  });

  it("builds add and update requests", () => {
    const f = { ...blankHost(), name: "db-1", hostname: " db.example.com ", port: "2200", keys: ["/k/a", "/k/b"], env: "prod", tags: "role=db", note: " no restarts " };
    expect(hostArgs(f)).toEqual({ name: "db-1", hostname: "db.example.com", port: 2200, identity_files: ["/k/a", "/k/b"], tags: ["env=prod", "role=db"], note: "no restarts" });
    expect(hostArgs({ ...blankHost(), name: "x", hostname: "x" })).toEqual({ name: "x", hostname: "x", identity_files: [], tags: [] });

    // An update states every field, so clearing one clears it; a rename uses new_name.
    const edit = { ...formFromHost(hosts[1]), name: "web-01", user: "", jump: "", env: "" };
    expect(updateArgs(edit, hosts[1])).toEqual({
      name: "web-1", new_name: "web-01", hostname: "10.0.1.1", port: 2222, user: "", identity_files: ["/k/id_web"], proxy_jump: "", tags: ["role=web"], note: "careful",
    });
    expect(updateArgs({ ...formFromHost(hosts[1]), port: "" }, hosts[1])).not.toHaveProperty("new_name");
    expect(updateArgs({ ...formFromHost(hosts[1]), port: "" }, hosts[1]).port).toBe(22);
  });

  it("checks the connection with the form's fields, not the saved ones", () => {
    const f = { ...formFromHost(hosts[1]), hostname: "10.0.1.9" };
    expect(connArgs(f)).toEqual({ name: "web-1", hostname: "10.0.1.9", port: 2222, user: "deploy", identity_files: ["/k/id_web"], proxy_jump: "bastion" });
    expect(connKey(f)).not.toBe(connKey(formFromHost(hosts[1])));
    expect(connKey({ ...f, note: "other" })).toBe(connKey(f)); // labels do not reset the check
  });

  it("knows the ssh-keygen command for a user's old key", () => {
    expect(keygenCommand("10.0.0.1:22")).toBe("ssh-keygen -R 10.0.0.1");
    expect(keygenCommand("10.0.0.1:2222")).toBe("ssh-keygen -R '[10.0.0.1]:2222'");
    expect(keygenCommand("[fe80::1]:22")).toBe("ssh-keygen -R fe80::1");
  });
});

const key = (status: HostKey["status"], host = "web-1"): HostKey => ({ host, address: "10.0.1.1:2222", type: "ssh-ed25519", fingerprint: "SHA256:abc", status });

/** fakeInvoke answers actions from a script, in order, and records the calls. */
function fakeInvoke(script: Record<string, Outcome[]>) {
  const calls: [string, Record<string, unknown>][] = [];
  const invoke = async <T,>(id: string, args: Record<string, unknown> = {}): Promise<Outcome<T>> => {
    calls.push([id, args]);
    const next = script[id]?.shift();
    if (!next) throw new Error("unexpected " + id);
    return next as Outcome<T>;
  };
  return { invoke, calls };
}

describe("connection check", () => {
  it("maps a scan to the next step", () => {
    expect(afterScan(key("trusted")).step).toBe("login");
    expect(afterScan(key("unknown")).step).toBe("confirm");
    expect(afterScan(key("changed")).step).toBe("changed");
  });

  it("logs in at once when the key is trusted", async () => {
    const { invoke, calls } = fakeInvoke({ "ssh.hosts.scan_key": [{ status: "ok", result: key("trusted") }], "ssh.hosts.test": [{ status: "ok", result: { host: "web-1", os: "Linux", duration_ms: 80 } }] });
    const steps: Check[] = [];
    await runCheck(invoke, { name: "web-1" }, (c) => steps.push(c));
    expect(steps.map((s) => s.step)).toEqual(["scanning", "login", "ok"]);
    expect(steps[2]).toMatchObject({ os: "Linux", ms: 80 });
    expect(calls.map((c) => c[0])).toEqual(["ssh.hosts.scan_key", "ssh.hosts.test"]);
  });

  it("stops for the user's confirmation of an unknown key", async () => {
    const { invoke, calls } = fakeInvoke({ "ssh.hosts.scan_key": [{ status: "ok", result: key("unknown") }] });
    const steps: Check[] = [];
    await runCheck(invoke, { name: "web-1" }, (c) => steps.push(c));
    expect(steps.at(-1)).toEqual({ step: "confirm", key: key("unknown") });
    expect(calls.length).toBe(1);
  });

  it("trusts the confirmed fingerprint, then checks the next hop and logs in", async () => {
    const { invoke, calls } = fakeInvoke({
      "ssh.hosts.trust_key": [{ status: "ok", result: key("trusted", "bastion") }, { status: "ok", result: key("trusted") }],
      "ssh.hosts.scan_key": [{ status: "ok", result: { ...key("unknown"), via: ["bastion"] } }, { status: "ok", result: key("trusted") }],
      "ssh.hosts.test": [{ status: "ok", result: { host: "web-1", os: "Darwin", duration_ms: 5 } }],
    });
    const steps: Check[] = [];
    await trustAndContinue(invoke, { name: "web-1" }, key("unknown", "bastion"), (c) => steps.push(c));
    expect(calls[0]).toEqual(["ssh.hosts.trust_key", { name: "web-1", fingerprint: "SHA256:abc" }]);
    expect(steps.map((s) => s.step)).toEqual(["trusting", "scanning", "confirm"]); // the target's own key is next
    const last = steps.at(-1) as Extract<Check, { step: "confirm" }>;
    steps.length = 0;
    await trustAndContinue(invoke, { name: "web-1" }, last.key, (c) => steps.push(c));
    expect(steps.map((s) => s.step)).toEqual(["trusting", "scanning", "login", "ok"]);
  });

  it("reports failures with the server's message", async () => {
    const { invoke } = fakeInvoke({ "ssh.hosts.scan_key": [{ status: "error", error: "dial tcp 10.0.1.1:2222: i/o timeout" }] });
    const steps: Check[] = [];
    await runCheck(invoke, { name: "web-1" }, (c) => steps.push(c));
    expect(steps.at(-1)).toEqual({ step: "failed", error: "dial tcp 10.0.1.1:2222: i/o timeout" });
  });
});
