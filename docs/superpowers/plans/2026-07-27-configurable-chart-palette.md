# Configurable Chart Palette Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a server-wide Settings preference that switches all categorical
charts between the agentsview palette family and a gray-free Matplotlib palette
with up to 36 distinct colors.

**Architecture:** A typed top-level Go configuration value is persisted through
the existing Settings API and loaded by the existing global frontend settings
store. A focused frontend palette resolver selects either each chart's legacy
palette rules or the adaptive Matplotlib family. Usage centralizes allocation
over its full category universe, while Skill Trend and Trends each consume one
resolved map for every chart/legend representation.

**Tech Stack:** Go, BurntSushi TOML, Huma/OpenAPI, Svelte 5 runes, TypeScript,
Paraglide JS, kit-ui `SegmentedControl`, Vitest, Testing Library, and testify.

## Global Constraints

- The only persisted values are `agentsview` and `matplotlib`; omission defaults
  to `agentsview`, while an explicitly present empty string is invalid.
- `agentsview` must preserve existing chart palettes and Usage's preferred hash
  plus linear-probing rules. Resolved Usage colors may move when allocation is
  shared across both panels because probing depends on the full identifier
  set.
- `matplotlib` uses gray-free families of 9, 18, and 36 exact Matplotlib v3.10.5
  colors, selected at active-series counts 1–9, 10–18, and 19 or more.
- Empty identifiers stay muted. In Matplotlib mode, `__other__` uses the general
  muted token; in agentsview mode, each surface preserves its current `Other`
  token. More than 36 active identifiers cycle only after all 36 colors are
  used.
- The setting applies to categorical series only. Do not change heatmaps,
  semantic status colors, agent badges, tool categories, or syntax colors.
- The current browser updates from the PUT response. Other browsers adopt the
  server-wide value on their next reload; do not add an SSE configuration
  event.
- Keep all five Paraglide catalogs (`en`, `zh-CN`, `zh-TW`, `ko`, `fr`) on
  identical key sets.
- Do not edit generated Paraglide or OpenAPI files by hand; run their
  generators.
- Add no database migration, dependency, compatibility adapter, or
  custom-palette editor.
- Matplotlib uses exact, non-theme-adaptive hex values. Treat reduced contrast
  in some themes as an accepted fidelity tradeoff, retain text/tooltip
  associations, and visually verify discernibility in light, dark, and
  high-contrast modes.
- Follow TDD for each task: observe the specified test fail for the missing
  behavior before writing production code.
- Before each Go commit, run `go fmt ./...` and `go vet ./...` as required by
  the repository instructions.
- Before every commit, invoke the mandatory commit skill and never bypass hooks.

______________________________________________________________________

### Task 1: Typed configuration and TOML persistence

**Files:**

- Modify: `internal/config/config.go:451-530`
- Modify: `internal/config/config.go:684-755`
- Modify: `internal/config/config.go:990-1125`
- Modify: `internal/config/config.go:2467-2535`
- Test: `internal/config/config_test.go`
- Test: `internal/config/persistence_test.go`

**Interfaces:**

- Produces: `type ChartPalette string`

- Produces: `ChartPaletteAgentsview`, `ChartPaletteMatplotlib`, and
  `DefaultChartPalette` constants

- Produces: `ParseChartPalette(value string) (ChartPalette, error)`

- Produces: `func (c Config) ResolvedChartPalette() ChartPalette`

- Produces: `Config.ChartPalette ChartPalette` serialized as `chart_palette`

- Consumes: existing `Config.applyConfigTOML` and `Config.SaveSettings`

- [ ] **Step 1: Write failing configuration tests**

Add table-driven tests using testify. The production changes that must make
these tests fail are a missing default, a missing TOML assignment, permissive
invalid input, or persistence that does not update both file and memory.

```go
func TestChartPaletteDefaultsAndLoads(t *testing.T) {
	tests := []struct {
		name string
		toml string
		want ChartPalette
	}{
		{name: "omitted", want: ChartPaletteAgentsview},
		{name: "agentsview", toml: `chart_palette = "agentsview"`, want: ChartPaletteAgentsview},
		{name: "matplotlib", toml: `chart_palette = "matplotlib"`, want: ChartPaletteMatplotlib},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Default()
			require.NoError(t, err)
			require.NoError(t, cfg.applyConfigTOML(tt.toml))
			assert.Equal(t, tt.want, cfg.ResolvedChartPalette())
		})
	}
}

func TestChartPaletteRejectsInvalidValue(t *testing.T) {
	tests := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "unknown",
			toml: `chart_palette = "neon"`,
			want: `chart_palette must be "agentsview" or "matplotlib" (got "neon")`,
		},
		{
			name: "explicit empty",
			toml: `chart_palette = ""`,
			want: `chart_palette must be "agentsview" or "matplotlib" (got "")`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Default()
			require.NoError(t, err)
			err = cfg.applyConfigTOML(tt.toml)
			require.EqualError(t, err, tt.want)
		})
	}
}

func TestSaveSettingsPersistsChartPalette(t *testing.T) {
	dir := setupTestEnv(t)
	cfg, err := Default()
	require.NoError(t, err)
	cfg.DataDir = dir
	require.NoError(t, cfg.SaveSettings(map[string]any{
		"chart_palette": ChartPaletteMatplotlib,
	}))
	assert.Equal(t, ChartPaletteMatplotlib, cfg.ChartPalette)
	fileCfg := readConfigFile(t, dir)
	assert.Equal(t, ChartPaletteMatplotlib, fileCfg.ChartPalette)
}
```

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/config -run 'TestChartPalette|TestSaveSettingsPersistsChartPalette' -count=1
```

Expected: compilation fails because `ChartPalette`, its constants, and
`ResolvedChartPalette` do not exist.

- [ ] **Step 3: Implement the typed configuration boundary**

Add the type and exact validation near the other top-level configuration types:

```go
type ChartPalette string

