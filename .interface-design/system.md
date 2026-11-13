# Coxswain desktop — design system

The decisions behind `web/src/styles.css`. The CSS custom properties at the
top of that file are the single source of truth for every value named here;
`web/src/theme.ts` mirrors the surface and ink values for xterm.js, which
cannot read CSS variables.

## Intent

**Who.** One ops engineer, on a Mac, for hours at a stretch. Next to them
two or three AI agents (Claude Code, Codex, Kimi) work the same servers
and clusters through MCP. The engineer is the coxswain: not rowing every
stroke, but watching the crew, calling corrections, and taking the tiller
when needed — jumping into an agent's terminal, closing its tunnel,
revoking it.

**What they must do.** Answer three questions at a glance, all day long:
*who* did that (me or an agent), *how dangerous* was it (read, write,
destructive), and *where* (is this production?). Then act: open a shell,
browse files, read a pod's logs, apply YAML with a diff.

**Feel.** A ship's bridge at night, not a SaaS dashboard: dense, calm,
instrument-like. Mostly graphite and paper; colour appears only when it
carries a signal, so that when red shows up it means something.

## Domain exploration

**Domain.** Coxswain and crew · the helm · the ship's log (the audit
stream, newest first) · watch and handover (taking over a terminal) ·
hazard markings and restricted zones (env=prod) · signal flags (risk) ·
mooring lines (tunnels) · cargo (transfers) · the chart (cluster browser).

**Colour world.** Graphite instrument panels; chart paper; the steel blue
of a helm display; the red of a hazard placard and of a restricted-zone
line; amber caution lamps; the green "clear" lamp; and, for the crew, a
single cool violet — a hue nothing else on the bridge uses, so a crew
member's mark can never be mistaken for a status.

**Signature — the crew rail.** Anything an agent did or owns carries a
2px violet rail on its leading edge: Activity rows, open-session rows in
the sidebar, the terminal's header strip, tunnel and transfer rows, the agent list. Your
own work carries nothing. Provenance is readable with peripheral vision,
without reading a single name. Its twin is the **hazard line**: anything
in production carries a red mark in the same place a person looks first —
a solid `env=prod` tag in tables, an outlined `prod` chip on its sidebar
row, and a red band across the top of a production pane (host, files,
cluster, terminal) — once per layer.

**Defaults rejected.**
- Coloured risk chips for all three levels (green read / orange write /
  red destructive) → *read is quiet* (outlined, grey). Reads are the
  majority of events; colouring them teaches the eye to ignore colour.
  Only write (amber tint) and destructive (solid red) light up.
- A brand accent used everywhere → *two hues with jobs*: helm blue for
  focus, selection and the primary action; crew violet for agent
  provenance. Everything else is graphite.
- Cards, shadows, pill-shaped everything → *borders-only*, 4–6px radii,
  hairline rules; one shadow in the whole app, on dialogs.
- Browser tabs for everything → a Mac source list with one selection;
  open sessions are sidebar rows that carry provenance (crew rail) and
  hazard (outlined `prod`) instead of chrome.

## Tokens

All colours are CSS custom properties on `:root`, swapped under
`prefers-color-scheme: dark` (live; xterm follows via `matchMedia`).

### Surfaces (one cool-neutral hue, lightness steps only)

| Token | Light | Dark | Use |
|---|---|---|---|
| `--hull` | `#fbfbfc` | `#16171a` | canvas: content, sidebar, rails (same surface; rules separate them) |
| `--deck` | `#f3f4f6` | `#1c1e22` | raised strips: tab bar, table headers, toolbars, status bar |
| `--bridge` | `#ffffff` | `#24262b` | overlays: dialogs (the one place with a shadow) |
| `--well` | `#eef0f3` | `#101114` | inset: inputs, code/yaml/log blocks, the dark terminal |
| `--hover` | `rgba(20,30,50,.045)` | `rgba(255,255,255,.04)` | row/button hover |
| `--select` | helm at 10% | helm at 18% | selected row, active kind |

### Ink (four levels)

| Token | Light | Dark | Use |
|---|---|---|---|
| `--ink-1` | `#16181d` | `#e7e9ee` | primary: names, values, commands |
| `--ink-2` | `#4a505c` | `#b2b7c1` | secondary: labels, body |
| `--ink-3` | `#737a87` | `#858b96` | metadata: times, addresses, counts |
| `--ink-4` | `#a4aab4` | `#5c616b` | disabled, separators in text |

### Rules (borders)

`--rule` (standard, 10% / 8%), `--rule-soft` (row dividers, 6% / 5%),
`--rule-strong` (inputs, 18% / 14%). Always translucent so they sit on
any surface.

### Signals

