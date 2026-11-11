import { useState, type KeyboardEvent, type ReactNode } from "react";
import { contextBadges, type ContextInfo } from "./k8s";
import { contextIsProd, contextMatches, extraTags, groupHosts, hostTitle, loadFolded, toggleFolded } from "./sidetree";
import { address, type ClientInfo, type Host } from "./types";
import { Tag } from "./ui";

interface Props {
  hosts: Host[];
  clients: ClientInfo[];
  contexts: ContextInfo[];
  contextsError: string; // why the kubeconfig could not be read
  onOpenCluster(context: string): void;
  onOpenTerminal(host: string): void;
  onOpenFiles(host: string): void;
  onAllow(name: string): void;
  onRevoke(name: string): void;
}

const storage = typeof window !== "undefined" ? window.localStorage : undefined;

function Chevron({ open }: { open: boolean }) {
  return (
    <svg className={open ? "chev open" : "chev"} width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
      <path d="M3 2l4 3-4 3" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function TerminalIcon() {
  return (
    <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
      <rect x="1.5" y="2.5" width="13" height="11" rx="1.5" fill="none" stroke="currentColor" strokeWidth="1.2" />
      <path d="M4.5 6l2 2-2 2M8 10h3.5" fill="none" stroke="currentColor" strokeWidth="1.2" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function FolderIcon() {
  return (
    <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
      <path d="M1.8 4.2c0-.6.5-1.1 1.1-1.1h3.2l1.4 1.5h5.6c.6 0 1.1.5 1.1 1.1v6.2c0 .6-.5 1.1-1.1 1.1H2.9c-.6 0-1.1-.5-1.1-1.1z" fill="none" stroke="currentColor" strokeWidth="1.2" strokeLinejoin="round" />
    </svg>
  );
}

function NoteIcon({ text }: { text: string }) {
  return (
    <span className="note-mark" title={`Note: ${text}`} aria-label="has a note">
      <svg width="12" height="12" viewBox="0 0 12 12" aria-hidden="true">
        <circle cx="6" cy="6" r="4.8" fill="none" stroke="currentColor" strokeWidth="1.1" />
        <path d="M6 5.3v3M6 3.6v.1" stroke="currentColor" strokeWidth="1.2" strokeLinecap="round" />
      </svg>
    </span>
  );
}

/** Section is a foldable sidebar block with a sticky header. */
function Section(props: { id: string; title: string; count: number; folded: Set<string>; onToggle(id: string): void; children: ReactNode }) {
  const open = !props.folded.has(props.id);
  return (
    <section className="side-section" aria-label={props.title}>
      <h3 className="side-head">
        <button className="side-toggle" aria-expanded={open} onClick={() => props.onToggle(props.id)}>
          <Chevron open={open} />
          <span>{props.title}</span>
          {props.count > 0 && <span className="count">{props.count}</span>}
        </button>
      </h3>
      {open && props.children}
    </section>
  );
}

function onRowKey(e: KeyboardEvent, run: () => void) {
  if (e.key === "Enter") {
    e.preventDefault();
    run();
  }
}

export function Sidebar({ hosts, clients, contexts, contextsError, onOpenCluster, onOpenTerminal, onOpenFiles, onAllow, onRevoke }: Props) {
  const [filter, setFilter] = useState("");
  const [folded, setFolded] = useState(() => loadFolded(storage));
  const toggle = (id: string) => setFolded((f) => toggleFolded(f, id, storage));

  const groups = groupHosts(hosts, filter);
  const shownHosts = groups.reduce((n, g) => n + g.hosts.length, 0);
  const shownContexts = contexts.filter((c) => contextMatches(c, filter));
  const filtering = filter.trim() !== "";

  return (
    <aside className="sidebar">
      <div className="side-filter">
        <input
          type="search"
          value={filter}
          placeholder="Filter hosts & clusters"
          aria-label="Filter hosts and clusters"
          onChange={(e) => setFilter(e.target.value)}
          onKeyDown={(e) => e.key === "Escape" && setFilter("")}
        />
      </div>

      <Section id="hosts" title="Hosts" count={filtering ? shownHosts : hosts.length} folded={folded} onToggle={toggle}>
        {hosts.length === 0 && <p className="side-empty">No hosts yet. Run cox init or cox ssh hosts import.</p>}
        {hosts.length > 0 && shownHosts === 0 && <p className="side-empty">No host matches “{filter.trim()}”.</p>}
        {groups.map((g) => {
          const gid = `hosts:${g.key}`;
          const open = filtering || !folded.has(gid);
          return (
            <div key={g.key} className={g.prod ? "side-group prod" : "side-group"}>
              {groups.length > 1 || g.prod ? (
                <button className="group-head" aria-expanded={open} onClick={() => toggle(gid)} disabled={filtering}>
                  <Chevron open={open} />
                  <span className="group-label">{g.label}</span>
                  <span className="count">{g.hosts.length}</span>
                </button>
              ) : null}
              {open && (
                <ul className="side-list">
                  {g.hosts.map((h) => {
                    const addr = address(h);
                    const extra = extraTags(h.tags);
                    return (
                      <li
                        key={h.name}
                        className={g.prod ? "side-row prod" : "side-row"}
                        tabIndex={0}
                        title={hostTitle(h, addr)}
                        onDoubleClick={() => onOpenTerminal(h.name)}
                        onKeyDown={(e) => onRowKey(e, () => onOpenTerminal(h.name))}
                      >
                        <span className="row-main">
                          <span className="row-name">{h.name}</span>
                          {h.note && <NoteIcon text={h.note} />}
                        </span>
                        <span className="row-sub mono">
                          {addr}
                          {extra.length > 0 && <span className="row-tags"> · {extra.join(" ")}</span>}
                        </span>
                        <span className="row-actions">
                          <button className="icon-btn" onClick={() => onOpenTerminal(h.name)} title={`Terminal on ${h.name}`} aria-label={`Terminal on ${h.name}`}>
                            <TerminalIcon />
                          </button>
                          <button className="icon-btn" onClick={() => onOpenFiles(h.name)} title={`Files on ${h.name}`} aria-label={`Files on ${h.name}`}>
                            <FolderIcon />
                          </button>
                        </span>
                      </li>
                    );
                  })}
                </ul>
              )}
            </div>
          );
        })}
      </Section>

      <Section id="clusters" title="Clusters" count={filtering ? shownContexts.length : contexts.length} folded={folded} onToggle={toggle}>
        {contextsError && (
          <p className="side-empty error-text" title={contextsError}>
            Cannot read the kubeconfig: {contextsError}
          </p>
        )}
        {!contextsError && contexts.length === 0 && <p className="side-empty">No Kubernetes contexts in your kubeconfig.</p>}
        {contexts.length > 0 && shownContexts.length === 0 && <p className="side-empty">No cluster matches “{filter.trim()}”.</p>}
        <ul className="side-list">
          {shownContexts.map((c) => {
            const prod = contextIsProd(c);
            const badges = contextBadges(c).filter((b) => !b.text.startsWith("env="));
            return (
              <li
                key={c.name}
                className={prod ? "side-row clickable prod" : "side-row clickable"}
                tabIndex={0}
                title={[c.name, c.server, c.note && `Note: ${c.note}`, "Click or Enter: open"].filter(Boolean).join("\n")}
                onClick={() => onOpenCluster(c.name)}
                onKeyDown={(e) => onRowKey(e, () => onOpenCluster(c.name))}
              >
                <span className="row-main">
                  <span className="row-name">{c.name}</span>
                  {prod && <span className="chip prod">prod</span>}
                  {c.note && <NoteIcon text={c.note} />}
                  {c.current && <span className="current-dot" title="kubectl's current context" aria-label="current context" />}
                </span>
                <span className="row-sub">
                  {c.namespace}
                  {badges.length > 0 && (
                    <span className="row-tags">
                      {" · "}
                      {badges.map((b) => {
                        const [k, ...v] = b.text.split("=");
                        return <Tag key={b.text} k={k} v={v.join("=")} inherited={b.inherited} from={c.inherited_from?.join(", ")} />;
                      })}
                    </span>
                  )}
                </span>
              </li>
            );
          })}
        </ul>
      </Section>

      <Section id="agents" title="Agents" count={clients.filter((c) => !c.revoked).length} folded={folded} onToggle={toggle}>
        {clients.length === 0 && <p className="side-empty">No agent has connected yet. One appears here the first time it connects over MCP.</p>}
        <ul className="side-list">
          {clients.map((c) => (
            <li key={c.name} className={c.revoked ? "agent-row revoked" : "agent-row"}>
              <span className="agent-dot" aria-hidden="true" />
              <span className="row-name">{c.name}</span>
              <span className="agent-state">{c.revoked ? "revoked" : "allowed"}</span>
              {c.revoked ? (
                <button className="text-btn" onClick={() => onAllow(c.name)} title={`Allow ${c.name} again`}>
                  Allow
                </button>
              ) : (
                <button className="text-btn danger" onClick={() => onRevoke(c.name)} title={`Revoke ${c.name}: its calls are refused from now on`}>
                  Revoke
                </button>
              )}
            </li>
          ))}
        </ul>
      </Section>
    </aside>
  );
}