const (
	ChartPaletteAgentsview ChartPalette = "agentsview"
	ChartPaletteMatplotlib ChartPalette = "matplotlib"
	DefaultChartPalette                 = ChartPaletteAgentsview
)

func ParseChartPalette(value string) (ChartPalette, error) {
	p := ChartPalette(value)
	switch p {
	case ChartPaletteAgentsview, ChartPaletteMatplotlib:
		return p, nil
	default:
		return "", fmt.Errorf(
			`chart_palette must be "agentsview" or "matplotlib" (got %q)`,
			value,
		)
	}
}

func (c Config) ResolvedChartPalette() ChartPalette {
	if c.ChartPalette == "" {
		return DefaultChartPalette
	}
	return c.ChartPalette
}
```

Add this field to `Config`:

```go
ChartPalette ChartPalette `json:"chart_palette" toml:"chart_palette"`
```

Initialize it to `DefaultChartPalette` in `Default` and decode it in the
anonymous TOML file struct. In `applyConfigTOML`, distinguish omission from an
explicit empty string with the existing TOML metadata:

```go
if meta.IsDefined("chart_palette") {
	p, err := ParseChartPalette(string(file.ChartPalette))
	if err != nil {
		return err
	}
	c.ChartPalette = p
}
```

Extend `SaveSettings`' known-key in-memory update:

```go
if v, ok := patch["chart_palette"]; ok {
	if p, ok := v.(ChartPalette); ok {
		c.ChartPalette = p
	}
}
```

Only callers that have already parsed the value may place this typed value in
the patch.

- [ ] **Step 4: Format and verify GREEN**

Run:

```bash
go fmt ./...
go vet ./...
CGO_ENABLED=1 go test -tags fts5 ./internal/config -run 'TestChartPalette|TestSaveSettingsPersistsChartPalette' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the configuration contract**

```bash
git add internal/config/config.go internal/config/config_test.go internal/config/persistence_test.go
git commit -m "feat(config): persist chart palette selection"
```

______________________________________________________________________

### Task 2: Settings API and generated client contract

**Files:**

- Modify: `internal/server/settings.go`
- Modify: `internal/server/huma_routes_settings.go:44-118`
- Modify: `internal/server/server_test.go:3420-3490`
- Regenerate: `frontend/src/lib/api/generated/models/SettingsResponse.ts`
- Regenerate: `frontend/src/lib/api/generated/models/SettingsUpdateRequest.ts`
- Regenerate: any other files changed by `npm run generate:api`

**Interfaces:**

- Consumes: `config.ParseChartPalette` and `Config.ResolvedChartPalette`

- Produces: required response field `chart_palette`

- Produces: optional PUT field `chart_palette`

- Produces: generated TypeScript fields on `SettingsResponse` and
  `SettingsUpdateRequest`

- [ ] **Step 1: Write failing Settings API tests**

Add one valid round-trip test and one invalid-update test. These exercise the
HTTP and persisted-file contracts rather than private handler state.

