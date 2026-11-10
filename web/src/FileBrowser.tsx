import { useCallback, useEffect, useRef, useState } from "react";
import type { Cox } from "./cox";
import { Confirm } from "./Confirm";
import { basename, crumbs, joinPath, normalizePath, parentPath, sortEntries, type FileEntry, type Listing } from "./files";
import { formatBytes, formatTime } from "./format";
import { activate } from "./keys";
import { Empty, HazardBand, Message } from "./ui";
import type { Transfer } from "./transfers";

interface Props {
  cox: Cox;
  host: string;
  prod: boolean; // the host is tagged env=prod
  note?: string; // the host's note
  active: boolean;
}

/** typeMark marks an entry's kind like ls -F. */
function typeMark(e: FileEntry): string {
  return e.type === "dir" ? "/" : e.type === "symlink" ? "@" : "";
}

/** FileBrowser lists a host's files over SFTP (as the user, "desktop"). */
export function FileBrowser({ cox, host, prod, note, active }: Props) {
  const [path, setPath] = useState("~");
  const [typed, setTyped] = useState("~");
  const [listing, setListing] = useState<Listing | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [hidden, setHidden] = useState(false);
  const [folder, setFolder] = useState<string | null>(null); // new folder name being typed
  const [removing, setRemoving] = useState<FileEntry | null>(null);
  const [opened, setOpened] = useState(false);
  const current = useRef(path);
  current.current = path;

  const load = useCallback(
    async (p: string) => {
      setLoading(true);
      try {
        const out = await cox.invoke<Listing>("file.list", { host, path: p, all: hidden });
        if (current.current !== p) return;
        if (out.status !== "ok") {
          setError(out.error ?? "cannot list this directory");
          setListing(null);
        } else {
          setError("");
          setListing(out.result ?? null);
        }
      } catch (e) {
        if (current.current === p) setError(e instanceof Error ? e.message : String(e));
      } finally {
        if (current.current === p) setLoading(false);
      }
    },
    [cox, host, hidden],
  );

  useEffect(() => {
    if (active) setOpened(true);
  }, [active]);
  useEffect(() => {
    if (opened) void load(path);
  }, [opened, path, load]);

  const go = (p: string) => {
    const n = normalizePath(p);
    setTyped(n);
    setNotice("");
    if (n === path) void load(n);
    else setPath(n);
  };

  const run = async <T,>(id: string, args: Record<string, unknown>): Promise<T | undefined> => {
    try {
      const out = await cox.invoke<T>(id, { host, ...args });
      if (out.status === "ok") return out.result;
      setError(out.error ?? `${id} failed`);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
    return undefined;
  };

  const download = (e: FileEntry) => {
    setNotice(`Downloading ${e.name}… (see Transfers)`);
    void run<Transfer>("file.download", { remote_path: joinPath(path, e.name) }).then((t) => {
      if (t) setNotice(`Downloaded ${e.name} to ${t.local_path}`);
      else setNotice("");
    });
  };

  const upload = async () => {
    let local: string;
    try {
      local = await cox.pickFile();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      return;
    }
    if (!local) return;
    const dir = path;
    const remote = joinPath(dir, basename(local));
    setNotice(`Uploading ${basename(local)}… (see Transfers)`);
    const t = await run<Transfer>("file.upload", { local_path: local, remote_path: remote });
    setNotice(t ? `Uploaded ${basename(local)} to ${remote}` : "");
    if (t && current.current === dir) void load(dir);
  };

  const mkdir = async (name: string) => {
    setFolder(null);
    if (!name.trim()) return;
    if (await run("file.mkdir", { path: joinPath(path, name.trim()), parents: false }) !== undefined) void load(path);
  };

  const remove = async (e: FileEntry) => {
    setRemoving(null);
    if (await run("file.remove", { path: joinPath(path, e.name) }) !== undefined) void load(path);
  };

  const parent = parentPath(path);
  const entries = listing ? sortEntries(listing.entries) : [];
  const where = crumbs(path);
  const enter = (e: FileEntry) => (e.type === "dir" || e.type === "symlink" ? () => go(joinPath(path, e.name)) : undefined);
  return (
    <div className="pane files" style={{ display: active ? "flex" : "none" }}>
      {prod && <HazardBand env="env=prod" where={host} note={note} />}
      <div className="toolbar">
        <nav className="crumbs" aria-label="Path">
          {where.map((c, i) => (
            <span key={c.path}>
              {i > 1 && <span className="sep">/</span>}
              <button className={i === where.length - 1 ? "link here" : "link"} onClick={() => go(c.path)}
                aria-current={i === where.length - 1 ? "location" : undefined}>
                {c.label}
              </button>
            </span>
          ))}
        </nav>
        <form
          className="goto"
          onSubmit={(e) => {
            e.preventDefault();
            go(typed);
          }}
        >
          <input value={typed} onChange={(e) => setTyped(e.target.value)} spellCheck={false} aria-label="Go to a path" title="Go to a path (Enter)" />
        </form>
        <label className="check">
          <input type="checkbox" checked={hidden} onChange={(e) => setHidden(e.target.checked)} /> Hidden files
        </label>
        <button className="small" onClick={() => go(path)} disabled={loading}>
          {loading ? "Loading…" : "Refresh"}
        </button>
        <button className="small" onClick={() => setFolder("")}>
          New folder
        </button>
        <button className="small primary" onClick={() => void upload()}>
          Upload…
        </button>
      </div>
      {listing?.host_note && !prod && <Message kind="note">Note: {listing.host_note}</Message>}
      {error && (
        <Message kind="error" onDismiss={() => setError("")}>
          {error}
        </Message>
      )}
      {notice && (
        <Message kind="notice" onDismiss={() => setNotice("")}>
          <span className="mono">{notice}</span>
        </Message>
      )}
      {listing?.truncated && <Message kind="note">{listing.hint}</Message>}
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th className="num">Size</th>
              <th>Mode</th>
              <th>Modified</th>
              <th>
                <span className="visually-hidden">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {folder !== null && (
              <tr>
                <td colSpan={5}>
                  <form
                    className="inline-form"
                    onSubmit={(e) => {
                      e.preventDefault();
                      void mkdir(folder);
                    }}
                  >
                    <input
                      autoFocus
                      placeholder="folder name"
                      aria-label="New folder name"
                      value={folder}
                      onChange={(e) => setFolder(e.target.value)}
                      onKeyDown={(e) => e.key === "Escape" && setFolder(null)}
                    />
                    <button className="small primary" type="submit">
                      Create
                    </button>
                    <button className="small" type="button" onClick={() => setFolder(null)}>
                      Cancel
                    </button>
                  </form>
                </td>
              </tr>
            )}
            {parent !== null && (
              <tr className="clickable" tabIndex={0} onClick={() => go(parent)} onKeyDown={activate(() => go(parent))} title="Up one directory">
                <td className="mono file-name dir" colSpan={5}>
                  ..
                </td>
              </tr>
            )}
            {entries.map((e) => {
              const open = enter(e);
              return (
              <tr
                key={e.name}
                className={open ? "clickable" : undefined}
                tabIndex={open ? 0 : undefined}
                onClick={open}
                onKeyDown={open && activate(open)}
              >
                <td className={e.type === "dir" ? "mono file-name dir" : "mono file-name"}>
                  {e.name}
                  <span className="file-mark">{typeMark(e)}</span>
                </td>
                <td className="num mono">{e.type === "dir" ? "" : formatBytes(e.size)}</td>
                <td className="mono meta">{e.mode}</td>
                <td className="meta">{formatTime(e.mod_time)}</td>
                <td className="row-actions" onClick={(ev) => ev.stopPropagation()}>
                  {e.type !== "dir" && (
                    <button className="ghost small" onClick={() => download(e)}>
                      Download
                    </button>
                  )}
                  <button className="ghost small danger-text" onClick={() => setRemoving(e)}>
                    Delete
                  </button>
                </td>
              </tr>
              );
            })}
          </tbody>
        </table>
        {listing && entries.length === 0 && !loading && <Empty title="Empty directory">Upload a file or create a folder here.</Empty>}
        {loading && (!listing || entries.length === 0) && <div className="loading">Loading {path}…</div>}
        {!listing && !loading && error && <Empty title="Cannot list this directory">Fix the path above, or go up a level.</Empty>}
      </div>
      {removing && (
        <Confirm
          message={`Delete ${joinPath(path, removing.name)} on ${host}? ${
            removing.type === "dir" ? "Only an empty directory can be deleted." : "This cannot be undone."
          }`}
          confirm="Delete"
          band={prod ? <HazardBand env="env=prod" where={host} /> : undefined}
          onYes={() => void remove(removing)}
          onNo={() => setRemoving(null)}
        />
      )}
    </div>
  );
}
