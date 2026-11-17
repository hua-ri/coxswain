import { describe, expect, it } from "vitest";
import {
  addArgoArgs, argoDetail, argoEditId, argoEditOf, argoErrors, argoId, argoLoginId, argoLoginOf, argoOf, blankArgo, formFromInstance, guessName,
  loginNeed, loginText, reloadsArgo, sortedInstances, updateArgoArgs, type ArgoInstance,
} from "./argo";

const inst = (p: Partial<ArgoInstance>): ArgoInstance => ({ kind: "argocd", name: "x", url: "https://x", login: { logged_in: true }, ...p });

describe("ids", () => {
  it("round-trips instance and login ids", () => {
    expect(argoOf(argoId("argocd", "prod"))).toEqual({ kind: "argocd", name: "prod" });
    expect(argoOf(argoId("argowf", "ci.v2"))).toEqual({ kind: "argowf", name: "ci.v2" });
    expect(argoOf("@host:x")).toBeNull();
    expect(argoLoginOf(argoLoginId("argowf", "a"))).toEqual({ kind: "argowf", name: "a" });
  });
});

describe("sidebar", () => {
  const list = [
    inst({ name: "dev", tags: { env: "dev" } }),
    inst({ name: "prod", tags: { env: "Production" }, login: { logged_in: false } }),
    inst({ name: "wf", kind: "argowf", login: { logged_in: false } }),
    inst({ name: "plain" }),
  ];
  it("lists one kind, production first, filtered", () => {
    expect(sortedInstances(list, "argocd").map((i) => i.name)).toEqual(["prod", "dev", "plain"]);
    expect(sortedInstances(list, "argocd", "env=dev").map((i) => i.name)).toEqual(["dev"]);
  });
  it("marks what is needed before use", () => {
    expect(loginNeed(list[1])).toBe("log in");
    expect(loginNeed(list[2])).toBe("token");
    expect(loginNeed(list[0])).toBe("");
    expect(loginNeed(inst({ login_pending: true, login: { logged_in: false } }))).toBe("waiting");
  });
});

describe("loginText", () => {
  const now = Date.parse("2026-11-13T12:00:00Z");
  it("says who, how and until when", () => {
    expect(loginText({ logged_in: true, method: "password", identity: "admin", expires_at: "2026-11-14T12:00:00Z" }, now)).toBe(
      "Logged in as admin with a password; expires in 24 h",
    );
    expect(loginText({ logged_in: true, method: "token", identity: "system:serviceaccount:argo:ops" }, now)).toBe(
      "Logged in as system:serviceaccount:argo:ops with a token; the token does not expire",
    );
    expect(loginText({ logged_in: true, method: "browser", expires_at: "2026-11-13T11:00:00Z", renewable: true }, now)).toContain("renews on next use");
    expect(loginText({ logged_in: false }, now)).toBe("Not logged in");
    expect(loginText({ logged_in: false, method: "token", rejected: true }, now)).toContain("rejected");
  });
});

describe("add form", () => {
  it("checks name, URL, namespace and port", () => {
    const f = { ...blankArgo("argowf"), name: "a b", url: "not a url", namespace: " ", ssoPort: "80" };
    const e = argoErrors(f, []);
    expect(Object.keys(e).sort()).toEqual(["name", "namespace", "ssoPort", "url"]);
    expect(argoErrors({ ...blankArgo(), name: "x", url: "argocd.example.com" }, [inst({})]).name).toContain("already exists");
    expect(argoErrors({ ...blankArgo(), name: "y", url: "https://argocd.example.com:8443/argo" }, [])).toEqual({});
  });
  it("sends only what is set", () => {
    const a = addArgoArgs({ ...blankArgo(), name: "cd", url: "https://cd", env: "prod", tags: "team=core", note: " careful ", ssoPort: "8090" });
    expect(a).toEqual({ kind: "argocd", name: "cd", url: "https://cd", tags: ["env=prod", "team=core"], note: "careful", sso_port: 8090 });
    expect(addArgoArgs({ ...blankArgo("argowf"), name: "w", url: "https://w" })).toMatchObject({ namespace: "argo" });
  });
  it("guesses a name from the host", () => {
    expect(guessName("https://argocd.example.com")).toBe("argocd");
    expect(guessName("127.0.0.1:8080")).toBe("");
  });
});

describe("edit form", () => {
  it("round-trips an instance and clears what was emptied", () => {
    const i = inst({ name: "cd", tags: { env: "prod", team: "core" }, note: "careful", k8s_context: "prod-hk", sso_port: 8090 });
    const f = formFromInstance(i);
    expect(f).toMatchObject({ env: "prod", tags: "team=core", k8sContext: "prod-hk", ssoPort: "8090" });
    expect(argoErrors(f, [i], "cd")).toEqual({});
    const a = updateArgoArgs({ ...f, name: "cd2", note: "", k8sContext: "", ssoPort: "" }, i);
    expect(a).toMatchObject({ name: "cd", new_name: "cd2", note: "", k8s_context: "", sso_port: 0, tags: ["env=prod", "team=core"] });
    expect(argoEditOf(argoEditId("argowf", "ci"))).toEqual({ kind: "argowf", name: "ci" });
  });
});

describe("activity", () => {
  it("summarises argo actions without secrets", () => {
    expect(argoDetail("argocd.sync", { app: "web", prune: true })).toBe("app/web sync prune=true");
    expect(argoDetail("argowf.retry", { name: "build-1" })).toBe("wf/build-1 retry");
    expect(argoDetail("argowf.submit", { template: "build", dry_run: true })).toBe("submit build (dry run)");
    expect(argoDetail("argocd.login", { method: "password", username: "admin", password: "<redacted 7 chars>" })).toBe("login with password as admin");
    expect(reloadsArgo("argo.add")).toBe(true);
    expect(reloadsArgo("argocd.login")).toBe(true);
    expect(reloadsArgo("argocd.sync")).toBe(false);
  });
});

describe("links", () => {
  it("point into the Argo UIs", async () => {
    const { appURL, workflowURL } = await import("./argo");
    expect(appURL({ url: "https://cd.example.com" }, { name: "web", namespace: "argocd" })).toBe("https://cd.example.com/applications/argocd/web");
    expect(appURL({ url: "https://cd.example.com/argo" }, { name: "a b" })).toBe("https://cd.example.com/argo/applications/a%20b");
    expect(workflowURL({ url: "http://localhost:2746" }, "ci", "build-1")).toBe("http://localhost:2746/workflows/ci/build-1");
    expect(workflowURL({ url: "http://localhost:2746" }, "ci")).toBe("http://localhost:2746/workflows/ci");
  });
});
