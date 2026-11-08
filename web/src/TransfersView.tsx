import { formatBytes, formatSpeed, formatTime, ownerLabel } from "./format";
import { progress, type TransferItem } from "./transfers";

interface Props {
  transfers: TransferItem[];
  onCancel(t: TransferItem): void;
}

function statusText(t: TransferItem): string {
  switch (t.status) {
    case "running":
      return [`${formatBytes(t.bytes)} of ${formatBytes(t.total)}`, formatSpeed(t.speed)].filter(Boolean).join(" · ");
    case "done": {
      const check = t.verified === true ? " · checksum verified" : "";
      return `${formatBytes(t.bytes)} · done ${formatTime(t.finished_at)}${check}`;
    }
    default:
      return `${t.status}${t.error ? `: ${t.error}` : ""}`;
  }
}

/** TransfersView lists uploads and downloads, running ones first. */
export function TransfersView({ transfers, onCancel }: Props) {
  const running = transfers.filter((t) => t.status === "running");
  const finished = transfers.filter((t) => t.status !== "running");
  return (
    <div className="panel">
      {transfers.length === 0 && <p className="muted">No transfers yet. Upload or download from a host's Files tab.</p>}
      {[running, finished].map((list, i) =>
        list.length === 0 ? null : (
          <section key={i}>
            <h4>{i === 0 ? "Running" : "Finished"}</h4>
            <ul className="transfers">
              {list.map((t) => (
                <li key={t.id}>
                  <div className="transfer-head">
                    <span className="badge">{t.direction}</span>
                    <span className="mono path" title={t.remote_path}>
                      {t.host}:{t.remote_path}
                    </span>
                    <span className="muted">{t.direction === "upload" ? "←" : "→"}</span>
                    <span className="mono path" title={t.local_path}>
                      {t.local_path}
                    </span>
                    <span className={t.owner.kind === "agent" ? "badge agent" : "badge"}>{ownerLabel(t.owner)}</span>
                    {t.status === "running" && (
                      <button className="small" onClick={() => onCancel(t)}>
                        Cancel
                      </button>
                    )}
                  </div>
                  {t.status === "running" && (
                    <div className="bar">
                      <div style={{ width: `${(progress(t) * 100).toFixed(1)}%` }} />
                    </div>
                  )}
                  <div className={t.status === "failed" ? "error" : "muted"}>{statusText(t)}</div>
                </li>
              ))}
            </ul>
          </section>
        ),
      )}
    </div>
  );
}
