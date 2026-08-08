# Recall Generated Insights and Quality Split

## Summary

AgentsView currently combines deterministic session-quality analysis and saved
LLM-generated reports on the **Insights** page. Recall now provides the clearer
home for generated, durable artifacts, while deterministic recommendations and
quality patterns have a distinct purpose.

This change will:

- make **Recall** a two-tab workspace containing **Corpus** and **Generated
  insights**;
- move the generated archive and generation workflow into Recall;
- rename the remaining deterministic page and route to **Quality**; and
- update navigation, deep links, screenshots, and documentation as a clean
  break from the old `/insights` route.

The split changes frontend organization only. It does not rename the persisted
insight entity or the `/api/v1/insights` backend API.

## Goals

- Put distilled Recall entries and saved model-generated reports under one
  understandable top-level destination.
- Keep the Recall corpus browser as the clean, expandable table users already
  approved.
- Give generated reports an explicit, visible input scope instead of inheriting
  hidden filters from another page.
- Give deterministic recommendations and quality patterns an accurate
  **Quality** name and a focused page.
- Preserve saved generated-insight access when generation is disabled or the
  Recall corpus backend is unavailable.
- Mount only the active Recall tab so inactive surfaces do no polling or archive
  reads.

## Non-goals

- Renaming insight database tables, Go types, stores, or HTTP API paths.
- Changing generation prompts, templates, model execution, streaming, storage,
  export, publishing, or deletion semantics.
- Adding a legacy `/insights` alias or redirect.
- Adding new Recall extraction, review, force-retirement, or model-run controls.
- Redesigning deterministic quality scoring.
- Making Recall corpus queries available on PostgreSQL or DuckDB.

## Information architecture

### Recall workspace

`/recall` becomes a workspace shell with URL-backed tabs:

- **Corpus** is the default when the Recall corpus is supported.
- **Generated insights** uses `?tab=generated`.

The tab selection belongs in the URL so refresh, back/forward navigation, copied
links, and direct entry all restore the same surface. An absent or unknown `tab`
value selects the first available tab unless a valid `insight` parameter is
present. In that case, the saved-report deep link implies the Generated insights
tab.

Only the selected panel mounts. The corpus panel therefore does not refresh or
poll extraction state while Generated insights is open, and the generated
archive does not load while Corpus is open.

### Quality page

`/quality` replaces `/insights`. It contains only:

- deterministic next-action recommendations;
- quality summary and score distribution;
- quality-pattern rows and their session evidence; and
- the existing date, project, agent, and session-scope controls used for those
  analytics.

The top-level desktop and mobile navigation label becomes **Quality**. The old
`insights` route is removed from the frontend router. Navigating to `/insights`
therefore resolves to the default Sessions route. Query parameters shared with
Sessions, including `window_days`, `date_from`, and `date_to`, continue to be
interpreted by Sessions through the router's existing unknown-route behavior.
No compatibility alias, redirect, or special handling for the former route is
introduced.

## Capability behavior

The two Recall tabs have different backend requirements and must be gated
independently.

- **Corpus** availability reuses the frontend's existing read-only derivation:
  `sync.readOnly || settings.readOnly`. Corpus is available when that expression
  is false and backend state is known. No new server capability is added. This
  keeps Corpus unavailable for current PostgreSQL and DuckDB read-only services.
- **Generated insights** remains available anywhere the existing insight archive
  is readable.
- The Recall top-level destination remains visible when at least one child tab
  is available.
- If Corpus is unavailable, `/recall` defaults to Generated insights and omits
  the Corpus tab rather than making the whole workspace unavailable.
- Insight-generation capability controls only the generation form. A disabled
  generator never hides already saved reports.

This preserves a practical capability of the current Insights page: read-only
backends can continue browsing stored generated reports even though they cannot
run a generator or query the Recall corpus.

## Component boundaries

### `RecallPage`

`RecallPage` becomes the small workspace shell. It:

- derives the selected tab from `router.params.tab`;
- derives available tabs from backend capabilities;
- writes tab changes through the router; and
- mounts exactly one child panel.

It does not own corpus queries, extraction status, generated archive state, or
generation inputs.

### `RecallCorpusPanel`

The current Recall page implementation moves into a corpus child panel without
changing its behavior. It continues to own:

- entry search, filtering, cursor pagination, and refresh;
- the expandable fact table and evidence actions;
- extraction coverage and progress; and
- safe generation activation and retirement.

### `GeneratedInsightsPanel`

The generated section of the current Insights page becomes a focused child
panel. It owns:

- visible scope and generation controls;
- loading and selecting saved reports;
- in-flight generation tasks, retry, and dismissal;
- rendered report detail;
- export, publish, copy-link, and delete actions; and
- route selection through the `insight` query parameter.

It may continue to use the existing `insights` store and insight API. Store and
API names describe the persisted resource and are not user-facing navigation
labels.

The visible generation scope remains in the `insights` store rather than local
panel state. It therefore survives panel unmounts and can be seeded by activity
or session entry points before navigation mounts the Generated insights panel.

### `QualityPage`

The remaining Insights component is renamed to `QualityPage`. It retains the
analytics store, date-yoke behavior, refresh interval, quality calculations,
and evidence navigation. It no longer imports or loads the generated-insight
store.

The retained analytics date owner changes from `insights` to `quality` so the
in-memory date-yoke state follows the renamed deterministic page and is not
mistaken for Generated insights scope.

## Generated insight scope

Every generated report must show the scope that will be sent to the generator.
The form contains:

- **Date range**;
- **Project**;
- **Session agent**;
- **Session scope** for human, automated, or both;
- **Template**;
- **Generator** agent; and
- **Optional focus**.

