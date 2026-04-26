# Session Duration UX

## Visual contract

The implementation must visually and behaviorally match
[`2026-04-26-session-duration-ux-mockup.html`](./2026-04-26-session-duration-ux-mockup.html)
in this directory. Open it in a browser to see the target. The mockup is the
visual contract: structural layout, color encoding per category, slow-tinting,
running-state animations, parallel-group treatment, sub-agent inline expansion,
right-panel section order, click-to-highlight behavior — all must match. Pixel-
exact identity is not required, but inventing different layouts, colors, or
interactions during implementation is not acceptable.

Before claiming any frontend milestone done, run the dev server
(`make frontend-dev` alongside `make dev`) and compare side-by-side with the
mockup. If something in the mockup is ambiguous or impractical, raise it
explicitly — do not silently substitute.

## Problem

A session in agentsview is a sequence of messages, but it's also a sequence of
operations with very different time costs. Sub-agents can run for minutes; a
`Read` resolves in milliseconds; a `Bash` test run takes 30 seconds. None of
that is currently visible in the UI. To answer "where did this session's time
go?" or "which call was slow?" a user has to eyeball timestamps message by
message.

This adds a duration UX in the session view that surfaces, at every level a user
might ask:

- **Spot the slow** — which calls and turns dominated wall clock.
- **Feel the shape** — when in the session each category of work happened.
- **Stay aware while reading** — duration visible inline next to every tool
  call.
- **Compare categories** — totals by tool category for the session.

## Non-goals

- **Per-call precision for parallel tool calls.** Source JSONL gives a single
  timestamp per assistant message, so two `Read`s fired in parallel share that
  one timestamp. Recovering individual durations requires OpenTelemetry
  ingestion, which is a separate follow-up spec.
- **Cross-session analytics.** Existing `/analytics` and `/usage` views own
  that. This spec is per-session only.
- **Mobile / narrow viewports.** Session view is desktop-first; the right panel
  collapses on narrow widths but its design is out of scope here.
- **Backfilling pre-existing sessions.** No source data changes; everything is
  computed from message and session timestamps already in the database.

## Solution

Replace the current `ActivityMinimap` (right column of the session view) with a
richer **Session Vital Signs** panel, and decorate each `ToolBlock` and
assistant message in the conversation with duration data computed from existing
timestamps.

The panel has four stacked sections, top to bottom:

1. **Session summary** — total wall clock, turn count, tool call count, slowest
   call, sub-agent count.
1. **Where time went** — per-category aggregate bars.
1. **Timeline lanes** — turns lane, one lane per category, plus the existing
   message-activity bars folded in as a bottom lane.
1. **Calls** — chronological list with horizontal duration bars, parallel groups
   bracketed, sub-agent rows expandable inline.

