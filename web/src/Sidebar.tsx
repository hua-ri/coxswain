import { useEffect, useRef, useState, type KeyboardEvent, type MouseEvent as ReactMouseEvent, type ReactNode } from "react";
import { KINDS, type ContextInfo } from "./k8s";
import { APPLY, DESTINATIONS, TRANSFERS, TUNNELS, AGENTS, clusterId, hostId, moveIndex, sessionCounts, visibleKinds, type Session } from "./nav";
import { contextIsProd, envOfContext, envOfHost, hostTitle, loadFolded, sortedContexts, sortedHosts, toggleFolded } from "./sidetree";
import { Icon } from "./icons";
import { address, type Host } from "./types";
import { Menu, type MenuItem } from "./ui";

/** An open session as the sidebar shows it. */
export interface SideSession extends Session {
  prod: boolean;
  typing: boolean;
  title: string;
}

interface Props {
  active: string;
  onSelect(id: string): void;
  hosts: Host[];
  contexts: ContextInfo[];
  contextsError: string; // why the kubeconfig could not be read
  sessions: SideSession[];
  running: number; // running transfers
  tunnels: number;
  agents: { allowed: number; typing: boolean };
  kindOf(context: string): string;
  onKind(context: string, kind: string): void;
  onOpenTerminal(host: string): void;
  onCloseSession(id: string): void;
  onAddHost(): void;
  onImportHosts(): void;
  onAddCluster(): void;
  hostMenu(host: Host): MenuItem[]; // a host row's context menu
  clusterMenu(context: ContextInfo): MenuItem[];
}

type OpenMenu = { at: { x: number; y: number }; items: MenuItem[]; label: string };

const storage = typeof window !== "undefined" ? window.localStorage : undefined;

function Chevron({ open }: { open: boolean }) {
  return (
    <svg className={open ? "chev open" : "chev"} width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
      <path d="M3 2l4 3-4 3" />
    </svg>
  );
}

/** EnvMark is a place's environment: production as an outlined hazard chip, others as quiet text. */
function EnvMark({ env }: { env: string }) {
  if (env === "prod") return <span className="chip prod-mark">prod</span>;
  return env ? <span className="env">{env}</span> : null;
}

function Count({ n, agent }: { n: number; agent: boolean }) {
  if (!n) return null;
  return (
    <span className="open-count" title={`${n} open session${n === 1 ? "" : "s"}${agent ? ", an agent holds one" : ""}`}>
      {agent && <span className="crew-dot" aria-hidden="true" />}
      {n}
    </span>
  );
}

/** Row is one source-list row: a button that selects, and optionally a close button. */
function Row(props: {
  id: string;
  active: string;
  label: ReactNode;
  icon?: string;
  tail?: ReactNode;
  title?: string;
  className?: string;
  onSelect(): void;
  onDoubleClick?(): void;
  onContextMenu?(e: ReactMouseEvent): void;
  onClose?(): void;
  closeLabel?: string;
  expanded?: boolean;
  after?: ReactNode; // a mark right after the label
}) {
  const sel = props.active === props.id;
  const cls = ["row", sel ? "sel" : "", props.className ?? ""].filter(Boolean).join(" ");
  return (
    <li className={cls}>
      <button className="row-btn" data-nav={props.id} tabIndex={sel ? 0 : -1} title={props.title}
        aria-current={sel ? "page" : undefined} aria-expanded={props.expanded}
        onClick={props.onSelect} onDoubleClick={props.onDoubleClick} onContextMenu={props.onContextMenu}>
        {props.icon && <Icon name={props.icon} />}
        <span className="row-label"><span className="row-text">{props.label}</span>{props.after}</span>
        {props.tail && <span className="row-tail">{props.tail}</span>}
      </button>
      {props.onClose && (
        <button className="row-close" tabIndex={-1} aria-label={props.closeLabel} title={props.closeLabel} onClick={props.onClose}>
          <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden="true"><path d="M2.5 2.5l5 5M7.5 2.5l-5 5" /></svg>
        </button>
      )}
    </li>
  );
}

