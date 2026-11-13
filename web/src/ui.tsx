// Small shared pieces of the desktop UI: chips that carry the two signals
// that matter most (who did it, and is it production), dialogs and inline
// messages. Styles are in styles.css; decisions in .interface-design/system.md.
import { useEffect, useId, useRef, type KeyboardEvent as ReactKeyboardEvent, type ReactNode } from "react";
import { isProdTag } from "./env";

/** Tag is a host or context tag; env=prod is the solid hazard chip. */
export function Tag({ k, v, inherited, from }: { k: string; v: string; inherited?: boolean; from?: string }) {
  const prod = isProdTag(k, v);
  return (
    <span className={prod ? "chip prod" : "chip"} title={inherited ? `from ${from} (same cluster)` : undefined}>
      {k}={v}
      {inherited && <span aria-label="inherited"> ↑</span>}
    </span>
  );
}

/** Tags lists a tag set (keys sorted, env first). */
export function Tags({ tags }: { tags?: Record<string, string> }) {
  const entries = Object.entries(tags ?? {}).sort(([a], [b]) => (a === "env" ? -1 : b === "env" ? 1 : a.localeCompare(b)));
  return (
    <>
      {entries.map(([k, v]) => (
        <Tag key={k} k={k} v={v} />
      ))}
    </>
  );
}

/** Owner names who started something: an agent's crew chip, or "you". */
export function Owner({ owner }: { owner: { kind: "human" | "agent"; name: string } }) {
  if (owner.kind === "agent") {
    return (
      <span className="chip agent" title={`opened by agent ${owner.name}`}>
        {owner.name}
      </span>
    );
  }
  return <span className="you">you</span>;
}

/** Risk is an action's risk: read is quiet, write amber, destructive solid red. */
export function Risk({ risk }: { risk?: string }) {
  if (!risk) return null;
  return <span className={`chip risk-${risk}`}>{risk}</span>;
}

/** HazardBand marks a production place across the top of a pane or dialog. */
export function HazardBand({ env, where, note }: { env: string; where: string; note?: string }) {
  return (
    <div className="hazard-band" role="note">
      <strong>Production</strong>
      <span className="mono">{where}</span>
      <span className="chip prod">{env}</span>
      {note && <span className="hazard-note" title={note}>{note}</span>}
    </div>
  );
}

/** Message is an inline line: an error (with Dismiss), a notice, or a note. */
export function Message({ kind, children, onDismiss }: { kind: "error" | "notice" | "note"; children: ReactNode; onDismiss?(): void }) {
  return (
    <div className={`message ${kind}`} role={kind === "error" ? "alert" : "status"}>
      <span className="message-text">{children}</span>
      {onDismiss && (
        <button className="ghost small" onClick={onDismiss}>
          Dismiss
        </button>
      )}
    </div>
  );
}

/** Empty says what will appear in an area, and how. */
export function Empty({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="empty">
      <div className="empty-title">{title}</div>
      {children && <div className="empty-body">{children}</div>}
    </div>
  );
}

/** Modal is an in-page dialog (the webview has no native confirm()). Escape
 * and a click outside cancel; focus starts on Cancel and goes back to where
 * it was. */
export function Modal(props: {
  title: ReactNode;
  band?: ReactNode; // a HazardBand for production
  children?: ReactNode;
  ok: string;
  danger?: boolean;
  disabled?: boolean;
  onOk(): void;
  onCancel(): void;
}) {
  const id = useId();
  const box = useRef<HTMLDivElement>(null);
  const cancel = useRef(props.onCancel);
  cancel.current = props.onCancel;
  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    const key = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        cancel.current();
      }
      if (e.key === "Tab" && box.current) {
        // Keep focus inside the dialog.
        const f = [...box.current.querySelectorAll<HTMLElement>("button, input, select, textarea")].filter((x) => !x.hasAttribute("disabled"));
        if (f.length === 0) return;
        const first = f[0];
        const last = f[f.length - 1];
        if (e.shiftKey && document.activeElement === first) {
          e.preventDefault();
          last.focus();
        } else if (!e.shiftKey && document.activeElement === last) {
          e.preventDefault();
          first.focus();
        }
      }
    };
    document.addEventListener("keydown", key, true);
    return () => {
      document.removeEventListener("keydown", key, true);
      opener?.focus?.();
    };
  }, []);
  return (
    <div className="overlay" onMouseDown={props.onCancel}>
      <div className="dialog" role="dialog" aria-modal="true" aria-labelledby={id} ref={box} onMouseDown={(e) => e.stopPropagation()}>
        {props.band}
        <div className="dialog-body">
          <h2 id={id} className="dialog-title">
            {props.title}
          </h2>
          {props.children}
        </div>
        <div className="dialog-buttons">
          <button onClick={props.onCancel} autoFocus>
            Cancel
          </button>
          <button className={props.danger ? "danger" : "primary"} disabled={props.disabled} onClick={props.onOk}>
            {props.ok}
          </button>
        </div>
      </div>
    </div>
  );
}

