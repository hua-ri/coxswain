// The 14px stroke icons of the shell (one set, one stroke weight).
import type { ReactNode } from "react";

const paths: Record<string, ReactNode> = {
  "@activity": <path d="M1.5 8h3l2-5 3 10 2-5h3" />,
  "@transfers": <path d="M5 2.5v11M2.5 11 5 13.5 7.5 11M11 13.5v-11M8.5 5 11 2.5 13.5 5" />,
  "@tunnels": <path d="M1.5 13.5V8a6.5 6.5 0 0 1 13 0v5.5M5 13.5V8a3 3 0 0 1 6 0v5.5" />,
  "@agents": <><circle cx="8" cy="5.5" r="2.8" /><path d="M2.5 14c.6-3 2.8-4.5 5.5-4.5s4.9 1.5 5.5 4.5" /></>,
  host: <><rect x="2" y="2.5" width="12" height="4.5" rx="1" /><rect x="2" y="9" width="12" height="4.5" rx="1" /><path d="M4.5 4.75h.01M4.5 11.25h.01" /></>,
  cluster: <><path d="M8 1.8 13.5 5v6L8 14.2 2.5 11V5z" /><path d="M8 8v6.2M8 8l5.5-3M8 8 2.5 5" /></>,
  terminal: <><rect x="1.5" y="2.5" width="13" height="11" rx="1.5" /><path d="M4.5 6l2 2-2 2M8 10h3.5" /></>,
  files: <path d="M1.8 4.2c0-.6.5-1.1 1.1-1.1h3.2l1.4 1.5h5.6c.6 0 1.1.5 1.1 1.1v6.2c0 .6-.5 1.1-1.1 1.1H2.9c-.6 0-1.1-.5-1.1-1.1z" />,
  search: <><circle cx="7" cy="7" r="4.5" /><path d="m10.5 10.5 3 3" /></>,
};

/** Icon is a 14px stroke icon from the sidebar's set. */
export function Icon({ name }: { name: string }) {
  return (
    <svg className="icon" width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
      {paths[name]}
    </svg>
  );
}
