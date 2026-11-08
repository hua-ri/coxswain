// Small display helpers shared by the panels.

/** formatBytes renders a size with binary units: "1.5 KB". */
export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${units[i]}`;
}

/** formatSpeed renders bytes per second, or "" when unknown. */
export function formatSpeed(bps: number | undefined): string {
  return bps === undefined ? "" : `${formatBytes(Math.round(bps))}/s`;
}

/** ownerLabel names who started something; the user is "you". */
export function ownerLabel(o: { kind: "human" | "agent"; name: string }): string {
  return o.kind === "agent" ? o.name : "you";
}

/** formatTime shows an ISO time as a local date and time. */
export function formatTime(iso: string | undefined): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  return d.toLocaleString([], { hour12: false, year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" });
}
