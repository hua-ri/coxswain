import { describe, expect, it } from "vitest";
import { basename, crumbs, joinPath, normalizePath, parentPath, sortEntries, type FileEntry } from "./files";

describe("paths", () => {
  it("normalizes what the user types", () => {
    expect(normalizePath("")).toBe("~");
    expect(normalizePath("  ~/ ")).toBe("~");
    expect(normalizePath("logs/app")).toBe("~/logs/app");
    expect(normalizePath("/var//log/./nginx/")).toBe("/var/log/nginx");
    expect(normalizePath("/var/log/..")).toBe("/var");
    expect(normalizePath("/..")).toBe("/");
    expect(normalizePath("~/a/../..")).toBe("~/..");
  });

  it("joins and goes up", () => {
    expect(joinPath("~", "a b")).toBe("~/a b");
    expect(joinPath("/", "etc")).toBe("/etc");
    expect(joinPath("/etc", "ssh")).toBe("/etc/ssh");
    expect(joinPath("/etc/ssh", "..")).toBe("/etc");
    expect(parentPath("~/a/b")).toBe("~/a");
    expect(parentPath("~/a")).toBe("~");
    expect(parentPath("/a")).toBe("/");
    expect(parentPath("/")).toBeNull();
    // Above the login directory the absolute path is unknown: "~/..".
    expect(parentPath("~")).toBe("~/..");
    expect(parentPath("~/..")).toBe("~/../..");
  });

  it("splits into breadcrumbs", () => {
    expect(crumbs("~/a/b")).toEqual([
      { label: "~", path: "~" },
      { label: "a", path: "~/a" },
      { label: "b", path: "~/a/b" },
    ]);
    expect(crumbs("/")).toEqual([{ label: "/", path: "/" }]);
    expect(crumbs("/var/log")).toEqual([
      { label: "/", path: "/" },
      { label: "var", path: "/var" },
      { label: "log", path: "/var/log" },
    ]);
  });

  it("takes a local file's name", () => {
    expect(basename("/Users/me/report.pdf")).toBe("report.pdf");
    expect(basename("report.pdf")).toBe("report.pdf");
  });
});

describe("sortEntries", () => {
  it("puts directories first, then sorts by name", () => {
    const e = (name: string, type: FileEntry["type"]): FileEntry => ({ name, type, size: 0, mode: "", mod_time: "" });
    const out = sortEntries([e("b.txt", "file"), e("z", "dir"), e("A.txt", "file"), e("a", "dir"), e("l", "symlink")]);
    expect(out.map((x) => x.name)).toEqual(["a", "z", "A.txt", "b.txt", "l"]);
  });
});
