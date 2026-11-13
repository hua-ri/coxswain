import { useEffect, useId, useState, type FormEvent } from "react";
import { addClusterArgs, blankCluster, clusterErrors, pickContext, sourceArgs, testClusterArgs, type Candidate, type ClusterForm } from "./clusterform";
import type { Cox } from "./cox";
import { ENVS } from "./hostform";
import { Icon } from "./icons";
import type { ContextInfo } from "./k8s";
import { EnvPicker, Field, HazardBand, Message, PaneHeader } from "./ui";

interface Props {
  cox: Cox;
  contexts: ContextInfo[];
  kubeconfig: string[]; // the user's files, for the read-only notice
  onSaved(name: string): void;
  onCancel(): void;
}

type Probe = { step: "idle" } | { step: "testing" } | { step: "ok"; version: string; ms: number } | { step: "failed"; error: string };

const AUTH: Record<string, string> = {
  token: "token", cert: "client certificate", exec: "credential plugin", oidc: "OIDC id-token", basic: "user name and password", none: "no credentials",
};

function message(out: { error?: string; hint?: string }): string {
  return [out.error, out.hint].filter(Boolean).join(" — ") || "failed";
}

/** ClusterEditor adds a cluster from a kubeconfig the user pastes or picks. It is stored in Coxswain's own directory. */
export function ClusterEditor({ cox, contexts, kubeconfig, onSaved, onCancel }: Props) {
  const [f, setF] = useState<ClusterForm>(blankCluster);
  const [cands, setCands] = useState<Candidate[]>([]);
  const [readError, setReadError] = useState("");
  const [tried, setTried] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [probe, setProbe] = useState<Probe>({ step: "idle" });
  const id = useId();
  const set = (patch: Partial<ClusterForm>) => setF((x) => ({ ...x, ...patch }));
  const src = sourceArgs(f);
  const srcKey = JSON.stringify(src);

  // Read the kubeconfig's contexts whenever it changes (pasted text: after a pause).
  useEffect(() => {
    setProbe({ step: "idle" });
    if (!src) {
      setCands([]);
      setReadError("");
      return;
    }
    let stale = false;
    const timer = setTimeout(async () => {
      try {
        const out = await cox.invoke<{ contexts: Candidate[] }>("k8s.clusters.inspect", src);
        if (stale) return;
        if (out.status !== "ok") {
          setCands([]);
          setReadError(message(out));
          return;
        }
        const list = out.result?.contexts ?? [];
        setCands(list);
        setReadError("");
        setF((x) => {
          const keep = list.some((c) => c.name === x.context) ? x.context : pickContext(list);
          return { ...x, context: keep, name: keep === x.context && x.name ? x.name : keep };
        });
      } catch (e) {
        if (!stale) setReadError(e instanceof Error ? e.message : String(e));
      }
    }, f.source === "paste" ? 350 : 0);
    return () => {
      stale = true;
      clearTimeout(timer);
    };
  }, [srcKey]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => setProbe({ step: "idle" }), [f.context]);

  const errors = clusterErrors(f, cands, contexts);
  const shown = (k: keyof typeof errors) => (tried ? errors[k] : undefined);
  const cand = cands.find((c) => c.name === f.context);
  const prod = f.env === "prod";

  const pick = async () => {
    try {
      const path = await cox.pickFile("kubeconfig");
      if (path) set({ path, source: "file" });
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };
  const test = async () => {
    setTried(true);
    if (errors.source || errors.context) return;
    setProbe({ step: "testing" });
    try {
      const out = await cox.invoke<{ version: string; duration_ms: number }>("k8s.clusters.test", testClusterArgs(f));
      setProbe(out.status === "ok" && out.result ? { step: "ok", version: out.result.version, ms: out.result.duration_ms } : { step: "failed", error: message(out) });
    } catch (e) {
      setProbe({ step: "failed", error: e instanceof Error ? e.message : String(e) });
    }
  };
  const save = async (e: FormEvent) => {
    e.preventDefault();
    setTried(true);
    if (Object.keys(errors).length) return;
    setSaving(true);
    setError("");
    try {
      const out = await cox.invoke<ContextInfo>("k8s.clusters.add", addClusterArgs(f));
      if (out.status !== "ok") throw new Error(message(out));
      onSaved(out.result?.name ?? f.name.trim());
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="pane editor" style={{ display: "flex" }}>
      {prod && <HazardBand env="env=prod" where={f.name.trim() || "new cluster"} note={f.note.trim() || undefined} />}
      <PaneHeader icon={<Icon name="cluster" />} title="Add cluster" />
      <form className="editor-form" onSubmit={save} noValidate aria-label="Add cluster">
        <div className="editor-body">
          {error && <Message kind="error" onDismiss={() => setError("")}>{error}</Message>}
          <p className="editor-intro">
            Coxswain keeps the cluster in <code>~/.coxswain/kubeconfigs</code>. Your kubeconfig
            {kubeconfig.length ? <> (<code>{kubeconfig.join(", ")}</code>)</> : null} is only read, never changed, and kubectl’s current context stays as it is.
          </p>
          <section className="form-section" aria-labelledby={id + "src"}>
            <h2 className="caption" id={id + "src"}>Kubeconfig</h2>
            <Field label="From" error={shown("source")}>
              <div className="segmented source-picker" role="radiogroup" aria-label="Kubeconfig source">
                <button type="button" role="radio" aria-checked={f.source === "paste"} aria-selected={f.source === "paste"} onClick={() => set({ source: "paste" })}>Paste YAML</button>
                <button type="button" role="radio" aria-checked={f.source === "file"} aria-selected={f.source === "file"} onClick={() => (f.path ? set({ source: "file" }) : void pick())}>File…</button>
              </div>
            </Field>
            {f.source === "paste" ? (
              <Field label="" htmlFor={id + "yaml"} hint="Only the context you choose, with its cluster and credentials, is kept.">
                <textarea id={id + "yaml"} className="mono yaml-input" rows={8} spellCheck={false} value={f.yaml} autoFocus
                  placeholder={"apiVersion: v1\nkind: Config\nclusters: …\nusers: …\ncontexts: …"} onChange={(e) => set({ yaml: e.target.value })} />
              </Field>
            ) : (
              <Field label="" hint="The file is only read.">
                <div className="inline-fields">
                  <span className="mono cut path-text" title={f.path}>{f.path}</span>
                  <button type="button" className="small" onClick={pick}>Choose another…</button>
                </div>
              </Field>
            )}
            {readError && <Field label=""><div className="field-error" role="alert">{readError}</div></Field>}
            {cands.length > 0 && (
              <>
                <Field label="Context" htmlFor={id + "ctx"} error={shown("context") ?? (cand?.problem || undefined)}
                  hint={cand ? <span className="mono">{[cand.server, cand.namespace && `namespace ${cand.namespace}`, AUTH[cand.auth] ?? cand.auth].filter(Boolean).join(" · ")}</span> : undefined}>
                  {cands.length === 1 ? (
                    <span className="mono static-value">{cands[0].name}</span>
                  ) : (
                    <select id={id + "ctx"} value={f.context} onChange={(e) => set({ context: e.target.value, name: e.target.value })}>
                      {!f.context && <option value="">Choose…</option>}
                      {cands.map((c) => (
                        <option key={c.name} value={c.name}>{c.name}{c.problem ? " (not supported)" : c.exists ? " (name taken)" : ""}</option>
                      ))}
                    </select>
                  )}
                </Field>
                <Field label="Name" htmlFor={id + "name"} error={shown("name") ?? (f.name.trim() && errors.name) ?? undefined}
                  hint={f.name.trim() !== f.context ? "The context gets this name in Coxswain’s copy only." : "What you and your agents call it."}>
                  <input id={id + "name"} className="mono" value={f.name} spellCheck={false} autoComplete="off" onChange={(e) => set({ name: e.target.value })} />
                </Field>
              </>
            )}
          </section>

          <section className="form-section" aria-labelledby={id + "labels"}>
            <h2 className="caption" id={id + "labels"}>What agents are told</h2>
            <Field label="Environment" hint={prod ? "Production: agents are told to check with you before changing anything here." : undefined}>
              <EnvPicker value={f.env} envs={ENVS} onChange={(env) => set({ env })} />
            </Field>
            <Field label="Tags" htmlFor={id + "tags"} error={shown("tags")}>
              <input id={id + "tags"} className="mono" value={f.tags} spellCheck={false} autoComplete="off" placeholder="region=hk, team=core" onChange={(e) => set({ tags: e.target.value })} />
            </Field>
            <Field label="Note" htmlFor={id + "note"} hint="Shown to agents with every result on this cluster.">
              <textarea id={id + "note"} rows={2} value={f.note} placeholder="e.g. Changes need a ticket." onChange={(e) => set({ note: e.target.value })} />
            </Field>
          </section>

          <section className="form-section" aria-labelledby={id + "check"}>
            <h2 className="caption" id={id + "check"}>Connection check</h2>
            {probe.step === "failed" ? (
              <div className="check-block failed" role="alert">
                <p className="check-text">{probe.error}</p>
                <div className="check-actions"><button type="button" onClick={test}>Test again</button></div>
              </div>
            ) : (
              <div className="check-line" role="status">
                <button type="button" onClick={test} disabled={probe.step === "testing" || !src}>{probe.step === "idle" ? "Test connection" : "Test again"}</button>
                {probe.step === "idle" && <span className="muted">Asks the server for its version. A credential plugin may run.</span>}
                {probe.step === "testing" && <span className="muted">Connecting…</span>}
                {probe.step === "ok" && (
                  <span className="state-line"><span className="state-dot" aria-hidden="true" />Connected: Kubernetes {probe.version}, {probe.ms} ms</span>
                )}
              </div>
            )}
          </section>
        </div>
        <footer className="editor-foot">
          <span className="spacer" />
          <button type="button" onClick={onCancel}>Cancel</button>
          <button type="submit" className="primary" disabled={saving}>{saving ? "Adding…" : "Add cluster"}</button>
        </footer>
      </form>
    </div>
  );
}
