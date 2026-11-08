import { useEffect, useState } from "react";
import { cox } from "./bridge";
import type { Status } from "./cox";

export function App() {
  const [status, setStatus] = useState<Status>({ connected: false });
  useEffect(() => {
    if (!cox) return;
    void cox.status().then(setStatus);
    return cox.onStatus((p) => setStatus(p.type === "connected" ? { connected: true, ...p } : { connected: false }));
  }, []);
  if (!cox) {
    return <div className="notice">Open this page with <code>cox desktop</code>.</div>;
  }
  return (
    <div className="app">
      <footer className="statusbar">
        {status.connected ? `Coxswain ${status.version} · ${status.addr}` : "Coxswain server is not running"}
      </footer>
    </div>
  );
}