```go
func TestSettingsChartPaletteRoundTrip(t *testing.T) {
	te := setup(t)
	putSettings := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/settings",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://127.0.0.1:0")
		w := httptest.NewRecorder()
		te.handler.ServeHTTP(w, req)
		return w
	}

	w := te.get(t, "/api/v1/settings")
	assertStatus(t, w, http.StatusOK)
	var initial struct {
		ChartPalette config.ChartPalette `json:"chart_palette"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &initial))
	assert.Equal(t, config.ChartPaletteAgentsview, initial.ChartPalette)

	w = putSettings(`{"chart_palette":"matplotlib"}`)
	assertStatus(t, w, http.StatusOK)
	var updated struct {
		ChartPalette config.ChartPalette `json:"chart_palette"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &updated))
	assert.Equal(t, config.ChartPaletteMatplotlib, updated.ChartPalette)

	var persisted struct {
		ChartPalette config.ChartPalette `toml:"chart_palette"`
	}
	_, err := toml.DecodeFile(filepath.Join(te.dataDir, "config.toml"), &persisted)
	require.NoError(t, err)
	assert.Equal(t, config.ChartPaletteMatplotlib, persisted.ChartPalette)
}

func TestSettingsRejectInvalidChartPaletteWithoutChangingSelection(t *testing.T) {
	te := setup(t)
	putSettings := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/settings",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://127.0.0.1:0")
		w := httptest.NewRecorder()
		te.handler.ServeHTTP(w, req)
		return w
	}

	w := putSettings(`{"chart_palette":"matplotlib"}`)
	assertStatus(t, w, http.StatusOK)

	w = putSettings(`{"chart_palette":"neon"}`)
	assertStatus(t, w, http.StatusBadRequest)
	assertBodyContains(t, w, `chart_palette must be`)
	w = putSettings(`{"chart_palette":""}`)
	assertStatus(t, w, http.StatusBadRequest)
	assertBodyContains(t, w, `chart_palette must be`)

	w = te.get(t, "/api/v1/settings")
	assertStatus(t, w, http.StatusOK)
	var got struct {
		ChartPalette config.ChartPalette `json:"chart_palette"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, config.ChartPaletteMatplotlib, got.ChartPalette)
}
```

Also change the existing `TestSettingsRemainLockedInPGMode` PUT body to
`{"chart_palette":"matplotlib"}`. Its existing HTTP 501 assertion protects the
new field from bypassing the read-only guard.

- [ ] **Step 2: Run the server tests and verify RED**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/server -run 'TestSettingsChartPalette|TestSettingsRejectInvalidChartPalette' -count=1
```

Expected: the GET response has no `chart_palette`, and PUT does not persist it.

- [ ] **Step 3: Add the API fields and validation**

In `settings.go`, import `internal/config` and add:

```go
ChartPalette config.ChartPalette `json:"chart_palette"`
```

to `settingsResponse`, plus:

```go
ChartPalette *string `json:"chart_palette,omitempty"`
```

to `settingsUpdateRequest`.

In `humaGetSettings`, return `s.cfg.ResolvedChartPalette()` while holding the
existing read lock. In `humaUpdateSettings`, validate before acquiring the write
lock or constructing a persisted patch:

```go
if in.Body.ChartPalette != nil {
	p, err := config.ParseChartPalette(*in.Body.ChartPalette)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	patch["chart_palette"] = p
}
```

Keep the existing read-only guard before all updates.

- [ ] **Step 4: Regenerate the frontend API client**

From `frontend/`, run:

```bash
npm run generate:api
```

Verify the generated response has a required `chart_palette: string` and the
request has `chart_palette?: string`. Do not hand-edit generated files.

- [ ] **Step 5: Format and verify GREEN**

Run:

```bash
go fmt ./...
go vet ./...
CGO_ENABLED=1 go test -tags fts5 ./internal/server -run 'TestSettingsChartPalette|TestSettingsRejectInvalidChartPalette|TestSettingsRemainLocked' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the API contract**

```bash
git add internal/server/settings.go internal/server/huma_routes_settings.go internal/server/server_test.go frontend/src/lib/api/generated
git commit -m "feat(api): expose chart palette setting"
```

______________________________________________________________________

### Task 3: Shared frontend palette resolver

**Files:**

- Create: `frontend/src/lib/utils/chartPalette.ts`
- Create: `frontend/src/lib/utils/chartPalette.test.ts`
- Consume: `frontend/src/lib/utils/projectColor.ts`

**Interfaces:**

- Produces: `type ChartPalette = "agentsview" | "matplotlib"`

- Produces: `DEFAULT_CHART_PALETTE`

- Produces: `isChartPalette(value: unknown): value is ChartPalette`

- Produces:
  `chartSeriesColorMap(ids, palette, agentsviewColor?, agentsviewOtherColor?)`

- Consumes: `seriesColorMap(ids)` for Usage's existing allocator

- [ ] **Step 1: Write failing resolver tests**

Use literal expected colors so the test does not reproduce the allocator's
implementation. The production changes that must fail these tests are wrong
family boundaries, a gray entry, unstable input ordering, or early cycling.

```ts
import { describe, expect, it } from "vite-plus/test";
import {
  chartSeriesColorMap,
  DEFAULT_CHART_PALETTE,
  isChartPalette,
} from "./chartPalette.js";

const ids = (count: number) =>
  Array.from({ length: count }, (_, i) => `series-${String(i).padStart(2, "0")}`);

const EXPECTED_TAB20 = [
  "#1f77b4", "#aec7e8", "#ff7f0e", "#ffbb78", "#2ca02c",
  "#98df8a", "#d62728", "#ff9896", "#9467bd", "#c5b0d5",
  "#8c564b", "#c49c94", "#e377c2", "#f7b6d2", "#bcbd22",
  "#dbdb8d", "#17becf", "#9edae5",
] as const;

const EXPECTED_TAB20B_AND_TAB20C = [
  "#393b79", "#5254a3", "#6b6ecf", "#9c9ede", "#637939",
  "#8ca252", "#b5cf6b", "#cedb9c", "#8c6d31", "#bd9e39",
  "#e7ba52", "#e7cb94", "#843c39", "#ad494a", "#d6616b",
  "#e7969c", "#7b4173", "#a55194", "#ce6dbd", "#de9ed6",
  "#3182bd", "#6baed6", "#9ecae1", "#c6dbef", "#e6550d",
  "#fd8d3c", "#fdae6b", "#fdd0a2", "#31a354", "#74c476",
  "#a1d99b", "#c7e9c0", "#756bb1", "#9e9ac8", "#bcbddc",
  "#dadaeb",
] as const;