/** PaneHeader names what a pane shows: an icon, a title, quiet metadata, and actions on the right. */
export function PaneHeader({ title, icon, meta, children }: { title: ReactNode; icon?: ReactNode; meta?: ReactNode; children?: ReactNode }) {
  return (
    <header className="pane-head">
      {icon}
      <h1 className="pane-title">{title}</h1>
      {meta && <span className="pane-meta">{meta}</span>}
      <span className="spacer" />
      {children}
    </header>
  );
}

export interface MenuItem {
  label: string;
  detail?: string; // a second line saying what it does
  onSelect(): void;
  danger?: boolean;
  disabled?: boolean;
  separatorBefore?: boolean;
}

/** Menu is a small popup menu at a point (a context menu, or under a button). ↑/↓ move, Return picks,
 * Escape or a click elsewhere closes; focus goes back to where it was. */
export function Menu({ at, items, label, onClose }: { at: { x: number; y: number }; items: MenuItem[]; label: string; onClose(): void }) {
  const box = useRef<HTMLDivElement>(null);
  const close = useRef(onClose);
  close.current = onClose;
  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    box.current?.querySelector<HTMLElement>("[role=menuitem]:not([disabled])")?.focus();
    const away = (e: MouseEvent) => {
      if (!box.current?.contains(e.target as Node)) close.current();
    };
    const key = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        close.current();
      }
    };
    document.addEventListener("mousedown", away, true);
    document.addEventListener("keydown", key, true);
    return () => {
      document.removeEventListener("mousedown", away, true);
      document.removeEventListener("keydown", key, true);
      if (opener?.isConnected) opener.focus();
    };
  }, []);
  const onKey = (e: ReactKeyboardEvent) => {
    const list = [...(box.current?.querySelectorAll<HTMLElement>("[role=menuitem]:not([disabled])") ?? [])];
    const i = list.indexOf(document.activeElement as HTMLElement);
    const j = e.key === "ArrowDown" ? Math.min(i + 1, list.length - 1) : e.key === "ArrowUp" ? Math.max(i - 1, 0) : -1;
    if (j >= 0) {
      e.preventDefault();
      list[j].focus();
    }
  };
  // Keep the menu inside the window.
  const x = Math.min(at.x, window.innerWidth - 220);
  const y = Math.min(at.y, window.innerHeight - 24 - items.reduce((n, it) => n + (it.detail ? 44 : 26), 0));
  return (
    <div className="menu" role="menu" aria-label={label} ref={box} style={{ left: x, top: y }} onKeyDown={onKey}>
      {items.map((it) => (
        <div key={it.label} className={it.separatorBefore ? "menu-group" : undefined}>
          <button role="menuitem" className={it.danger ? "danger-text" : undefined} disabled={it.disabled}
            onClick={() => {
              close.current();
              it.onSelect();
            }}>
            <span className="menu-label">{it.label}</span>
            {it.detail && <span className="menu-detail">{it.detail}</span>}
          </button>
        </div>
      ))}
    </div>
  );
}

/** Field is one labelled row of a form grid: label on the left, the control, then an error or a hint. */
export function Field({ label, htmlFor, error, hint, children }: { label: string; htmlFor?: string; error?: string; hint?: ReactNode; children: ReactNode }) {
  return (
    <div className={error ? "field-row invalid" : "field-row"}>
      <label className="field-label" htmlFor={htmlFor}>{label}</label>
      <div className="field-control">
        {children}
        {error ? <div className="field-error" role="alert">{error}</div> : hint ? <div className="field-hint">{hint}</div> : null}
      </div>
    </div>
  );
}

/** EnvPicker chooses a place's environment; production is the hazard. */
export function EnvPicker({ value, onChange, envs }: { value: string; onChange(env: string): void; envs: string[] }) {
  const options = ["", ...envs.filter((e) => e !== "prod"), ...(value && !envs.includes(value) ? [value] : []), "prod"];
  return (
    <div className="segmented env-picker" role="radiogroup" aria-label="Environment">
      {options.map((e) => (
        <button key={e || "none"} type="button" role="radio" aria-checked={value === e} aria-selected={value === e}
          className={e === "prod" ? "prod-option" : undefined} onClick={() => onChange(e)}>
          {e || "None"}
        </button>
      ))}
    </div>
  );
}
