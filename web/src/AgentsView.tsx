import type { Row } from "./activity";
import { footprintText, lastAction, type Footprint } from "./nav";
import { RecentRows } from "./HostView";
import { Icon } from "./icons";
import type { ClientInfo } from "./types";
import { Empty, PaneHeader } from "./ui";

interface Props {
  active: boolean;
  clients: ClientInfo[];
  rows: Row[];
  footprintOf(name: string): Footprint;
  typing(name: string): boolean;
  onAllow(name: string): void;
  onRevoke(name: string): void; // asks first
  onSelect(id: string): void;
}

function ago(iso?: string): string {
  if (!iso) return "";
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (isNaN(s)) return "";
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ${Math.round((s % 3600) / 60)}m ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

/** AgentsView is the crew: who may act, what each did last and what it holds open. */
export function AgentsView({ active, clients, rows, footprintOf, typing, onAllow, onRevoke, onSelect }: Props) {
  const allowed = clients.filter((c) => !c.revoked).length;
  const recent = rows.filter((r) => r.agent).slice(0, 12);
  return (
    <div className="pane agents-page" style={{ display: active ? "flex" : "none" }}>
      <PaneHeader icon={<Icon name="@agents" />} title="Agents"
        meta={clients.length ? `${allowed} allowed${clients.length > allowed ? ` · ${clients.length - allowed} revoked` : ""}` : undefined} />
      <div className="agents-body">
        {clients.length === 0 ? (
          <Empty title="No agent has connected yet">An agent appears here the first time it connects over MCP. You can revoke it here at any time.</Empty>
        ) : (
          <table className="agents">
            <thead>
              <tr>
                <th>Agent</th>
                <th>Access</th>
                <th>Last action</th>
                <th>Holds open</th>
                <th><span className="visually-hidden">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              {clients.map((c) => {
                const last = lastAction(rows, c.name);
                const held = footprintText(footprintOf(c.name));
                return (
                  <tr key={c.name} className={c.revoked ? "revoked" : "agent"}>
                    <td className="cut">
                      <span className={c.revoked ? "agent-name off" : "agent-name"}>{c.name}</span>
                      {typing(c.name) && <span className="pulse" title={`${c.name} is typing`} />}
                    </td>
                    <td>
                      {c.revoked ? (
                        <span className="chip" title={c.revoked_at}>revoked {ago(c.revoked_at)}</span>
                      ) : (
                        <span className="chip ok" title={c.authorized_at && `allowed ${c.authorized_at}`}>allowed</span>
                      )}
                    </td>
                    <td className="mono cut" title={last ? `${last.action} ${last.hosts.join(", ")}` : undefined}>
                      {last ? <>{last.action} <span className="muted">{last.hosts.join(", ")}</span></> : <span className="muted">none since this window opened</span>}
                    </td>
                    <td className="cut">{held || <span className="muted">nothing</span>}</td>
                    <td className="actions">
                      {c.revoked ? (
                        <button className="small" onClick={() => onAllow(c.name)}>Allow</button>
                      ) : (
                        <button className="small danger-outline" onClick={() => onRevoke(c.name)}>Revoke…</button>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
        {recent.length > 0 && (
          <section className="host-section" aria-label="Recent actions by agents">
            <h2 className="caption">Recent actions by agents</h2>
            <RecentRows rows={recent} onSelect={onSelect} where />
          </section>
        )}
      </div>
    </div>
  );
}