it("uses gray-free tab10 in Matplotlib order", () => {
  const colors = chartSeriesColorMap(ids(9), "matplotlib");
  expect([...colors.values()]).toEqual([
    "#1f77b4", "#ff7f0e", "#2ca02c", "#d62728", "#9467bd",
    "#8c564b", "#e377c2", "#bcbd22", "#17becf",
  ]);
});

it("uses exact families through their advertised capacities", () => {
  expect([...chartSeriesColorMap(ids(10), "matplotlib").values()])
    .toEqual(EXPECTED_TAB20.slice(0, 10));
  expect([...chartSeriesColorMap(ids(18), "matplotlib").values()])
    .toEqual(EXPECTED_TAB20);
  expect([...chartSeriesColorMap(ids(19), "matplotlib").values()])
    .toEqual(EXPECTED_TAB20B_AND_TAB20C.slice(0, 19));
  expect([...chartSeriesColorMap(ids(36), "matplotlib").values()])
    .toEqual(EXPECTED_TAB20B_AND_TAB20C);
});

it("keeps every advertised Matplotlib family gray-free", () => {
  const isAchromatic = (hex: string) => {
    const red = hex.slice(1, 3);
    const green = hex.slice(3, 5);
    const blue = hex.slice(5, 7);
    return red === green && green === blue;
  };
  for (const count of [9, 18, 36]) {
    expect([...chartSeriesColorMap(ids(count), "matplotlib").values()]
      .some(isAchromatic)).toBe(false);
  }
});

it("uses all 36 colors before cycling", () => {
  const atCapacity = chartSeriesColorMap(ids(36), "matplotlib");
  expect(new Set(atCapacity.values()).size).toBe(36);
  const overflow = chartSeriesColorMap(ids(37), "matplotlib");
  expect(overflow.get("series-36")).toBe(overflow.get("series-00"));
});

it("is stable across permutations and keeps Other muted", () => {
  const input = ["zeta", "__other__", "alpha", "zeta"];
  expect([...chartSeriesColorMap(input, "matplotlib")]).toEqual([
    ...chartSeriesColorMap([...input].reverse(), "matplotlib"),
  ]);
  expect(chartSeriesColorMap(input, "matplotlib").get("__other__"))
    .toBe("var(--text-muted)");
});

it("preserves the supplied agentsview colors", () => {
  const colors = chartSeriesColorMap(
    ["beta", "alpha"],
    DEFAULT_CHART_PALETTE,
    (_id, index) => [`legacy-a`, `legacy-b`][index]!,
  );
  expect([...colors.values()]).toEqual(["legacy-a", "legacy-b"]);
  expect(isChartPalette("agentsview")).toBe(true);
  expect(isChartPalette("matplotlib")).toBe(true);
  expect(isChartPalette("neon")).toBe(false);
});

it("preserves a surface-specific agentsview Other token", () => {
  const colors = chartSeriesColorMap(
    ["commit", "__other__"],
    "agentsview",
    () => "legacy",
    "var(--chart-series-other)",
  );
  expect(colors.get("__other__")).toBe("var(--chart-series-other)");
});
```

- [ ] **Step 2: Run the resolver test and verify RED**

From `frontend/`, run:

```bash
npm test -- src/lib/utils/chartPalette.test.ts
```

Expected: FAIL because `chartPalette.ts` does not exist.

- [ ] **Step 3: Implement the resolver with exact palettes**

Create the type guard and three constant arrays. Port the exact values:

```ts
export type ChartPalette = "agentsview" | "matplotlib";
export const DEFAULT_CHART_PALETTE: ChartPalette = "agentsview";
const MUTED = "var(--text-muted)";

const TAB10 = [
  "#1f77b4", "#ff7f0e", "#2ca02c", "#d62728", "#9467bd",
  "#8c564b", "#e377c2", "#bcbd22", "#17becf",
] as const;

const TAB20 = [
  "#1f77b4", "#aec7e8", "#ff7f0e", "#ffbb78", "#2ca02c",
  "#98df8a", "#d62728", "#ff9896", "#9467bd", "#c5b0d5",
  "#8c564b", "#c49c94", "#e377c2", "#f7b6d2", "#bcbd22",
  "#dbdb8d", "#17becf", "#9edae5",
] as const;

