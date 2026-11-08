// Shapes returned by the server's HTTP API.

export interface Host {
  name: string;
  hostname: string;
  port?: number;
  user?: string;
  proxy_jump?: string;
  tags?: Record<string, string>;
  note?: string;
}

export interface ClientInfo {
  name: string;
  authorized_at?: string;
  revoked?: boolean;
  revoked_at?: string;
}

export interface TermSession {
  id: string;
  host: string;
  owner: { kind: "human" | "agent"; name: string };
  created_at: string;
  cols: number;
  rows: number;
  closed: boolean;
  exit_code?: number;
}

/** address renders user@host:port. */
export function address(h: Host): string {
  let s = h.hostname;
  if (h.user) s = `${h.user}@${s}`;
  if (h.port && h.port !== 22) s += `:${h.port}`;
  return s;
}
