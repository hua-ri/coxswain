// Shapes returned by the server's HTTP API.

export interface Host {
  name: string;
  hostname: string;
  port?: number;
  user?: string;
  proxy_jump?: string;
  identity_files?: string[];
  tags?: Record<string, string>;
  note?: string;
  source?: "ssh_config" | "manual";
}

export interface ClientInfo {
  name: string;
  authorized_at?: string;
  revoked?: boolean;
  revoked_at?: string;
}

export interface TermSession {
  id: string;
  kind?: "ssh" | "pod";
  host: string; // ssh
  // pod terminals
  context?: string;
  namespace?: string;
  pod?: string;
  container?: string;
  owner: { kind: "human" | "agent"; name: string };
  created_at: string;
  cols: number;
  rows: number;
  closed: boolean;
  exit_code?: number;
}

/** An audit event from the activity stream (GET /api/v1/events). */
export interface AuditEvent {
  time: string;
  type: string;
  actor?: { kind: "human" | "agent"; name: string; session?: string };
  action?: string;
  args?: Record<string, unknown>;
  risk?: "read" | "write" | "destructive";
  targets?: { kind: string; id: string; tags?: Record<string, string> }[];
  duration_ms?: number;
  client?: string;
  reason?: string;
  error?: string;
  terminal?: string;
  exit_code?: number;
  transfer?: string; // transfer.* events: the transfer id
  tunnel?: string; // tunnel.* events: the tunnel id
  batch?: string; // batch.* events: the batch run id
  /** The object the event is about: a transfer, a tunnel, a batch's start, host result or end. */
  detail?: unknown;
}

/** address renders user@host:port. */
export function address(h: Host): string {
  let s = h.hostname;
  if (h.user) s = `${h.user}@${s}`;
  if (h.port && h.port !== 22) s += `:${h.port}`;
  return s;
}