const TAB20B_AND_TAB20C = [
  "#393b79", "#5254a3", "#6b6ecf", "#9c9ede", "#637939",
  "#8ca252", "#b5cf6b", "#cedb9c", "#8c6d31", "#bd9e39",
  "#e7ba52", "#e7cb94", "#843c39", "#ad494a", "#d6616b",
  "#e7969c", "#7b4173", "#a55194", "#ce6dbd", "#de9ed6",
  "#3182bd", "#6baed6", "#9ecae1", "#c6dbef", "#e6550d",
  "#fd8d3c", "#fdae6b", "#fdd0a2", "#31a354", "#74c476",
  "#a1d99b", "#c7e9c0", "#756bb1", "#9e9ac8", "#bcbddc",
  "#dadaeb",
] as const;
```

Implement `chartSeriesColorMap` with this exact signature:

```ts
export function chartSeriesColorMap(
  ids: readonly string[],
  palette: ChartPalette,
  agentsviewColor?: (id: string, index: number) => string,
  agentsviewOtherColor?: string,
): ReadonlyMap<string, string>
```

For `agentsview`, deduplicate non-empty identifiers in first-seen order and use
the supplied callback. When there is no callback, use the existing
`seriesColorMap(ids)` so Usage retains its stable hash and collision resolution.
Exclude `__other__` from the active count and assign it
`agentsviewOtherColor ?? MUTED`.

For `matplotlib`, deduplicate and lexically sort non-empty, non-`__other__`
identifiers, choose the array by active count, then assign
`colors[index % colors.length]`. Add empty and `__other__` entries as `MUTED`
after allocation.

- [ ] **Step 4: Verify GREEN**

From `frontend/`, run:

```bash
npm test -- src/lib/utils/chartPalette.test.ts src/lib/utils/projectColor.test.ts
```

Expected: PASS.

- [ ] **Step 5: Commit the palette resolver**

```bash
git add frontend/src/lib/utils/chartPalette.ts frontend/src/lib/utils/chartPalette.test.ts
git commit -m "feat(frontend): add adaptive Matplotlib chart palette"
```

______________________________________________________________________

### Task 4: Server-backed Appearance preference and localization

**Files:**

- Modify: `frontend/src/lib/stores/settings.svelte.ts`
- Modify: `frontend/src/lib/stores/settings.test.ts`
- Modify: `frontend/src/lib/components/settings/AppearanceSettings.svelte`
- Modify: `frontend/src/lib/components/settings/AppearanceSettings.test.ts`
- Modify: `frontend/src/lib/components/settings/SettingsPage.test.ts`
- Modify: `frontend/messages/en.json`
- Modify: `frontend/messages/zh-CN.json`
- Modify: `frontend/messages/zh-TW.json`
- Modify: `frontend/messages/ko.json`
- Modify: `frontend/messages/fr.json`

**Interfaces:**

- Consumes: `ChartPalette`, `DEFAULT_CHART_PALETTE`, and `isChartPalette`

- Consumes: generated `SettingsResponse.chart_palette` and
  `SettingsUpdateRequest.chart_palette`

- Produces: reactive `settings.chartPalette: ChartPalette`

- Produces: server-backed Appearance `SegmentedControl`

- [ ] **Step 1: Write failing store tests**

Reset `settings.chartPalette` to `DEFAULT_CHART_PALETTE` in `beforeEach`. Add a
load test and a save/failure test using the existing generated-service mock:

```ts
it("loads the server chart palette", async () => {
  settingsService.getApiV1Settings.mockResolvedValue({
    agent_dirs: {}, chart_palette: "matplotlib", github_configured: false,
    host: "127.0.0.1", port: 8080, read_only: false,
    require_auth: false, terminal: { mode: "auto" },
  });
  await settings.load();
  expect(settings.chartPalette).toBe("matplotlib");
});

it("keeps the confirmed palette when saving fails", async () => {
  settings.chartPalette = "agentsview";
  settingsService.putApiV1Settings.mockRejectedValue(new Error("save failed"));
  await settings.save({ chart_palette: "matplotlib" });
  expect(settings.chartPalette).toBe("agentsview");
  expect(settings.error).toBe("save failed");
});
```

- [ ] **Step 2: Write the failing Appearance interaction test**

Mock `putApiV1Settings`, return a full Settings response with
`chart_palette: "matplotlib"`, click the Matplotlib radio, and assert both the
exact request and the confirmed selection:

```ts
expect(settingsService.putApiV1Settings).toHaveBeenCalledWith({
  requestBody: { chart_palette: "matplotlib" },
});
expect(getByRole("radio", { name: "Matplotlib" }).getAttribute("aria-checked"))
  .toBe("true");
