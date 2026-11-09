import { Fragment, useState } from "react";
import { formatBytes, formatTime, ownerLabel } from "./format";
import { failureText, tunnelRequest, tunnelRoute, type Tunnel, type TunnelForm } from "./tunnels";
import type { Host } from "./types";

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
      <div className="panel-head">
        <span className="muted">
          {tunnels.length} tunnel{tunnels.length === 1 ? "" : "s"} open
        </span>
        {!form && (
          <button
            className="small"
            onClick={() => setForm({ host: hosts[0]?.name ?? "", type: "local", localPort: "", remoteHost: "", remotePort: "" })}
          >
            New tunnel
          </button>
        )}
      </div>
      {form && (
        <form
          className="tunnel-form"
          onSubmit={(e) => {
            e.preventDefault();
            submit();
          }}
        >
          <label>
            Host
            <select value={form.host} onChange={(e) => set({ host: e.target.value })}>
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
              />
            </label>
          )}
          <button type="submit" disabled={busy}>
            Open
          </button>
          <button type="button" onClick={() => (setForm(null), setError(""))}>
            Cancel
          </button>
          <div className="muted help">{HELP[form.type]}</div>
          {error && <div className="error help">{error}</div>}
        </form>
      )}
      {tunnels.length === 0 && !form && <p className="muted">No tunnels. Agents open them with tunnel_open; you can too.</p>}
      {tunnels.length > 0 && (
        <table>
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
              <th />
            </tr>
          </thead>
          <tbody>
            {tunnels.map((t) => (
              <Fragment key={t.id}>
              <tr className={failureText(t) ? "has-failure" : undefined}>
                <td>
                  <span className={t.type === "remote" ? "badge risk-destructive" : "badge"}>{t.type}</span>
                </td>
                <td className="mono">{tunnelRoute(t)}</td>
                <td className={t.status === "open" ? "" : "error"} title={t.error}>
                  {t.status}
                  {t.error ? `: ${t.error}` : ""}
                </td>
                <td>
                  <span className={t.owner.kind === "agent" ? "badge agent" : "badge"}>{ownerLabel(t.owner)}</span>
                </td>
                <td className="num" title={`${t.total_connections} in total`}>
                  {t.connections} / {t.total_connections}
                </td>
                <td className="num">{formatBytes(t.bytes_out)}</td>
                <td className="num">{formatBytes(t.bytes_in)}</td>
                <td className="muted">{t.idle_minutes > 0 ? `after ${t.idle_minutes} min` : "never"}</td>
                <td className="row-actions">
                  <button className="small" onClick={() => onClose(t)}>
                    Close
                  </button>
                </td>
              </tr>
              {failureText(t) && (
                <tr className="failure">
                  <td />
                  <td colSpan={8} className="error" title={t.last_error}>
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
