import { address, type ClientInfo, type Host } from "./types";

interface Props {
  hosts: Host[];
  clients: ClientInfo[];
  onOpenTerminal(host: string): void;
  onOpenFiles(host: string): void;
  onAllow(name: string): void;
  onRevoke(name: string): void;
}

export function Sidebar({ hosts, clients, onOpenTerminal, onOpenFiles, onAllow, onRevoke }: Props) {
  return (
    <aside className="sidebar">
      <h3>Hosts</h3>
      {hosts.length === 0 && <p className="muted">No hosts yet. Run cox init or cox ssh hosts import.</p>}
      <ul className="hosts">
        {hosts.map((h) => (
          <li key={h.name} title={h.note || undefined}>
            <div className="host-head">
              <span className="host-name" title={h.name}>
                {h.name}
              </span>
              <span className="host-buttons">
                <button className="small" onClick={() => onOpenFiles(h.name)} title="Browse files">
                  Files
                </button>
                <button className="small" onClick={() => onOpenTerminal(h.name)} title="Open a terminal">
                  Terminal
                </button>
              </span>
            </div>
            <div className="muted host-address" title={address(h)}>
              {address(h)}
            </div>
            <div className="tags">
              {Object.entries(h.tags ?? {}).map(([k, v]) => (
                <span key={k} className={k === "env" && v === "prod" ? "badge danger" : "badge"}>
                  {k}={v}
                </span>
              ))}
              {h.note && <span className="badge note">note</span>}
            </div>
          </li>
        ))}
      </ul>
      <h3>Agents</h3>
      {clients.length === 0 && <p className="muted">No agent has asked yet.</p>}
      <ul className="clients">
        {clients.map((c) => (
          <li key={c.name}>
            <span className={c.revoked ? "revoked" : ""}>{c.name}</span>
            <span className="muted">{c.revoked ? "revoked" : "allowed"}</span>
            {c.revoked ? (
              <button className="small" onClick={() => onAllow(c.name)}>Allow</button>
            ) : (
              <button className="small" onClick={() => onRevoke(c.name)}>Revoke</button>
            )}
          </li>
        ))}
      </ul>
    </aside>
  );
}