```

Add a read-only assertion that both palette radio buttons are disabled.

Update every successful `getApiV1Settings` response fixture in both
`settings.test.ts` and `SettingsPage.test.ts` to include
`chart_palette: "agentsview"`, including the existing read-only-mode store test.
Those fixtures exercise the real settings store and must satisfy the new
required API response contract.

- [ ] **Step 3: Run the focused tests and verify RED**

From `frontend/`, run:

```bash
npm test -- src/lib/stores/settings.test.ts src/lib/components/settings/AppearanceSettings.test.ts src/lib/components/settings/SettingsPage.test.ts
```

Expected: FAIL because the store property and chart-color control do not exist.

- [ ] **Step 4: Implement the settings store field**

Import the palette type and helpers. Add:

```ts
chartPalette: ChartPalette = $state(DEFAULT_CHART_PALETTE);
```

to `SettingsStore`. In both successful `load` and `save`, require the server
value to pass `isChartPalette`; if it does not, throw an `Error` naming the
invalid response value so the existing catch path retains the last confirmed
palette. Assign `this.chartPalette = data.chart_palette` only after validation.

- [ ] **Step 5: Add localized Appearance copy**

Add identical keys to all five catalogs:

```json
"appearance_chart_palette": "Chart colors",
"appearance_chart_palette_agentsview": "Agentsview",
"appearance_chart_palette_matplotlib": "Matplotlib"
```

Use these translations for the label key:

- `zh-CN`: `图表配色`
- `zh-TW`: `圖表配色`
- `ko`: `차트 색상`
- `fr`: `Couleurs des graphiques`

Keep the two technical palette names untranslated. Update
`appearance_description` to mention chart colors in each language:

- `en`: `Theme, layout, chart colors, and block visibility preferences.`

- `zh-CN`: `主题、布局、图表配色和内容块可见性偏好。`

- `zh-TW`: `主題、版面配置、圖表配色與內容區塊顯示偏好。`

- `ko`: `테마, 레이아웃, 차트 색상, 블록 표시 여부 설정입니다.`

- `fr`:
  `Préférences de thème, de disposition, de couleurs des graphiques et de visibilité des blocs.`

- [ ] **Step 6: Add the shared control to Appearance**

Create a localized `$derived` options array and render a new `.option-row`:

```svelte
<SegmentedControl
  options={CHART_PALETTE_OPTIONS}
  value={settings.chartPalette}
  ariaLabel={m.appearance_chart_palette()}
  disabled={settings.saving || settings.readOnly}
  onchange={(value) => {
    if (!isChartPalette(value)) return;
    void settings.save({ chart_palette: value });
  }}
/>
```

Use the existing responsive `.option-row` rules. Do not add one-off control
chrome. The root `App.svelte` already calls `settings.load()` at startup, so do
not add a second bootstrap request.

- [ ] **Step 7: Generate localization and verify GREEN**

From `frontend/`, run:

```bash
npm run i18n:compile
npm test -- src/lib/stores/settings.test.ts src/lib/components/settings/AppearanceSettings.test.ts src/lib/components/settings/SettingsPage.test.ts
npm run check
```

Expected: PASS with matching locale keys and no Svelte type errors.

- [ ] **Step 8: Commit the server-backed preference UI**

```bash
git add frontend/src/lib/stores/settings.svelte.ts frontend/src/lib/stores/settings.test.ts frontend/src/lib/components/settings/AppearanceSettings.svelte frontend/src/lib/components/settings/AppearanceSettings.test.ts frontend/src/lib/components/settings/SettingsPage.test.ts frontend/messages
git commit -m "feat(settings): select the server chart palette"
```

______________________________________________________________________

### Task 5: Usage chart integration

**Files:**

- Create: `frontend/src/lib/utils/usageChartColors.ts`
- Create: `frontend/src/lib/utils/usageChartColors.test.ts`
- Modify: `frontend/src/lib/components/usage/UsagePage.svelte`
- Modify: `frontend/src/lib/components/usage/UsagePage.test.ts`
- Modify: `frontend/src/lib/components/usage/CostTimeSeriesChart.svelte`
- Modify: `frontend/src/lib/components/usage/CostTimeSeriesChart.test.ts`
- Modify: `frontend/src/lib/components/usage/AttributionPanel.svelte`
- Modify: `frontend/src/lib/components/usage/AttributionPanel.test.ts`

**Interfaces:**

- Consumes: `settings.chartPalette`

- Consumes: `chartSeriesColorMap(ids, palette)` with no legacy callback

- Produces: `usageChartColorMaps(summary, palette)`, containing one
  full-universe map for each `GroupBy` value

- Produces: matching colors across time-series paths/legend and attribution
  list/treemap/rail

- [ ] **Step 1: Write failing Matplotlib rendering tests**

In the new utility suite, construct a summary with these ten model totals:
`model-alpha`, `model-bravo`, `model-charlie`, `model-delta`, `model-echo`,
`model-foxtrot`, `model-golf`, `model-hotel`, `model-india`, and `model-zulu`.
Give the last five enough cost to become the Cost Over Time leaders. Assert that
`usageChartColorMaps(summary, "matplotlib").model` contains all ten identifiers
and uses the 18-color Matplotlib family. This test fails if the family is chosen
from the capped time-series list.

In the Usage page suite, return that summary from the existing API mock and
select Model for both panels. Assert `model-zulu` has the expected shared color
in both Cost Over Time and Cost Attribution:

- agentsview: `var(--accent-sky)` from the full-universe collision allocation,
  rather than the capped subset's `var(--accent-indigo)`;
- matplotlib: `#c5b0d5` from full-universe `tab20`, rather than the capped
  subset's `#9467bd` from `tab10`.

This is the cross-component regression: both assertions fail if either child
computes a local active-set map.

In the isolated Attribution child suite, pass a deliberately distinguishable
supplied color map and assert that its list rows and treemap tiles use those
exact colors. This independently fails if Attribution ignores the shared map and
recomputes a local allocation whose full totals happen to match the shared
universe.