In the conversation column, each `ToolBlock` gains a duration badge in its
header, and each assistant message gains a turn-summary line ("turn 2m 18s · 3
calls").

## Duration semantics

This is the load-bearing part of the design. Source data limits what we can
honestly report.

### What the source gives us

Each message has one timestamp (`messages.timestamp`, RFC3339). Tool calls
within a message share that timestamp. Tool results arrive in the next message
(typically a user-role message containing `tool_result` content blocks), also
with one timestamp. Sub-agent sessions have their own `sessions.started_at` and
`sessions.ended_at`.

There is no `tool_use_id`-keyed timing in any agent's transcript today. This is
confirmed for Claude Code, Codex, Gemini, and OpenCode.

### Units of time

- **Turn** — an assistant message that contains at least one `tool_use` block.
  Bounded by its own timestamp and the timestamp of the next message in the
  session.
- **Turn duration** — `next_message.timestamp - assistant_message.timestamp`.
  Always exact when there is a next message. For the last message in a session
  with `ended_at`, use `session.ended_at`. For a live session with neither,
  treat as running and use `now()`.
- **Solo call** — a turn with exactly one `tool_use` block. Its duration equals
  the turn duration. Exact.
- **Parallel calls** — a turn with N > 1 `tool_use` blocks. Each call's
  individual duration is unknowable from source. Group has one turn duration and
  a count.
- **Sub-agent call** — a `tool_use` whose `subagent_session_id` resolves to a
  child session. Its duration is computed from the child session's `started_at`
  / `ended_at`, regardless of whether it was solo or parallel. Always exact when
  the child session has bounds.

### Display rules

In the conversation, parallel turns (turns with N > 1 `tool_use` blocks) render
as a single group block — solid neutral left rail, faint background tint, header
row with `parallel` label + count chip + `≤duration` upper bound — wrapping the
constituent ToolBlocks. The group block owns the duration; individual ToolBlocks
inside it carry no per-call duration badge unless they are sub-agents (which
always get their exact duration).

| Case                               | Inline badge on ToolBlock                             | Calls-list bar                                    |
| ---------------------------------- | ----------------------------------------------------- | ------------------------------------------------- |
| Solo call                          | Exact duration                                        | Solid, exact width                                |
| Sub-agent (solo or parallel)       | Exact duration                                        | Solid, exact width, ★ marks slowest in group      |
| Parallel non-sub-agent sibling     | None (the group header carries the upper-bound label) | Striped, width = turn duration, label `≤duration` |
| Last turn, session running         | `running 1m 28s+` (live, pulsing)                     | Live pulsing animated bar                         |
| Missing or out-of-order timestamps | `—`                                                   | No bar                                            |

Turn-summary line on the assistant message header reads `turn 2m 18s · 3 calls`
(no `(N parallel)` qualifier — the group block handles that visually).

### Aggregate attribution rule

For "Where time went" and per-category lane bars, attribute each turn's
wall-clock duration to a single category:

- If the turn contains any sub-agent calls, compute the union (interval merge)
  of their `[started_at, ended_at]` ranges and attribute that union duration to
  `Task`. Subtract the union from the turn duration; the remainder goes to the
  dominant non-sub-agent category in the turn. The union, not the sum, prevents
  double-counting when parallel sub-agents overlap.
- Otherwise, attribute the full turn duration to the dominant category (strictly
  more than half the calls share a category).
- If no category strictly dominates (ties or even splits), attribute to `Mixed`.

This keeps per-category sums approximately equal to total tool wall-clock. It
loses precision only when a parallel turn mixes categories without a clear
majority — uncommon in practice.

The session-summary "tool time" total is the sum of attributed durations (equal
to total tool wall-clock).

## Changes

### Backend

#### Database

No schema changes. All durations are computed at query time from
`messages.timestamp` and `sessions.started_at` / `sessions.ended_at`.

#### Per-message turn duration

**File:** `internal/db/messages.go`

`messages.ordinal` is the canonical per-session chronological order. The schema
declares `UNIQUE(session_id, ordinal)` and indexes `(session_id, ordinal)`; the
sync engine assigns ordinals as it writes new messages from the agent's JSONL
transcript, in source order. The query relies on this invariant —
`LEAD ... ORDER BY ordinal` therefore selects the next chronological message
even if a later `m.timestamp` is older or unrelated (which can happen when a
watcher replays an out-of-order file).

The schema also has `messages.has_tool_use INTEGER NOT NULL DEFAULT 0`, set to
`1` by the parser when the message contains any `tool_use` block. The query uses
this flag — no subquery against `tool_calls` is needed.

Extend the per-session message query to include `turn_duration_ms`. Compute via
window function, joining the parent session so the last message can fall back to
`sessions.ended_at`:

```sql
SELECT
  m2.*,
  CASE
    WHEN m2.has_tool_use = 0 THEN NULL
    WHEN m2.delta_ms < 0    THEN NULL          -- clamp non-monotonic pairs
    ELSE m2.delta_ms
  END AS turn_duration_ms
FROM (
  SELECT
    m.*,
    CAST(
      (julianday(
        COALESCE(
          LEAD(m.timestamp) OVER (ORDER BY m.ordinal),
          s.ended_at
        )
      ) - julianday(m.timestamp)) * 86400000 AS INTEGER
    ) AS delta_ms
  FROM messages m
  LEFT JOIN sessions s ON s.id = m.session_id
  WHERE m.session_id = ?
) m2
ORDER BY m2.ordinal
```

Edge cases:

- **Last message in a completed session** — `LEAD` returns null, `COALESCE`
  falls through to `sessions.ended_at`, giving a real duration.
- **Last message in a live session** — both `LEAD` and `ended_at` are null, so
  `delta_ms` is null and `turn_duration_ms` is null. Frontend renders the
  running state from this null plus the `running` flag in the timing summary.
- **Non-monotonic timestamp pair** — `delta_ms` is negative; the outer `CASE`
  clamps to null and the frontend renders `—`. This catches cases where
  `ordinal` ordering and `timestamp` ordering disagree (rare; a sign of corrupt
  input).
- **Message with no tool calls** — `has_tool_use = 0` short-circuits to null
  without computing the delta.

#### Per-tool sub-agent duration

**File:** `internal/db/tool_calls.go` (or extend the existing `tool-calls` route
handler in `internal/server/tool_calls_route.go`)

For each tool call where `subagent_session_id` is set, join to the child
`sessions` row and compute `subagent_duration_ms`:

```sql
LEFT JOIN sessions s_sub ON s_sub.id = tc.subagent_session_id
... CAST(
  (julianday(COALESCE(s_sub.ended_at, ?)) - julianday(s_sub.started_at))
  * 86400000 AS INTEGER
) AS subagent_duration_ms
```

The `?` parameter is the current time, used when the sub-agent is still running.

#### Session timing summary endpoint

**New file:** `internal/server/session_timing.go`

Add `GET /api/v1/sessions/{id}/timing` returning:

```go
type SessionTiming struct {
    SessionID       string             `json:"session_id"`
    TotalDurationMs int64              `json:"total_duration_ms"`
    ToolDurationMs  int64              `json:"tool_duration_ms"`
    TurnCount       int                `json:"turn_count"`
    ToolCallCount   int                `json:"tool_call_count"`
    SubagentCount   int                `json:"subagent_count"`
    SlowestCall     *CallTiming        `json:"slowest_call"`
    ByCategory      []CategoryTotal    `json:"by_category"`
    Turns           []TurnTiming       `json:"turns"`
    Running         bool               `json:"running"`
}

type CategoryTotal struct {
    Category   string `json:"category"`     // one of taxonomy.go's normalized values
    DurationMs int64  `json:"duration_ms"`
    CallCount  int    `json:"call_count"`
}

type TurnTiming struct {
    MessageID       int64        `json:"message_id"`
    Ordinal         int          `json:"ordinal"`             // for ui.scrollToOrdinal()
    StartedAt       string       `json:"started_at"`
    DurationMs      *int64       `json:"duration_ms"`         // null if running or unknown
    PrimaryCategory string       `json:"primary_category"`
    Calls           []CallTiming `json:"calls"`
}

type CallTiming struct {
    ToolUseID         string  `json:"tool_use_id"`
    ToolName          string  `json:"tool_name"`
    Category          string  `json:"category"`             // from NormalizeToolCategory
    SkillName         *string `json:"skill_name,omitempty"`
    SubagentSessionID *string `json:"subagent_session_id,omitempty"`
    DurationMs        *int64  `json:"duration_ms"`          // null if parallel non-sub-agent
    IsParallel        bool    `json:"is_parallel"`
    InputPreview      string  `json:"input_preview"`        // short arg snippet for the row label
}
```

`InputPreview` is computed via the existing `extractToolParamMeta` /
`generateFallbackContent` helpers. It mirrors what `ToolBlock` shows in
`previewLine`, so the right-panel rows match the conversation rows.

##### Store interface and PG mirror

The HTTP handlers go through the `db.Store` interface (`internal/db/store.go`).
Add:

```go
GetSessionTiming(ctx context.Context, sessionID string) (*SessionTiming, error)
```

Both the SQLite implementation (`*db.DB`) and the PostgreSQL read store
(`*postgres.Store`) must implement it. The PG mirror runs the same logic against
PG; the differences are dialect-only:

- `EXTRACT(EPOCH FROM (next_ts - ts)) * 1000` instead of
  `(julianday(next_ts) - julianday(ts)) * 86400000`.
- `LEAD(...) OVER (ORDER BY ordinal)` is identical syntax (PG supports it).
- `has_tool_use` is `BOOLEAN` in PostgreSQL (`internal/postgres/schema.go`) but
  `INTEGER` in SQLite. The SQLite predicate `m2.has_tool_use = 0` becomes
  `NOT m2.has_tool_use` (or `m2.has_tool_use = FALSE`) in PG. PG will reject
  integer-to-boolean comparison.
- The `messages.ordinal` column already exists in the PG schema (mirrored by the
  push sync); verify in `internal/postgres/schema.go` before implementing and
  add an index if missing.

The PG implementation lives in `internal/postgres/session_timing.go` and is
exercised by integration tests under the `pgtest` build tag (run via
`make test-postgres`, which uses `docker-compose.test.yml` to start a PG
container automatically).

#### SSE updates

**File:** `internal/server/events.go`

Existing message-broadcast SSE already fires when new messages land. Add a
follow-on `session.timing` event that re-emits the timing summary on each new
message. Frontend consumes this to refresh the right panel.

### Frontend

#### Inline duration badge in `ToolBlock`

**File:** `frontend/src/lib/components/content/ToolBlock.svelte`

Add a `durationLabel` derived value. It is non-null only for solo calls and
sub-agent calls; parallel non-sub-agent siblings get nothing here (the wrapping
`ParallelGroup` carries the duration). Render right-aligned inside
`tool-header`:

```svelte
{#if durationLabel}
  <span class="tool-duration" class:slow={isSlow} class:running={isRunning}>
    {durationLabel}
  </span>
{/if}
```

Rules:

- Solo call: exact duration, `slow` class if in top 10% of session calls.
- Sub-agent (solo or parallel): exact duration, `slow` if applicable.
- Parallel non-sub-agent: badge is omitted entirely.
- Running call: `running 1m 28s+` with `running` class (pulsing animation, green
  family — see Visual encoding).

CSS: `tool-duration` uses `--font-mono`, 11px, subtle background tint
(`rgba(white, 0.04)` + `1px solid rgba(white, 0.04)` border); `slow` swaps to a
red-family token; `running` swaps to a green-family token plus
`animation: pulse 1.6s ease-in-out infinite` (opacity 1 → 0.55 → 1).

#### Parallel group rendering in `MessageList`

**File:** `frontend/src/lib/components/content/MessageList.svelte` (or its
message-renderer child; verify by reading the current component before editing)

The renderer walks the message's content in order and groups `tool_use` blocks.
The exact behavior depends on whether the API preserves per-block ordering:

- **Ideal (with ordered blocks):** Group only **contiguous runs** of `tool_use`
  blocks. A run of length ≥ 2 becomes one `ParallelGroup` rendered at the
  position of its first `tool_use`. A run of length 1 (or any solo `tool_use`
  between text blocks) renders as a normal `ToolBlock`. Text blocks are always
  rendered at their original position — never reordered.

- **v1 simplification (current API):** The existing `Message` shape exposes
  `content: string` (concatenated text) and `tool_calls?: ToolCall[]`
  separately, with no per-block ordering preserved through the database. The v1
  implementation renders all `content` text first, then groups every `tool_call`
  into one `ParallelGroup` (if `tool_calls.length ≥ 2`) or a flat `ToolBlock`.
  This is correct for the common case (text leads tool calls) and
  acknowledged-imperfect for rare interleaved sequences. Adding a per-message
  ordered-blocks API is a follow-up.

Examples (`T` = text, `U` = tool_use):

| Source order  | Ideal renders as                                        | v1 renders as                          |
| ------------- | ------------------------------------------------------- | -------------------------------------- |
| `T U U U`     | text, then ParallelGroup of 3                           | same                                   |
| `T U T U`     | text, ToolBlock, text, ToolBlock (no group — runs of 1) | all text, then ParallelGroup of 2 (\*) |
| `T U U T U U` | text, ParallelGroup of 2, text, ParallelGroup of 2      | all text, then ParallelGroup of 4 (\*) |
| `U U T U`     | ParallelGroup of 2, text, ToolBlock                     | all text, then ParallelGroup of 3 (\*) |

(\*) v1 trade-off: tool calls always appear after all text; mid-text tool calls
are not visually anchored to the surrounding text. Acceptable for v1 — Claude
Code rarely interleaves text mid-message.

`ParallelGroup` never reorders the **tool_calls themselves** in either mode —
original order is preserved within the group.

**New component:** `frontend/src/lib/components/content/ParallelGroup.svelte`

Props:

```ts
interface Props {
  toolCalls: ToolCall[];          // contiguous run of parallel siblings (length >= 2)
  turnDurationMs: number | null;  // null when running or unknown
  isRunning: boolean;
  highlightQuery?: string;
  isCurrentHighlight?: boolean;
}
```

Layout:

- Outer container: `border-left: 2px solid var(--border-strong)` (or a dedicated
  `--cat-mixed` neutral grey), `background: rgba(white, 0.025)`, rounded right
  corners.
- Header row at top: `parallel` label + count chip (`{N} calls`) + upper-bound
  duration on the right (`≤ {formatDuration}` or `running 1m 28s+` when
  running).
- Members: each child `ToolBlock` rendered with squared corners and flush layout
  (no inter-block margin). The block's own colored category rail extends the
  full row height inside the group.
- The `ToolBlock` component takes a new optional prop `inGroup: boolean` that
  switches off its outer margin and `border-radius` when true.

#### Turn summary on assistant message

**File:** `frontend/src/lib/components/content/MessageList.svelte` (or its
message renderer component)

For each assistant message that has tool calls, render a turn-summary line in
the message header area, right-aligned alongside the existing role label and
timestamp.

Timing data comes from the `SessionTiming` payload (loaded via the
`sessionTiming` store), indexed at render time by `message.id`. The existing
`Message` type is **not** extended with a `turn_duration_ms` field; instead the
renderer looks up the matching `TurnTiming`:

```svelte
{@const turn = turnByMessage.get(message.id)}
{#if turn?.duration_ms != null}
  <span class="turn-summary" class:slow={turnIsSlow}>
    turn {formatDuration(turn.duration_ms)} ·
    {message.tool_calls?.length ?? 0} call{(message.tool_calls?.length ?? 0) === 1 ? '' : 's'}
  </span>
{:else if sessionTiming.timing?.running && turn != null}
  <span class="turn-summary running">
    running {formatDuration(elapsedSinceStart(message))}+ ·
    {message.tool_calls?.length ?? 0} call{(message.tool_calls?.length ?? 0) === 1 ? '' : 's'}
  </span>
{/if}
```

`turnByMessage` is a `Map<number, TurnTiming>` derived once per render from
`sessionTiming.timing.turns`.

The `(N parallel)` qualifier is omitted — when the turn is parallel, the
`ParallelGroup` block in the body already shows it. The `running` class drives
the same pulsing animation as the per-call running badge.

#### Session Vital Signs component

**New file:** `frontend/src/lib/components/content/SessionVitals.svelte`

Replaces `ActivityMinimap` in the right column.

Props:

```ts
interface Props {
  sessionId: string;
}
```

State, loaded from `/api/v1/sessions/{id}/timing` and re-fetched on the SSE
`session.timing` event:

```ts
let timing = $state<SessionTiming | null>(null);
let categoryFilter = $state<string | null>(null);  // for highlight on click
```

Sections:

1. **Session summary** — fixed grid of label/value pairs.
1. **Where time went** — `each` over `timing.byCategory`, render category name +
   bar (width = `category.durationMs / timing.toolDurationMs`) + formatted
   total. Click a row to toggle `categoryFilter`.
1. **Timeline lanes** —
   - `turns` lane: one mark per `TurnTiming`, color = `primaryCategory`.
   - One lane per non-zero category in `byCategory`: marks for each turn
     attributed to that category.
   - `activity` lane: existing message-density bars from
     `sessionActivity.buckets` (the store keeps working unchanged).
   - All lanes share a horizontal scale from `session.startedAt` to
     `session.endedAt` (or `now()` for running sessions).
   - Click a mark: dispatch `ui.scrollToOrdinal(turn.messageId)` (existing UI
     store function, see `ActivityMinimap.svelte:106-112`).
   - Hover: tooltip with category, duration, call count.
1. **Calls** — flat list rendered from `timing.turns`. Each turn is either:
   - a single solo call → one `<CallRow>`, or
   - a parallel group → wrapped in `<CallGroup>` with a left rail, group header
     (count + group bar + group duration), and one `<CallRow>` per child call.
   - Sub-agent rows have an expand chevron. Click expands inline to render a
     nested `<SessionVitals>` (or just a calls list) for the child session,
     fetched on demand from `/api/v1/sessions/{subagent_session_id}/timing`.

Click behavior across the panel:

- Clicking a row in **Where time went** toggles `categoryFilter`.
  - The clicked agg row gets a tinted background and a `1px` ring in its own
    category color; other agg rows fade to 40% opacity.
  - A removable filter chip appears in the section header (`Bash ×`); clicking
    the × or clicking the active row again clears.
- When `categoryFilter` is set:
  - **Timeline lanes** — the matching lane stays full color; other lanes
    (including `turns`) fade to 40%. Marks within the matching lane stay at full
    intensity.
  - **Calls** — non-matching `<CallRow>`s dim to 30% opacity. A parallel group
    dims iff its dominant category (per the attribution rule) is not the filter
    — even if some of its children would individually match.
- Clicking a **call row body** dispatches `ui.scrollToOrdinal(turn.messageId)`
  to jump the conversation; clicking a **lane mark** does the same.
- Clicking a sub-agent row's **chevron** toggles the inline expansion; clicking
  the row body (chevron not the target) still scrolls.

The right column widens from current ~200px to 320px to accommodate this.

#### CallRow / CallGroup

**New files:** `frontend/src/lib/components/content/CallRow.svelte`,
`frontend/src/lib/components/content/CallGroup.svelte`

`CallRow` props: `call: CallTiming`, `barScalePxPerMs: number`,
`expanded?: boolean`, `onClick?: () => void`. Renders the per-call row from v3
mockup: chevron (when sub-agent), tool name, arg preview, bar, duration.

`CallGroup` props: `calls: CallTiming[]`, `groupDurationMs: number`,
`barScalePxPerMs: number`. Renders the rail + group header + child rows.

Both consume the existing tool-category color tokens defined in `app.css`
(extending `--accent-*` variables; see "Visual encoding" below).

#### Routing & layout

**Files:** `frontend/src/App.svelte` (line 9, 426 — import + render),
`frontend/src/lib/stores/ui.svelte.ts` (line 412 — `toggleActivityMinimap`),
`frontend/src/lib/components/layout/SessionBreadcrumb.svelte` (line 576 — toggle
button).

Replace `<ActivityMinimap />` with `<SessionVitals />` in the right column and
widen the column. Rename the existing `toggleActivityMinimap` /
`activityMinimapVisible` UI store members to `toggleVitals` / `vitalsVisible`
and update the breadcrumb call site accordingly. Remove `ActivityMinimap.svelte`
once no other view references it (verify via grep before deletion).

The existing `sessionActivity` store remains in use; `SessionVitals` consumes it
for the activity lane.

### Visual encoding

#### Category color map

The normalized category values come from `internal/parser/taxonomy.go`: `Read`,
`Edit`, `Write`, `Bash`, `Grep`, `Glob`, `Task`, `Tool`, `Other`. Skill
invocations carry category `Tool` and a populated `skill_name`.

Map them to design tokens. Add new tokens to `frontend/src/app.css` if not
present:

| Category                               | Token         | Approx hex                    |
| -------------------------------------- | ------------- | ----------------------------- |
| `Read`, `Grep`, `Glob`                 | `--cat-read`  | blue family (`#4a7ba8`)       |
| `Edit`, `Write`                        | `--cat-edit`  | green family (`#5a8b3a`)      |
| `Bash`                                 | `--cat-bash`  | amber family (`#d4a35a`)      |
| `Task` (sub-agents)                    | `--cat-task`  | red family (`#c45a5a`)        |
| `Tool` (skills, MCP, misc)             | `--cat-tool`  | purple family (`#7a5fa8`)     |
| `Other`                                | `--cat-other` | neutral grey                  |
| `Mixed` (no dominant category in turn) | `--cat-mixed` | neutral grey, slightly cooler |

Tokens already in `app.css` should be preferred where they match (e.g.,
`--accent-amber` ↔ `--cat-bash`); add only the missing ones.

#### Slow threshold

Two thresholds are computed frontend-side from `timing.turns`:

- **Slow call** — top 10% of measurable call durations (solo calls and
  sub-agents). When fewer than 10 measurable calls exist, mark the longest
  single call only.
- **Slow turn** — top 10% of `turn.durationMs` values. Same minimum-N rule.

Both recompute reactively when the timing summary updates. Slow rows get a
`slow` class: pink dot before the duration label, `rgba(<red>, 0.10)` tint
behind the bar. The exact red is the existing accent token in `app.css`
(currently `--accent-rose` / `--accent-coral`; pick the one that reads as
"warning" against the dark theme).

#### Bar scale

All bars in the Calls section use a session-relative scale: full width = the
session's total wall-clock duration. Sub-second calls render as nubs by design —
the bars are for "where the time went" reading; the duration label to the right
gives the precise value. Per-category lane bars use the same scale.

#### Duration formatting

Add `formatDuration(ms: number): string` in
`frontend/src/lib/utils/duration.ts`:

- `< 1000ms` → `"123ms"`
- `< 60000ms` → `"5.4s"` (one decimal)
- `< 3600000ms` → `"4m 21s"`
- `≥ 3600000ms` → `"1h 12m"`

`formatDurationLong` for tooltips: always include all units down to seconds.

### Live (running) sessions

The frontend treats a session as running when `timing.running === true`. In that
mode:

- **Session header meta** shows `running {elapsed}+` in the green-family token,
  pulsing.
- **Stat grid** swaps in two extra tiles: `in flight` (the running call's tool
  name + elapsed) and `slowest done` (the slowest *completed* call only). The
  running call is excluded from "slowest" until it finishes.
- **Where time went** counts only completed turns. The in-flight turn doesn't
  contribute, by design — its category attribution isn't decidable until the
  turn ends.
- **Calls** — the running row's bar uses a green gradient with a combined CSS
  pulse + slow-grow animation; the duration label pulses too. Between SSE
  events, the bar grows linearly each second (interpolated from `started_at` to
  `now()`); the SSE event is the truth source and the local interpolation just
  smooths the gap.
- **Scale axis** — the last tick is labeled `now` rather than the session's
  `ended_at`, since the right edge advances.
- **Conversation column** — the running turn's summary line and the running
  call's `ToolBlock` badge both pulse with the same animation.
- The activity lane keeps updating from the existing `sessionActivity` store via
  SSE.

When SSE emits `session.timing`, the frontend swaps in the new `SessionTiming`
object reactively. Bar widths, aggregates, and slow thresholds all recompute via
`$derived` with no manual cache invalidation.

## Performance

Sessions can have thousands of messages. Two concerns:

1. **Backend query** — the timing endpoint runs one window-function pass over
   `messages` plus a join to `sessions` for sub-agent lookups. Required indexes:
   `messages(session_id, id)` and `tool_calls(subagent_session_id)`. Check
   `internal/db/schema.sql` and add migrations only for indexes that don't
   already exist. With those indexes the pass is O(N) over the session's
   messages and acceptable up to ~10K messages. Pagination can be added later if
   needed.

1. **Frontend rendering** — the Calls list can have hundreds of rows. Reuse the
   same virtualization approach as `MessageList.svelte` (svelte-virtual or
   equivalent) when call count exceeds ~200. Below that, render directly. The
   Timeline lanes are O(turns) in DOM nodes regardless and don't virtualize.

## Testing

- **Backend Go tests (SQLite)** — table-driven tests using the existing
  `testDB(t)` helper in `internal/db/db_test.go`, covering: solo turn, parallel
  turn, sub-agent turn, mixed turn, last turn with no successor, running session
  (no `ended_at`), out-of-order timestamps (clamp to null), message with no tool
  calls (`has_tool_use = 0`).
- **Backend Go tests (PostgreSQL)** — mirror tests under the `pgtest` build tag
  in `internal/postgres/session_timing_pgtest_test.go`. Run via
  `make test-postgres`, which uses `docker-compose.test.yml` to spin up a PG
  container automatically.
- **Frontend unit tests (vitest)** — `formatDuration` covers each bucket;
  `categoryAttribution` covers each rule case (solo dominant, parallel dominant,
  mixed, sub-agent + remainder, parallel sub-agent union).
- **Playwright E2E** — open a session with known fixtures, assert the right
  panel shows expected aggregates, click a category row and verify highlighting,
  click a Calls row and verify the conversation scrolls to the matching message,
  simulate a live SSE update and verify the running row swaps to a completed
  row.
- **Test fixtures** — extend `cmd/testfixture/` to generate a session with
  sub-agents, parallel calls, and a long-running tool, used by both the Go tests
  and the Playwright E2E.

## What does NOT change

- `tool_calls`, `messages`, `sessions` schemas — no new columns. Computed fields
  only.
- The existing `sessionActivity` store and its SSE update path. The
  activity-lane fold-in reuses it.
- Other right-column views (analytics, usage, trends pages). Those have their
  own routes and aren't affected.
- The conversation column's structural layout — only the `ToolBlock` header and
  assistant message header gain new badges.

## Out of scope, follow-up specs

- **OTEL ingestion for per-call precision.** Embedded OTLP receiver, span/event
  → tool_call matching, schema additions for per-call timing, graceful upgrade
  from this v1 honest UX. Separate spec.
- **Full-screen "Calls" timeline mode.** Pop the Calls section into a
  modal/route covering the full viewport. Possible if the right-panel density
  proves limiting in practice.
- **Sort / filter modes for the Calls list.** Sort by duration desc, filter by
  skill name, etc. Add only when there's evidence of need.
- **Cross-session category aggregates that include duration.** The existing
  `/analytics` `ToolsAnalyticsResponse` would need a duration aggregation.
  Separate from this session-scoped feature.
