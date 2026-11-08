// Terminal colors for the system light and dark appearance.
import type { ITheme } from "@xterm/xterm";

const light: ITheme = {
  background: "#ffffff",
  foreground: "#1d1d1f",
  cursor: "#1d1d1f",
  cursorAccent: "#ffffff",
  selectionBackground: "#b4d5fe",
  black: "#1d1d1f",
  red: "#c4302b",
  green: "#2a7d2e",
  yellow: "#946b00",
  blue: "#1f5fbf",
  magenta: "#a03ca0",
  cyan: "#1a7f8e",
  white: "#bfbfc4",
  brightBlack: "#6e6e73",
  brightRed: "#e0443e",
  brightGreen: "#3a9a3f",
  brightYellow: "#b38600",
  brightBlue: "#3a7be0",
  brightMagenta: "#bf55bf",
  brightCyan: "#2a9aab",
  brightWhite: "#e8e8ed",
};

const dark: ITheme = {
  background: "#1e1e1e",
  foreground: "#e8e8ed",
  cursor: "#e8e8ed",
  cursorAccent: "#1e1e1e",
  selectionBackground: "#3a5a8c",
  black: "#2a2a2c",
  red: "#ff6b63",
  green: "#5fd068",
  yellow: "#e8c24a",
  blue: "#5c9dff",
  magenta: "#d77fe0",
  cyan: "#4fc6d6",
  white: "#d0d0d5",
  brightBlack: "#7c7c82",
  brightRed: "#ff8a83",
  brightGreen: "#7fe087",
  brightYellow: "#f5d670",
  brightBlue: "#80b4ff",
  brightMagenta: "#e6a0ec",
  brightCyan: "#76d8e5",
  brightWhite: "#ffffff",
};

export function terminalTheme(isDark: boolean): ITheme {
  return isDark ? dark : light;
}
