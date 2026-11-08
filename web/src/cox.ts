// The page's side of the desktop bridge. Go binds the cox* functions
// into the page (each returns a Promise) and pushes streams by calling
// window.__cox.emit(subID, payload). The page never talks to the server
// itself. Bound functions run on the UI thread, so coxApi only returns a
// call id; its result arrives as a "reply" message.

import type { AuditEvent } from "./types";

export type { AuditEvent };

export type Payload =
  | { type: "event"; event: AuditEvent }
  | { type: "data"; data: string; backlog?: boolean }
  | { type: "gap" }
  | { type: "exit"; code: number | null }
  | { type: "connected"; addr: string; version: string; pid: number }
  | { type: "disconnected" }
  | { type: "reply"; call: string; ok: boolean; value?: unknown; error?: string };

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
  /** coxApi starts a call and resolves to its id; the result comes on REPLY_CHANNEL. */
  coxApi(method: string, path: string, body: unknown): Promise<string>;
  coxSubscribe(kind: "events" | "terminal", id: string): Promise<string>;
  coxUnsubscribe(subID: string): Promise<void>;
  coxInput(termID: string, base64: string): Promise<void>;
  coxResize(termID: string, cols: number, rows: number): Promise<void>;
  coxStatus(): Promise<Status>;
  /** coxPickFile opens a native file dialog; the chosen absolute path ("" if cancelled) comes on REPLY_CHANNEL. */
  coxPickFile(): Promise<string>;
}

/** Subscription id of connection changes. */
export const STATUS_CHANNEL = "bridge";
/** Subscription id of coxApi results. */
export const REPLY_CHANNEL = "reply";

type Reply = Extract<Payload, { type: "reply" }>;

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
  const replies = new Map<string, Reply>(); // arrived before their call id
  const waiting = new Map<string, (r: Reply) => void>();

  function call(method: string, path: string, body: unknown): Promise<unknown> {
    return reply(b.coxApi(method, path, body ?? null));
  }

  /** reply waits for the result of the call whose id start resolves to. */
  async function reply(start: Promise<string>): Promise<unknown> {
    const id = await start;
    const r = await new Promise<Reply>((resolve) => {
      const early = replies.get(id);
      if (early) {
        replies.delete(id);
        resolve(early);
      } else {
        waiting.set(id, resolve);
      }
    });
    if (!r.ok) throw new Error(r.error ?? "request failed");
    return r.value;
  }

  const cox = {
    /** Called by Go for every pushed message. */
    emit(subID: string, payload: Payload) {
      if (subID === REPLY_CHANNEL) {
        const r = payload as Reply;
        const resolve = waiting.get(r.call);
        waiting.delete(r.call);
        if (resolve) resolve(r);
        else replies.set(r.call, r);
        return;
      }
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
      return call(method, path, body) as Promise<T>;
    },

    invoke<T = unknown>(id: string, args: Record<string, unknown> = {}): Promise<Outcome<T>> {
      return call("POST", "/api/v1/actions/" + id, args) as Promise<Outcome<T>>;
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

    /** pickFile asks for a local file: its absolute path, or "" if cancelled. */
    pickFile: () => reply(b.coxPickFile()) as Promise<string>,
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
