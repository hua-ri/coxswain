// Syncing hosts from ~/.ssh/config: the preview (ssh.hosts.import with
// dry_run), which edited hosts to overwrite, and the sentence that says
// what happened. ~/.ssh/config itself is only read.

export interface FieldChange {
  field: string;
  from: string;
  to: string;
}

export interface ImportOutput {
  path: string;
  dry_run?: boolean;
  added: { name: string; address: string }[];
  updated: { name: string; changes: FieldChange[] }[];
  unchanged: string[];
  conflicts: ({ name: string } & FieldChange)[]; // hosts edited in Coxswain: from is Coxswain's value
}

/** conflictHosts groups the conflicting fields per host, in order. */
export function conflictHosts(conflicts: ImportOutput["conflicts"]): { name: string; changes: FieldChange[] }[] {
  const out: { name: string; changes: FieldChange[] }[] = [];
  for (const { name, ...c } of conflicts) {
    let h = out.find((x) => x.name === name);
    if (!h) out.push((h = { name, changes: [] }));
    h.changes.push(c);
  }
  return out;
}

/** hasWork: syncing would change something. */
export function hasWork(p: ImportOutput, overwrite: string[]): boolean {
  return p.added.length > 0 || p.updated.length > 0 || overwrite.length > 0;
}

export function syncArgs(overwrite: string[]): Record<string, unknown> {
  return overwrite.length ? { overwrite } : {};
}

/** shown is a field value as the preview shows it. */
export function shown(v: string): string {
  return v === "" ? "(none)" : v;
}

const changeText = (c: FieldChange) => `${c.field} ${shown(c.from)} → ${shown(c.to)}`;

/** syncSummary is the notice after a sync. */
export function syncSummary(r: ImportOutput): string {
  const parts: string[] = [];
  if (r.added.length) parts.push(`Added ${r.added.length} host${r.added.length === 1 ? "" : "s"}`);
  if (r.updated.length) {
    const first = r.updated.slice(0, 3).map((u) => `${u.name}: ${u.changes.map(changeText).join(", ")}`);
    if (r.updated.length > 3) first.push(`${r.updated.length - 3} more`);
    parts.push(`${parts.length ? "updated" : "Updated"} ${r.updated.length} (${first.join("; ")})`);
  }
  const kept = conflictHosts(r.conflicts).map((c) => c.name);
  if (!parts.length && !kept.length) return `Already up to date: ${r.unchanged.length} host${r.unchanged.length === 1 ? "" : "s"} unchanged.`;
  let s = parts.length ? parts.join(", ") + "." : "Nothing added or updated.";
  if (r.unchanged.length) s += ` ${r.unchanged.length} unchanged.`;
  if (kept.length) s += ` Left ${kept.join(", ")} as edited in Coxswain.`;
  return s;
}