| Token | Light | Dark | Meaning |
|---|---|---|---|
| `--helm` | `#2563c9` | `#5b93f0` | focus ring, selection, primary action, links |
| `--crew` | `#6d4fd1` | `#a48dff` | agent provenance (rail, chip, typing dot) |
| `--hazard` | `#d1342c` | `#e5534b` | env=prod, destructive risk, failure, delete |
| `--caution` | `#b7791f` | `#e0a542` | write risk, warnings, reconnecting |
| `--clear` | `#1f8a4c` | `#4cc782` | ok / ready / running |

Each signal has `-ink` (text on canvas, contrast ≥ 4.5:1) and `-tint`
(9–16% fill for chips and row washes). Solid fills (`--hazard` with white
text) are reserved for the two things that must stop the eye: `env=prod`
and `destructive`.

### Mapping

- Risk: **read** → neutral outline chip · **write** → caution tint ·
  **destructive** → hazard solid; destructive rows also get a faint hazard
  wash.
- Provenance: **agent** → crew rail + crew chip with the agent's name ·
  **you** → plain "you" in ink-2, no mark.
- Environment: **env=prod** (or production) → solid hazard tag everywhere
  it appears; other tags are neutral outline chips. Inherited tags are
  marked `↑` with the source in a tooltip.
- Status (k8s, batch hosts, tunnels): ok → clear tint · warn → caution
  tint · fail → hazard tint (not solid: a failing pod is information, not
  a production boundary).

## Typography

SF Pro / SF Mono through `-apple-system` and `ui-monospace`. Deliberate:
this is a Mac-only tool inside WKWebView; the system faces match the
native file dialog and menus around it, have true tabular figures and
optical sizes, and cost nothing to ship under a `default-src 'self'` CSP.
The design's character comes from *what is set in mono*, not from a
display face: anything you could paste into a shell — commands, paths,
addresses, pod names, ports, times, sizes — is mono; everything a person
names (hosts, agents, labels) is sans.

Scale (ratio ≈ 1.2 from 13px, rounded to whole pixels):

| Step | Size / line | Use |
|---|---|---|
| micro | 10px/14, 600, +0.04em, uppercase | chip text in tight spots |
| caption | 11px/16, 600, +0.06em, uppercase | section labels (HOSTS), table headers |
| small | 12px/16 | metadata, chips, mono data |
| body | 13px/18 | default UI text |
| title | 15px/20, 600, −0.01em | pane titles (context, object in detail) |
| heading | 18px/24, 600, −0.015em | empty-state headings, dialog titles |

Hierarchy is weight + ink before size: value 500/ink-1, label 400/ink-2,
meta 400/ink-3. Mono is set at 12px next to 13px sans so x-heights match.
All numbers use `tabular-nums`.

## Spacing and density

Base unit 4px. Scale: 2, 4, 6, 8, 12, 16, 24, 32.

- Table rows: 28px (6px × 10px cells); detail tables 24px.
- Sidebar: 224px wide (204px under 1180px); rows one line, 26px (kind
  children 24px), 6px inset, 5px radius.
- Pane header 40px. Toolbars/filter rows: 40px. Status bar: 24px.
- K8s: list flexible · detail 40%, min 280px (kinds live in the sidebar).
- Controls: 26px (default) / 22px (small) — macOS control sizes. A desktop
  pointer tool, used at this density, cannot give every control 44px; hit
  areas are kept ≥ 22px with ≥ 4px gaps, and every control is reachable by
  keyboard.

## Radii

3px chips · 5px buttons and inputs · 6px blocks (pre, findings) ·
10px dialogs. Tabs are square. Nothing is pill-shaped except the
count badge on a tab.

## Depth

Borders-only. Surfaces step a few percent in lightness; hairline
translucent rules divide zones. The single exception is dialogs:
`0 0 0 1px var(--rule), 0 12px 32px rgba(0,0,0,.18)` (dark: ring only plus
a deeper backdrop).

## Motion

Minimal and purposeful; everything is repeated hundreds of times a day.

- No motion on tab switches, row selection, or hover (colour change only,
  80ms).
- Dialogs: 140ms opacity + scale(.98→1), `cubic-bezier(.23,1,.32,1)`.
- Live signals only: the agent-typing dot and the "running" dot pulse
  (1.2s, opacity). Progress bars move by `transform: scaleX` (300ms).
- `prefers-reduced-motion: reduce` drops the pulses and the dialog scale.

## Shell and navigation

Decided 2026-11-12 after an Impeccable critique of the old shell and three
structural comps (screenshots in the private `ui-review/directions/`).

