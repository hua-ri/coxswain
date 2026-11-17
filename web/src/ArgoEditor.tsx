import { useEffect, useId, useState, type FormEvent } from "react";
import { addArgoArgs, argoErrors, blankArgo, formFromInstance, guessName, loginText, updateArgoArgs, type ArgoForm, type ArgoInstance, type ArgoKind, type Methods } from "./argo";
import type { Cox, Outcome } from "./cox";
import { ENVS } from "./hostform";
import { Icon } from "./icons";
import { EnvPicker, Field, HazardBand, Message, PaneHeader } from "./ui";

function message(out: { error?: string; hint?: string }): string {
  return [out.error, out.hint].filter(Boolean).join(" — ") || "failed";
}

const KIND_LABEL: Record<ArgoKind, string> = { argocd: "Argo CD", argowf: "Argo Workflows" };

/** ArgoEditor adds an Argo CD or Argo Workflows instance by its URL (saving it opens the login pane), or edits one. */
export function ArgoEditor({ cox, instances, contexts, kind, original, onSaved, onCancel }: {
  cox: Cox;
  instances: ArgoInstance[];
  contexts: string[]; // Coxswain's clusters, for "runs in"
  kind: ArgoKind;
  original?: ArgoInstance;
  onSaved(kind: ArgoKind, name: string): void;
  onCancel(): void;
}) {
  const [f, setF] = useState<ArgoForm>(() => (original ? formFromInstance(original) : blankArgo(kind)));
  const [named, setNamed] = useState(!!original); // the user typed a name: stop guessing
  const [tried, setTried] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const id = useId();
  const set = (patch: Partial<ArgoForm>) => setF((x) => ({ ...x, ...patch }));
  const errors = argoErrors(f, instances, original?.name);
  const shown = (k: keyof typeof errors) => (tried ? errors[k] : undefined);
  const prod = f.env === "prod";

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setTried(true);
    if (Object.keys(errors).length) return;
    setSaving(true);
    setError("");
    try {
      const out = await cox.invoke<ArgoInstance>(original ? "argo.update" : "argo.add", original ? updateArgoArgs(f, original) : addArgoArgs(f));
      if (out.status !== "ok") throw new Error(message(out));
      onSaved(f.kind, f.name.trim());
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="pane editor" style={{ display: "flex" }}>
      {prod && <HazardBand env="env=prod" where={f.name.trim() || "new instance"} note={f.note.trim() || undefined} />}
      <PaneHeader icon={<Icon name={f.kind} />} title={original ? `Edit ${original.name}` : `Add ${KIND_LABEL[f.kind]}`} />
      <form className="editor-form" onSubmit={save} noValidate aria-label={original ? `Edit ${original.name}` : `Add ${KIND_LABEL[f.kind]}`}>
        <div className="editor-body">
          {error && <Message kind="error" onDismiss={() => setError("")}>{error}</Message>}
          <p className="editor-intro">
            Coxswain talks to the server through its official API. Settings go to <code>~/.coxswain/argo.json</code>;
            the login is kept only in your macOS keychain and is never shown again, to you or to agents.
          </p>
          <section className="form-section" aria-labelledby={id + "srv"}>
            <h2 className="caption" id={id + "srv"}>Server</h2>
            {!original && <Field label="Kind">
              <div className="segmented source-picker" role="radiogroup" aria-label="Kind">
                {(["argocd", "argowf"] as ArgoKind[]).map((k) => (
                  <button key={k} type="button" role="radio" aria-checked={f.kind === k} aria-selected={f.kind === k}
                    onClick={() => set({ kind: k, namespace: k === "argowf" ? f.namespace || "argo" : "" })}>{KIND_LABEL[k]}</button>
                ))}
              </div>
            </Field>}
            <Field label="URL" htmlFor={id + "url"} error={shown("url")} hint="As you open it in the browser.">
              <input id={id + "url"} className="mono" value={f.url} spellCheck={false} autoComplete="off" autoFocus
                placeholder={f.kind === "argocd" ? "https://argocd.example.com" : "https://argo.example.com"}
                onChange={(e) => set({ url: e.target.value, ...(named ? {} : { name: guessName(e.target.value) }) })} />
            </Field>
            <Field label="Name" htmlFor={id + "name"} error={shown("name")} hint="What you and your agents call it.">
              <input id={id + "name"} className="mono" value={f.name} spellCheck={false} autoComplete="off"
                onChange={(e) => { setNamed(true); set({ name: e.target.value }); }} />
            </Field>
            {f.kind === "argowf" && (
              <Field label="Namespace" htmlFor={id + "ns"} error={shown("namespace")} hint="Workflows are listed here unless another namespace is given.">
                <input id={id + "ns"} className="mono" value={f.namespace} spellCheck={false} autoComplete="off" onChange={(e) => set({ namespace: e.target.value })} />
              </Field>
            )}
            <Field label="TLS" hint={f.insecure ? <span className="warn-text">The server’s certificate is not checked. Anyone on the path could read the login.</span> : undefined}>
              <label className="check">
                <input type="checkbox" checked={f.insecure} onChange={(e) => set({ insecure: e.target.checked })} /> Skip certificate check
              </label>
            </Field>
            <Field label="CA file" htmlFor={id + "ca"} hint="A PEM file, if the server uses a private CA.">
              <div className="inline-fields">
                <input id={id + "ca"} className="mono" value={f.caFile} spellCheck={false} autoComplete="off" placeholder="optional" onChange={(e) => set({ caFile: e.target.value })} />
                <button type="button" className="small" onClick={() => void cox.pickFile("upload").then((p) => p && set({ caFile: p }))}>Choose…</button>
              </div>
            </Field>
            {f.kind === "argocd" && (
              <Field label="SSO port" htmlFor={id + "port"} error={shown("ssoPort")} hint="Local port for the browser login’s callback; default 8085, as argocd login --sso.">
                <input id={id + "port"} className="mono port" value={f.ssoPort} inputMode="numeric" placeholder="8085" onChange={(e) => set({ ssoPort: e.target.value })} />
              </Field>
            )}
          </section>

          <section className="form-section" aria-labelledby={id + "k8s"}>
            <h2 className="caption" id={id + "k8s"}>Cluster</h2>
            <Field label="Runs in" htmlFor={id + "ctx"} hint={f.kind === "argocd"
              ? "The cluster Argo CD itself runs in (its in-cluster destination). Diagnoses then follow unhealthy pods there, to their nodes and SSH hosts. Other destinations are matched by their server address."
              : "The cluster the workflows’ pods run in. Diagnoses then follow a failed step’s pod to its node and SSH host."}>
              <select id={id + "ctx"} value={f.k8sContext} onChange={(e) => set({ k8sContext: e.target.value })}>
                <option value="">Not in Coxswain</option>
                {contexts.map((c) => <option key={c} value={c}>{c}</option>)}
              </select>
            </Field>
          </section>

          <section className="form-section" aria-labelledby={id + "labels"}>
            <h2 className="caption" id={id + "labels"}>What agents are told</h2>
            <Field label="Environment" hint={prod ? "Production: agents are told to check with you before changing anything here." : undefined}>
              <EnvPicker value={f.env} envs={ENVS} onChange={(env) => set({ env })} />
            </Field>
            <Field label="Tags" htmlFor={id + "tags"} error={shown("tags")}>
              <input id={id + "tags"} className="mono" value={f.tags} spellCheck={false} autoComplete="off" placeholder="team=core" onChange={(e) => set({ tags: e.target.value })} />
            </Field>
            <Field label="Note" htmlFor={id + "note"} hint="Shown to agents with every result on this instance.">
              <textarea id={id + "note"} rows={2} value={f.note} placeholder="e.g. Sync only after the release review." onChange={(e) => set({ note: e.target.value })} />
            </Field>
          </section>
        </div>
        <footer className="editor-foot">
          <span className="spacer" />
          <button type="button" onClick={onCancel}>Cancel</button>
          <button type="submit" className="primary" disabled={saving}>{saving ? "Saving…" : original ? "Save" : "Add and log in…"}</button>
        </footer>
      </form>
    </div>
  );
}

