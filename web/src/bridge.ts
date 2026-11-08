import { createCox, inDesktop, type Bindings, type Cox } from "./cox";

/** The bridge, or null when the page runs outside `cox desktop` (vite dev). */
export const cox: Cox | null = inDesktop()
  ? createCox(window as unknown as Bindings, window as unknown as { __cox?: unknown })
  : null;
