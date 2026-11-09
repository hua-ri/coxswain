import { describe, expect, it } from "vitest";
import type { AuditEvent } from "./cox";
import { failureText, tunnelRequest, tunnelRoute, tunnelsReducer, type Tunnel, type TunnelForm } from "./tunnels";

const kimi = { kind: "agent" as const, name: "kimi" };

function tun(over: Partial<Tunnel> = {}): Tunnel {
  return {
    id: "tn_1", type: "local", host: "dev", listen: "127.0.0.1:5432", target: "127.0.0.1:5432", owner: kimi,
    created_at: "2026-11-08T09:00:00Z", status: "open", connections: 0, total_connections: 0, bytes_out: 0,
    bytes_in: 0, idle_minutes: 30, ...over,
  };
}

function ev(type: string, t: Tunnel, extra: Partial<AuditEvent> = {}): AuditEvent {
  return { time: "2026-11-08T09:00:00Z", type, actor: kimi, tunnel: t.id, detail: t, ...extra };
}

describe("tunnelsReducer", () => {
  it("follows tunnel events", () => {
    let s: Tunnel[] = [];
    s = tunnelsReducer(s, { type: "event", event: ev("tunnel.opened", tun()) });
    s = tunnelsReducer(s, { type: "event", event: ev("tunnel.opened", tun({ id: "tn_2", created_at: "2026-11-08T09:01:00Z" })) });
    expect(s.map((t) => t.id)).toEqual(["tn_2", "tn_1"]);
    s = tunnelsReducer(s, { type: "event", event: ev("tunnel.reconnecting", tun({ status: "reconnecting", error: "EOF" })) });
    expect(s[1]).toMatchObject({ status: "reconnecting", error: "EOF" });
    s = tunnelsReducer(s, { type: "event", event: ev("tunnel.reconnected", tun({ bytes_in: 10 })) });
    expect(s[1]).toMatchObject({ status: "open", bytes_in: 10 });
    s = tunnelsReducer(s, { type: "event", event: ev("tunnel.closed", tun(), { reason: "closed by you" }) });
    expect(s.map((t) => t.id)).toEqual(["tn_2"]);
    // A late event for a closed tunnel does not bring it back.
    s = tunnelsReducer(s, { type: "event", event: ev("tunnel.reconnected", tun()) });
    expect(s.map((t) => t.id)).toEqual(["tn_2"]);
  });

  it("records failed connections", () => {
    let s = tunnelsReducer([], { type: "event", event: ev("tunnel.opened", tun()) });
    const failed = tun({ failed_connections: 3, last_error: "administratively prohibited", last_error_at: "2026-11-09T10:00:00Z" });
    s = tunnelsReducer(s, { type: "event", event: ev("tunnel.connection_failed", failed, { error: "administratively prohibited" }) });
    expect(s[0]).toMatchObject({ status: "open", failed_connections: 3, last_error: "administratively prohibited" });
    expect(failureText(s[0])).toBe("3 failed connections; last: administratively prohibited");
    expect(failureText(tun({ failed_connections: 1, last_error: "refused" }))).toBe("1 failed connection; last: refused");
    expect(failureText(tun())).toBe("");
  });

  it("syncs with the server's list and ignores other events", () => {
    const s = tunnelsReducer([tun({ id: "gone" })], { type: "sync", list: [tun()] });
    expect(s.map((t) => t.id)).toEqual(["tn_1"]);
    expect(tunnelsReducer(s, { type: "event", event: { time: "", type: "terminal.opened" } })).toBe(s);
  });
});

describe("tunnelRequest", () => {
  const form = (over: Partial<TunnelForm>): TunnelForm => ({
    host: "dev", type: "local", localPort: "", remoteHost: "", remotePort: "", ...over,
  });

  it("builds the request for each type", () => {
    expect(tunnelRequest(form({ remotePort: "5432" }))).toEqual({ body: { host: "dev", type: "local", remote_port: 5432 } });
    expect(tunnelRequest(form({ localPort: "15432", remoteHost: "db.internal", remotePort: "5432" }))).toEqual({
      body: { host: "dev", type: "local", local_port: 15432, remote_host: "db.internal", remote_port: 5432 },
    });
    expect(tunnelRequest(form({ type: "remote", localPort: "3000", remoteHost: "x", remotePort: "8080" }))).toEqual({
      body: { host: "dev", type: "remote", local_port: 3000, remote_port: 8080 },
    });
    expect(tunnelRequest(form({ type: "socks", localPort: " 1080 ", remotePort: "9" }))).toEqual({
      body: { host: "dev", type: "socks", local_port: 1080 },
    });
  });

  it("says what is missing or wrong", () => {
    expect(tunnelRequest(form({ host: "" }))).toEqual({ error: "Choose a host." });
    expect(tunnelRequest(form({}))).toEqual({ error: "A local tunnel needs the remote port." });
    expect(tunnelRequest(form({ type: "remote" }))).toEqual({ error: "A remote tunnel needs the local port it leads to." });
    expect(tunnelRequest(form({ remotePort: "70000" }))).toEqual({ error: "Ports are numbers from 1 to 65535." });
    expect(tunnelRequest(form({ remotePort: "ab" }))).toEqual({ error: "Ports are numbers from 1 to 65535." });
  });
});

describe("tunnelRoute", () => {
  it("describes where connections go", () => {
    expect(tunnelRoute(tun())).toBe("127.0.0.1:5432 → 127.0.0.1:5432 on dev");
    expect(tunnelRoute(tun({ type: "remote", listen: "127.0.0.1:8080", target: "127.0.0.1:3000" }))).toBe(
      "127.0.0.1:8080 on dev → 127.0.0.1:3000 here",
    );
    expect(tunnelRoute(tun({ type: "socks", listen: "127.0.0.1:1080", target: undefined }))).toBe(
      "SOCKS5 127.0.0.1:1080 → via dev",
    );
  });
});