/** ArgoLogin offers the instance's login methods and performs the one the user picks. */
export function ArgoLogin({ cox, instance, onDone, onCancel }: {
  cox: Cox;
  instance: ArgoInstance;
  onDone(): void;
  onCancel(): void;
}) {
  const { kind, name } = instance;
  const [methods, setMethods] = useState<Methods | null>(null);
  const [method, setMethod] = useState("");
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const id = useId();

  useEffect(() => {
    let stale = false;
    cox.invoke<Methods>(`${kind}.login_methods`, { instance: name })
      .then((out) => {
        if (stale) return;
        if (out.status !== "ok" || !out.result) return setError(message(out));
        setMethods(out.result);
        setMethod(out.result.methods[0]?.method ?? "");
      })
      .catch((e) => !stale && setError(String(e)));
    return () => {
      stale = true;
    };
  }, [cox, kind, name]);

  const login = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    const args: Record<string, unknown> = { instance: name, method };
    if (method === "password") Object.assign(args, { username: username.trim(), password });
    if (method === "token") args.token = token;
    try {
      const out: Outcome = await cox.invoke(`${kind}.login`, args);
      if (out.status !== "ok") throw new Error(message(out));
      setPassword("");
      setToken("");
      onDone();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };
  const cancelBrowser = () => void cox.invoke(`${kind}.cancel_login`, { instance: name });

  const chosen = methods?.methods.find((m) => m.method === method);
  const ready = method === "password" ? !!username.trim() && !!password : method === "token" ? !!token.trim() : !!method;
  const label = kind === "argocd" ? "Argo CD" : "Argo Workflows";
  return (
    <div className="pane editor" style={{ display: "flex" }}>
      <PaneHeader icon={<Icon name={kind} />} title={`Log in to ${name}`} meta={instance.url} />
      <form className="editor-form" onSubmit={login} noValidate aria-label={`Log in to ${name}`}>
        <div className="editor-body">
          {error && <Message kind="error" onDismiss={() => setError("")}>{error}</Message>}
          {methods?.problem && <Message kind="note">{methods.problem}</Message>}
          <p className="editor-intro">
            {loginText(instance.login)}. The {label} server offers the ways below; Coxswain keeps the resulting
            {kind === "argocd" ? " session" : ""} token in your keychain. Agents can use it but never read it.
          </p>
          <section className="form-section" aria-labelledby={id + "how"}>
            <h2 className="caption" id={id + "how"}>How</h2>
            {!methods && !error && <div className="loading">Asking the server…</div>}
            {methods && (
              <Field label="Method" hint={chosen?.detail}>
                <div className="segmented source-picker" role="radiogroup" aria-label="Login method">
                  {methods.methods.map((m) => (
                    <button key={m.method} type="button" role="radio" aria-checked={method === m.method} aria-selected={method === m.method}
                      onClick={() => setMethod(m.method)}>{m.label}</button>
                  ))}
                </div>
              </Field>
            )}
            {method === "password" && (
              <>
                <Field label="Username" htmlFor={id + "user"}>
                  <input id={id + "user"} className="mono" value={username} autoComplete="username" spellCheck={false} onChange={(e) => setUsername(e.target.value)} />
                </Field>
                <Field label="Password" htmlFor={id + "pw"} hint="Exchanged for a session token; the password itself is not kept.">
                  <input id={id + "pw"} type="password" value={password} autoComplete="current-password" autoFocus onChange={(e) => setPassword(e.target.value)} />
                </Field>
              </>
            )}
            {method === "token" && (
              <Field label="Token" htmlFor={id + "tok"} hint={kind === "argowf"
                ? "A Kubernetes token (kubectl create token <service account> -n <namespace>), or from the Argo UI’s user info page. “Bearer ” is fine."
                : "An Argo CD account or project role token."}>
                <textarea id={id + "tok"} className="mono secret-input" rows={3} value={token} spellCheck={false} autoComplete="off" autoFocus onChange={(e) => setToken(e.target.value)} />
              </Field>
            )}
            {method === "browser" && busy && (
              <div className="check-line" role="status">
                <span className="pulse running" aria-hidden="true" />
                <span>Waiting for you to finish logging in in your browser…</span>
                <button type="button" className="small" onClick={cancelBrowser}>Cancel</button>
              </div>
            )}
          </section>
        </div>
        <footer className="editor-foot">
          {instance.login.method && (
            <button type="button" className="ghost danger-text" onClick={() => void cox.invoke(`${kind}.logout`, { instance: name }).then(onDone)}>Log out</button>
          )}
          <span className="spacer" />
          <button type="button" onClick={onCancel}>{instance.login.logged_in ? "Close" : "Later"}</button>
          <button type="submit" className="primary" disabled={!ready || busy}>
            {busy ? (method === "browser" ? "Waiting…" : "Logging in…") : method === "browser" ? "Open browser" : method === "none" ? "Use without credential" : "Log in"}
          </button>
        </footer>
      </form>
    </div>
  );
}
