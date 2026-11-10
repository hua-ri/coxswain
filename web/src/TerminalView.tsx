import { useEffect, useRef } from "react";
import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { fromBase64, type Cox } from "./cox";
import type { Tab } from "./terminals";
import { terminalTheme } from "./theme";
import { Owner } from "./ui";

const darkQuery = window.matchMedia("(prefers-color-scheme: dark)");

interface Props {
  cox: Cox;
  tab: Tab;
  prod: boolean; // where it runs is tagged env=prod
  typing: boolean; // an agent is typing into it
  active: boolean;
  onExit(code: number | null): void;
}

/** TerminalView shows one shared terminal: output from the server, keystrokes to it. */
export function TerminalView({ cox, tab, prod, typing, active, onExit }: Props) {
  const { id, closed } = tab;
  const box = useRef<HTMLDivElement>(null);
  const term = useRef<Terminal | null>(null);
  const fit = useRef<FitAddon | null>(null);
  const closedRef = useRef(closed);
  closedRef.current = closed;
  const onExitRef = useRef(onExit);
  onExitRef.current = onExit;

  useEffect(() => {
    const t = new Terminal({
      fontFamily: 'ui-monospace, "SF Mono", SFMono-Regular, Menlo, monospace',
      fontSize: 12,
      lineHeight: 1.2,
      cursorBlink: true,
      scrollback: 5000,
      theme: terminalTheme(darkQuery.matches),
    });
    const f = new FitAddon();
    t.loadAddon(f);
    t.open(box.current!);
    term.current = t;
    fit.current = f;

    // The frame around the terminal takes its background, live.
    const theme = () => {
      const th = terminalTheme(darkQuery.matches);
      t.options.theme = th;
      box.current!.style.background = th.background ?? "";
    };
    theme();
    darkQuery.addEventListener("change", theme);
    const input = t.onData((d) => {
      if (!closedRef.current) void cox.input(id, d).catch(() => {});
    });
    const resize = t.onResize(({ cols, rows }) => {
      if (!closedRef.current) void cox.resize(id, cols, rows).catch(() => {});
    });
    const observer = new ResizeObserver(() => {
      if (box.current && box.current.offsetWidth > 0) f.fit();
    });
    observer.observe(box.current!);

    let stop: (() => void) | undefined;
    let disposed = false;
    void cox
      .subscribe("terminal", id, (p) => {
        switch (p.type) {
          case "data":
            if (p.backlog) t.reset();
            t.write(fromBase64(p.data));
            break;
          case "gap":
            t.write("\r\n\x1b[2m[some output was skipped]\x1b[0m\r\n");
            break;
          case "exit":
            t.write(`\r\n\x1b[2m[terminal ended${p.code === null ? "" : ` with exit code ${p.code}`}]\x1b[0m\r\n`);
            onExitRef.current(p.code);
            break;
        }
      })
      .then((s) => (disposed ? s() : (stop = s)));

    return () => {
      disposed = true;
      stop?.();
      observer.disconnect();
      input.dispose();
      resize.dispose();
      darkQuery.removeEventListener("change", theme);
      t.dispose();
    };
  }, [cox, id]);

  useEffect(() => {
    if (!active) return;
    // Size it now that it is visible, and take the keyboard.
    requestAnimationFrame(() => {
      fit.current?.fit();
      term.current?.focus();
    });
  }, [active]);

  const agent = tab.owner.kind === "agent";
  const cls = ["term-head", agent ? "agent" : "", prod ? "prod" : ""].filter(Boolean).join(" ");
  return (
    <div className="term-pane" style={{ display: active ? "flex" : "none" }}>
      <div className={cls}>
        {prod && <span className="chip prod">env=prod</span>}
        <span className="where mono">{tab.host}</span>
        <span>opened by</span>
        <Owner owner={tab.owner} />
        <span className="spacer" />
        {typing && (
          <span className="typing">
            <span className="pulse" aria-hidden="true" />
            {tab.owner.name} is typing
          </span>
        )}
        {closed && <span className="muted">ended{tab.exitCode !== undefined ? ` · exit ${tab.exitCode}` : ""}</span>}
        {!closed && agent && !typing && <span className="muted">shared — you can type here too</span>}
      </div>
      <div className="terminal" ref={box} />
    </div>
  );
}