**The old shell scored 20/40.** The sidebar mixed three jobs: places (hosts,
clusters), crew permissions (Allow/Revoke) and row commands. Hosts opened
on double-click with hover icons, clusters on a single click without them.
No sidebar row showed what the main area was displaying. The tab strip
mixed fixed views (Activity, Transfers, Tunnels) with sessions and hid
whatever overflowed. The K8s pane added a second nav column of kinds.
Production was marked four or five times on one screen. Agents were the
last, smallest rows, with nowhere to go.

**Chosen: one source list, one selection (direction A, 28/40 as a comp).**
The sidebar is a Mac source list, and the one selected row is what the main
area shows. There is no tab strip.

- **Destinations** (no header): Activity ⌘1 · Transfers ⌘2 · Tunnels ⌘3 ·
  Agents ⌘4. A live count shows on the right: running transfers as a helm
  badge, open tunnels as a plain number, allowed agents with a crew dot
  while one is typing.
- **Open**: every open session (terminals, file browsers) in the order it
  was opened. A row carries a crew rail and the agent's name when an agent
  owns it, and a hazard mark when it runs in production. Close is a hover
  or focus ×; an agent's live terminal asks first.
- **Hosts** and **Clusters**: one row shape for both. Icon, name, then on
  the right a session count (with a crew dot if an agent holds one) and the
  environment: `prod` as an outlined hazard chip, other environments as
  ink-4 text. Production comes first, then environments by name, then
  untagged places. Clicking a host opens its *host page*: address, tags,
  note, what is open there, recent activity there, and the Terminal and
  Files actions. Double-clicking a host still opens a terminal. Clicking a
  cluster opens its browser, and its resource kinds disclose beneath it as
  child rows. That replaces the K8s pane's own kinds column. Eight common
  kinds show by default; "More kinds" reveals the rest, a free-form kind
  field and Apply YAML.
- **Agents page**: one row per agent with access, last action, and what it
  holds open (terminals, tunnels, running transfers). Revoke… asks for
  confirmation and says what the agent still holds. Allow needs no
  confirmation.
- **Pane header**: every pane starts with one 40px header naming what it
  shows (title, mono meta, actions on the right; Close for sessions). The
  production band always sits above it.
- **Selection** follows macOS: a helm fill with white text while the
  sidebar has focus, a neutral fill (`--side-sel`) with ink-1 text while
  focus is in the main area. A session's place row gets no highlight of
  its own; its count tells you it is open.
- **Keyboard**: the sidebar is one Tab stop. ↑/↓/Home/End move, Return or
  Space opens, ←/→ fold a cluster's kinds, ⌘F focuses the filter, Escape
  clears it. ⌘1–⌘4 are global. ⌘W is not used, because the native Window
  menu owns it (it closes the window). Closing a session returns you to
  the previous selection, not to Activity.
- **Sidebar surface**: `--side` sits one step off the hull (`#f2f3f6` /
  `#1b1c20`). This is the Operate rule of a second neutral layer for
  navigation, and how macOS separates a source list from content. Section
  headers are 11px/600 ink-3 in sentence case ("Hosts"), as macOS source
  lists set them, rather than the uppercase caption used for table
  headers.

**Prod, once per layer.** Sidebar row: an outlined hazard chip. Pane: the
hazard band. Activity and other tables: a solid chip in the row. The old
group header, sidebar rail, tab top line and K8s context card are gone.

**Rejected.**
- *B — a destinations toolbar, a sidebar of places only, global session
  tabs (26/40).* Three controls each changed the main area, so it was
  never clear which one owned it. It also brought back the old tab-strip
  overflow, and a web-style segmented toolbar is not what Mac users expect
  for top-level navigation. Kept from it: visible ⌘1–⌘4 hints.
- *C — places own their workspaces, with local tabs per host or cluster
  (27/40).* The calmest comp, but sessions end up scattered across places,
  so watching the crew means remembering which host codex is on. Kept from
  it: session counts on place rows, and the host page listing what is open
  there.
- *Keeping env sub-groups in the sidebar.* Five grey group bands for six
  hosts, and they repeated the prod signal. Sorting plus the
  per-row env mark carries the same information.

## Component patterns

**Buttons** — `button` (secondary): 26px, 0 10px, 5px radius, deck fill,
rule-strong border, 13px/500 ink-1. `.small`: 22px, 0 8px, 12px.
`.primary`: helm fill, white. `.danger`: hazard fill, white. `.ghost`: no
fill/border until hover (row actions, sidebar actions). `.link`: helm
text. Disabled: 45% opacity. Focus: 2px helm ring, 1px offset
(`:focus-visible`).

**Inputs / selects** — 26px, well fill, rule-strong border, 5px radius,
mono for paths/ports/YAML. Selects use a drawn chevron. Checkboxes use
`accent-color: var(--helm)`.

