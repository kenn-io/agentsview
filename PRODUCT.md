# Product

## Register

product

## Users

agentsview is for developers who run multiple AI coding agents and need to
inspect, search, compare, and audit their local session history. They are
usually in a debugging, review, or cost-monitoring workflow where dense,
trustworthy data is more useful than promotional framing.

## Product Purpose

agentsview syncs agent session files into a local archive and serves a fast web
UI for browsing sessions, searching transcripts, tracking usage, and reviewing
costs across projects, models, and agents. Success means users can understand
what happened across their agent runs without sending data to an external
account or re-parsing raw files by hand.

## Brand Personality

Local-first, technical, focused. The interface should feel like an operational
tool: direct, compact, and calm enough for repeated daily use.

## Anti-references

Avoid marketing-site hero patterns, decorative SaaS dashboards, vague
assistant-themed illustrations, and visual treatments that make local developer
data feel like a hosted analytics product. Avoid hiding density behind oversized
cards or ornamental motion.

## Design Principles

- Put the session data first.
- Preserve local-first trust.
- Keep repeated workflows compact and predictable.
- Make freshness, filtering, and state legible.
- Prefer familiar product affordances over novelty.

## Accessibility & Inclusion

agentsview targets [WCAG 2.2](https://www.w3.org/TR/WCAG22/) level AA in light,
dark, and high-contrast themes, at every interface zoom level the app offers.

`frontend/e2e/axe-routes.spec.ts` runs axe-core against every main route with
the WCAG 2.2 A and AA rules and fails on any serious or critical violation.
`frontend/e2e/appearance-a11y.spec.ts` and `frontend/e2e/text-contrast.spec.ts`
cover zoom and contrast cases that axe cannot see. Automated checks catch only
part of WCAG, so review new controls with a keyboard and a screen reader too.

### Everywhere

- Every control is reachable with Tab and works with Enter or Space. Focus is
  always visible, and its order follows the reading order.
- A control's accessible name contains its visible text, so speech input can
  find it. Icon-only controls carry a localized `aria-label`.
- Interactive elements are never nested inside each other. A row with a
  primary action and secondary buttons makes the primary action its own link
  or button.
- Click targets are at least 24 by 24 CSS pixels, or have that much clear
  space around them. Dense rows can keep a small look by growing the hit box
  with padding and a matching negative margin.
- Text meets 4.5:1 contrast, and status marks and focus rings meet 3:1.
- Hover-only detail, such as chart tooltips, is also exposed as visible text,
  an accessible name, or a screen-reader table.
- Overlays (command palette, menus, dialogs) close with Escape.
- Transitions respect `prefers-reduced-motion`. Chart labels stay visible at
  every supported viewport size.

### Main routes

- **Sessions list:** each session row is one link to the session. Its expand
  toggle, select checkbox, and star are separate controls beside the link, and
  its actions open from the context menu. `[` and `]` move between sessions.
- **Session transcript:** the header's copy, grade, and usage controls are
  buttons named by their visible text. Long tool previews are focusable
  scrolling regions with a label.
- **Command palette** (`Ctrl+K` or `Cmd+K`): a modal dialog. The search box is
  a combobox; arrow keys move through the results, Enter opens one, and the
  right arrow reaches its copy-ID button.
- **Analytics, Usage, Activity, Trends, Quality:** rows that filter or open
  something are buttons. Charts expose their numbers as accessible names or a
  screen-reader table, and selection controls have keyboard equivalents.
- **Settings, Data, Recent edits, Pinned, Trash, Recall, Friction:** standard
  forms and tables, named by their headings.

Known gaps and their removal conditions are tracked in
[DESIGN.md](DESIGN.md#known-kit-ui-gaps-tracked-upstream).
