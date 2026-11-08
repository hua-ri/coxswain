import { useEffect, useRef } from "react";
import { FitAddon } from "@xterm/addon-fit";
import { Terminal, type ITheme } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { fromBase64, type Cox } from "./cox";

const light: ITheme = { background: "#ffffff", foreground: "#1d1d1f", cursor: "#1d1d1f", selectionBackground: "#b4d5fe" };
const dark: ITheme = { background: "#1e1e1e", foreground: "#e8e8ed", cursor: "#e8e8ed", selectionBackground: "#3a5a8c" };
const darkQuery = window.matchMedia("(prefers-color-scheme: dark)");

interface Props {
  cox: Cox;
  id: string;
  active: boolean;
  closed: boolean;
  onExit(code: number | null): void;
}

/** TerminalView shows one shared terminal: output from the server, keystrokes to it. */
export function TerminalView({ cox, id, active, closed, onExit }: Props) {
  const box = useRef<HTMLDivElement>(null);
  const term = useRef<Terminal | null>(null);
  const fit = useRef<FitAddon | null>(null);
  const closedRef = useRef(closed);
  closedRef.current = closed;
  const onExitRef = useRef(onExit);
  onExitRef.current = onExit;

  useEffect(() => {
    const t = new Terminal({
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
      fontSize: 12,
      cursorBlink: true,
      scrollback: 5000,
      theme: darkQuery.matches ? dark : light,
    });
    const f = new FitAddon();
    t.loadAddon(f);
    t.open(box.current!);
    term.current = t;
    fit.current = f;

    const theme = () => (t.options.theme = darkQuery.matches ? dark : light);
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

  return <div className="terminal" ref={box} style={{ display: active ? "block" : "none" }} />;
}