In both child component suites, reset `settings.chartPalette = "agentsview"`
during cleanup. Add Matplotlib cases using the reported collision pair:

```ts
settings.chartPalette = "matplotlib";
// Active IDs sort as claude-opus-5, gpt-5.6-sol.
// Assert the rendered colors are #1f77b4 and #ff7f0e and that every legend or
// treemap representation equals its corresponding series color.
```

For Cost Over Time, assert SVG `fill` attributes and legend-dot styles. For
Attribution list and treemap, assert the two row/tile colors are distinct and
correspond to the exact first two Matplotlib colors after browser style
normalization. Keep the existing agentsview collision tests unchanged so the
default behavior remains protected.

Add one reactive Usage page assertion: mount in `agentsview` mode, capture the
Cost Over Time path fills and Cost Attribution dots, assign
`settings.chartPalette = "matplotlib"`, await `tick()`, and assert both panels
now use their shared Matplotlib map. This is the observable guarantee that a
successful Settings response updates open charts without a reload.

- [ ] **Step 2: Run the Usage component tests and verify RED**

From `frontend/`, run:

```bash
npm test -- src/lib/utils/usageChartColors.test.ts src/lib/components/usage/UsagePage.test.ts src/lib/components/usage/CostTimeSeriesChart.test.ts src/lib/components/usage/AttributionPanel.test.ts
```

Expected: the utility is missing, child components do not accept a shared map,
and the two Usage panels cannot maintain cross-panel color parity.

- [ ] **Step 3: Route Usage through the shared resolver**

Create `usageChartColorMaps(summary, palette)` in `usageChartColors.ts`. For
each of `project`, `model`, and `agent`, union identifiers from the
corresponding summary totals and every daily breakdown, then pass the sorted
full universe to `chartSeriesColorMap`. Return:

```typescript
export interface UsageChartColorMaps {
  project: ReadonlyMap<string, string>;
  model: ReadonlyMap<string, string>;
  agent: ReadonlyMap<string, string>;
}

export function usageChartColorMaps(
  summary: UsageSummaryResponse | null,
  palette: ChartPalette,
): UsageChartColorMaps
```

In `UsagePage.svelte`, derive these maps once from `usage.summary` and
`settings.chartPalette`. Pass the map matching
`usage.toggles.timeSeries.groupBy` to Cost Over Time and the map matching
`usage.toggles.attribution.groupBy` to Cost Attribution.

Add a required `colorMap: ReadonlyMap<string, string>` prop to both child
components. Remove their local allocator calls and use the supplied map for
paths, legend dots, treemap items, rails, and list fills. Keep `__other__` out
of the shared active universe and muted in rendering. Update isolated child
tests to pass a map created by `usageChartColorMaps`.

- [ ] **Step 4: Verify GREEN**

From `frontend/`, run:

```bash
npm test -- src/lib/utils/usageChartColors.test.ts src/lib/components/usage/UsagePage.test.ts src/lib/components/usage/CostTimeSeriesChart.test.ts src/lib/components/usage/AttributionPanel.test.ts src/lib/utils/projectColor.test.ts src/lib/utils/chartPalette.test.ts
```

Expected: PASS in both palette modes.

- [ ] **Step 5: Commit Usage integration**

```bash
git add frontend/src/lib/utils/usageChartColors.ts frontend/src/lib/utils/usageChartColors.test.ts frontend/src/lib/components/usage/UsagePage.svelte frontend/src/lib/components/usage/UsagePage.test.ts frontend/src/lib/components/usage/CostTimeSeriesChart.svelte frontend/src/lib/components/usage/CostTimeSeriesChart.test.ts frontend/src/lib/components/usage/AttributionPanel.svelte frontend/src/lib/components/usage/AttributionPanel.test.ts
git commit -m "feat(usage): honor the selected chart palette"
```

______________________________________________________________________

### Task 6: Skill Trend and Trends integration

**Files:**

- Modify: `frontend/src/lib/components/analytics/SkillTrend.svelte`
- Modify: `frontend/src/lib/components/analytics/SkillTrend.test.ts`
- Modify: `frontend/src/lib/components/trends/TrendsPage.svelte`
- Modify: `frontend/src/lib/components/trends/TrendsPage.test.ts`

**Interfaces:**

- Consumes: `settings.chartPalette`

- Consumes:
  `chartSeriesColorMap(ids, palette, agentsviewColor, agentsviewOtherColor)`

- Preserves: Skill Trend's `--chart-series-N` and Trends' `--trend-*` colors in
  agentsview mode

- Produces: one color map per active series set, shared by chart, tooltip,
  legend, and table consumers

- [ ] **Step 1: Write failing Skill Trend tests**

Reset `settings.chartPalette` around every test. Add a Matplotlib test that
mounts the existing three-skill fixture and asserts colors by series identifier,
not by palette order:

```ts
expect(legendColorBySkill).toEqual({
  commit: "#1f77b4",
  deploy: "#ff7f0e",
  review: "#2ca02c",
});
// DOM line order remains volume-ranked: commit, review, deploy.
expect(lineColors).toEqual(["#1f77b4", "#2ca02c", "#ff7f0e"]);
```

