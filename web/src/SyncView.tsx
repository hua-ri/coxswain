import { useEffect, useState } from "react";
import type { Cox } from "./cox";
import { Icon } from "./icons";
import { conflictHosts, hasWork, shown, syncArgs, type FieldChange, type ImportOutput } from "./sync";
import { Message, PaneHeader } from "./ui";

interface Props {
  cox: Cox;
  onDone(result: ImportOutput): void;
  onCancel(): void;
}

function Changes({ changes }: { changes: FieldChange[] }) {
  return (
    <ul className="sync-changes">
      {changes.map((c) => (
        <li key={c.field}>
          <span className="sync-field">{c.field}</span>
          <span className="mono old">{shown(c.from)}</span>
          <span className="arrow" aria-label="becomes">→</span>
          <span className="mono">{shown(c.to)}</span>
        </li>
      ))}
    </ul>
  );
}

/** SyncView previews syncing hosts from ~/.ssh/config, then syncs. Hosts edited in Coxswain are kept unless ticked. */
export function SyncView({ cox, onDone, onCancel }: Props) {
  const [plan, setPlan] = useState<ImportOutput | null>(null);
  const [error, setError] = useState("");
  const [overwrite, setOverwrite] = useState<string[]>([]);
  const [showUnchanged, setShowUnchanged] = useState(false);
  const [syncing, setSyncing] = useState(false);

  useEffect(() => {
    let stale = false;
    cox
      .invoke<ImportOutput>("ssh.hosts.import", { dry_run: true })
      .then((out) => {
        if (stale) return;
        if (out.status !== "ok" || !out.result) setError([out.error, out.hint].filter(Boolean).join(" — "));
        else setPlan(out.result);
      })
      .catch((e) => !stale && setError(e instanceof Error ? e.message : String(e)));
    return () => {
      stale = true;
    };
  }, [cox]);

  const sync = async () => {
    setSyncing(true);
    setError("");
    try {
      const out = await cox.invoke<ImportOutput>("ssh.hosts.import", syncArgs(overwrite));
      if (out.status !== "ok" || !out.result) throw new Error([out.error, out.hint].filter(Boolean).join(" — "));
      onDone(out.result);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setSyncing(false);
    }
  };

  const conflicts = plan ? conflictHosts(plan.conflicts) : [];
  const work = plan ? hasWork(plan, overwrite) : false;
  const toggle = (name: string) => setOverwrite((o) => (o.includes(name) ? o.filter((x) => x !== name) : [...o, name]));

  return (
    <div className="pane editor" style={{ display: "flex" }}>
      <PaneHeader icon={<Icon name="host" />} title="Sync from ~/.ssh/config" meta={plan ? <span className="mono">{plan.path}</span> : undefined} />
      <div className="editor-form">
        <div className="editor-body">
          {error && <Message kind="error" onDismiss={() => setError("")}>{error}</Message>}
          <p className="editor-intro">
            Adds the hosts in your ssh config and refreshes the address, port, user, keys and jump host of the ones Coxswain has.
            Your ssh config is only read. Coxswain’s tags and notes stay as they are.
          </p>
          {!plan && !error && <p className="sync-loading muted">Reading ~/.ssh/config…</p>}
          {plan && (
            <>
              {plan.added.length > 0 && (
                <section className="form-section" aria-label="New">
                  <h2 className="caption">New <span className="n">{plan.added.length}</span></h2>
                  <ul className="sync-list">
                    {plan.added.map((h) => (
                      <li key={h.name}><span className="sync-name">{h.name}</span><span className="mono muted">{h.address}</span></li>
                    ))}
                  </ul>
                </section>
              )}
              {plan.updated.length > 0 && (
                <section className="form-section" aria-label="Will update">
                  <h2 className="caption">Will update <span className="n">{plan.updated.length}</span></h2>
                  <ul className="sync-list">
                    {plan.updated.map((h) => (
                      <li key={h.name}><span className="sync-name">{h.name}</span><Changes changes={h.changes} /></li>
                    ))}
                  </ul>
                </section>
              )}
              {conflicts.length > 0 && (
                <section className="form-section" aria-label="Edited in Coxswain">
                  <h2 className="caption">Edited in Coxswain <span className="n">{conflicts.length}</span></h2>
                  <p className="field-hint sync-hint">You changed these here (or added them by hand). They stay as they are unless you tick them.</p>
                  <ul className="sync-list">
                    {conflicts.map((h) => (
                      <li key={h.name}>
                        <span className="sync-name">{h.name}</span>
                        <div className="sync-conflict">
                          <Changes changes={h.changes} />
                          <label className="check">
                            <input type="checkbox" checked={overwrite.includes(h.name)} onChange={() => toggle(h.name)} />
                            Overwrite with ~/.ssh/config
                          </label>
                        </div>
                      </li>
                    ))}
                  </ul>
                </section>
              )}
              <section className="form-section" aria-label="Unchanged">
                {plan.unchanged.length === 0 ? (
                  <p className="muted">No other hosts in the config.</p>
                ) : (
                  <>
                    <button type="button" className="link disclose" aria-expanded={showUnchanged} onClick={() => setShowUnchanged((x) => !x)}>
                      {showUnchanged ? "Hide" : "Show"} {plan.unchanged.length} unchanged
                    </button>
                    {showUnchanged && <p className="sync-unchanged">{plan.unchanged.join(", ")}</p>}
                  </>
                )}
              </section>
            </>
          )}
        </div>
        <footer className="editor-foot">
          {plan && !work && <span className="state-line"><span className="state-dot" aria-hidden="true" />Already up to date</span>}
          <span className="spacer" />
          <button type="button" onClick={onCancel}>Cancel</button>
          <button type="button" className="primary" disabled={!plan || !work || syncing} onClick={sync}>{syncing ? "Syncing…" : "Sync"}</button>
        </footer>
      </div>
    </div>
  );
}
