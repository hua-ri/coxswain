import { useMemo, useState } from "react";
import { clientLabel, filterOptions, filterRows, type Filter, type Row } from "./activity";

interface Props {
  rows: Row[];
  onOpenTerminal(id: string): void;
}

function time(iso: string): string {
  const d = new Date(iso);
  return isNaN(d.getTime()) ? "" : d.toLocaleTimeString([], { hour12: false });
}

function result(r: Row): string {
  switch (r.status) {
    case "running":
      return "running…";
    case "ok":
      return `${r.durationMs} ms`;
    case "failed":
      return `failed: ${r.error ?? ""}`;
    case "refused":
      return `refused: ${r.error ?? ""}`;
    default:
      return r.detail ?? "";
  }
}

export function ActivityView({ rows, onOpenTerminal }: Props) {
  const [filter, setFilter] = useState<Filter>({ client: "", host: "" });
  const options = useMemo(() => filterOptions(rows), [rows]);
  const shown = useMemo(() => filterRows(rows, filter), [rows, filter]);
  return (
    <div className="activity">
      <div className="filters">
        <select value={filter.client} onChange={(e) => setFilter({ ...filter, client: e.target.value })}>
          <option value="">All clients</option>
          {options.clients.map((c) => (
            <option key={c} value={c}>
              {clientLabel(c) === c ? c : `you (${c})`}
            </option>
          ))}
        </select>
        <select value={filter.host} onChange={(e) => setFilter({ ...filter, host: e.target.value })}>
          <option value="">All hosts</option>
          {options.hosts.map((h) => (
            <option key={h}>{h}</option>
          ))}
        </select>
        <span className="muted">{shown.length} events since this window opened</span>
      </div>
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Time</th>
              <th>Client</th>
              <th>Action</th>
              <th>Host</th>
              <th>Risk</th>
              <th>Details</th>
              <th>Result</th>
            </tr>
          </thead>
          <tbody>
            {shown.map((r) => (
              <tr
                key={r.key}
                className={r.terminal ? "clickable" : undefined}
                onClick={r.terminal ? () => onOpenTerminal(r.terminal!) : undefined}
                title={r.terminal ? "Show this terminal" : undefined}
              >
                <td className="mono">{time(r.time)}</td>
                <td title={r.client}>
                  {r.who}
                  {r.agent && <span className="badge agent">agent</span>}
                </td>
                <td className="mono">{r.action}</td>
                <td>{r.hosts.join(", ")}</td>
                <td>{r.risk && <span className={`badge risk-${r.risk}`}>{r.risk}</span>}</td>
                <td className="mono detail">{r.status === "info" ? "" : (r.detail ?? "")}</td>
                <td className={r.status === "failed" || r.status === "refused" ? "error" : "muted"}>{result(r)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
