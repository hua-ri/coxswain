import type { ReactNode } from "react";
import { contextBadges, type ContextInfo } from "./k8s";
import { address, type ClientInfo, type Host } from "./types";
import { Tag, Tags } from "./ui";

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

function Section({ title, count, children }: { title: string; count: number; children: ReactNode }) {
  return (
    <section className="side-section" aria-label={title}>
      <h3 className="side-head">
        {title}
        {count > 0 && <span className="count">{count}</span>}
      </h3>
      {children}
    </section>
  );
}

export function Sidebar({ hosts, clients, contexts, contextsError, onOpenCluster, onOpenTerminal, onOpenFiles, onAllow, onRevoke }: Props) {
  return (
    <aside className="sidebar">
      <Section title="Hosts" count={hosts.length}>
        {hosts.length === 0 && <p className="side-empty">No hosts yet. Run cox init or cox ssh hosts import.</p>}
        <ul className="side-list">
          {hosts.map((h) => (
            <li key={h.name} className="side-item" title={h.note || undefined}>
              <div className="item-line">
                <span className="item-name" title={h.name}>
                  {h.name}
                </span>
                <span className="item-actions">
                  <button className="ghost small" onClick={() => onOpenFiles(h.name)} title={`Browse files on ${h.name}`}>
                    Files
                  </button>
                  <button className="ghost small" onClick={() => onOpenTerminal(h.name)} title={`Open a terminal on ${h.name}`}>
                    Terminal
                  </button>
                </span>
              </div>
              <span className="item-meta mono" title={address(h)}>
                {address(h)}
              </span>
              {(h.tags || h.note) && (
                <div className="item-tags">
                  <Tags tags={h.tags} />
                  {h.note && <span className="chip note">note</span>}
                </div>
              )}
            </li>
          ))}
        </ul>
      </Section>
      <Section title="Clusters" count={contexts.length}>
        {contextsError && (
          <p className="side-empty error-text" title={contextsError}>
            Cannot read the kubeconfig: {contextsError}
          </p>
        )}
        {!contextsError && contexts.length === 0 && <p className="side-empty">No Kubernetes contexts in your kubeconfig.</p>}
        <ul className="side-list">
          {contexts.map((c) => {
            const badges = contextBadges(c);
            return (
              <li key={c.name} className="side-item" title={c.note || undefined}>
                <div className="item-line">
                  <span className="item-name" title={c.name}>
                    {c.name}
                  </span>
                  {c.current && <span className="chip info">current</span>}
                  <span className="item-actions">
                    <button className="ghost small" onClick={() => onOpenCluster(c.name)} title={`Browse ${c.name}`}>
                      Open
                    </button>
                  </span>
                </div>
                <span className="item-meta" title={c.server}>
                  {c.cluster} · {c.namespace}
                </span>
                {(badges.length > 0 || c.note) && (
                  <div className="item-tags">
                    {badges.map((b) => {
                      const [k, ...v] = b.text.split("=");
                      return <Tag key={b.text} k={k} v={v.join("=")} inherited={b.inherited} from={c.inherited_from?.join(", ")} />;
                    })}
                    {c.note && <span className="chip note">note</span>}
                  </div>
                )}
              </li>
            );
          })}
        </ul>
      </Section>
      <Section title="Agents" count={clients.filter((c) => !c.revoked).length}>
        {clients.length === 0 && <p className="side-empty">No agent has asked yet. One appears here the first time it connects over MCP.</p>}
        <ul className="side-list">
          {clients.map((c) => (
            <li key={c.name} className={c.revoked ? "agent-item revoked" : "agent-item"}>
              <span className="item-name">{c.name}</span>
              <span className="state">{c.revoked ? "revoked" : "allowed"}</span>
              {c.revoked ? (
                <button className="ghost small" onClick={() => onAllow(c.name)} title={`Allow ${c.name} again`}>
                  Allow
                </button>
              ) : (
                <button className="ghost small danger-text" onClick={() => onRevoke(c.name)} title={`Revoke ${c.name}: its calls are refused from now on`}>
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