The labels must distinguish the session-agent filter from the agent executable
that generates the report.

These visible inputs are the complete source of truth for a canned generation
request. The panel must not silently inherit machine, termination, minimum
message, recent-activity, one-shot, or automation filters from the Sessions or
Quality pages. Backend defaults apply to dimensions not represented by the
form.

The client timezone continues to be sent with every generation request as
request context. It is not a session-scope filter and does not need a visible
form control; preserving it keeps date bucketing aligned with the user's local
calendar.

The generated archive continues to display all saved model-generated insight
types, including daily activity, agent analysis, single-session analysis, and
canned reports. Moving the generation form does not migrate or filter stored
rows.

## Navigation and deep links

- Selecting the generated tab produces `/recall?tab=generated`.
- Selecting or copying a saved report produces
  `/recall?tab=generated&insight=<id>`.
- A valid `insight` ID is selected after the archive loads.
- A valid `insight` parameter implies the Generated insights tab when `tab` is
  absent or unknown.
- A missing, malformed, or absent ID leaves the archive unselected without
  blocking the rest of the page.
- Selecting an in-flight task removes the saved `insight` parameter.
- Starting single-session analysis from a session starts and selects the task,
  then navigates to the Generated insights tab.
- Activity entry points seed the generated panel's visible date and project
  scope, then navigate to the Generated insights tab.
- Existing in-app links to deterministic analysis navigate to `/quality`.

There is no redirect from `/insights`. The router applies its normal
unknown-route fallback to Sessions, including the normal interpretation of
query parameters that Sessions recognizes.

## Loading and error behavior

- Corpus and generated loading states remain local to their active panels.
- Switching tabs destroys the inactive panel and cancels its in-flight reads
  through the existing cancellation hooks.
- Archive loading failures remain local to Generated insights and do not affect
  Corpus or Quality. The relocation does not change the archive store's retry
  or empty-state semantics.
- If generation is unavailable, the Generate action is disabled with an
  explanatory label while archive selection and report actions remain enabled.
- In-flight task failures retain retry and dismissal actions.
- Quality analytics and evidence failures remain isolated from Recall.

No new retry loop, compatibility fallback, or cross-panel shared loading state
is introduced.

## Localization and design system

- The workspace tab labels, Quality navigation label, headings, empty states,
  capability messages, and renamed actions use Paraglide messages in every
  supported locale.
- Existing insight-resource terminology may remain where it describes a saved
  generated report rather than the former page name.
- The Recall workspace uses the shared segmented/tab control and existing
  shared form controls. It does not add hand-styled native selectors.
- Corpus and generated content keep the established padded table/list-detail
  layouts; no multi-column card grid is introduced.

## Documentation

Documentation changes ship with the UI change:

- expand `docs/recall.md` with the Generated insights tab, visible generation
  scope, archive management, privacy boundary, and screenshots;
- replace `docs/insights.md` with a focused `docs/quality.md` describing
  deterministic recommendations, patterns, filters, and evidence;
- update documentation navigation, configuration references, activity and usage
  links, historical links that would otherwise become dead, screenshot
  hydration, and built-site checks;
- capture separate Recall Corpus, Recall Generated insights, and Quality page
  screenshots; and
- remove current copy that describes generated insights as living under the
  Insights navigation item.

The docs should continue to state that generation sends selected session
content to the configured local agent executable and that saved reports live in
the local archive.

## Validation strategy

Tests should assert rendered behavior and requests, not search component source
text.

### Router and application shell

- `/quality` parses and renders Quality.
- `/insights` resolves to Sessions through the ordinary unknown-route fallback,
  with recognized session query parameters preserved.
- desktop and mobile navigation expose Quality.
- Recall remains visible when only Generated insights is supported.
- `/recall` selects the first available tab.

### Recall workspace

- the default Corpus tab mounts only the corpus panel;
- `?tab=generated` mounts only the generated panel;
- switching tabs updates the URL and cancels inactive reads; and
- read-only mode omits Corpus while keeping the generated archive available.

### Generated insights

- a direct `insight` link selects the saved report after loading;
- an `insight` parameter without `tab` opens Generated insights;
- copied links use the new Recall URL;
- session and activity entry points open the generated tab with the expected
  selection or visible scope;
- generation requests contain the visible scope and client timezone while
  excluding hidden Sessions filters;
- unavailable generation leaves saved reports readable; and
- task failure, retry, export, publish, and delete behavior remains intact.

### Quality

- deterministic recommendations, quality patterns, date/filter synchronization,
  and evidence navigation remain observable;
- mounting Quality does not load the generated archive; and
- source-text assertions invalidated by the component split are replaced with
  user-visible component assertions.

### Documentation and build

- locale catalogues compile with matching keys;
- Svelte type checking and frontend unit tests pass;
- kit-ui validation and the production frontend build pass; and
- documentation links, generated screenshots, and the built site pass their
  existing checks.

## Acceptance criteria

- Recall presents Corpus and Generated insights as separate URL-backed tabs.
- Only the active Recall panel performs work.
- Generated reports can be scoped, generated, browsed, linked, exported,
  published, and deleted from Recall.
- Generated scope survives tab switches and can be seeded before navigation.
- The generated request is determined by visible scope controls plus the client
  timezone supplied as request context.
- Quality contains no generated archive or generator controls.
- `/quality` and all in-app entry points use the new information architecture.
- `/insights` has no frontend alias or redirect and falls back to Sessions.
- Read-only users retain access to saved generated reports.
- User-facing copy and documentation consistently describe Recall, Generated
  insights, and Quality.
- No backend schema or insight API compatibility layer is added.