The allocation is lexical while rendering remains volume-ranked. Retain the
existing survivor-stability test and run it in Matplotlib mode too: hiding
`commit` must not repaint `review`. Extend the folded-tail test to assert that
Skill Trend's line and legend entry for `Other` still use
`var(--chart-series-other)` in agentsview mode.

- [ ] **Step 2: Write a failing Trends test**

Use a response with terms intentionally returned out of lexical order, set
`settings.chartPalette = "matplotlib"`, and assert that chart strokes and term
table swatches use the same color per term. Assert lexical identifier assignment
(`alpha` gets `#1f77b4`, `zeta` gets `#ff7f0e`) independently of response order.
Keep the current `--trend-*` agentsview test unchanged.

- [ ] **Step 3: Run both suites and verify RED**

From `frontend/`, run:

```bash
npm test -- src/lib/components/analytics/SkillTrend.test.ts src/lib/components/trends/TrendsPage.test.ts
```

Expected: new tests see the existing six- and twelve-color component palettes.

- [ ] **Step 4: Integrate Skill Trend**

Import `settings` and `chartSeriesColorMap`. Derive one map from
`allSeries.map((series) => series.key)` and supply the existing color tokens as
the legacy callback:

```ts
const colorMap = $derived(chartSeriesColorMap(
  allSeries.map((series) => series.key),
  settings.chartPalette,
  (_key, index) => `var(--chart-series-${index + 1})`,
  "var(--chart-series-other)",
));
```

Look colors up by `series.key` everywhere instead of recalculating from visible
series. The resolver keeps `__other__` on `--chart-series-other` in agentsview
mode and uses the general muted fallback in Matplotlib mode. This preserves
colors when a legend chip hides a series.

- [ ] **Step 5: Integrate Trends**

Derive active term IDs from `trends.response?.series`. Replace the direct index
lookup in `colorFor` with a shared resolved map:

```ts
const termColorMap = $derived(chartSeriesColorMap(
  (trends.response?.series ?? []).map((item) => item.term),
  settings.chartPalette,
  (_term, index) => TREND_PALETTE[index % TREND_PALETTE.length]!,
));

function colorFor(term: string, index: number): string {
  return termColorMap.get(term) ?? TREND_PALETTE[index % TREND_PALETTE.length]!;
}
```

Continue passing the same `colorFor` function to `TrendsLineChart` and
`TermTable`.

- [ ] **Step 6: Verify GREEN**

From `frontend/`, run:

```bash
npm test -- src/lib/components/analytics/SkillTrend.test.ts src/lib/components/trends/TrendsPage.test.ts
```

Expected: PASS, including survivor stability and chart/table parity.

- [ ] **Step 7: Commit remaining chart integrations**

```bash
git add frontend/src/lib/components/analytics/SkillTrend.svelte frontend/src/lib/components/analytics/SkillTrend.test.ts frontend/src/lib/components/trends/TrendsPage.svelte frontend/src/lib/components/trends/TrendsPage.test.ts
git commit -m "feat(charts): apply the configured categorical palette"
```

______________________________________________________________________

### Task 7: Full verification and clean generated state

**Files:**

- Verify only; modify a production or test file only if a preceding requirement
  is not met, then repeat that task's RED/GREEN cycle and create a focused
  commit.

**Interfaces:**

- Consumes: all prior task outputs

- Produces: a clean worktree with generated artifacts synchronized and all
  relevant checks passing

- [ ] **Step 1: Re-run generators and confirm no drift**

From `frontend/`, run:

```bash
npm run generate:api
npm run i18n:compile
git diff --exit-code -- src/lib/api/generated src/lib/paraglide
```

Expected: no diff.

- [ ] **Step 2: Run frontend verification**

From `frontend/`, run:

```bash
npm test
npm run check
npm run check:kit-ui
```

Expected: all tests and checks pass without warnings attributable to this
change.

- [ ] **Step 3: Inspect Matplotlib colors in every supported appearance mode**

Start the repository's isolated fixture server from the repository root:

```bash
bash scripts/e2e-server.sh
```

Use the T3 preview at `http://127.0.0.1:8090`. Select Matplotlib under Settings,
then inspect Usage, Skill Trend, and Trends in light, dark, and high-contrast
modes. Confirm that series marks remain discernible, and that every mark can be
identified through its text legend, table row, or tooltip. The raw hex fills and
strokes must remain the exact Matplotlib values; reduced contrast in a specific
theme is an accepted fidelity tradeoff, not a reason to substitute colors.

Stop only the fixture-server process created for this step after inspection.

- [ ] **Step 4: Run Go verification**

From the repository root, run:

```bash
go fmt ./...
go vet ./...
CGO_ENABLED=1 go test -tags fts5 ./internal/config ./internal/server
```

Expected: PASS.

- [ ] **Step 5: Inspect the final branch state**

Run:

```bash
git diff --check
git status --short
git log --oneline --decorate -8
```

Expected: no uncommitted tracked changes and the focused implementation commits
from Tasks 1–6 following the design-spec and plan commits.
