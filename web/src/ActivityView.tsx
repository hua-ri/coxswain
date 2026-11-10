import { Fragment, useMemo, useState } from "react";
import { clientLabel, filterOptions, filterRows, type Filter, type Row } from "./activity";
import { activate } from "./keys";
import { Empty, Risk } from "./ui";
import { batchProgress, type BatchInfo, type HostOutput } from "./batch";

interface Props {
  rows: Row[];
  onOpenTerminal(id: string): void;
  /** loadOutput reads one host's output of a finished batch. */
  loadOutput(batch: BatchInfo, host: string): Promise<HostOutput>;
}

type Output = { loading: true } | { out: HostOutput } | { error: string };

function time(iso: string): string {
  const d = new Date(iso);
  return isNaN(d.getTime()) ? "" : d.toLocaleTimeString([], { hour12: false });
}

function result(r: Row): string {
  if (r.batch && r.status !== "refused") {
    const p = batchProgress(r.batch);
    return r.status === "failed" && r.error ? `${p} · ${r.error}` : p;
  }
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

export function ActivityView({ rows, onOpenTerminal, loadOutput }: Props) {
  const [filter, setFilter] = useState<Filter>({ client: "", host: "" });
  const [expanded, setExpanded] = useState<Set<number>>(new Set());
  const [outputs, setOutputs] = useState<Record<string, Output>>({});
  const toggle = (key: number) =>
    setExpanded((s) => {
      const n = new Set(s);
      if (n.has(key)) n.delete(key);
      else n.add(key);
      return n;
    });
  const showOutput = (b: BatchInfo, host: string) => {
    const k = `${b.id}/${host}`;
    if (outputs[k] && !("error" in outputs[k])) {
      // A second click hides it.
      return setOutputs((o) => Object.fromEntries(Object.entries(o).filter(([key]) => key !== k)));
    }
    setOutputs((o) => ({ ...o, [k]: { loading: true } }));
    loadOutput(b, host).then(
      (out) => setOutputs((o) => ({ ...o, [k]: { out } })),
      (e) => setOutputs((o) => ({ ...o, [k]: { error: e instanceof Error ? e.message : String(e) } })),
    );
  };
  const options = useMemo(() => filterOptions(rows), [rows]);
  const shown = useMemo(() => filterRows(rows, filter), [rows, filter]);
  const rowClass = (r: Row) =>
    [r.terminal || r.batch ? "clickable" : "", r.agent ? "agent" : "", r.risk === "destructive" ? "destructive" : ""].filter(Boolean).join(" ") || undefined;
  return (
    <div className="activity">
      <div className="toolbar">
        <select aria-label="Client" value={filter.client} onChange={(e) => setFilter({ ...filter, client: e.target.value })}>
          <option value="">All clients</option>
          {options.clients.map((c) => (
            <option key={c} value={c}>
              {clientLabel(c) === c ? c : `you (${c})`}
            </option>
          ))}
        </select>
        <select aria-label="Host" value={filter.host} onChange={(e) => setFilter({ ...filter, host: e.target.value })}>
          <option value="">All hosts</option>
          {options.hosts.map((h) => (
            <option key={h}>{h}</option>
          ))}
        </select>
        <span className="meta">
          {shown.length} event{shown.length === 1 ? "" : "s"} since this window opened
        </span>
      </div>
      <div className="table-wrap">
        <table className="events">
          <colgroup>
            <col className="c-time" />
            <col className="c-client" />
            <col className="c-action" />
            <col className="c-host" />
            <col className="c-risk" />
            <col className="c-detail" />
            <col className="c-result" />
          </colgroup>
          <thead>
            <tr>
              <th>Time</th>
              <th>Who</th>
              <th>Action</th>
              <th>Where</th>
              <th>Risk</th>
              <th>Details</th>
              <th>Result</th>
            </tr>
          </thead>
          <tbody>
            {shown.map((r) => {
              const open = r.terminal ? () => onOpenTerminal(r.terminal!) : r.batch ? () => toggle(r.key) : undefined;
              const failed = r.status === "failed" || r.status === "refused";
              return (
              <Fragment key={r.key}>
              <tr
                className={rowClass(r)}
                onClick={open}
                onKeyDown={open && activate(open)}
                tabIndex={open ? 0 : undefined}
                aria-expanded={r.batch ? expanded.has(r.key) : undefined}
                title={r.terminal ? "Show this terminal" : r.batch ? "Show each host" : undefined}
              >
                <td className="mono time">{time(r.time)}</td>
                <td className="cut" title={r.agent ? `agent ${r.client}` : r.client}>
                  <span className={r.agent ? "who agent" : "who"}>{r.who}</span>
                </td>
                <td className="mono cut" title={r.action}>
                  {r.batch && <span className="disclosure" aria-hidden="true">{expanded.has(r.key) ? "▾" : "▸"}</span>}
                  {r.action}
                </td>
                <td className="cut" title={r.hosts.join(", ") + (r.prod ? " (env=prod)" : "")}>
                  <span className="target">
                    {r.prod && <span className="chip prod">prod</span>}
                    <span className="text">{r.hosts.join(", ")}</span>
                  </span>
                </td>
                <td>
                  <Risk risk={r.risk} />
                </td>
                <td className="mono cut" title={r.status === "info" ? undefined : r.detail}>
                  {r.status === "info" ? "" : (r.detail ?? "")}
                </td>
                <td className={`result ${failed ? "failed" : r.status === "running" ? "running" : ""}`}>
                  {r.status === "running" && !r.batch ? (
                    <span className="state">
                      <span className="pulse running" aria-hidden="true" />
                      running…
                    </span>
                  ) : (
                    <span className="clamp" title={result(r)}>{result(r)}</span>
                  )}
                </td>
              </tr>
              {r.batch && expanded.has(r.key) && (
                <tr className="expand">
                  <td colSpan={7}>
                    <BatchHosts batch={r.batch} outputs={outputs} onOutput={showOutput} />
                  </td>
                </tr>
              )}
              </Fragment>
              );
            })}
          </tbody>
        </table>
        {shown.length === 0 && (
          <Empty title={rows.length === 0 ? "Nothing has happened yet" : "No events match these filters"}>
            {rows.length === 0
              ? "Every action — yours and your agents' — appears here as it happens, newest first: who did it, where, and how risky it was."
              : "Choose All clients and All hosts to see everything."}
          </Empty>
        )}
      </div>
    </div>
  );
}

function hostResult(h: BatchInfo["hosts"][number]): string {
  switch (h.status) {
    case "pending":
      return "waiting…";
    case "skipped":
      return "";
    default: {
      const parts = [];
      if (h.exitCode !== undefined) parts.push(`exit ${h.exitCode}`);
      if (h.durationMs !== undefined) parts.push(`${h.durationMs} ms`);
      if (h.error) parts.push(h.error);
      return parts.join(" · ");
    }
  }
}

function BatchHosts({
  batch,
  outputs,
  onOutput,
}: {
  batch: BatchInfo;
  outputs: Record<string, Output>;
  onOutput(b: BatchInfo, host: string): void;
}) {
  return (
    <div className="batch">
      <div className="mono command">$ {batch.command}</div>
      {batch.stopped && <div className="error">stopped: {batch.stopped}</div>}
      <table>
        <tbody>
          {batch.hosts.map((h) => {
            const o = outputs[`${batch.id}/${h.host}`];
            const canRead = batch.finished && batch.artifactId && h.status !== "skipped";
            return (
              <Fragment key={h.host}>
                <tr>
                  <td className="host">{h.host}</td>
                  <td>
                    <span className={`chip host-${h.status}`}>{h.status}</span>
                  </td>
                  <td className={h.status === "failed" ? "error mono" : "muted mono"}>{hostResult(h)}</td>
                  <td className="row-actions">
                    {canRead && (
                      <button className="small" aria-expanded={!!o && !("error" in o)} onClick={() => onOutput(batch, h.host)}>
                        {o && !("error" in o) ? "Hide output" : "Output"}
                      </button>
                    )}
                  </td>
                </tr>
                {o && (
                  <tr>
                    <td colSpan={4}>
                      {"loading" in o && <span className="muted">Loading…</span>}
                      {"error" in o && <span className="error">{o.error}</span>}
                      {"out" in o && (
                        <>
                          {!o.out.stdout && !o.out.stderr && <span className="muted">no output</span>}
                          {o.out.stdout && <pre className="output">{o.out.stdout}</pre>}
                          {o.out.stderr && <pre className="output stderr">{o.out.stderr}</pre>}
                          {o.out.truncated && (
                            <div className="muted">
                              Showing the first 500 lines; all of it is in artifact {batch.artifactId}.
                            </div>
                          )}
                        </>
                      )}
                    </td>
                  </tr>
                )}
              </Fragment>
            );
          })}
        </tbody>
      </table>
      {batch.finished && !batch.artifactId && <div className="muted">No host printed anything.</div>}
    </div>
  );
}
