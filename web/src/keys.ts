// Keyboard helpers for the hand-made controls (clickable table rows).
// The sidebar's arrow keys are nav.ts moveIndex.
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
