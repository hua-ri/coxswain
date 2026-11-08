// The file browser's model: remote paths and directory entries.
// Paths are what the user sees: "~" is the login directory (where SFTP
// starts), absolute paths start with "/".

export interface FileEntry {
  name: string;
  type: "file" | "dir" | "symlink" | "other";
  size: number;
  mode: string; // as ls -l shows it
  mod_time: string;
}

export interface Listing {
  host: string;
  host_note?: string;
  path: string;
  entries: FileEntry[];
  total: number;
  truncated?: boolean;
  hint?: string;
}

/** normalizePath cleans a typed path: relative paths start at "~". */
export function normalizePath(input: string): string {
  let p = input.trim();
  if (p === "" || p === "~") return "~";
  let root: "~" | "/";
  if (p.startsWith("/")) {
    root = "/";
  } else {
    root = "~";
    if (p === "~" || p.startsWith("~/")) p = p.slice(1);
  }
  const out: string[] = [];
  for (const seg of p.split("/")) {
    if (seg === "" || seg === ".") continue;
    if (seg === "..") {
      if (out.length > 0 && out[out.length - 1] !== "..") out.pop();
      else if (root === "~") out.push(".."); // above the login directory
      continue;
    }
    out.push(seg);
  }
  if (root === "/") return "/" + out.join("/");
  return out.length ? "~/" + out.join("/") : "~";
}

/** joinPath is dir/name, with ".." going up. */
export function joinPath(dir: string, name: string): string {
  if (name === "..") return parentPath(dir) ?? dir;
  return dir === "/" ? "/" + name : `${dir}/${name}`;
}

/** parentPath is the directory above p, or null at "/". */
export function parentPath(p: string): string | null {
  if (p === "/") return null;
  return normalizePath(p + "/..");
}

/** crumbs splits p into clickable parts. */
export function crumbs(p: string): { label: string; path: string }[] {
  const root = p.startsWith("/") ? "/" : "~";
  const out = [{ label: root, path: root }];
  const rest = root === "/" ? p.slice(1) : p.slice(2);
  let cur = root;
  for (const seg of rest.split("/")) {
    if (!seg) continue;
    cur = joinPath(cur, seg);
    out.push({ label: seg, path: cur });
  }
  return out;
}

/** basename is the last part of a local path. */
export function basename(p: string): string {
  const parts = p.split(/[\\/]/).filter(Boolean);
  return parts[parts.length - 1] ?? p;
}

/** sortEntries lists directories first, each group by name. */
export function sortEntries(entries: FileEntry[]): FileEntry[] {
  const rank = (e: FileEntry) => (e.type === "dir" ? 0 : 1);
  return entries.slice().sort((a, b) => rank(a) - rank(b) || (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
}
