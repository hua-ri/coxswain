// The terminal tabs' model.
import type { TermSession } from "./types";

export interface Tab {
  id: string;
  host: string; // where it runs: the host, or context/namespace/pod[/container]
  owner: { kind: "human" | "agent"; name: string };
  closed: boolean;
  exitCode?: number;
  typingUntil?: number; // an agent typed into it; show a dot until then (ms)
}

export type TabAction =
  | { type: "sync"; sessions: TermSession[] }
  | { type: "opened"; id: string; host: string; owner: Tab["owner"] }
  | { type: "closed"; id: string; code?: number }
  | { type: "typing"; id: string; until: number }
  | { type: "remove"; id: string };

export function tabsReducer(tabs: Tab[], a: TabAction): Tab[] {
  switch (a.type) {
    case "sync": {
      const live = new Map(a.sessions.map((s) => [s.id, s]));
      const out = tabs.map((t) => (live.has(t.id) || t.closed ? t : { ...t, closed: true }));
      for (const s of a.sessions) {
        if (!s.closed && !tabs.some((t) => t.id === s.id)) {
          out.push({ id: s.id, host: sessionWhere(s), owner: { kind: s.owner.kind, name: s.owner.name }, closed: false });
        }
      }
      return out;
    }
    case "opened":
      if (tabs.some((t) => t.id === a.id)) return tabs;
      return [...tabs, { id: a.id, host: a.host, owner: a.owner, closed: false }];
    case "closed":
      return tabs.map((t) => (t.id === a.id ? { ...t, closed: true, exitCode: a.code ?? t.exitCode } : t));
    case "typing":
      return tabs.map((t) => (t.id === a.id ? { ...t, typingUntil: a.until } : t));
    case "remove":
      return tabs.filter((t) => t.id !== a.id);
  }
}

/** sessionWhere is where a terminal runs: its host, or context/namespace/pod[/container]. */
export function sessionWhere(s: TermSession): string {
  if (s.kind !== "pod") return s.host;
  return [s.context, s.namespace, s.pod, s.container].filter((x) => x).join("/");
}

/** tabTitle is "where · opener"; the user is "you". */
export function tabTitle(t: Tab): string {
  return `${t.host} · ${t.owner.kind === "agent" ? t.owner.name : "you"}`;
}

/** needsConfirm: closing an agent's running terminal asks first. */
export function needsConfirm(t: Tab): boolean {
  return t.owner.kind === "agent" && !t.closed;
}