/** Section is a foldable block with a sentence-case header, and optionally a "+" that adds to it. */
function Section(props: {
  id: string;
  title: string;
  count?: number;
  folded: Set<string>;
  onToggle(id: string): void;
  add?: { label: string; onClick(e: ReactMouseEvent<HTMLButtonElement>): void; menu?: boolean };
  children: ReactNode;
}) {
  const open = !props.folded.has(props.id);
  return (
    <section className="src-section" aria-label={props.title}>
      <h3 className={props.add ? "src-head with-add" : "src-head"}>
        <button className="src-toggle" data-nav={"@section:" + props.id} tabIndex={-1} aria-expanded={open} onClick={() => props.onToggle(props.id)}>
          <span>{props.title}</span>
          <Chevron open={open} />
          {props.count !== undefined && <span className="n">{props.count}</span>}
        </button>
        {props.add && (
          <button className="src-add" data-nav={"@add:" + props.id} tabIndex={-1} aria-label={props.add.label} title={props.add.label}
            aria-haspopup={props.add.menu ? "menu" : undefined} onClick={props.add.onClick}>
            <Icon name="plus" />
          </button>
        )}
      </h3>
      {open && props.children}
    </section>
  );
}

export function Sidebar(p: Props) {
  const [filter, setFilter] = useState("");
  const [menu, setMenu] = useState<OpenMenu | null>(null);
  const below = (e: ReactMouseEvent<HTMLElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    return { x: r.left, y: r.bottom + 2 };
  };
  const hostsMenu = (e: ReactMouseEvent<HTMLButtonElement>) =>
    setMenu({ at: below(e), label: "Add hosts", items: [
      { label: "Add host…", onSelect: p.onAddHost },
      { label: "Import ~/.ssh/config", onSelect: p.onImportHosts },
    ] });
  const contextMenu = (items: MenuItem[], label: string) => (e: ReactMouseEvent) => {
    e.preventDefault();
    setMenu({ at: { x: e.clientX, y: e.clientY }, items, label });
  };
  const [folded, setFolded] = useState(() => loadFolded(storage));
  const toggle = (id: string) => setFolded((f) => toggleFolded(f, id, storage));
  const input = useRef<HTMLInputElement>(null);
  const list = useRef<HTMLDivElement>(null);

  // ⌘F focuses the filter.
  useEffect(() => {
    const onKey = (e: globalThis.KeyboardEvent) => {
      if (e.metaKey && !e.shiftKey && !e.altKey && !e.ctrlKey && e.key.toLowerCase() === "f") {
        e.preventDefault();
        input.current?.focus();
        input.current?.select();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const f = filter.trim().toLowerCase();
  const filtering = f !== "";
  const hosts = sortedHosts(p.hosts, filter);
  const contexts = sortedContexts(p.contexts, filter);
  const sessions = p.sessions.filter((s) => !filtering || f.split(/\s+/).every((w) => s.where.toLowerCase().includes(w) || s.owner.name.toLowerCase().includes(w)));
  const counts = sessionCounts(p.sessions);
  const activeCluster = p.active.startsWith(clusterId("")) ? p.active.slice(clusterId("").length) : "";

  const nav = () => [...(list.current?.querySelectorAll<HTMLElement>("[data-nav]") ?? [])];
  // One Tab stop: the selected row, or the first row when the selection is filtered out.
  useEffect(() => {
    const items = nav();
    if (items.length && !items.some((x) => x.tabIndex === 0)) items[0].tabIndex = 0;
  });
  const onKey = (e: KeyboardEvent<HTMLDivElement>) => {
    const items = nav();
    const i = items.indexOf(document.activeElement as HTMLElement);
    if (i < 0) return;
    const id = items[i].dataset.nav ?? "";
    if (e.key === "ArrowLeft" || e.key === "ArrowRight") {
      const ctx = id.startsWith(clusterId("")) ? id.slice(clusterId("").length) : "";
      if (ctx && ctx === activeCluster) {
        const isOpen = !folded.has("kinds:" + ctx);
        if ((e.key === "ArrowLeft") === isOpen) toggle("kinds:" + ctx);
        e.preventDefault();
      }
      if (id.startsWith("@section:")) {
        const sec = id.slice("@section:".length);
        if ((e.key === "ArrowLeft") === !folded.has(sec)) toggle(sec);
        e.preventDefault();
      }
      return;
    }
    const j = moveIndex(e.key, i, items.length);
    if (j < 0) return;
    e.preventDefault();
    items[j].focus();
  };
  const fromFilter = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Escape") setFilter("");
    if (e.key === "ArrowDown" || e.key === "Enter") {
      e.preventDefault();
      nav()[0]?.focus();
    }
  };

  const destTail = (id: string) => {
    if (id === TRANSFERS && p.running > 0) return <span className="live" title={`${p.running} running`}>{p.running}</span>;
    if (id === TUNNELS && p.tunnels > 0) return <span className="n">{p.tunnels}</span>;
    if (id === AGENTS)
      return (
        <span className="n" title={`${p.agents.allowed} allowed${p.agents.typing ? ", one is typing" : ""}`}>
          {p.agents.typing && <span className="crew-dot pulsing" aria-hidden="true" />}
          {p.agents.allowed}
        </span>
      );
    return null;
  };

  return (
    <aside className="src" aria-label="Sidebar">
      <div className="src-filter">
        <label className="src-field">
          <Icon name="search" />
          <input ref={input} type="search" value={filter} placeholder="Filter" aria-label="Filter places and sessions"
            onChange={(e) => setFilter(e.target.value)} onKeyDown={fromFilter} />
          {!filter && <kbd aria-hidden="true">⌘F</kbd>}
        </label>
      </div>
      <div className="src-scroll" ref={list} onKeyDown={onKey}>
        <ul className="src-list" aria-label="Destinations">
          {DESTINATIONS.map((d) => (
            <Row key={d.id} id={d.id} active={p.active} icon={d.id} label={d.label} title={`${d.label} (⌘${d.key})`}
              tail={destTail(d.id) ?? <kbd className="key">⌘{d.key}</kbd>} onSelect={() => p.onSelect(d.id)} />
          ))}
        </ul>

        {p.sessions.length > 0 && (
          <Section id="open" title="Open" count={filtering ? sessions.length : p.sessions.length} folded={folded} onToggle={toggle}>
            {sessions.length === 0 && <p className="src-empty">No open session matches “{filter.trim()}”.</p>}
            <ul className="src-list">
              {sessions.map((s) => (
                <Row key={s.id} id={s.id} active={p.active} icon={s.kind} title={s.title}
                  className={[s.owner.kind === "agent" ? "agent" : "", s.closed ? "ended" : ""].filter(Boolean).join(" ")}
                  label={<span className="mono">{s.where}</span>}
                  tail={
                    <>
                      {s.typing && <span className="crew-dot pulsing" title={`${s.owner.name} is typing`} />}
                      {s.owner.kind === "agent" && <span className="crew-name">{s.owner.name}</span>}
                      {s.closed && <span className="env">ended</span>}
                      {s.prod && <EnvMark env="prod" />}
                    </>
                  }
                  onSelect={() => p.onSelect(s.id)} onClose={() => p.onCloseSession(s.id)}
                  closeLabel={s.kind === "files" ? `Close files of ${s.where}` : `Close terminal on ${s.where}`} />
              ))}
            </ul>
          </Section>
        )}

        <Section id="hosts" title="Hosts" count={filtering ? hosts.length : p.hosts.length} folded={folded} onToggle={toggle}
          add={{ label: "Add a host", onClick: hostsMenu, menu: true }}>
          {p.hosts.length === 0 && (
            <div className="src-empty">
              <p>No hosts yet.</p>
              <p className="src-empty-actions">
                <button className="link" onClick={p.onAddHost}>Add a host…</button>
                <button className="link" onClick={p.onImportHosts}>Import ~/.ssh/config</button>
              </p>
            </div>
          )}
          {p.hosts.length > 0 && hosts.length === 0 && <p className="src-empty">No host matches “{filter.trim()}”.</p>}
          <ul className="src-list">
            {hosts.map((h) => {
              const c = counts.get("host:" + h.name);
              return (
                <Row key={h.name} id={hostId(h.name)} active={p.active} icon="host" title={hostTitle(h, address(h))}
                  label={h.name} tail={<><Count n={c?.n ?? 0} agent={c?.agent ?? false} /><EnvMark env={envOfHost(h)} /></>}
                  onSelect={() => p.onSelect(hostId(h.name))} onDoubleClick={() => p.onOpenTerminal(h.name)}
                  onContextMenu={contextMenu(p.hostMenu(h), h.name)} />
              );
            })}
          </ul>
        </Section>

        <Section id="clusters" title="Clusters" count={filtering ? contexts.length : p.contexts.length} folded={folded} onToggle={toggle}
          add={{ label: "Add a cluster", onClick: () => p.onAddCluster() }}>
          {p.contextsError && <p className="src-empty error-text" title={p.contextsError}>Cannot read the kubeconfig: {p.contextsError}</p>}
          {!p.contextsError && p.contexts.length === 0 && (
            <div className="src-empty">
              <p>No Kubernetes contexts in your kubeconfig.</p>
              <p className="src-empty-actions"><button className="link" onClick={p.onAddCluster}>Add a cluster…</button></p>
            </div>
          )}
          {p.contexts.length > 0 && contexts.length === 0 && <p className="src-empty">No cluster matches “{filter.trim()}”.</p>}
          <ul className="src-list">
            {contexts.map((c) => {
              const n = counts.get("cluster:" + c.name);
              const here = c.name === activeCluster;
              const kindsOpen = here && !folded.has("kinds:" + c.name);
              const title = [c.name, c.server, `namespace ${c.namespace}`, c.current && "kubectl's current context", c.note && `Note: ${c.note}`].filter(Boolean).join("\n");
              return (
                <li key={c.name} className="cluster-item">
                  <ul className="src-list">
                    <Row id={clusterId(c.name)} active={kindsOpen ? "" : p.active} icon="cluster" title={title} expanded={here ? kindsOpen : undefined}
                      after={c.current ? <span className="current" role="img" aria-label="kubectl's current context" /> : undefined}
                      label={c.name}
                      tail={<><Count n={n?.n ?? 0} agent={n?.agent ?? false} /><EnvMark env={contextIsProd(c) ? "prod" : envOfContext(c)} /></>}
                      onSelect={() => (here ? toggle("kinds:" + c.name) : p.onSelect(clusterId(c.name)))}
                      onContextMenu={contextMenu(p.clusterMenu(c), c.name)} />
                  </ul>
                  {kindsOpen && <Kinds ctx={c.name} kind={p.kindOf(c.name)} more={folded.has("more:" + c.name)}
                    onMore={() => toggle("more:" + c.name)} onKind={(k) => p.onKind(c.name, k)} />}
                </li>
              );
            })}
          </ul>
        </Section>
      </div>
      {menu && <Menu at={menu.at} items={menu.items} label={menu.label} onClose={() => setMenu(null)} />}
    </aside>
  );
}

/** Kinds are a selected cluster's resource kinds, as child rows. */
function Kinds(props: { ctx: string; kind: string; more: boolean; onMore(): void; onKind(kind: string): void }) {
  const [typed, setTyped] = useState("");
  const known = KINDS.some((k) => k.kind === props.kind);
  const shown = visibleKinds(KINDS, props.more, props.kind);
  const id = (k: string) => `@kind:${props.ctx}:${k}`;
  const current = id(props.kind);
  return (
    <ul className="src-list kinds" aria-label={`${props.ctx} resource kinds`}>
      {shown.map((k) => (
        <Row key={k.kind} id={id(k.kind)} active={current} label={k.label} className="child" onSelect={() => props.onKind(k.kind)} />
      ))}
      {!known && props.kind !== APPLY && <Row id={id(props.kind)} active={current} label={<span className="mono">{props.kind}</span>} className="child" onSelect={() => props.onKind(props.kind)} />}
      {props.more && (
        <li className="row child kind-input">
          <form onSubmit={(e) => { e.preventDefault(); if (typed.trim()) props.onKind(typed.trim()); }}>
            <input data-nav={id("@typed")} tabIndex={-1} placeholder="Other kind…" aria-label="Other kind" value={typed} onChange={(e) => setTyped(e.target.value)} />
          </form>
        </li>
      )}
      <Row id={id("@more")} active="" label={props.more ? "Fewer kinds" : "More kinds…"} className="child quiet" onSelect={props.onMore} />
      <Row id={id(APPLY)} active={current} label="Apply YAML…" className="child" onSelect={() => props.onKind(APPLY)} />
    </ul>
  );
}
