# Configurable Chart Palette Design

## Summary

agentsview will offer a server-wide choice between its existing categorical
palette family and a larger gray-free Matplotlib palette. The selection will be
persisted in `config.toml`, exposed through the existing Settings API, and
editable from the Appearance section without restarting the server.

The new setting applies to categorical data series. It does not change semantic
status colors, heatmaps, agent identity badges, tool-category colors, or syntax
highlighting.

## Goals

- Let an administrator select the chart palette once for every client of an
  agentsview server.
- Preserve existing theme-aware palettes and each identifier's preferred hash;
  only collided Usage fallback slots may move to enforce cross-panel parity.
- Provide as many as 36 distinct, non-gray categorical colors before cycling.
- Keep a series color consistent across its chart, legend, treemap, and
  attribution list.
- Apply a saved preference immediately in the browser that changes it and on the
  next application load for every other client.
- Keep the setting available in `config.toml` for headless administration.

## Non-goals

- User-specific or browser-specific palette preferences.
- Custom user-authored color lists.
- Changing the five-series limit or the `Other` aggregation in Cost Over Time.
- Recoloring semantic states, heatmaps, agent badges, or tool categories.
- Broadcasting palette changes to other already-open browsers over SSE. Those
  clients adopt the server-wide value when they next reload.

## Configuration and API Contract

Add a typed top-level configuration value:

```toml
chart_palette = "agentsview"
```

The accepted values are `agentsview` and `matplotlib`. An omitted value resolves
to `agentsview`. An explicitly present empty string is invalid in both TOML and
the Settings API; omission, not an empty sentinel, selects the default. This
preserves existing installations without writing a new key merely because the
server was upgraded.

Configuration loading rejects any non-empty value outside the accepted set with
an error that names `chart_palette` and the valid values. The Settings API uses
the same validation and returns HTTP 400 for an invalid update.

`GET /api/v1/settings` adds `chart_palette` to its response.
`PUT /api/v1/settings` accepts an optional `chart_palette` field. A valid update
is written through the existing locked partial-settings path, updates the
server's in-memory configuration, and is returned in the response. The
read-only-server behavior remains unchanged: updates return the existing
not-implemented error.

The generated frontend API types are regenerated from the updated Huma schema.

## Palette Behavior

### Agentsview

`agentsview` preserves each chart's existing palette and theme-aware tokens. In
particular, Usage keeps its 12-color stable name hash with deterministic
collision resolution, Skill Trend keeps its existing six categorical series
tokens, and Trends keeps its existing 12-color term palette. `Other` remains the
muted gray token.

This mode deliberately does not consolidate the chart-specific palettes. Usage
keeps the same preferred hash and linear-probing rules, but any resolved slot
may change when allocation moves from a panel-local subset to the full shared
Usage universe: an earlier collision can occupy a later identifier's preferred
slot. That tradeoff is required for cross-panel consistency.

### Matplotlib

`matplotlib` uses the qualitative palette policy and exact color values from
Matplotlib v3.10.5:

- Up to 9 active series use gray-free `tab10`.
- 10 through 18 active series use gray-free `tab20`.
- 19 through 36 active series use `tab20b` followed by gray-free `tab20c`.
- More than 36 active series cycle through the 36-color resolved palette.

The gray entries are omitted because agentsview reserves muted gray for `Other`
and de-emphasized data.

Before allocation, active identifiers are deduplicated and sorted by their
stable technical identifier. The selected family is based on that active count,
and colors are assigned in palette order. Sorting makes the result independent
of API response order. Crossing the 9/10 or 18/19 thresholds intentionally
selects a different Matplotlib family and may recolor the active set.

Usage owns one color map per grouping dimension (project, model, and agent).
Each map is computed from the full category universe in the Usage response,
before Cost Over Time applies its five-series cap. Cost Over Time and Cost
Attribution receive the same map for their selected dimension, so the same
identifier cannot change color merely because attribution renders more rows or
crosses a Matplotlib family threshold. The agentsview mode keeps its existing
12-color hash preference and collision rules, but resolves collisions against
that same shared universe for cross-panel consistency.

