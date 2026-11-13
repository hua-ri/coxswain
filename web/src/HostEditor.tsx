import { useEffect, useId, useRef, useState, type FormEvent } from "react";
import type { Cox } from "./cox";
import {
  ENVS, blankHost, connArgs, connKey, forgetAndRescan, formErrors, formFromHost, hostArgs, keygenCommand, runCheck,
  trustAndContinue, updateArgs, type Check, type HostForm, type HostKey,
} from "./hostform";
import { Icon } from "./icons";
import type { Host } from "./types";
import { EnvPicker, Field, HazardBand, Message, Modal, PaneHeader } from "./ui";

interface Props {
  cox: Cox;
  hosts: Host[];
  original?: Host; // the host being edited; none: a new one
  onSaved(name: string): void;
  onCancel(): void;
  onRemove?(): void;
}

const DEFAULT_KEYS = "ssh-agent, then ~/.ssh/id_ed25519, id_ecdsa, id_rsa";

/** HostEditor is the add/edit host pane: connection, labels, and a connection check that trusts the host key. */
export function HostEditor({ cox, hosts, original, onSaved, onCancel, onRemove }: Props) {
  const [f, setF] = useState<HostForm>(() => (original ? formFromHost(original) : blankHost()));
  const [touched, setTouched] = useState<Set<string>>(new Set());
  const [tried, setTried] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [check, setCheck] = useState<Check>({ step: "idle" });
  const [forgetting, setForgetting] = useState<HostKey | null>(null);
  const id = useId();
  const key = connKey(f);

  // Changing where or how it connects starts the check over.
  useEffect(() => setCheck({ step: "idle" }), [key]);
  // A result (above all a key to confirm) is shown where it can be read.
  const checkBox = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (check.step !== "idle") checkBox.current?.scrollIntoView({ block: "nearest" });
  }, [check.step]);

  const set = (patch: Partial<HostForm>) => setF((x) => ({ ...x, ...patch }));
  const touch = (name: string) => setTouched((t) => new Set(t).add(name));
  const errors = formErrors(f, hosts, original?.name);
  const shown = (k: keyof HostForm) => (tried || touched.has(k) ? errors[k] : undefined);
  const others = hosts.filter((h) => h.name !== original?.name);
  const prod = f.env === "prod";

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setTried(true);
    if (Object.keys(errors).length) return;
    setSaving(true);
    setError("");
    try {
      const out = original
        ? await cox.invoke<Host>("ssh.hosts.update", updateArgs(f, original))
        : await cox.invoke<Host>("ssh.hosts.add", hostArgs(f));
      if (out.status !== "ok") throw new Error([out.error, out.hint].filter(Boolean).join(" — "));
      onSaved(out.result?.name ?? f.name.trim());
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const addKey = async () => {
    try {
      const path = await cox.pickFile("key");
      if (path && !f.keys.includes(path)) set({ keys: [...f.keys, path] });
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const canCheck = !errors.name && !errors.hostname && !errors.port && !errors.user && !errors.jump;
  const test = () => {
    setTried(true);
    if (canCheck) void runCheck(cox.invoke, connArgs(f), setCheck);
  };

  return (
    <div className="pane editor" style={{ display: "flex" }}>
      {prod && <HazardBand env="env=prod" where={f.name.trim() || "new host"} note={f.note.trim() || undefined} />}
      <PaneHeader icon={<Icon name="host" />} title={original ? `Edit ${original.name}` : "Add host"}
        meta={original?.source === "ssh_config" ? "imported from ~/.ssh/config" : undefined} />
      <form className="editor-form" onSubmit={save} noValidate aria-label={original ? `Edit ${original.name}` : "Add host"}>
        <div className="editor-body">
          {error && <Message kind="error" onDismiss={() => setError("")}>{error}</Message>}
          <section className="form-section" aria-labelledby={id + "conn"}>
            <h2 className="caption" id={id + "conn"}>Connection</h2>
            <Field label="Name" htmlFor={id + "name"} error={shown("name")} hint="What you and your agents call it, e.g. in ssh_exec">
              <input id={id + "name"} value={f.name} autoFocus={!original} spellCheck={false} autoComplete="off" placeholder="web-1"
                onChange={(e) => set({ name: e.target.value })} onBlur={() => touch("name")} />
            </Field>
            <Field label="Address" htmlFor={id + "addr"} error={shown("hostname") ?? shown("port")}>
              <div className="inline-fields">
                <input id={id + "addr"} className="mono grow" value={f.hostname} spellCheck={false} autoComplete="off" placeholder="10.0.1.11 or web-1.example.com"
                  onChange={(e) => set({ hostname: e.target.value })} onBlur={() => touch("hostname")} />
                <label className="inline-label" htmlFor={id + "port"}>Port</label>
                <input id={id + "port"} className="mono port" value={f.port} inputMode="numeric" placeholder="22"
                  onChange={(e) => set({ port: e.target.value })} onBlur={() => touch("port")} />
              </div>
            </Field>
            <Field label="User" htmlFor={id + "user"} error={shown("user")}>
              <input id={id + "user"} className="mono" value={f.user} spellCheck={false} autoComplete="off" placeholder="your login name"
                onChange={(e) => set({ user: e.target.value })} onBlur={() => touch("user")} />
            </Field>
            <Field label="Keys" hint={f.keys.length ? "Tried after ssh-agent's keys. Coxswain keeps only the path." : undefined}>
              <div className="key-list">
                {f.keys.length === 0 && <span className="muted">{DEFAULT_KEYS}</span>}
                {f.keys.map((k) => (
                  <span key={k} className="key-item">
                    <span className="mono cut" title={k}>{k}</span>
                    <button type="button" className="ghost small icon-only" aria-label={`Remove key ${k}`} title="Remove"
                      onClick={() => set({ keys: f.keys.filter((x) => x !== k) })}>
                      <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden="true"><path d="M2.5 2.5l5 5M7.5 2.5l-5 5" /></svg>
                    </button>
                  </span>
                ))}
                <span>
                  <button type="button" className="small" onClick={addKey}>{f.keys.length ? "Add another key…" : "Choose key file…"}</button>
                  {f.keys.length > 0 && (
                    <button type="button" className="link small-link" onClick={() => set({ keys: [] })}>Use ssh-agent only</button>
                  )}
                </span>
              </div>
            </Field>
            <Field label="Jump host" htmlFor={id + "jump"} error={shown("jump")} hint="A saved host, or user@host:port">
              <input id={id + "jump"} className="mono" list={id + "hosts"} value={f.jump} spellCheck={false} autoComplete="off" placeholder="none"
                onChange={(e) => set({ jump: e.target.value })} onBlur={() => touch("jump")} />
              <datalist id={id + "hosts"}>
                {others.map((h) => <option key={h.name} value={h.name}>{h.hostname}</option>)}
              </datalist>
            </Field>
          </section>

          <section className="form-section" aria-labelledby={id + "labels"}>
            <h2 className="caption" id={id + "labels"}>What agents are told</h2>
            <Field label="Environment" hint={prod ? "Production: agents are told to check with you before changing anything here." : undefined}>
              <EnvPicker value={f.env} envs={ENVS} onChange={(env) => set({ env })} />
            </Field>
            <Field label="Tags" htmlFor={id + "tags"} error={shown("tags")}>
              <input id={id + "tags"} className="mono" value={f.tags} spellCheck={false} autoComplete="off" placeholder="role=web, team=core"
                onChange={(e) => set({ tags: e.target.value })} onBlur={() => touch("tags")} />
            </Field>
            <Field label="Note" htmlFor={id + "note"} hint="Shown to agents with every result on this host.">
              <textarea id={id + "note"} rows={2} value={f.note} placeholder="e.g. No restarts 9:00–18:00; page #ops first."
                onChange={(e) => set({ note: e.target.value })} />
            </Field>
          </section>

          <section className="form-section" aria-labelledby={id + "check"}>
            <h2 className="caption" id={id + "check"}>Connection check</h2>
            <div ref={checkBox}>
            <CheckPanel check={check} canCheck={canCheck} onTest={test} name={f.name.trim() || f.hostname.trim()}
              onTrust={(k) => void trustAndContinue(cox.invoke, connArgs(f), k, setCheck)}
              onForget={setForgetting} onDismiss={() => setCheck({ step: "idle" })} />
            </div>
          </section>
        </div>
        <footer className="editor-foot">
          {onRemove && (
            <button type="button" className="ghost danger-text remove" onClick={onRemove}>Remove…</button>
          )}
          <span className="spacer" />
          <button type="button" onClick={onCancel}>Cancel</button>
          <button type="submit" className="primary" disabled={saving}>{saving ? "Saving…" : original ? "Save" : "Add host"}</button>
        </footer>
      </form>
      {forgetting && (
        <Modal title={`Remove the old key of ${forgetting.host}?`} ok="Remove old key" danger
          band={<HazardBand env="host key changed" where={forgetting.address} />}
          onCancel={() => setForgetting(null)}
          onOk={() => {
            const k = forgetting;
            setForgetting(null);
            void forgetAndRescan(cox.invoke, connArgs(f), k, setCheck);
          }}>
          <p>Only do this if you know why the key changed, for example the host was reinstalled. Then compare the new fingerprint before you trust it.</p>
        </Modal>
      )}
    </div>
  );
}

function Fingerprint({ k }: { k: HostKey }) {
  return (
    <div className="fingerprint">
      <span className="fp-type">{k.type}</span>
      <code className="fp-value">{k.fingerprint}</code>
    </div>
  );
}

/** CheckPanel shows the connection check: scan, the key to confirm (or a changed key), and the login. */
function CheckPanel(props: {
  check: Check;
  canCheck: boolean;
  name: string;
  onTest(): void;
  onTrust(k: HostKey): void;
  onForget(k: HostKey): void;
  onDismiss(): void;
}) {
  const c = props.check;
  const busy = c.step === "scanning" || c.step === "trusting" || c.step === "login" || c.step === "forgetting";
  const testButton = (
    <button type="button" onClick={props.onTest} disabled={busy}>
      {c.step === "idle" ? "Test connection" : "Test again"}
    </button>
  );
  switch (c.step) {
    case "idle":
      return (
        <div className="check-line">
          {testButton}
          <span className="muted">Shows the host’s key for you to check, then logs in as this user.</span>
        </div>
      );
    case "scanning":
    case "trusting":
    case "forgetting":
    case "login":
      return (
        <div className="check-line" role="status">
          {testButton}
          <span className="muted">
            {c.step === "scanning" ? "Reading the host key…" : c.step === "trusting" ? "Trusting the key…" : c.step === "forgetting" ? "Removing the old key…" : "Logging in…"}
          </span>
        </div>
      );
    case "ok":
      return (
        <div className="check-line" role="status">
          {testButton}
          <span className="state-line"><span className="state-dot" aria-hidden="true" />Logged in to {props.name}: {c.os}, {c.ms} ms</span>
        </div>
      );
    case "failed":
      return (
        <div className="check-block failed" role="alert">
          <p className="check-text">{c.error}</p>
          <div className="check-actions">{testButton}</div>
        </div>
      );
    case "confirm": {
      const k = c.key;
      const jump = k.host !== props.name;
      return (
        <div className="check-block confirm" role="group" aria-label="Confirm host key">
          <p className="check-text">
            {jump ? <>The jump host <strong>{k.host}</strong> is not trusted yet. </> : <>First connection to <strong>{k.host}</strong>. </>}
            Compare this fingerprint with the one its admin gave you, or run <code>ssh-keygen -lf /etc/ssh/ssh_host_{shortType(k.type)}_key.pub</code> on it.
          </p>
          <div className="check-where mono">{k.address}{k.via?.length ? ` via ${k.via.join(" → ")}` : ""}</div>
          <Fingerprint k={k} />
          <div className="check-actions">
            <button type="button" className="primary" onClick={() => props.onTrust(k)}>Trust this key</button>
            <button type="button" onClick={props.onDismiss}>Not now</button>
            <span className="muted">Saved in ~/.coxswain/known_hosts; ~/.ssh is never changed.</span>
          </div>
        </div>
      );
    }
    case "changed": {
      const k = c.key;
      const ours = (k.known ?? []).filter((x) => x.coxswain);
      const theirs = (k.known ?? []).filter((x) => !x.coxswain);
      return (
        <div className="check-block changed" role="alert">
          <p className="check-title">Host key changed for {k.host}</p>
          <p className="check-text">
            {k.address} now presents a different key than the one recorded. This can mean someone is intercepting the connection.
            Coxswain will not connect, and will not trust the new key until the old one is removed.
          </p>
          <Fingerprint k={k} />
          {(k.known ?? []).map((x) => (
            <div key={x.file + x.line} className="check-where mono">recorded: {x.type} {x.fingerprint} ({x.file}:{x.line})</div>
          ))}
          <div className="check-actions">
            {ours.length > 0 && <button type="button" className="danger-outline" onClick={() => props.onForget(k)}>Remove old key…</button>}
            {theirs.length > 0 && (
              <span className="muted">
                The old key is in your own known_hosts, which Coxswain never edits. Remove it in a terminal: <code>{keygenCommand(k.address)}</code>
              </span>
            )}
            {testButton}
          </div>
        </div>
      );
    }
  }
}

function shortType(t: string): string {
  if (t.includes("ed25519")) return "ed25519";
  if (t.includes("ecdsa")) return "ecdsa";
  return "rsa";
}
