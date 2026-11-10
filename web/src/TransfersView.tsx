import { formatBytes, formatSpeed, formatTime } from "./format";
import { progress, type TransferItem } from "./transfers";
import { Empty, Owner } from "./ui";

interface Props {
  transfers: TransferItem[];
  onCancel(t: TransferItem): void;
}

function statusText(t: TransferItem): string {
  switch (t.status) {
    case "running":
      return [`${formatBytes(t.bytes)} of ${formatBytes(t.total)}`, formatSpeed(t.speed)].filter(Boolean).join(" · ");
    case "done": {
      return [formatBytes(t.bytes), formatTime(t.finished_at), t.verified === true ? "checksum verified" : ""].filter(Boolean).join(" · ");
    }
    default:
      return [formatTime(t.finished_at), t.error].filter(Boolean).join(" · ");
  }
}

/** TransfersView lists uploads and downloads, running ones first. */
export function TransfersView({ transfers, onCancel }: Props) {
  const running = transfers.filter((t) => t.status === "running");
  const finished = transfers.filter((t) => t.status !== "running");
  if (transfers.length === 0) {
    return (
      <Empty title="No transfers yet">
        Upload or download from a host's Files tab. Transfers agents start show up here too, and you can cancel them.
      </Empty>
    );
  }
  return (
    <div className="panel">
      {[running, finished].map((list, i) =>
        list.length === 0 ? null : (
          <section key={i} aria-label={i === 0 ? "Running" : "Finished"}>
            <h4 className="group-head">
              {i === 0 ? "Running" : "Finished"}
              <span className="count">{list.length}</span>
            </h4>
            <ul className="transfers">
              {list.map((t) => (
                <li key={t.id} className={t.owner.kind === "agent" ? "transfer agent" : "transfer"}>
                  <span className="dir" title={t.direction} aria-label={t.direction}>
                    {t.direction === "upload" ? "↑" : "↓"}
                  </span>
                  <div className="paths">
                    <span className="mono path" title={`${t.host}:${t.remote_path}`}>
                      <span>{t.host}:{t.remote_path}</span>
                    </span>
                    <span className="arrow">{t.direction === "upload" ? "←" : "→"}</span>
                    <span className="mono path" title={t.local_path}>
                      <span>{t.local_path}</span>
                    </span>
                  </div>
                  <div className="side">
                    <Owner owner={t.owner} />
                    {t.status === "running" && (
                      <button className="small" onClick={() => onCancel(t)}>
                        Cancel
                      </button>
                    )}
                  </div>
                  <div className={t.status === "failed" ? "status failed" : "status"}>
                    {t.status === "running" && (
                      <div className="bar" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(progress(t) * 100)}>
                        <div style={{ transform: `scaleX(${progress(t).toFixed(3)})` }} />
                      </div>
                    )}
                    {t.status === "done" && <span className="chip ok">done</span>}
                    {t.status === "failed" && <span className="chip bad">failed</span>}
                    {t.status === "cancelled" && <span className="chip">cancelled</span>}
                    <span className="mono">{statusText(t)}</span>
                  </div>
                </li>
              ))}
            </ul>
          </section>
        ),
      )}
    </div>
  );
}
