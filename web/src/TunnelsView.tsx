import { Fragment, useState } from "react";
import { formatBytes, formatTime } from "./format";
import { failureText, tunnelRequest, tunnelRoute, type Tunnel, type TunnelForm } from "./tunnels";
import type { Host } from "./types";
import { Icon } from "./icons";
import { Empty, Owner, PaneHeader } from "./ui";

interface Props {
  tunnels: Tunnel[];
  hosts: Host[];
  onClose(t: Tunnel): void;
  /** onCreate opens a tunnel; it rejects with the server's error. */
  onCreate(body: Record<string, unknown>): Promise<void>;
}

const HELP: Record<TunnelForm["type"], string> = {
  local: "Like ssh -L: a port here leads to host:port as seen from the server (default 127.0.0.1, the server itself).",
  remote: "Like ssh -R: a port on the server leads to a port here. This exposes a local service to the server.",
  socks: "Like ssh -D: a SOCKS5 proxy here whose connections leave from the server.",
};

/** TunnelsView lists the server's tunnels and opens new ones. */
export function TunnelsView({ tunnels, hosts, onClose, onCreate }: Props) {
  const [form, setForm] = useState<TunnelForm | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = () => {
    if (!form) return;
    const req = tunnelRequest(form);
    if ("error" in req) return setError(req.error);
    setBusy(true);
    onCreate(req.body)
      .then(() => {
        setForm(null);
        setError("");
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setBusy(false));
  };
  const set = (patch: Partial<TunnelForm>) => form && setForm({ ...form, ...patch });

  return (
    <div className="panel">
      <PaneHeader icon={<Icon name="@tunnels" />} title="Tunnels" meta={`${tunnels.length} open`}>
        {!form && (
          <button
            className="small primary"
            onClick={() => setForm({ host: hosts[0]?.name ?? "", type: "local", localPort: "", remoteHost: "", remotePort: "" })}
          >
            New tunnel
          </button>
        )}
      </PaneHeader>
      {form && (
        <form
          className="tunnel-form"
          aria-label="New tunnel"
          onSubmit={(e) => {
            e.preventDefault();
            submit();
          }}
          onKeyDown={(e) => e.key === "Escape" && (setForm(null), setError(""))}
        >
          <label>
            Host
            <select value={form.host} onChange={(e) => set({ host: e.target.value })} autoFocus>
              {hosts.map((h) => (
                <option key={h.name}>{h.name}</option>
              ))}
            </select>
          </label>
          <label>
            Type
            <select value={form.type} onChange={(e) => set({ type: e.target.value as TunnelForm["type"] })}>
              <option value="local">local (-L)</option>
              <option value="remote">remote (-R)</option>
              <option value="socks">socks (-D)</option>
            </select>
          </label>
          <label>
            {form.type === "remote" ? "Local port (target)" : "Local port"}
            <input
              value={form.localPort}
              onChange={(e) => set({ localPort: e.target.value })}
              placeholder={form.type === "remote" ? "required" : "free"}
              size={7}
              inputMode="numeric"
            />
          </label>
          {form.type === "local" && (
            <label>
              Remote host
              <input value={form.remoteHost} onChange={(e) => set({ remoteHost: e.target.value })} placeholder="127.0.0.1" size={14} />
            </label>
          )}
          {form.type !== "socks" && (
            <label>
              {form.type === "local" ? "Remote port" : "Port on server"}
              <input
                value={form.remotePort}
                onChange={(e) => set({ remotePort: e.target.value })}
                placeholder={form.type === "local" ? "required" : "free"}
                size={7}
                inputMode="numeric"
              />
            </label>
          )}
          <span className="form-buttons">
            <button type="submit" className="primary" disabled={busy}>
              {busy ? "Opening…" : "Open"}
            </button>
            <button type="button" onClick={() => (setForm(null), setError(""))}>
              Cancel
            </button>
          </span>
          <div className="help">{HELP[form.type]}</div>
          {error && <div className="help error" role="alert">{error}</div>}
        </form>
      )}
      {tunnels.length === 0 && !form && (
        <Empty title="No tunnels open">Agents open them with tunnel_open; you can too with New tunnel. Each one shows its traffic here, and you can close any of them.</Empty>
      )}
      {tunnels.length > 0 && (
        <table className="tunnels">
          <thead>
            <tr>
              <th>Type</th>
              <th>Route</th>
              <th>Status</th>
              <th>Owner</th>
              <th className="num">Conns</th>
              <th className="num">Out</th>
              <th className="num">In</th>
              <th>Idle close</th>
              <th>
                <span className="visually-hidden">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {tunnels.map((t) => (
              <Fragment key={t.id}>
              <tr className={[failureText(t) ? "has-failure" : "", t.owner.kind === "agent" ? "agent" : ""].filter(Boolean).join(" ") || undefined}>
                <td>
                  <span className={t.type === "remote" ? "chip outline-hazard" : "chip"} title={t.type === "remote" ? "Exposes a local service to the server" : undefined}>
                    {t.type}
                  </span>
                </td>
                <td className="mono route" title={tunnelRoute(t)}>{tunnelRoute(t)}</td>
                <td title={t.error}>
                  <span className={t.status === "open" ? "state-line" : "state-line warn-text"}>
                    <span className={t.status === "open" ? "state-dot" : "state-dot warn"} aria-hidden="true" />
                    {t.status}
                  </span>
                  {t.error && <div className="error-text mono">{t.error}</div>}
                </td>
                <td>
                  <Owner owner={t.owner} />
                </td>
                <td className="num mono" title={`${t.total_connections} in total`}>
                  {t.connections} <span className="muted">/ {t.total_connections}</span>
                </td>
                <td className="num mono">{formatBytes(t.bytes_out)}</td>
                <td className="num mono">{formatBytes(t.bytes_in)}</td>
                <td className="meta">{t.idle_minutes > 0 ? `after ${t.idle_minutes} min` : "never"}</td>
                <td className="row-actions">
                  <button className="ghost small danger-text" onClick={() => onClose(t)}>
                    Close
                  </button>
                </td>
              </tr>
              {failureText(t) && (
                <tr className="failure">
                  <td />
                  <td colSpan={8} title={t.last_error}>
                    {failureText(t)}
                    {t.last_error_at && <span className="muted"> ({formatTime(t.last_error_at)})</span>}
                  </td>
                </tr>
              )}
              </Fragment>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
