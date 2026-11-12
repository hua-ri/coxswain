import type { Row } from "./activity";
import { Icon } from "./icons";
import type { SideSession } from "./Sidebar";
import { tunnelRoute, type Tunnel } from "./tunnels";
import { address, type Host } from "./types";
import { activate } from "./keys";
import { HazardBand, Owner, PaneHeader, Risk, Tags } from "./ui";

interface Props {
  host: Host;
  prod: boolean;
  active: boolean;
  sessions: SideSession[]; // open here
  tunnels: Tunnel[]; // through this host
  rows: Row[]; // recent activity here, newest first
  onTerminal(): void;
  onFiles(): void;
  onSelect(id: string): void;
  onTunnels(): void;
}

function time(iso: string): string {
  const d = new Date(iso);
  return isNaN(d.getTime()) ? "" : d.toLocaleTimeString([], { hour12: false });
}

function outcome(r: Row): string {
  switch (r.status) {
    case "running":
      return "running…";
    case "ok":
      return r.durationMs !== undefined ? `${r.durationMs} ms` : "";
    case "failed":
      return `failed: ${r.error ?? ""}`;
    case "refused":
      return `refused: ${r.error ?? ""}`;
    default:
      return r.detail ?? "";
  }
}

/** HostView is a host's page: where it is, what is open there, and what happened there. */
export function HostView({ host, prod, active, sessions, tunnels, rows, onTerminal, onFiles, onSelect, onTunnels }: Props) {
  return (
    <div className="pane host-page" style={{ display: active ? "flex" : "none" }}>
      {prod && <HazardBand env="env=prod" where={host.name} note={host.note} />}
      <PaneHeader icon={<Icon name="host" />} title={host.name} meta={<span className="mono">{address(host)}</span>}>
        <button className="small primary" onClick={onTerminal} title="Open a terminal (or double-click the host)">
          <Icon name="terminal" /> Terminal
        </button>
        <button className="small" onClick={onFiles}>
          <Icon name="files" /> Files
        </button>
      </PaneHeader>
      <div className="host-body">
        <dl className="facts">
          <dt>Address</dt>
          <dd className="mono">{address(host)}{!host.port || host.port === 22 ? ":22" : ""}</dd>
          {host.proxy_jump && (
            <>
              <dt>Jump host</dt>
              <dd className="mono">{host.proxy_jump}</dd>
            </>
          )}
          <dt>Tags</dt>
          <dd className="chips">{Object.keys(host.tags ?? {}).length ? <Tags tags={host.tags} /> : <span className="muted">none</span>}</dd>
          {host.note && !prod && (
            <>
              <dt>Note</dt>
              <dd>{host.note}</dd>
            </>
          )}
        </dl>

        <section className="host-section" aria-label="Open here">
          <h2 className="caption">Open here</h2>
          {sessions.length === 0 && tunnels.length === 0 && <p className="muted">Nothing open on {host.name}. Terminal and Files open here.</p>}
          <ul className="plain-list">
            {sessions.map((s) => (
              <li key={s.id} className={s.owner.kind === "agent" ? "agent" : undefined}>
                <button className="link-row" onClick={() => onSelect(s.id)}>
                  <Icon name={s.kind} />
                  <span>{s.kind === "files" ? "Files" : "Terminal"}</span>
                  <Owner owner={s.owner} />
                  {s.closed && <span className="muted">ended</span>}
                </button>
              </li>
            ))}
            {tunnels.map((t) => (
              <li key={t.id} className={t.owner.kind === "agent" ? "agent" : undefined}>
                <button className="link-row" onClick={onTunnels}>
                  <Icon name="@tunnels" />
                  <span className="mono">{tunnelRoute(t)}</span>
                  <Owner owner={t.owner} />
                </button>
              </li>
            ))}
          </ul>
        </section>

        <section className="host-section" aria-label={`Recent activity on ${host.name}`}>
          <h2 className="caption">Recent activity</h2>
          {rows.length === 0 ? (
            <p className="muted">Nothing has happened on {host.name} since this window opened.</p>
          ) : (
            <RecentRows rows={rows} onSelect={onSelect} />
          )}
        </section>
      </div>
    </div>
  );
}

/** RecentRows is a compact activity table; rows with a terminal jump to it. */
export function RecentRows({ rows, onSelect, where }: { rows: Row[]; onSelect(id: string): void; where?: boolean }) {
  return (
    <table className="events compact">
      <colgroup>
        <col className="c-time" />
        <col className="c-client" />
        <col className="c-action" />
        {where && <col className="c-host" />}
        <col className="c-risk" />
        <col />
        <col className="c-result" />
      </colgroup>
      <tbody>
        {rows.map((r) => {
          const failed = r.status === "failed" || r.status === "refused";
          const open = r.terminal ? () => onSelect(r.terminal!) : undefined;
          return (
            <tr key={r.key} className={[r.agent ? "agent" : "", open ? "clickable" : ""].filter(Boolean).join(" ") || undefined}
              onClick={open} onKeyDown={open && activate(open)} tabIndex={open ? 0 : undefined}>
              <td className="mono time">{time(r.time)}</td>
              <td className="cut who-cell"><span className={r.agent ? "who agent" : "who"}>{r.who}</span></td>
              <td className="mono cut" title={r.action}>{r.action}</td>
              {where && (
                <td className="cut" title={r.hosts.join(", ")}>
                  {r.prod && <span className="chip prod">prod</span>} {r.hosts.join(", ")}
                </td>
              )}
              <td className="risk-cell"><Risk risk={r.risk} /></td>
              <td className="mono cut" title={r.detail}>{r.detail}</td>
              <td className={failed ? "cut result failed" : "cut result"} title={outcome(r)}>{outcome(r)}</td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}