**Source list rows** — 26px, 0 8px, 6px side inset, 5px radius; 14px
stroke icon in ink-3, name 13px ink-1 (sessions: mono 12px), a right-aligned
tail (counts in ink-3, env in ink-4, `prod` outlined hazard chip, agent
name in crew-ink). Hover = `--hover`. Selected = helm fill with white text
while the sidebar is focused, `--side-sel` otherwise. Agent-owned sessions
carry the 2px crew rail at the row's leading edge. Kind children are 24px
and indented 30px. A row's only button (close ×) shows on hover or focus.
(This replaces the tab strip, which is gone; see Shell and navigation.)

**Chips** — 18px high, 0 6px, 3px radius, 11px/500. Variants: neutral
outline (tags, read), `.prod`/`.destructive` hazard solid, `.write`
caution tint, `.agent` crew tint, `.ok/.warn/.bad` status tints.

**Tables** — sticky 28px header on deck, caption style; rows 28px with
soft dividers; numbers right-aligned tabular; long cells truncate with the
full value in `title`; clickable rows are focusable (Enter/Space) and
show hover; selected row = select tint with a 2px helm inset rail.

**Crew rail** — `box-shadow: inset 2px 0 var(--crew)` on the row/tab/strip.

**Hazard band** — 28px strip, hazard tint background, 2px hazard top
line, hazard-ink text: "Production · prod-hk · env=prod" + note. Used on
production k8s panes, production terminals, production dialogs.

**Dialogs** — `role="dialog"`, `aria-modal`, Escape cancels, focus starts
on Cancel and returns to the opener; 440px max; title 15px/600; production
dialogs open with the hazard band.

**Editors (add/edit host, add cluster)** — a pane in the main area, not a
dialog: the selection is the form (it is never returned to by history). Pane
header names it ("Add host", "Edit web-1"); sections with caption heads
(Connection · What agents are told · Connection check); a form grid with
104px right-aligned labels and controls up to 560px; hints in ink-3, errors in
hazard-ink under the control (shown after blur or a save attempt). Footer bar
on deck: Remove… (hazard-ink ghost) left, Cancel and the primary action right.
Choosing env=prod raises the hazard band above the header at once.
Environment is a segmented radio: None · staging · dev · prod, prod in
hazard-ink and solid hazard when chosen.

**Host key confirmation** — the one thing in an editor to read closely: a
deck block with the address (mono, ink-3), the key type and the SHA256
fingerprint in a well chip (13px mono, selectable), then *Trust this key*
(primary) and *Not now*. A changed key is a hazard-tint block with a hazard
border and an uppercase "Host key changed" title; it never offers *Trust*,
only *Remove old key…* (with its own confirmation) when the old entry is
Coxswain's, or the `ssh-keygen -R` command when it is the user's.

**Menus** — the Hosts "+" and row context menus: bridge surface, the dialog
shadow, 24px items, helm fill on hover/focus (as macOS menus), destructive
items in hazard-ink turning to a hazard fill. The section "+" is an 18px
ink-3 icon button at the right of the header.

**Detail pane** — header (kind label caption + name title + close), a
segmented control for overview/yaml/events/logs/diagnose, scrolling body
with caption-style section heads.

**States** — Empty: one sentence in ink-2 saying what will appear and
how to cause it, at the top-left of the area (not centered in a void).
Loading: "Loading …" in ink-3 with a short delay-free text (no spinners).
Error: a hazard-tint line with the message and a Dismiss button; banners
for app-wide problems (server stopped: hazard band, persistent; request
error: dismissable).

## Decisions

| Decision | Rationale | Date |
|---|---|---|
| Borders-only depth | Dense tool, dark mode for hours; shadows add weight without information | 2026-11-10 |
| System SF Pro / SF Mono | Native Mac tool in WKWebView; CSP forbids remote fonts; mono-vs-sans carries meaning | 2026-11-10 |
| Read risk is neutral | Most events are reads; colour reserved for write/destructive keeps it meaningful | 2026-11-10 |
| Violet only for agents | Provenance must never be confused with status or selection | 2026-11-10 |
| Solid red only for prod and destructive | The two things that must stop the eye | 2026-11-10 |
| One source list, one selection; no tab strip | The sidebar and the main area can never disagree; sessions, places and the crew sit in one scannable list (critique 20/40 → comp 28/40) | 2026-11-12 |
| Prod marked once per layer | Repetition taught the eye to skip red | 2026-11-12 |
| Add/edit forms are panes, not dialogs | Forms with a connection check need room and should not block watching the crew; dialogs stay for confirming destructive steps | 2026-11-13 |
| A changed host key is never one click from trusted | Mismatch is the MITM case; removing the old entry is its own confirmed step | 2026-11-13 |
| 26/22px controls | macOS control sizes; density over 44px touch targets on a pointer tool | 2026-11-10 |
