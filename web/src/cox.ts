// The page's side of the desktop bridge. Go binds the cox* functions
// into the page (each returns a Promise) and pushes streams by calling
// window.__cox.emit(subID, payload). The page never talks to the server
// itself.

/** An audit event from the activity stream. */
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
}

export type Payload =
  | { type: "event"; event: AuditEvent }
  | { type: "data"; data: string; backlog?: boolean }
  | { type: "gap" }
  | { type: "exit"; code: number | null }
  | { type: "connected"; addr: string; version: string; pid: number }
  | { type: "disconnected" };

export type StatusPayload = Extract<Payload, { type: "connected" | "disconnected" }>;

export interface Status {
  connected: boolean;
  addr?: string;
  version?: string;
  pid?: number;
}

/** An action's result. */
export interface Outcome<T = unknown> {
  status: "ok" | "error";
  result?: T;
  error?: string;
  hint?: string;
}

/** The functions Go binds into the page. */
export interface Bindings {
  coxApi(method: string, path: string, body?: unknown): Promise<unknown>;
  coxSubscribe(kind: "events" | "terminal", id: string): Promise<string>;
  coxUnsubscribe(subID: string): Promise<void>;
  coxInput(termID: string, base64: string): Promise<void>;
  coxResize(termID: string, cols: number, rows: number): Promise<void>;
  coxStatus(): Promise<Status>;
}

/** Subscription id of connection changes. */
export const STATUS_CHANNEL = "bridge";

/** How many early messages are kept for a subscription not yet known. */
const PENDING_LIMIT = 10000;

export function toBase64(text: string): string {
  const bytes = new TextEncoder().encode(text);
  let s = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    s += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  }
  return btoa(s);
}

export function fromBase64(b64: string): Uint8Array {
  const s = atob(b64);
  const out = new Uint8Array(s.length);
  for (let i = 0; i < s.length; i++) out[i] = s.charCodeAt(i);
  return out;
}

export type Cox = ReturnType<typeof createCox>;

/** createCox wraps the bindings and installs target.__cox (the window). */
export function createCox(b: Bindings, target: { __cox?: unknown }) {
  const handlers = new Map<string, (p: Payload) => void>();
  const pending = new Map<string, Payload[]>();
  const dead = new Set<string>();
  const statusHandlers = new Set<(p: StatusPayload) => void>();

  const cox = {
    /** Called by Go for every pushed message. */
    emit(subID: string, payload: Payload) {
      if (subID === STATUS_CHANNEL) {
        statusHandlers.forEach((h) => h(payload as StatusPayload));
        return;
      }
      const h = handlers.get(subID);
      if (h) {
        h(payload);
      } else if (!dead.has(subID)) {
        // Streams start before coxSubscribe's promise resolves.
        const list = pending.get(subID) ?? [];
        if (list.length < PENDING_LIMIT) list.push(payload);
        pending.set(subID, list);
      }
    },

    api<T = unknown>(method: string, path: string, body?: unknown): Promise<T> {
      return b.coxApi(method, path, body ?? null) as Promise<T>;
    },

    invoke<T = unknown>(id: string, args: Record<string, unknown> = {}): Promise<Outcome<T>> {
      return b.coxApi("POST", "/api/v1/actions/" + id, args) as Promise<Outcome<T>>;
    },

    /** subscribe streams to handler until the returned function is called. */
    async subscribe(kind: "events" | "terminal", id: string, handler: (p: Payload) => void): Promise<() => void> {
      const subID = await b.coxSubscribe(kind, id);
      for (const p of pending.get(subID) ?? []) handler(p);
      pending.delete(subID);
      handlers.set(subID, handler);
      let stopped = false;
      return () => {
        if (stopped) return;
        stopped = true;
        handlers.delete(subID);
        dead.add(subID);
        void b.coxUnsubscribe(subID);
      };
    },

    onStatus(handler: (p: StatusPayload) => void): () => void {
      statusHandlers.add(handler);
      return () => statusHandlers.delete(handler);
    },

    status: () => b.coxStatus(),
    input: (termID: string, text: string) => b.coxInput(termID, toBase64(text)),
    resize: (termID: string, cols: number, rows: number) => b.coxResize(termID, cols, rows),
  };
  target.__cox = cox;
  return cox;
}

/** inDesktop reports whether the page runs inside `cox desktop`. */
export function inDesktop(w: object = window): boolean {
  return typeof (w as Partial<Bindings>).coxApi === "function";
}
