// Terminal colors for the system light and dark appearance. They mirror
// the tokens in styles.css (xterm.js cannot read CSS variables): the
// light terminal sits on --hull, the dark one on --well, text is --ink-1,
// and the ANSI colors are the signal hues (hazard, clear, caution, helm,
// crew) tuned to stay readable on that background.
import type { ITheme } from "@xterm/xterm";

const light: ITheme = {
  background: "#fbfbfc", // --hull
  foreground: "#16181d", // --ink-1
  cursor: "#16181d",
  cursorAccent: "#fbfbfc",
  selectionBackground: "#c9daf6", // --helm at ~22% on --hull
  black: "#16181d",
  red: "#b42a23", // --hazard-ink
  green: "#1a7340", // --clear-ink
  yellow: "#8a5a00", // --caution-ink
  blue: "#1f56b3",
  magenta: "#5a3fc0", // --crew-ink
  cyan: "#0f6f7c",
  white: "#a4aab4", // --ink-4
  brightBlack: "#737a87", // --ink-3
  brightRed: "#d1342c",
  brightGreen: "#1f8a4c",
  brightYellow: "#a8700c",
  brightBlue: "#2563c9",
  brightMagenta: "#6d4fd1",
  brightCyan: "#168494",
  brightWhite: "#4a505c", // --ink-2: "bright white" must stay visible on a light ground
};

const dark: ITheme = {
  background: "#101114", // --well
  foreground: "#e7e9ee", // --ink-1
  cursor: "#e7e9ee",
  cursorAccent: "#101114",
  selectionBackground: "#2a3c5e", // --helm at ~30% on --well
  black: "#24262b",
  red: "#f0716a",
  green: "#4cc782", // --clear
  yellow: "#e0a542", // --caution
  blue: "#5b93f0", // --helm
  magenta: "#a48dff", // --crew
  cyan: "#4fbfcc",
  white: "#b2b7c1", // --ink-2
  brightBlack: "#6b717c",
  brightRed: "#ff8f88",
  brightGreen: "#74dba0",
  brightYellow: "#f0c26a",
  brightBlue: "#86b0f6",
  brightMagenta: "#c2b3ff",
  brightCyan: "#7dd6e0",
  brightWhite: "#f5f6f8",
};

export function terminalTheme(isDark: boolean): ITheme {
  return isDark ? dark : light;
}
