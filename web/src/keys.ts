// Keyboard helpers for the hand-made controls: clickable table rows and
// the tab strip.
import type { KeyboardEvent } from "react";

/** activate makes an element act on Enter and Space like a button. */
export function activate(fn: () => void) {
  return (e: KeyboardEvent) => {
    if (e.target !== e.currentTarget) return; // a button inside the row handles its own keys
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      fn();
    }
  };
}

/** stepIndex is where an arrow/Home/End key moves focus in a list of n, or -1. */
export function stepIndex(key: string, i: number, n: number): number {
  if (n === 0) return -1;
  switch (key) {
    case "ArrowRight":
    case "ArrowDown":
      return (i + 1) % n;
    case "ArrowLeft":
    case "ArrowUp":
      return (i - 1 + n) % n;
    case "Home":
      return 0;
    case "End":
      return n - 1;
  }
  return -1;
}
