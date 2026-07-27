# Configurable Chart Palette Design

## Summary

agentsview will offer a server-wide choice between its existing categorical
chart colors and a larger gray-free Matplotlib palette. The selection will be
persisted in `config.toml`, exposed through the existing Settings API, and
editable from the Appearance section without restarting the server.

The new setting applies to categorical data series. It does not change semantic
status colors, heatmaps, agent identity badges, tool-category colors, or syntax
highlighting.

## Goals

- Let an administrator select the chart palette once for every client of an
  agentsview server.
- Preserve the current chart appearance unless the administrator explicitly
  selects Matplotlib.
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
to `agentsview`. This preserves existing installations without writing a new key
merely because the server was upgraded.

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

`agentsview` preserves each chart's existing allocator and theme-aware tokens.
In particular, Usage keeps its 12-color stable name hash with deterministic
collision resolution, and Skill Trend keeps its existing six categorical series
tokens. Trends keeps its existing 12-color term palette. `Other` remains the
muted gray token.

This mode deliberately does not consolidate current allocators. Its purpose is
to make the new setting behavior-preserving by default.

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

Every visual representation in one component consumes the same computed color
map. Paths, bars, legend dots, treemap tiles, rails, and list fills therefore
cannot drift. A missing identifier and the synthetic `__other__` identifier use
the muted fallback.

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
- an invalid value is rejected;
- Settings GET returns the effective value;
- Settings PUT persists and immediately returns a valid change;
- invalid and read-only updates do not persist a change.

Frontend tests will protect behavior users can see:

- Matplotlib allocation uses the exact gray-free 9-color sequence;
- the allocator selects the 18- and 36-color families at the two boundaries;
- each family remains unique through its advertised capacity and cycles only
  beyond 36;
- allocation is deterministic for reordered and duplicate identifiers;
- the reported colliding model names remain distinct in both modes;
- Usage paths and legends, attribution views, and Skill Trend consume their
  selected mode's colors;
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
and retain current rendering. Selecting Matplotlib writes one top-level TOML
value. Downgrading to a version that does not know the field leaves the value
inert in the config file; upgrading again restores the selected mode.