Every visual representation consumes its owning surface's computed color map.
Paths, bars, legend dots, treemap tiles, rails, and list fills therefore cannot
drift. A missing identifier uses the general muted fallback. In Matplotlib mode,
the synthetic `__other__` identifier also uses that fallback. In agentsview
mode, each existing surface retains its current `Other` token, including Skill
Trend's `--chart-series-other`.

### Theme Tradeoff

The Matplotlib option deliberately uses the exact source hex values instead of
theme-specific replacements. Some light entries have lower contrast on light
surfaces, and some dark entries have lower contrast on dark surfaces. This is an
accepted tradeoff for an optional fidelity-oriented palette. Color remains
supplemental: every affected chart has a matching text legend, table row, or
tooltip, and existing keyboard/hover associations remain intact. Visual
verification must confirm that marks remain discernible in light, dark, and
high-contrast modes; it must not silently alter the source hex values.

## Frontend Data Flow

The existing settings store gains a typed `chartPalette` value whose initial
value is `agentsview`. Application startup loads the Settings API so charts on
any route receive the effective server value, not only after the Settings page
has been visited. Until that request completes, charts render with the
behavior-preserving default; they reactively update if the server returns
`matplotlib`.

The Appearance section adds a shared `SegmentedControl` with two localized
choices, Agentsview and Matplotlib. Choosing an option sends a partial Settings
API update. The control reflects the returned server value, is disabled while
the update is in flight or when the server is read-only, and leaves the prior
selection in place with the existing settings error treatment if saving fails.

A shared frontend palette module owns the mode type, Matplotlib constants, and
active-series allocation. Usage components and Skill Trend request colors from
that boundary, as do the Trends line chart and term table. Existing
agentsview-mode logic remains reachable through the same boundary so consumers
do not implement mode checks independently.

All new labels and descriptions use Paraglide messages. The `en`, `zh-CN`,
`zh-TW`, `ko`, and `fr` catalogs receive identical keys.

## Error Handling

- Invalid values in `config.toml` fail configuration loading with an actionable
  validation error.
- Invalid API values return HTTP 400 and do not modify either the file or the
  in-memory configuration.
- Persistence failures use the existing internal settings error response and
  leave the prior in-memory value active.
- Frontend load failures retain the `agentsview` default.
- Frontend save failures retain the last server-confirmed selection and surface
  the existing Settings error state.

## Testing

Backend tests will cover the observable configuration and HTTP contracts:

- omitted configuration resolves to `agentsview`;
- both accepted values load from TOML;
- an explicitly empty value is rejected while omission defaults correctly;
- an invalid value is rejected;
- Settings GET returns the effective value;
- Settings PUT persists and immediately returns a valid change;
- invalid and read-only updates do not persist a change.

Frontend tests will protect behavior users can see:

- Matplotlib allocation uses the complete exact ordered gray-free 9-, 18-, and
  36-color sequences;
- the allocator selects the 18-color family at 10 and the 36-color family at 19;
- each family remains unique through its advertised capacity and cycles only
  beyond 36;
- no advertised family contains an achromatic gray entry;
- allocation is deterministic for reordered and duplicate identifiers;
- the reported colliding model names remain distinct in both modes;
- Usage paths, legends, attribution lists, and treemaps use the same identifier
  colors even when the two panels render different series counts;
- Skill Trend consumes the selected mode's colors;
- Skill Trend retains `--chart-series-other` in agentsview mode;
- Trends chart lines, points, and table indicators share the selected mode's
  colors;
- changing the Appearance control sends the server update and updates rendered
  charts;
- a failed update retains the previously confirmed selection.

Localization compilation and the normal frontend type/component checks must pass
after the catalogs and generated API types are updated. Relevant Go config and
server tests must pass before the implementation is committed.

## Rollout and Compatibility

No database migration is required. Existing configuration files omit the new key
and remain in agentsview palette mode. Selecting Matplotlib writes one top-level
TOML value. Downgrading to a version that does not know the field leaves the
value inert in the config file; upgrading again restores the selected mode.

The shared Usage universe can reassign resolved colors in the default agentsview
mode because linear-probing outcomes depend on the complete sorted identifier
set. The preferred hash function, palette, and every non-Usage chart remain
unchanged.
