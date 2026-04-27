# Session Duration UX Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the Session Duration UX from the design spec — a Session Vital
Signs right panel (replacing ActivityMinimap), inline duration badges and turn
summaries in the conversation, ParallelGroup blocks for parallel turns,
sub-agent inline expansion, and live session updates — visually matching the
bound mockup.

**Architecture:** Backend computes turn and sub-agent durations on read from the
existing `messages.timestamp` and `sessions.started_at`/`ended_at` (no schema
changes), via a window function over `messages.ordinal`. A new
`GetSessionTiming` store method is implemented in both SQLite (`internal/db`)
and PostgreSQL (`internal/postgres`) backends and exposed at
`GET /api/v1/sessions/{id}/timing`, with a `session.timing` SSE event. The
frontend adds a `SessionVitals.svelte` panel composed of section components, a
`ParallelGroup.svelte` wrapper for parallel `tool_use` runs in `MessageList`, a
duration badge on `ToolBlock`, and a turn-summary line on assistant messages.
Visual encoding (category colors, slow/running states) comes from new CSS tokens
in `app.css`.

**Tech stack:** Go 1.23+ with SQLite (CGO + fts5 build tag) and PostgreSQL
drivers; Svelte 5 + TypeScript + Vite; vitest for frontend unit tests;
Playwright for E2E. No new dependencies.

______________________________________________________________________

## Visual contract

**This plan must produce a UI that visually and behaviorally matches
`docs/superpowers/specs/2026-04-26-session-duration-ux-mockup.html`** (the file
is in this repo). Open it in a browser before each frontend task and again at
every visual checkpoint. Pixel-exact identity isn't required, but inventing
different layouts, colors, animations, or interactions during implementation is
not acceptable.

When implementing CSS-heavy components (`ParallelGroup`, `SessionVitals`
sections, `CallRow`, `CallGroup`), copy structure and styling from the
corresponding region of the mockup file. Do not write CSS from scratch hoping
it'll look right.

If something in the mockup is impossible or impractical for some reason, surface
it explicitly before substituting. Don't silently rewrite.

______________________________________________________________________

## Spec reference

The implementation reference is
`docs/superpowers/specs/2026-04-26-session-duration-ux-design.md`. Tasks below
cite section names from that document. Read the spec section before starting
each task.

______________________________________________________________________

## Common commands

| Purpose                     | Command                                                                                |
| --------------------------- | -------------------------------------------------------------------------------------- |
| Run Go server (dev)         | `make dev`                                                                             |
| Run frontend dev server     | `make frontend-dev` (Vite, hot reload)                                                 |
| Run Go unit tests           | `make test`                                                                            |
| Run Go PG integration tests | `make test-postgres` (boots PG via `docker-compose.test.yml`)                          |
| Run frontend unit tests     | `nix shell nixpkgs#nodejs -c npm --prefix frontend test`                               |
| Run Playwright E2E          | `make e2e`                                                                             |
| Format Go                   | `nix shell nixpkgs#go -c go fmt ./...`                                                 |
| Vet                         | `make vet`                                                                             |
| Lint                        | `make lint`                                                                            |
| Format markdown             | `nix shell nixpkgs#uv -c uv tool run --with mdformat-tables mdformat --wrap 80 <file>` |

______________________________________________________________________

## Phase A — Foundations (utilities and tokens)

### Task 1: `formatDuration` utility

**Files:**

- Create: `frontend/src/lib/utils/duration.ts`

- Create: `frontend/src/lib/utils/duration.test.ts`

- [ ] **Step 1: Write the failing test**

Create `frontend/src/lib/utils/duration.test.ts`:

```ts
import { describe, expect, it } from "vitest";
import { formatDuration } from "./duration.js";

describe("formatDuration", () => {
  it("formats sub-second values as ms", () => {
    expect(formatDuration(0)).toBe("0ms");
    expect(formatDuration(8)).toBe("8ms");
    expect(formatDuration(312)).toBe("312ms");
    expect(formatDuration(999)).toBe("999ms");
  });

  it("formats sub-minute values as one-decimal seconds", () => {
    expect(formatDuration(1000)).toBe("1.0s");
    expect(formatDuration(2400)).toBe("2.4s");
    expect(formatDuration(28400)).toBe("28.4s");
    expect(formatDuration(59999)).toBe("60.0s");
  });

  it("formats sub-hour values as `Nm Ss`", () => {
    expect(formatDuration(60_000)).toBe("1m 0s");
    expect(formatDuration(138_000)).toBe("2m 18s");
    expect(formatDuration(3_599_000)).toBe("59m 59s");
  });

  it("formats hour-plus values as `Nh Mm`", () => {
    expect(formatDuration(3_600_000)).toBe("1h 0m");
    expect(formatDuration(4_320_000)).toBe("1h 12m");
    expect(formatDuration(86_400_000)).toBe("24h 0m");
  });

  it("treats negative or NaN as the dash sentinel", () => {
    expect(formatDuration(-1)).toBe("—");
    expect(formatDuration(Number.NaN)).toBe("—");
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

```
nix shell nixpkgs#nodejs -c npm --prefix frontend test -- duration
```

Expected: FAIL — module `./duration.js` not found.

- [ ] **Step 3: Implement `formatDuration`**

Create `frontend/src/lib/utils/duration.ts`:

```ts
const DASH = "—";

export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return DASH;
  if (ms < 1_000) return `${Math.trunc(ms)}ms`;
  if (ms < 60_000) return `${(ms / 1_000).toFixed(1)}s`;
  if (ms < 3_600_000) {
    const m = Math.floor(ms / 60_000);
    const s = Math.floor((ms % 60_000) / 1_000);
    return `${m}m ${s}s`;
  }
  const h = Math.floor(ms / 3_600_000);
  const m = Math.floor((ms % 3_600_000) / 60_000);
  return `${h}h ${m}m`;
}
```

- [ ] **Step 4: Run test to verify it passes**

```
nix shell nixpkgs#nodejs -c npm --prefix frontend test -- duration
```

Expected: PASS — all six cases.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/lib/utils/duration.ts frontend/src/lib/utils/duration.test.ts
git commit -m "feat(frontend): add formatDuration utility for ms→human-readable"
```

______________________________________________________________________

### Task 2: Category attribution utility

This is the function that decides which category a turn's wall-clock duration is
attributed to. Spec section: **Aggregate attribution rule**.

**Files:**

- Create: `frontend/src/lib/utils/categoryAttribution.ts`

- Create: `frontend/src/lib/utils/categoryAttribution.test.ts`

- [ ] **Step 1: Write the failing test**

Create `frontend/src/lib/utils/categoryAttribution.test.ts`:

```ts
import { describe, expect, it } from "vitest";
import {
  attributeTurn,
  type TurnAttributionInput,
} from "./categoryAttribution.js";

function turn(p: Partial<TurnAttributionInput> = {}): TurnAttributionInput {
  return {
    turnDurationMs: 10_000,
    calls: [],
    ...p,
  };
}

describe("attributeTurn", () => {
  it("returns null when turn duration is null (running)", () => {
    expect(attributeTurn(turn({ turnDurationMs: null }))).toBeNull();
  });

  it("attributes a solo turn to the call's category", () => {
    expect(
      attributeTurn(turn({
        turnDurationMs: 5000,
        calls: [{ category: "Bash", durationMs: 5000, isSubagent: false }],
      })),
    ).toEqual({ category: "Bash", durationMs: 5000 });
  });

  it("attributes a parallel turn to the strict majority category", () => {
    expect(
      attributeTurn(turn({
        turnDurationMs: 1400,
        calls: [
          { category: "Read", durationMs: null, isSubagent: false },
          { category: "Read", durationMs: null, isSubagent: false },
          { category: "Read", durationMs: null, isSubagent: false },
          { category: "Bash", durationMs: null, isSubagent: false },
        ],
      })),
    ).toEqual({ category: "Read", durationMs: 1400 });
  });

  it("attributes a non-dominated parallel turn to Mixed", () => {
    expect(
      attributeTurn(turn({
        turnDurationMs: 2000,
        calls: [
          { category: "Read", durationMs: null, isSubagent: false },
          { category: "Bash", durationMs: null, isSubagent: false },
        ],
      })),
    ).toEqual({ category: "Mixed", durationMs: 2000 });
  });

  it("splits sub-agent and non-sub-agent attribution", () => {
    // 3-call parallel turn: 2 reads + 1 sub-agent (2m of the 2m18s turn)
    const result = attributeTurn(turn({
      turnDurationMs: 138_000,
      calls: [
        { category: "Read", durationMs: null, isSubagent: false },
        { category: "Read", durationMs: null, isSubagent: false },
        {
          category: "Task",
          durationMs: 120_000,
          isSubagent: true,
          subagentRange: { startedAtMs: 0, endedAtMs: 120_000 },
        },
      ],
    }));
    // Returns the dominant non-sub-agent attribution (2 reads → "Read");
    // sub-agent contribution is reported separately via byCategory.
    // Spec says: subtract sub-agent UNION from turn duration; remainder
    // goes to dominant non-sub-agent.
    expect(result).toEqual({ category: "Read", durationMs: 18_000 });
  });

  it("uses the union of overlapping parallel sub-agent ranges", () => {
    // 2 sub-agents in parallel; A=[0,100], B=[50,200]; union=[0,200]=200
    // turn duration = 220, remainder = 20 attributed to dominant non-sub-agent
    // (none here → 'Mixed').
    const result = attributeTurn(turn({
      turnDurationMs: 220,
      calls: [
        {
          category: "Task",
          durationMs: 100,
          isSubagent: true,
          subagentRange: { startedAtMs: 0, endedAtMs: 100 },
        },
        {
          category: "Task",
          durationMs: 150,
          isSubagent: true,
          subagentRange: { startedAtMs: 50, endedAtMs: 200 },
        },
      ],
    }));
    expect(result).toEqual({ category: "Mixed", durationMs: 20 });
  });

  it("treats Read/Grep/Glob as non-distinct for dominance? No — exact strings", () => {
    // Per spec, attribution operates on exact normalized category values.
    // The frontend color map treats Read/Grep/Glob as one color, but
    // attribution is per category.
    expect(
      attributeTurn(turn({
        turnDurationMs: 500,
        calls: [
          { category: "Read", durationMs: null, isSubagent: false },
          { category: "Grep", durationMs: null, isSubagent: false },
        ],
      })),
    ).toEqual({ category: "Mixed", durationMs: 500 });
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

```
nix shell nixpkgs#nodejs -c npm --prefix frontend test -- categoryAttribution
```

Expected: FAIL — module not found.

- [ ] **Step 3: Implement `attributeTurn`**

Create `frontend/src/lib/utils/categoryAttribution.ts`:

```ts
export interface CallAttributionInput {
  category: string;
  durationMs: number | null;
  isSubagent: boolean;
  subagentRange?: { startedAtMs: number; endedAtMs: number };
}

export interface TurnAttributionInput {
  turnDurationMs: number | null;
  calls: CallAttributionInput[];
}

export interface TurnAttribution {
  category: string;
  durationMs: number;
}

export function attributeTurn(t: TurnAttributionInput): TurnAttribution | null {
  if (t.turnDurationMs == null) return null;
  if (t.calls.length === 0) return null;

  const subagents = t.calls.filter((c) => c.isSubagent && c.subagentRange);
  const nonSub = t.calls.filter((c) => !c.isSubagent);

  let remainder = t.turnDurationMs;
  if (subagents.length > 0) {
    const union = unionDuration(
      subagents.map((c) => c.subagentRange!),
    );
    remainder = Math.max(0, t.turnDurationMs - union);
    if (nonSub.length === 0) {
      // All calls were sub-agents; the remainder is whatever pre/post
      // overhead the parent turn had. Attribute to Mixed.
      return { category: "Mixed", durationMs: remainder };
    }
  }

  // Dominant non-sub-agent category: strict majority
  const counts = new Map<string, number>();
  for (const c of nonSub) {
    counts.set(c.category, (counts.get(c.category) ?? 0) + 1);
  }
  const total = nonSub.length;
  let dominant: string | null = null;
  for (const [cat, n] of counts) {
    if (n * 2 > total) {
      dominant = cat;
      break;
    }
  }

  return {
    category: dominant ?? "Mixed",
    durationMs: remainder,
  };
}

function unionDuration(
  ranges: Array<{ startedAtMs: number; endedAtMs: number }>,
): number {
  if (ranges.length === 0) return 0;
  const sorted = [...ranges].sort((a, b) => a.startedAtMs - b.startedAtMs);
  let total = 0;
  let curStart = sorted[0]!.startedAtMs;
  let curEnd = sorted[0]!.endedAtMs;
  for (let i = 1; i < sorted.length; i++) {
    const r = sorted[i]!;
    if (r.startedAtMs <= curEnd) {
      curEnd = Math.max(curEnd, r.endedAtMs);
    } else {
      total += curEnd - curStart;
      curStart = r.startedAtMs;
      curEnd = r.endedAtMs;
    }
  }
  total += curEnd - curStart;
  return Math.max(0, total);
}
```

- [ ] **Step 4: Run test to verify it passes**

```
nix shell nixpkgs#nodejs -c npm --prefix frontend test -- categoryAttribution
```

Expected: PASS — all seven cases.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/lib/utils/categoryAttribution.ts frontend/src/lib/utils/categoryAttribution.test.ts
git commit -m "feat(frontend): add per-turn category attribution utility"
```

______________________________________________________________________

### Task 3: Visual encoding tokens (CSS)

Add the category color tokens, slow/running utility classes, and `cat-mixed`
neutral that the new components depend on. Spec section: **Visual encoding ›
Category color map**.

**Files:**

- Modify: `frontend/src/app.css`

- [ ] **Step 1: Read the existing tokens**

Open `frontend/src/app.css` and locate the `:root { ... }` block and any
existing `--accent-*` tokens. Note which colors already exist (e.g.
`--accent-amber`, `--accent-rose`).

- [ ] **Step 2: Open the mockup as the source of truth**

```bash
xdg-open docs/superpowers/specs/2026-04-26-session-duration-ux-mockup.html
```

(or open it in your browser manually). Identify the hex values used for each
category in the legend, the slow tint, the running pulse, the parallel-group
rail, and the chip backgrounds. The mockup hexes are the target — match them in
the new tokens.

- [ ] **Step 3: Add tokens to `:root`**

Inside the existing `:root { ... }` block in `app.css`, add a new section. Pull
hexes from the mockup file:

```css
/* Tool-category color map (see taxonomy.go for normalized values) */
--cat-read:  #4a7ba8; /* Read, Grep, Glob */
--cat-edit:  #5a8b3a; /* Edit, Write */
--cat-bash:  #d4a35a; /* Bash */
--cat-task:  #c45a5a; /* Task / sub-agents */
--cat-tool:  #7a5fa8; /* Tool: Skills, MCP, misc */
--cat-other: #888888; /* Other */
--cat-mixed: #4a4a4a; /* Mixed: parallel-group rail, no-dominant */

/* Duration UX state colors */
--slow-fg:    #f29070;
--slow-bg:    rgba(242, 144, 112, 0.12);
--slow-ring:  rgba(242, 144, 112, 0.20);
--running-fg: #6ad0a8;
--running-bg: rgba(106, 208, 168, 0.12);
--running-ring: rgba(106, 208, 168, 0.22);
```

If a token already covers one of these (e.g. `--accent-amber` ≈ `--cat-bash`),
still add the `--cat-*` alias and set its value to `var(--accent-amber)` so the
duration UI references one stable name.

- [ ] **Step 4: Add the pulse keyframe**

Append to `app.css` (outside `:root`):

```css
@keyframes duration-pulse {
  0%, 100% { opacity: 1; }
  50%      { opacity: 0.55; }
}
```

- [ ] **Step 5: Verify the mockup still renders**

Open the mockup file in a browser (it's self-contained — uses inline styles
only). Confirm the colors are unchanged. Then run:

```
make build
```

Expected: build succeeds. (No frontend test changes here; this is a token
addition.)

- [ ] **Step 6: Commit**

```bash
git add frontend/src/app.css
git commit -m "feat(frontend): add tool-category and duration-state CSS tokens"
```

______________________________________________________________________

## Phase B — Backend timing data

### Task 4: `SessionTiming` Go types and `Store` interface method

Spec section: **Session timing summary endpoint** and **Store interface and PG
mirror**.

**Files:**

- Modify: `internal/db/store.go` (add interface method)

- Create: `internal/db/timing.go` (types + helpers)

- [ ] **Step 1: Add the types in a new file**

Create `internal/db/timing.go`:

```go
package db

// SessionTiming is the payload of GET /api/v1/sessions/{id}/timing.
// All durations are in milliseconds. *int64 fields are null when the
// underlying value is unknown (running, missing timestamp, parallel
// non-sub-agent call).
type SessionTiming struct {
	SessionID       string          `json:"session_id"`
	TotalDurationMs int64           `json:"total_duration_ms"`
	ToolDurationMs  int64           `json:"tool_duration_ms"`
	TurnCount       int             `json:"turn_count"`
	ToolCallCount   int             `json:"tool_call_count"`
	SubagentCount   int             `json:"subagent_count"`
	SlowestCall     *CallTiming     `json:"slowest_call"`
	ByCategory      []CategoryTotal `json:"by_category"`
	Turns           []TurnTiming    `json:"turns"`
	Running         bool            `json:"running"`
}

type CategoryTotal struct {
	Category   string `json:"category"`
	DurationMs int64  `json:"duration_ms"`
	CallCount  int    `json:"call_count"`
}

type TurnTiming struct {
	MessageID       int64        `json:"message_id"`
	Ordinal         int          `json:"ordinal"` // for ui.scrollToOrdinal
	StartedAt       string       `json:"started_at"`
	DurationMs      *int64       `json:"duration_ms"`
	PrimaryCategory string       `json:"primary_category"`
	Calls           []CallTiming `json:"calls"`
}

type CallTiming struct {
	ToolUseID         string  `json:"tool_use_id"`
	ToolName          string  `json:"tool_name"`
	Category          string  `json:"category"`
	SkillName         *string `json:"skill_name,omitempty"`
	SubagentSessionID *string `json:"subagent_session_id,omitempty"`
	DurationMs        *int64  `json:"duration_ms"`
	IsParallel        bool    `json:"is_parallel"`
	InputPreview      string  `json:"input_preview"`
}
```

- [ ] **Step 2: Add the interface method**

Edit `internal/db/store.go`. Find the `// Messages.` section and add the timing
method right after `GetSessionActivity`:

```go
// Timing.
GetSessionTiming(ctx context.Context, sessionID string) (*SessionTiming, error)
```

- [ ] **Step 3: Build to confirm interface gap**

```
nix shell nixpkgs#go -c go build -tags fts5 ./...
```

Expected: FAIL —
`*DB does not implement db.Store: missing method GetSessionTiming`. (And the
same for `*postgres.Store` if it's wired into the build.) That's the correct red
state.

- [ ] **Step 4: Stub out implementations to unblock build**

Edit `internal/db/timing.go`. Add `context` and `errors` to the existing
top-level import block (right after `package db`), and append the stub method
below the type declarations:

```go
package db

import (
	"context"
	"errors"
)

// (existing types: SessionTiming, CategoryTotal, TurnTiming, CallTiming)

// GetSessionTiming computes the per-session timing summary. Implemented in
// the next task; this stub returns "not implemented" so the package builds.
func (db *DB) GetSessionTiming(ctx context.Context, sessionID string) (*SessionTiming, error) {
	return nil, errors.New("not implemented")
}
```

In `internal/postgres/`, find the file that holds existing methods on `*Store`
(e.g. `internal/postgres/sessions.go`) and add at its bottom — or in a new
`internal/postgres/session_timing.go`:

```go
package postgres

import (
	"context"
	"errors"

	"github.com/agentsview/agentsview/internal/db"
)

func (s *Store) GetSessionTiming(ctx context.Context, sessionID string) (*db.SessionTiming, error) {
	return nil, errors.New("not implemented")
}
```

- [ ] **Step 5: Build green**

```
nix shell nixpkgs#go -c go build -tags fts5 ./...
```

Expected: PASS. Stubs make both implementations satisfy `Store`.

- [ ] **Step 6: Commit**

```bash
git add internal/db/store.go internal/db/timing.go internal/postgres/session_timing.go
git commit -m "feat(db): add SessionTiming types and Store interface method"
```

______________________________________________________________________

### Task 5: SQLite `GetSessionTiming` implementation

Spec section: **Per-message turn duration**, **Per-tool sub-agent duration**.

**Files:**

- Modify: `internal/db/timing.go`

- Create: `internal/db/timing_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/db/timing_test.go`:

```go
package db

import (
	"context"
	"testing"
)

func TestGetSessionTiming_Solo(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	insertSession(t, db, "s1", "2026-04-26T10:00:00Z", "2026-04-26T10:00:30Z")
	// One assistant message with one Bash tool call.
	insertMessage(t, db, "s1", 0, "user", "go", "2026-04-26T10:00:00Z", false)
	insertMessage(t, db, "s1", 1, "assistant", "running test", "2026-04-26T10:00:01Z", true)
	insertToolCall(t, db, "s1", msgID(t, db, "s1", 1), "tu_1", "Bash", "Bash", "")
	insertMessage(t, db, "s1", 2, "user", "ok", "2026-04-26T10:00:30Z", false)

	got, err := db.GetSessionTiming(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	if got.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1", got.TurnCount)
	}
	if got.ToolCallCount != 1 {
		t.Errorf("ToolCallCount = %d, want 1", got.ToolCallCount)
	}
	if got.Running {
		t.Errorf("Running = true, want false")
	}
	if len(got.Turns) != 1 {
		t.Fatalf("len(Turns) = %d, want 1", len(got.Turns))
	}
	if got.Turns[0].DurationMs == nil || *got.Turns[0].DurationMs != 29_000 {
		t.Errorf("turn duration = %v, want 29000", got.Turns[0].DurationMs)
	}
	if got.Turns[0].Calls[0].DurationMs == nil || *got.Turns[0].Calls[0].DurationMs != 29_000 {
		t.Errorf("call duration = %v, want 29000", got.Turns[0].Calls[0].DurationMs)
	}
}

func TestGetSessionTiming_LastMessageFallsBackToSessionEnd(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	insertSession(t, db, "s1", "2026-04-26T10:00:00Z", "2026-04-26T10:00:30Z")
	insertMessage(t, db, "s1", 0, "user", "run", "2026-04-26T10:00:00Z", false)
	insertMessage(t, db, "s1", 1, "assistant", "doing", "2026-04-26T10:00:10Z", true)
	insertToolCall(t, db, "s1", msgID(t, db, "s1", 1), "tu_1", "Bash", "Bash", "")
	// No following user message; session.ended_at = 10:00:30 → 20s.

	got, err := db.GetSessionTiming(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	if got.Turns[0].DurationMs == nil || *got.Turns[0].DurationMs != 20_000 {
		t.Errorf("turn duration = %v, want 20000 (fallback to ended_at)", got.Turns[0].DurationMs)
	}
}

func TestGetSessionTiming_RunningSessionLastTurnNull(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	insertSession(t, db, "s1", "2026-04-26T10:00:00Z", "")
	insertMessage(t, db, "s1", 0, "user", "run", "2026-04-26T10:00:00Z", false)
	insertMessage(t, db, "s1", 1, "assistant", "doing", "2026-04-26T10:00:10Z", true)
	insertToolCall(t, db, "s1", msgID(t, db, "s1", 1), "tu_1", "Bash", "Bash", "")

	got, err := db.GetSessionTiming(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	if !got.Running {
		t.Errorf("Running = false, want true")
	}
	if got.Turns[0].DurationMs != nil {
		t.Errorf("turn duration = %v, want nil (running)", *got.Turns[0].DurationMs)
	}
}

func TestGetSessionTiming_NonMonotonicTimestampClampsNull(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	insertSession(t, db, "s1", "2026-04-26T10:00:00Z", "2026-04-26T10:00:30Z")
	insertMessage(t, db, "s1", 0, "user", "run", "2026-04-26T10:00:20Z", false)
	insertMessage(t, db, "s1", 1, "assistant", "broken", "2026-04-26T10:00:25Z", true)
	insertToolCall(t, db, "s1", msgID(t, db, "s1", 1), "tu_1", "Bash", "Bash", "")
	// Next user message has earlier timestamp than the assistant's →
	// negative delta → clamp to null.
	insertMessage(t, db, "s1", 2, "user", "ok", "2026-04-26T10:00:00Z", false)

	got, err := db.GetSessionTiming(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	if got.Turns[0].DurationMs != nil {
		t.Errorf("turn duration = %v, want nil (clamp)", *got.Turns[0].DurationMs)
	}
}

func TestGetSessionTiming_NoToolUseHasNoTurnDuration(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	insertSession(t, db, "s1", "2026-04-26T10:00:00Z", "2026-04-26T10:00:30Z")
	insertMessage(t, db, "s1", 0, "user", "hi", "2026-04-26T10:00:00Z", false)
	insertMessage(t, db, "s1", 1, "assistant", "hi back", "2026-04-26T10:00:01Z", false)

	got, err := db.GetSessionTiming(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	if got.TurnCount != 0 {
		t.Errorf("TurnCount = %d, want 0", got.TurnCount)
	}
}

func TestGetSessionTiming_SubagentExactDuration(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	insertSession(t, db, "parent", "2026-04-26T10:00:00Z", "2026-04-26T10:05:00Z")
	insertSession(t, db, "child", "2026-04-26T10:00:01Z", "2026-04-26T10:02:15Z")
	insertMessage(t, db, "parent", 0, "user", "go", "2026-04-26T10:00:00Z", false)
	insertMessage(t, db, "parent", 1, "assistant", "spawning", "2026-04-26T10:00:01Z", true)
	insertToolCall(t, db, "parent", msgID(t, db, "parent", 1), "tu_a", "Agent", "Task", "child")
	insertMessage(t, db, "parent", 2, "user", "done", "2026-04-26T10:02:16Z", false)

	got, err := db.GetSessionTiming(ctx, "parent")
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	d := got.Turns[0].Calls[0].DurationMs
	if d == nil || *d != 134_000 {
		t.Errorf("subagent duration = %v, want 134000", d)
	}
	if got.SubagentCount != 1 {
		t.Errorf("SubagentCount = %d, want 1", got.SubagentCount)
	}
}

// --- helpers ------------------------------------------------------------
// The DB struct exposes db.getWriter() (returns *sql.DB) — see
// internal/db/db.go:85. Test files live in package db, so unexported
// methods are visible.

func insertSession(t *testing.T, db *DB, id, started, ended string) {
	t.Helper()
	endedAt := any(nil)
	if ended != "" {
		endedAt = ended
	}
	_, err := db.getWriter().ExecContext(context.Background(), `
		INSERT INTO sessions (id, started_at, ended_at, project, agent, machine)
		VALUES (?, ?, ?, '', '', '')
	`, id, started, endedAt)
	if err != nil {
		t.Fatalf("insertSession: %v", err)
	}
}

func insertMessage(t *testing.T, db *DB, sessionID string, ordinal int, role, content, ts string, hasToolUse bool) {
	t.Helper()
	flag := 0
	if hasToolUse {
		flag = 1
	}
	_, err := db.getWriter().ExecContext(context.Background(), `
		INSERT INTO messages (session_id, ordinal, role, content, timestamp, has_tool_use)
		VALUES (?, ?, ?, ?, ?, ?)
	`, sessionID, ordinal, role, content, ts, flag)
	if err != nil {
		t.Fatalf("insertMessage: %v", err)
	}
}

func msgID(t *testing.T, db *DB, sessionID string, ordinal int) int64 {
	t.Helper()
	var id int64
	err := db.getReader().QueryRowContext(context.Background(),
		`SELECT id FROM messages WHERE session_id = ? AND ordinal = ?`,
		sessionID, ordinal,
	).Scan(&id)
	if err != nil {
		t.Fatalf("msgID: %v", err)
	}
	return id
}

func insertToolCall(t *testing.T, db *DB, sessionID string, messageID int64, toolUseID, toolName, category, subagentSessionID string) {
	t.Helper()
	var sub any
	if subagentSessionID != "" {
		sub = subagentSessionID
	}
	_, err := db.getWriter().ExecContext(context.Background(), `
		INSERT INTO tool_calls (session_id, message_id, tool_use_id, tool_name, category, input_json, subagent_session_id)
		VALUES (?, ?, ?, ?, ?, '{}', ?)
	`, sessionID, messageID, toolUseID, toolName, category, sub)
	if err != nil {
		t.Fatalf("insertToolCall: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to confirm RED**

```
make test
```

Expected: All `TestGetSessionTiming_*` fail with "not implemented".

- [ ] **Step 3: Implement `GetSessionTiming`**

Replace the stub in `internal/db/timing.go`:

```go
func (db *DB) GetSessionTiming(ctx context.Context, sessionID string) (*SessionTiming, error) {
	// 1. Fetch session metadata (running flag, total duration, started_at).
	// 2. Run the per-message window-function query for this session.
	// 3. Run the per-tool query joining sub-agent sessions.
	// 4. Stitch into Turns + per-call data, attribute categories,
	//    aggregate by category, find slowest call.
	// 5. Return.

	sess, err := db.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		// Mirror GetSession's contract: (nil, nil) on miss. The
		// HTTP handler turns this into a 404.
		return nil, nil
	}
	// Session.StartedAt and Session.EndedAt are *string
	// (internal/db/sessions.go:94-95) — nil-check before deref.
	running := sess.EndedAt == nil || *sess.EndedAt == ""

	turnRows, err := db.getReader().QueryContext(ctx, `
		SELECT
		  m2.id, m2.ordinal, m2.timestamp, m2.has_tool_use,
		  CASE
		    WHEN m2.has_tool_use = 0 THEN NULL
		    WHEN m2.delta_ms < 0    THEN NULL
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
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer turnRows.Close()

	type turnRow struct {
		id, ordinal int64
		ts          string
		hasToolUse  bool
		dur         *int64
	}
	var turns []turnRow
	for turnRows.Next() {
		var r turnRow
		var hasFlag int
		if err := turnRows.Scan(&r.id, &r.ordinal, &r.ts, &hasFlag, &r.dur); err != nil {
			return nil, err
		}
		r.hasToolUse = hasFlag == 1
		turns = append(turns, r)
	}
	if err := turnRows.Err(); err != nil {
		return nil, err
	}

	// Tool calls with sub-agent durations.
	callRows, err := db.getReader().QueryContext(ctx, `
		SELECT
		  tc.message_id,
		  tc.tool_use_id,
		  tc.tool_name,
		  tc.category,
		  tc.skill_name,
		  tc.subagent_session_id,
		  tc.input_json,
		  CASE
		    WHEN tc.subagent_session_id IS NOT NULL THEN
		      CAST(
		        (julianday(COALESCE(s_sub.ended_at, ?)) - julianday(s_sub.started_at))
		        * 86400000 AS INTEGER
		      )
		    ELSE NULL
		  END AS subagent_duration_ms
		FROM tool_calls tc
		LEFT JOIN sessions s_sub ON s_sub.id = tc.subagent_session_id
		WHERE tc.session_id = ?
		ORDER BY tc.message_id, tc.id
	`, time.Now().UTC().Format(time.RFC3339), sessionID)
	if err != nil {
		return nil, err
	}
	defer callRows.Close()

	// Group call rows by message_id, then walk turn rows in order.
	callsByMsg := map[int64][]CallTiming{}
	for callRows.Next() {
		var c CallTiming
		var skill, sub sql.NullString
		var subDur sql.NullInt64
		var inputJSON, msgID = "", int64(0)
		if err := callRows.Scan(
			&msgID, &c.ToolUseID, &c.ToolName, &c.Category,
			&skill, &sub, &inputJSON, &subDur,
		); err != nil {
			return nil, err
		}
		if skill.Valid { s := skill.String; c.SkillName = &s }
		if sub.Valid   { s := sub.String;   c.SubagentSessionID = &s }
		if subDur.Valid { v := subDur.Int64; c.DurationMs = &v }
		c.InputPreview = makeInputPreview(c.ToolName, inputJSON)
		callsByMsg[msgID] = append(callsByMsg[msgID], c)
	}

	// Build TurnTiming per row, attribute category, accumulate
	// per-category totals, track the slowest call.
	out := &SessionTiming{SessionID: sessionID, Running: running}
	if sess.StartedAt != nil && *sess.StartedAt != "" &&
		sess.EndedAt != nil && *sess.EndedAt != "" {
		out.TotalDurationMs = millisBetween(*sess.StartedAt, *sess.EndedAt)
	} else if sess.StartedAt != nil && *sess.StartedAt != "" {
		out.TotalDurationMs = millisBetween(*sess.StartedAt, time.Now().UTC().Format(time.RFC3339))
	}

	categoryTotals := map[string]*CategoryTotal{}
	var slowest *CallTiming
	for _, t := range turns {
		if !t.hasToolUse {
			continue
		}
		out.TurnCount++
		calls := callsByMsg[t.id]

		// IsParallel marker for client display.
		for i := range calls {
			calls[i].IsParallel = len(calls) > 1
			if calls[i].SubagentSessionID != nil {
				out.SubagentCount++
			}
			if calls[i].DurationMs != nil {
				if slowest == nil || *calls[i].DurationMs > *slowest.DurationMs {
					c := calls[i]
					slowest = &c
				}
			}
		}
		out.ToolCallCount += len(calls)

		// Category attribution: port of attributeTurn from Task 2.
		// Returns (primaryCat, attributedMs); also yields any
		// per-sub-agent contributions to be added to byCategory.
		attribution := attributeTurnGo(t.dur, calls)
		// Bucket the attributed remainder under its category.
		bucket(categoryTotals, attribution.PrimaryCategory, attribution.RemainderMs, len(calls))
		// Each sub-agent contributes its exact duration to "Task".
		for _, sa := range attribution.SubagentDurations {
			bucket(categoryTotals, "Task", sa, 1)
		}

		out.Turns = append(out.Turns, TurnTiming{
			MessageID:       t.id,
			Ordinal:         int(t.ordinal),
			StartedAt:       t.ts,
			DurationMs:      t.dur,
			PrimaryCategory: attribution.PrimaryCategory,
			Calls:           calls,
		})
		if t.dur != nil {
			out.ToolDurationMs += *t.dur
		}
	}
	out.SlowestCall = slowest
	for _, total := range categoryTotals {
		out.ByCategory = append(out.ByCategory, *total)
	}
	sort.Slice(out.ByCategory, func(i, j int) bool {
		return out.ByCategory[i].DurationMs > out.ByCategory[j].DurationMs
	})

	return out, nil
}

// attributeTurnGo is a Go port of the JS attributeTurn from Task 2.
// Returns the primary non-sub-agent category, the remainder duration
// to attribute to it, and per-sub-agent durations.
type turnAttribution struct {
	PrimaryCategory   string
	RemainderMs       int64
	SubagentDurations []int64
}

func attributeTurnGo(turnDur *int64, calls []CallTiming) turnAttribution {
	if turnDur == nil {
		return turnAttribution{PrimaryCategory: "Mixed"}
	}
	var subTotals []int64
	var nonSub []CallTiming
	for _, c := range calls {
		if c.SubagentSessionID != nil && c.DurationMs != nil {
			subTotals = append(subTotals, *c.DurationMs)
		} else {
			nonSub = append(nonSub, c)
		}
	}
	// v1 approximation: union ≈ max(durations). Exact for single
	// sub-agent; under-estimates for parallel sub-agents that
	// don't fully overlap. To get exact union, extend the call
	// query to return started_at/ended_at and merge intervals.
	subUnion := int64(0)
	for _, d := range subTotals {
		if d > subUnion {
			subUnion = d
		}
	}
	remainder := *turnDur - subUnion
	if remainder < 0 {
		remainder = 0
	}
	if len(nonSub) == 0 {
		return turnAttribution{
			PrimaryCategory:   "Mixed",
			RemainderMs:       remainder,
			SubagentDurations: subTotals,
		}
	}
	counts := map[string]int{}
	for _, c := range nonSub {
		counts[c.Category]++
	}
	primary := "Mixed"
	for cat, n := range counts {
		if n*2 > len(nonSub) {
			primary = cat
			break
		}
	}
	return turnAttribution{
		PrimaryCategory:   primary,
		RemainderMs:       remainder,
		SubagentDurations: subTotals,
	}
}

func bucket(m map[string]*CategoryTotal, cat string, dur int64, callCount int) {
	if dur <= 0 {
		return
	}
	t, ok := m[cat]
	if !ok {
		t = &CategoryTotal{Category: cat}
		m[cat] = t
	}
	t.DurationMs += dur
	t.CallCount += callCount
}
```

`makeInputPreview` parses `input_json` and produces the same short arg snippet
that `ToolBlock`'s `previewLine` shows (look at
`frontend/src/lib/utils/tool-params.ts` for the source-of-truth and port the
relevant case to Go).

`millisBetween(a, b)` parses two RFC3339 timestamps and returns
`(b - a).Milliseconds()`. Add as a small helper at the bottom of this file.

**Note on parallel sub-agent ranges:** the v1 implementation above treats the
sub-agent union as `max(durations)`, which is exact for the common case (single
sub-agent per turn) and approximate when multiple sub-agents run in parallel
(rare). The exact interval-union requires returning `started_at` and `ended_at`
from the sub-agent session in the call query — note this as a known v1
simplification.

- [ ] **Step 4: Run tests until green**

```
make test
```

Expected: PASS for all `TestGetSessionTiming_*`.

- [ ] **Step 5: Run lint and vet**

```
make vet
make lint
```

Expected: clean. Fix any warnings.

- [ ] **Step 6: Commit**

```bash
git add internal/db/timing.go internal/db/timing_test.go
git commit -m "feat(db): implement GetSessionTiming for SQLite"
```

______________________________________________________________________

### Task 6: PostgreSQL `GetSessionTiming` mirror

Same logic as Task 5, dialect-translated for PG. Spec section: **Store interface
and PG mirror**.

**Files:**

- Modify: `internal/postgres/session_timing.go` (created in Task 4)

- Create: `internal/postgres/session_timing_pgtest_test.go`

- [ ] **Step 1: Verify PG schema parity**

Read `internal/postgres/schema.go` (the PG schema is defined in Go, not in a
`.sql` file). Confirm `messages.ordinal`, `messages.has_tool_use`,
`tool_calls.subagent_session_id`, `sessions.ended_at` all exist. If any are
missing, add to the schema (the push-sync code may already mirror them).

Note that PG types differ from SQLite: in PG, `has_tool_use` is
`BOOLEAN NOT NULL DEFAULT FALSE`, while SQLite stores it as
`INTEGER NOT NULL DEFAULT 0`. The query needs adapting accordingly — see Step 4.

- [ ] **Step 2: Write the failing test**

Create `internal/postgres/session_timing_pgtest_test.go`:

```go
//go:build pgtest

package postgres

import (
	"context"
	"testing"
)

func TestPGGetSessionTiming_Solo(t *testing.T) {
	store, cleanup := pgTestStore(t)
	defer cleanup()
	ctx := context.Background()

	pgInsertSession(t, store, "s1", "2026-04-26T10:00:00Z", "2026-04-26T10:00:30Z")
	pgInsertMessage(t, store, "s1", 0, "user", "2026-04-26T10:00:00Z", false)
	pgInsertMessage(t, store, "s1", 1, "assistant", "2026-04-26T10:00:01Z", true)
	pgInsertToolCall(t, store, "s1", 1, "tu_1", "Bash", "Bash", "")
	pgInsertMessage(t, store, "s1", 2, "user", "2026-04-26T10:00:30Z", false)

	got, err := store.GetSessionTiming(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	if got.Turns[0].DurationMs == nil || *got.Turns[0].DurationMs != 29_000 {
		t.Errorf("turn duration = %v, want 29000", got.Turns[0].DurationMs)
	}
}

// Add the parallel test cases from Task 5 here, adapted for the
// pg test helpers. Helpers (pgTestStore, pgInsertSession, etc.)
// follow whatever pattern the existing pgtest files use; read
// internal/postgres/messages_pgtest_test.go for the convention.
```

Add the same test scenarios as Task 5's table tests (Solo, FallbackToSessionEnd,
Running, NonMonotonic, NoToolUse, Subagent). Reuse helpers; copy/extract test
fixtures from existing pgtest files.

- [ ] **Step 3: Run pg tests to confirm RED**

```
make test-postgres
```

Expected: FAIL with "not implemented" (the stub from Task 4).

- [ ] **Step 4: Implement the PG mirror**

In `internal/postgres/session_timing.go`, replace the stub. The query is
structurally identical to the SQLite version with these substitutions:

| SQLite                              | PostgreSQL                                              |
| ----------------------------------- | ------------------------------------------------------- |
| `julianday(x)` minus                | `EXTRACT(EPOCH FROM (a::timestamptz - b::timestamptz))` |
| `* 86400000`                        | `* 1000` (since EPOCH already returns seconds)          |
| `CAST(... AS INTEGER)`              | `(...)::bigint`                                         |
| `?` placeholders                    | `$1`, `$2`, ...                                         |
| `LEAD(...) OVER (ORDER BY ordinal)` | identical                                               |
| `m2.has_tool_use = 0`               | `NOT m2.has_tool_use` (PG `has_tool_use` is `BOOLEAN`)  |

Sketch:

```go
func (s *Store) GetSessionTiming(ctx context.Context, sessionID string) (*db.SessionTiming, error) {
	// 1. Get session metadata (use existing GetSession)
	// 2. Per-turn query with PG-dialect window function
	// 3. Per-call query joining sub-agent sessions
	// 4. Stitch via the SAME attribution/aggregation logic as SQLite

	// The stitching is data-shape-only; extract it into a private
	// helper that takes raw rows and returns *db.SessionTiming, used
	// by both backends. Put the helper in internal/db/timing.go and
	// export it with a name like `assembleTiming(turns, calls,
	// session, now) *SessionTiming`.
}
```

If you extract a shared assembler, the SQLite test and the PG test exercise the
same logic — only the SQL changes per backend.

- [ ] **Step 5: Run pg tests until green**

```
make test-postgres
```

Expected: PASS.

- [ ] **Step 6: Run vet and lint**

```
make vet
make lint
```

- [ ] **Step 7: Commit**

```bash
git add internal/postgres/session_timing.go internal/postgres/session_timing_pgtest_test.go internal/db/timing.go
git commit -m "feat(postgres): implement GetSessionTiming for PG read store"
```

______________________________________________________________________

### Task 7: HTTP handler `GET /api/v1/sessions/{id}/timing`

Spec section: **Session timing summary endpoint**.

**Files:**

- Create: `internal/server/session_timing.go`

- Create: `internal/server/session_timing_test.go`

- Modify: `internal/server/server.go` (route registration)

- [ ] **Step 1: Write the failing test**

Create `internal/server/session_timing_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentsview/agentsview/internal/db"
)

func TestHandleSessionTiming_OK(t *testing.T) {
	srv := newTestServer(t) // existing helper; see server_test.go
	// Insert a session with one tool call and a clean turn boundary.
	seedTimingFixture(t, srv.db, "s1")

	req := httptest.NewRequest("GET", "/api/v1/sessions/s1/timing", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body)
	}
	var got db.SessionTiming
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1", got.SessionID)
	}
	if got.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1", got.TurnCount)
	}
}

func TestHandleSessionTiming_NotFound(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/v1/sessions/missing/timing", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func seedTimingFixture(t *testing.T, store db.Store, sessionID string) {
	t.Helper()
	// Use the same insertSession/insertMessage/insertToolCall helpers
	// from internal/db/timing_test.go (export them or duplicate).
	_ = context.TODO()
}
```

If `newTestServer` doesn't exist, read `internal/server/server_test.go` for the
existing test bootstrap pattern and follow it.

- [ ] **Step 2: Run tests to confirm RED**

```
make test
```

Expected: FAIL — handler not registered.

- [ ] **Step 3: Implement the handler**

Create `internal/server/session_timing.go`:

```go
package server

import (
	"encoding/json"
	"log"
	"net/http"
)

func (s *Server) handleSessionTiming(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if sessionID == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}

	// GetSessionTiming should return (nil, nil) when the session
	// doesn't exist (mirroring db.GetSession's contract — see
	// internal/db/sessions.go:475 — there is no db.ErrNotFound
	// sentinel). Implementations must follow this convention.
	timing, err := s.db.GetSessionTiming(r.Context(), sessionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if timing == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(timing); err != nil {
		// already wrote headers; just log
		log.Printf("encode session timing: %v", err)
	}
}
```

The Server struct's data-access field is `s.db` (type `db.Store`, see
`internal/server/server.go:36`), not `s.store`. Existing handlers log via the
stdlib `log` package (see `server.go:779,978`); there is no `s.logger`.

- [ ] **Step 4: Register the route**

Edit `internal/server/server.go`. Find the route-registration block (search for
`s.mux.HandleFunc("GET /api/v1/sessions`). Add:

```go
s.mux.HandleFunc("GET /api/v1/sessions/{id}/timing", s.handleSessionTiming)
```

Place it next to the existing session-related routes for consistency.

- [ ] **Step 5: Run tests to verify green**

```
make test
```

Expected: PASS.

- [ ] **Step 6: Verify the endpoint manually**

```
make dev
# in another shell:
curl -s http://localhost:8080/api/v1/sessions/<existing-session-id>/timing | jq .
```

Expected: a `SessionTiming` JSON payload. Stop `make dev` after.

- [ ] **Step 7: Commit**

```bash
git add internal/server/session_timing.go internal/server/session_timing_test.go internal/server/server.go
git commit -m "feat(server): add GET /api/v1/sessions/{id}/timing endpoint"
```

______________________________________________________________________

### Task 8: SSE `session.timing` event

Spec section: **SSE updates**.

The SSE architecture in this codebase is per-stream + polling, not broadcast.
`handleWatchSession` (`internal/server/events.go:217`) opens one SSE stream per
connected client and runs a goroutine (`sessionMonitor`) that polls
`GetSessionVersion` for changes. When the monitor detects a change, it sends a
`session_updated` event on that one stream via `stream.Send(eventType, data)`.
There is no global broadcast helper; do not invent one.

The simplest way to deliver `session.timing` to clients is to extend
`handleWatchSession` to also send the timing payload alongside
`session_updated`. The frontend's existing event listener can then either (a)
consume the `session.timing` event directly (if we add JSON encoding), or (b)
treat any `session_updated` as a hint to re-fetch
`/api/v1/sessions/{id}/timing`.

This task uses approach (a): server-side compute + emit, client-side consume. It
saves an HTTP round-trip per change and keeps the existing event-name contract
straightforward.

**Files:**

- Modify: `internal/server/events.go`

- [ ] **Step 1: Read the existing watch handler**

Read `internal/server/events.go:217-249`. The `handleWatchSession` function
uses:

```go
stream, err := NewSSEStream(w)
// ...
case _, ok := <-updates:
    // ...
    stream.Send("session_updated", sessionID)
```

The `stream.Send(eventType, data string)` API is the per-event sender. For
structured payloads, the same package has `stream.SendJSON(eventType, payload)`
(used in `handleTriggerSync` at events.go:268).

- [ ] **Step 2: Send `session.timing` alongside `session_updated`**

Edit `handleWatchSession`. After the existing `session_updated` send, compute
and send the timing:

```go
case _, ok := <-updates:
    if !ok {
        return
    }
    stream.Send("session_updated", sessionID)
    // New: push the latest timing summary so the frontend can update
    // the right panel without an extra HTTP round-trip.
    timing, err := s.db.GetSessionTiming(r.Context(), sessionID)
    if err != nil {
        log.Printf("session timing compute: %v", err)
    } else if timing != nil {
        stream.SendJSON("session.timing", timing)
    }
```

The first delivery on connect should also include the initial timing so a
freshly-loaded panel doesn't wait for the next change. Add right after
`NewSSEStream(w)` succeeds:

```go
if t, err := s.db.GetSessionTiming(r.Context(), sessionID); err == nil && t != nil {
    stream.SendJSON("session.timing", t)
}
```

- [ ] **Step 3: Verify the event fires**

```
make dev
# in another shell:
curl -N "http://localhost:8080/api/v1/sessions/<id>/watch"
```

(Use whatever the existing watch endpoint URL is — check `server.go` route
registration; the handler is `handleWatchSession`.)

Trigger a change in that session (touch the source JSONL or wait for the agent
to write). Observe both `session_updated` and `session.timing` events.

- [ ] **Step 4: Commit**

```bash
git add internal/server/events.go
git commit -m "feat(server): send session.timing on watch SSE stream"
```

______________________________________________________________________

## Phase C — Frontend API and store

### Task 9: TypeScript types, API client, and timing store

**Files:**

- Create: `frontend/src/lib/api/types/timing.ts`

- Create: `frontend/src/lib/api/timing.ts`

- Create: `frontend/src/lib/stores/sessionTiming.svelte.ts`

- [ ] **Step 1: Mirror the Go types in TypeScript**

Create `frontend/src/lib/api/types/timing.ts`:

```ts
export interface SessionTiming {
  session_id: string;
  total_duration_ms: number;
  tool_duration_ms: number;
  turn_count: number;
  tool_call_count: number;
  subagent_count: number;
  slowest_call: CallTiming | null;
  by_category: CategoryTotal[];
  turns: TurnTiming[];
  running: boolean;
}

export interface CategoryTotal {
  category: string;
  duration_ms: number;
  call_count: number;
}

export interface TurnTiming {
  message_id: number;
  ordinal: number;
  started_at: string;
  duration_ms: number | null;
  primary_category: string;
  calls: CallTiming[];
}

export interface CallTiming {
  tool_use_id: string;
  tool_name: string;
  category: string;
  skill_name?: string;
  subagent_session_id?: string;
  duration_ms: number | null;
  is_parallel: boolean;
  input_preview: string;
}
```

- [ ] **Step 2: Add the API client**

Create `frontend/src/lib/api/timing.ts`:

```ts
import type { SessionTiming } from "./types/timing.js";

export async function fetchSessionTiming(sessionId: string): Promise<SessionTiming> {
  const res = await fetch(`/api/v1/sessions/${encodeURIComponent(sessionId)}/timing`);
  if (!res.ok) {
    throw new Error(`session timing ${res.status}: ${await res.text()}`);
  }
  return res.json() as Promise<SessionTiming>;
}
```

- [ ] **Step 3: Add the SSE-aware store**

Create `frontend/src/lib/stores/sessionTiming.svelte.ts`:

```ts
import { fetchSessionTiming } from "../api/timing.js";
import type { SessionTiming } from "../api/types/timing.js";

class SessionTimingStore {
  timing = $state<SessionTiming | null>(null);
  loading = $state(false);
  error = $state<string | null>(null);

  private currentSessionId: string | null = null;

  async load(sessionId: string): Promise<void> {
    if (this.currentSessionId === sessionId && this.timing) return;
    this.currentSessionId = sessionId;
    this.loading = true;
    this.error = null;
    try {
      this.timing = await fetchSessionTiming(sessionId);
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
      this.timing = null;
    } finally {
      this.loading = false;
    }
  }

  /** Called by the SSE handler when a session.timing event arrives. */
  applyEvent(sessionId: string, payload: SessionTiming): void {
    if (sessionId !== this.currentSessionId) return;
    this.timing = payload;
  }

  reset(): void {
    this.currentSessionId = null;
    this.timing = null;
    this.error = null;
  }
}

export const sessionTiming = new SessionTimingStore();
```

- [ ] **Step 4: Wire the SSE event into the store**

Find the SSE consumer (likely `frontend/src/lib/api/sse.ts` or similar — grep
for `EventSource` in the frontend). Add a handler for the `session.timing`
event:

```ts
es.addEventListener("session.timing", (ev: MessageEvent) => {
  try {
    const payload = JSON.parse(ev.data) as SessionTiming;
    sessionTiming.applyEvent(payload.session_id, payload);
  } catch (e) {
    console.warn("session.timing parse failed", e);
  }
});
```

- [ ] **Step 5: Type-check the frontend**

```
nix shell nixpkgs#nodejs -c npm --prefix frontend run check
```

Expected: clean, no errors.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/lib/api/types/timing.ts frontend/src/lib/api/timing.ts frontend/src/lib/stores/sessionTiming.svelte.ts <wherever-sse.ts-is>
git commit -m "feat(frontend): add timing API client and SSE-aware store"
```

______________________________________________________________________

## Phase D — Conversation column

> **Before starting Phase D**: open
> `docs/superpowers/specs/2026-04-26-session-duration-ux-mockup.html` in a
> browser and keep it open in a second window. Compare every component you build
> against the corresponding region.

### Task 10: `ToolBlock` duration badge and `inGroup` prop

Spec section: **Inline duration badge in `ToolBlock`**.

**Files:**

- Modify: `frontend/src/lib/components/content/ToolBlock.svelte`

- [ ] **Step 1: Find the `tool-header` block**

Open `frontend/src/lib/components/content/ToolBlock.svelte`. Locate the
`<button class="tool-header">` element (around line 222 today). The badge goes
inside it, right-aligned.

- [ ] **Step 2: Add the new props**

In the `Props` interface (around line 12), add:

```ts
durationLabel?: string;     // pre-formatted string from formatDuration; null/undefined = no badge
isSlow?: boolean;
isRunning?: boolean;
inGroup?: boolean;          // when true, suppress outer margin and corner radii
```

In the destructure (around line 20), add the new props with defaults.

- [ ] **Step 3: Render the badge**

Add inside `tool-header`, just before the closing `</button>`:

```svelte
{#if durationLabel}
  <span
    class="tool-duration"
    class:slow={isSlow}
    class:running={isRunning}
  >
    {durationLabel}
  </span>
{/if}
```

- [ ] **Step 4: Add the badge styles**

In the `<style>` block, add:

```css
.tool-duration {
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--text-muted);
  padding: 2px 6px;
  background: rgba(255, 255, 255, 0.04);
  border: 1px solid rgba(255, 255, 255, 0.04);
  border-radius: var(--radius-sm);
  flex-shrink: 0;
  margin-left: auto;
}

.tool-duration.slow {
  color: var(--slow-fg);
  background: var(--slow-bg);
  border-color: var(--slow-ring);
}

.tool-duration.running {
  color: var(--running-fg);
  background: var(--running-bg);
  border-color: var(--running-ring);
  animation: duration-pulse 1.6s ease-in-out infinite;
}
```

Compare these against the corresponding `.tb-dur` rules in the mockup file.
Match colors and sizing.

- [ ] **Step 5: Honor `inGroup` to flatten margins**

In the `.tool-block` rule (around line 339), prepare for the in-group case:

```css
.tool-block {
  border-left: 2px solid var(--accent-amber);
  background: var(--tool-bg);
  border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
  margin: 0;
}

.tool-block.in-group {
  margin: 0;
  border-radius: 0;
}
```

And in the template, add `class:in-group={inGroup}` to the outer
`<div class="tool-block">`.

- [ ] **Step 6: Visual check**

Start the dev server:

```
make dev          # backend
# in another shell:
make frontend-dev # vite dev server (default :5173)
```

Open the browser at `http://localhost:5173` (or whatever port Vite reports).
Click any session that has tool calls. Open the spec mockup in a second
tab/window. Compare a single ToolBlock between the live app and the mockup:

- Badge style, font, colors (slow tint matches?)
- Badge position (right-aligned in header?)
- The `running` and `slow` variants render correctly when forced manually

If the live app doesn't show durations yet, that's expected — we haven't passed
`durationLabel` from the parent. To smoke-test the rendering, temporarily set
`durationLabel="2m 14s"` on a single ToolBlock instance and reload.

Revert the smoke-test value before committing.

- [ ] **Step 7: Commit**

```bash
git add frontend/src/lib/components/content/ToolBlock.svelte
git commit -m "feat(frontend): add duration badge and inGroup prop to ToolBlock"
```

______________________________________________________________________

### Task 11: Turn-summary line on assistant messages

Spec section: **Turn summary on assistant message**.

**Files:**

- Modify: `frontend/src/lib/components/content/MessageList.svelte` (or its child
  message-renderer; verify by reading the file)

- [ ] **Step 1: Find the assistant message renderer**

Open `MessageList.svelte` and locate where assistant messages are rendered (the
`msg-role` line containing the role label and timestamp). This is where the
turn-summary chip goes — right-aligned alongside the timestamp.

- [ ] **Step 2: Compute the turn-summary string**

The `Message` type already has `has_tool_use: boolean` and
`tool_calls?: ToolCall[]` (`frontend/src/lib/api/types/core.ts:74-92`). The new
`turn_duration_ms` value comes from the `SessionTiming` payload loaded by
`sessionTiming` store (Task 9), looked up by `message.id`. The frontend doesn't
extend the `Message` type itself.

Near the top of the assistant-message render block:

```svelte
<script lang="ts">
  import { formatDuration } from "../../utils/duration.js";
  import { sessionTiming } from "../../stores/sessionTiming.svelte.js";
  // existing imports...

  // Build once, used for every message in this list. Task 13 builds the
  // same index — extract it to a shared helper or compute once at the
  // MessageList level and pass down.
  let turnByMessage = $derived.by(() => {
    const m = new Map<number, import("../../api/types/timing.js").TurnTiming>();
    for (const t of sessionTiming.timing?.turns ?? []) {
      m.set(t.message_id, t);
    }
    return m;
  });

  // Per-message — call inside the message render block:
  function turnSummary(message: Message) {
    if (!message.has_tool_use) return null;
    const calls = message.tool_calls?.length ?? 0;
    const turn = turnByMessage.get(message.id);
    if (turn?.duration_ms != null) {
      return {
        text: `turn ${formatDuration(turn.duration_ms)} · ${calls} call${calls === 1 ? "" : "s"}`,
        slow: false, // slow threshold wired in Task 21
        running: false,
      };
    }
    // Live running turn: timing.running == true and this is the last
    // assistant message — the index has the turn but with duration_ms
    // == null. Compute elapsed from message.timestamp.
    if (sessionTiming.timing?.running && turn != null) {
      const elapsed = Date.now() - new Date(message.timestamp).getTime();
      return {
        text: `running ${formatDuration(elapsed)}+ · ${calls} call${calls === 1 ? "" : "s"}`,
        slow: false,
        running: true,
      };
    }
    return null;
  }
</script>
```

- [ ] **Step 3: Render the chip in the message header**

```svelte
{@const ts = turnSummary(message)}
<div class="msg-role">
  <span>{message.role}</span>
  <div class="role-meta">
    {#if ts}
      <span
        class="turn-summary"
        class:slow={ts.slow}
        class:running={ts.running}
      >
        {ts.text}
      </span>
    {/if}
    <span class="timestamp">{formatTimestamp(message.timestamp)}</span>
  </div>
</div>
```

`formatTimestamp` lives at `frontend/src/lib/utils/format.ts:27` — import it
from there.

Adjust `role-meta` flex/gap styles to put items in a row with spacing — copy
from the mockup `msg-role .role-meta` rule.

- [ ] **Step 4: Add styles**

```css
.turn-summary {
  font-family: var(--font-mono);
  font-size: 10px;
  color: var(--text-muted);
  background: rgba(255, 255, 255, 0.04);
  padding: 2px 8px;
  border-radius: var(--radius-sm);
  border: 1px solid rgba(255, 255, 255, 0.04);
}
.turn-summary.slow {
  color: var(--slow-fg);
  background: var(--slow-bg);
  border-color: var(--slow-ring);
}
.turn-summary.running {
  color: var(--running-fg);
  background: var(--running-bg);
  border-color: var(--running-ring);
  animation: duration-pulse 1.6s ease-in-out infinite;
}
```

- [ ] **Step 5: Visual check**

Start `make dev` + `make frontend-dev`. Open a completed session in the browser.
Compare an assistant message header against the mockup:

- Chip appears right-aligned, before the timestamp

- Font / size / colors match

- Slow / running variants look right (force them in code temporarily to verify)

- [ ] **Step 6: Commit**

```bash
git add frontend/src/lib/components/content/MessageList.svelte
git commit -m "feat(frontend): add turn-summary line on assistant messages"
```

______________________________________________________________________

### Task 12: `ParallelGroup` component

Spec section: **Parallel group rendering in `MessageList`**.

**Files:**

- Create: `frontend/src/lib/components/content/ParallelGroup.svelte`

- [ ] **Step 1: Open the mockup region**

In the mockup file, locate the `.pg` and `.pg-header` and `.pg .members .tb`
rules. This is the visual contract for `ParallelGroup`. Read the structure in
the mockup's "ASSISTANT 2 — parallel turn" section.

- [ ] **Step 2: Define props**

Create `frontend/src/lib/components/content/ParallelGroup.svelte`:

```svelte
<script lang="ts">
  import type { ToolCall } from "../../api/types.js";
  import type { CallTiming } from "../../api/types/timing.js";
  import ToolBlock from "./ToolBlock.svelte";
  import { formatDuration } from "../../utils/duration.js";

  interface Props {
    toolCalls: ToolCall[];                          // parallel siblings (length >= 2)
    turnDurationMs: number | null;                  // null when running or unknown
    callTimingByID?: Map<string, CallTiming>;       // for sub-agent durations
    isRunning?: boolean;
    highlightQuery?: string;
    isCurrentHighlight?: boolean;
  }

  let {
    toolCalls,
    turnDurationMs,
    callTimingByID,
    isRunning = false,
    highlightQuery = "",
    isCurrentHighlight = false,
  }: Props = $props();

  let upperBoundLabel = $derived.by(() => {
    if (isRunning) return null; // running indicator takes over
    if (turnDurationMs == null) return null;
    return `≤ ${formatDuration(turnDurationMs)} each`;
  });
</script>

<div class="parallel-group">
  <div class="pg-header">
    <span class="pg-label">parallel</span>
    <span class="pg-count">{toolCalls.length} calls</span>
    <span class="pg-spacer"></span>
    {#if isRunning}
      <span class="pg-running">running…</span>
    {:else if upperBoundLabel}
      <span class="pg-upper">{upperBoundLabel}</span>
    {/if}
  </div>
  <div class="pg-members">
    {#each toolCalls as toolCall (toolCall.tool_use_id)}
      {@const ct = callTimingByID?.get(toolCall.tool_use_id ?? "")}
      {@const dur = (ct?.subagent_session_id && ct.duration_ms != null)
        ? formatDuration(ct.duration_ms)
        : undefined}
      <ToolBlock
        toolCall={toolCall}
        content=""
        label={toolCall.tool_name}
        durationLabel={dur}
        inGroup={true}
        {highlightQuery}
        {isCurrentHighlight}
      />
    {/each}
  </div>
</div>

<style>
  .parallel-group {
    border-left: 2px solid var(--cat-mixed);
    background: rgba(255, 255, 255, 0.025);
    border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
    margin: 6px 0;
    padding: 4px 0;
    overflow: hidden;
  }
  .pg-header {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 5px 12px 7px;
    font-family: var(--font-mono);
    font-size: 10px;
    color: var(--text-muted);
  }
  .pg-label { color: var(--text-secondary); font-weight: 500; }
  .pg-count {
    background: rgba(255, 255, 255, 0.06);
    padding: 1px 7px;
    border-radius: 999px;
    font-size: 9px;
    color: var(--text-primary);
  }
  .pg-spacer { flex: 1; }
  .pg-upper { color: var(--text-muted); font-size: 10px; }
  .pg-running {
    color: var(--running-fg);
    font-size: 10px;
    animation: duration-pulse 1.6s ease-in-out infinite;
  }
  .pg-members :global(.tool-block) {
    margin: 0;
    border-radius: 0;
  }
  .pg-members :global(.tool-block + .tool-block) {
    border-top: 1px solid rgba(255, 255, 255, 0.04);
  }
  .pg-members :global(.tool-block:last-child) {
    border-bottom-right-radius: var(--radius-sm);
  }
</style>
```

`ToolBlock` already derives its preview line from `toolCall.input_json` via
`extractToolParamMeta` / `generateFallbackContent` (see
`ToolBlock.svelte:96-115`); passing `content=""` to `ToolBlock` lets it use that
existing logic. The `callTimingByID` lookup is what supplies sub-agent
durations.

- [ ] **Step 3: Type-check**

```
nix shell nixpkgs#nodejs -c npm --prefix frontend run check
```

Expected: clean.

- [ ] **Step 4: Smoke-test in isolation**

In `MessageList.svelte`, temporarily render a `ParallelGroup` with three
hard-coded `ToolCall` objects. Reload, compare side-by-side with the mockup's
`.pg` block.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/lib/components/content/ParallelGroup.svelte
git commit -m "feat(frontend): add ParallelGroup component"
```

______________________________________________________________________

### Task 13: `MessageList` parallel grouping

Spec section: **Parallel group rendering in `MessageList`** (note the v1
simplification described there).

The existing `Message` type (`frontend/src/lib/api/types/core.ts:74-92`) exposes
`content: string` (concatenated text) and `tool_calls?: ToolCall[]` separately —
there is **no per-block ordering data** preserved in the API today. The v1
simplification: render `message.content` text first, then the tool calls. If
`tool_calls.length >= 2`, group them in one `ParallelGroup`; otherwise render
the single tool call as a flat `ToolBlock`. Interleaved text/tool sequences
(rare in Claude Code transcripts) render with all text before all tools — an
acknowledged v1 trade-off documented in the spec.

The frontend doesn't extend `Message` or `ToolCall` to add `turn_duration_ms` /
`subagent_duration_ms`. Instead it builds an index from the `SessionTiming`
payload and looks up timing per message/tool at render time.

**Files:**

- Modify: `frontend/src/lib/components/content/MessageList.svelte`

- [ ] **Step 1: Extend the timing index**

Task 11 already declared `turnByMessage` in `MessageList.svelte`. Add
`callByToolUseID` alongside it (don't redeclare `turnByMessage`):

```ts
import type { CallTiming } from "../../api/types/timing.js";

let callByToolUseID = $derived.by(() => {
  const m = new Map<string, CallTiming>();
  for (const t of sessionTiming.timing?.turns ?? []) {
    for (const c of t.calls) m.set(c.tool_use_id, c);
  }
  return m;
});
```

- [ ] **Step 2: Locate the per-message render block**

Find the assistant-message render block (the same one Task 11 modified). The
current code renders `message.content` and iterates `message.tool_calls` in some
way. This task changes the iteration to either render a single `ToolBlock` or
one `ParallelGroup`.

- [ ] **Step 3: Render text + grouped tools**

Inside the existing assistant-message body of `MessageList.svelte`, replace the
per-message tool-rendering loop with the block below. Keep the surrounding
structure (role chip, timestamp, etc.) untouched — only the tool-call rendering
changes:

```svelte
{@const turn = turnByMessage.get(message.id)}
{#if message.content}
  <pre class="msg-text">{message.content}</pre>
{/if}
{#if message.tool_calls && message.tool_calls.length === 1}
  {@const call = message.tool_calls[0]}
  {@const callTiming = callByToolUseID.get(call.tool_use_id ?? "")}
  <ToolBlock
    toolCall={call}
    content=""
    label={call.tool_name}
    durationLabel={soloDurationLabel(callTiming, turn)}
    isSlow={false /* slow threshold wired in Task 21 */}
    isRunning={turn?.duration_ms == null && isRunningTurn(message)}
  />
{:else if message.tool_calls && message.tool_calls.length >= 2}
  <ParallelGroup
    toolCalls={message.tool_calls}
    callTimingByID={callByToolUseID}
    turnDurationMs={turn?.duration_ms ?? null}
    isRunning={turn?.duration_ms == null && isRunningTurn(message)}
  />
{/if}
```

`soloDurationLabel(callTiming, turn)` is a small local helper:

```ts
function soloDurationLabel(
  ct: CallTiming | undefined,
  turn: TurnTiming | undefined,
): string | undefined {
  if (ct?.subagent_session_id && ct.duration_ms != null) {
    return formatDuration(ct.duration_ms); // sub-agent exact
  }
  if (turn?.duration_ms != null) {
    return formatDuration(turn.duration_ms); // solo call = turn duration
  }
  return undefined;
}
```

`isRunningTurn(message)` returns `true` iff `sessionTiming.timing?.running` and
`message` is the last assistant message in the session — implement by comparing
`message.id` to the highest-id assistant message in the current message list.

- [ ] **Step 4: Verify `ParallelGroup` accepts the index**

Task 12 already adds `callTimingByID?: Map<string, CallTiming>` to
`ParallelGroup`'s props (Pass 1 fix). Confirm the prop exists and that the
each-loop inside uses it to derive sub-agent duration labels. No changes here
unless Task 12 was implemented before Pass 1's update landed; in that case, port
the prop addition forward.

- [ ] **Step 5: Visual check**

Start dev servers. Find a real session that has:

- a solo turn (one tool_use in the message)
- a parallel turn (≥2 tool_uses in one message)
- ideally a parallel turn that includes a sub-agent

Compare each against the mockup:

- Solo turn: ToolBlock shows exact duration (= turn duration), no group wrapper

- Parallel turn: wrapped in `ParallelGroup`, header shows count + upper-bound,
  child ToolBlocks have no duration badge except sub-agents

- Sub-agent inside the group: shows its exact `subagent_duration_ms`

- Pure-text assistant messages render as before with no badge

- [ ] **Step 6: Commit**

```bash
git add frontend/src/lib/components/content/MessageList.svelte frontend/src/lib/components/content/ParallelGroup.svelte
git commit -m "feat(frontend): group parallel tool_use runs in MessageList"
```

______________________________________________________________________

### Task 14: Visual checkpoint A — conversation column

This is a forced gate before continuing to the right panel.

- [ ] **Step 1: Open the mockup and the live app side by side**

```
make dev
make frontend-dev
```

Open `docs/superpowers/specs/2026-04-26-session-duration-ux-mockup.html` in one
browser window/tab and `http://localhost:5173` in another. Pick a session that
exercises:

- Solo turn (single tool_use)

- Parallel turn (≥2 tool_uses)

- Sub-agent inside a parallel turn

- Slow turn (longer than ~p90 of the session)

- [ ] **Step 2: Compare each surface**

For each, verify against the mockup:

| Surface                                                        | Match? |
| -------------------------------------------------------------- | ------ |
| `.msg-role` chip placement and styling                         |        |
| Turn-summary text (`turn 2m 18s · 3 calls`)                    |        |
| Slow turn red tint on chip                                     |        |
| ParallelGroup wrapper rail color, padding, header layout       |        |
| ParallelGroup header: `parallel` + `N calls` chip + `≤ X each` |        |
| Inside group: child ToolBlocks have no badges except sub-agent |        |
| Sub-agent ToolBlock badge shows exact duration                 |        |
| Mixed text/tool ordering preserved                             |        |
| Solo turn ToolBlock badge shows exact duration                 |        |

- [ ] **Step 3: If anything diverges, fix it**

Diverge = looks meaningfully different from the mockup. Pixel-perfect not
required, but layouts, colors, and structure must match. Fix in place (as a
follow-up commit per fix), do not move on with known divergences.

- [ ] **Step 4: Capture a screenshot and commit it**

Save a screenshot of the live conversation column at
`docs/superpowers/specs/checkpoints/2026-04-26-conversation-column.png`. Commit:

```bash
git add docs/superpowers/specs/checkpoints/2026-04-26-conversation-column.png
git commit -m "docs: checkpoint A — conversation column matches mockup"
```

______________________________________________________________________

## Phase E — Right panel (Session Vital Signs)

> Open the mockup again. The right panel sections in order are: **Session
> summary**, **Time spent**, **Timeline**, **Calls**. Build them in that
> order.

### Task 15: `SessionVitals` shell + Session summary section

Spec section: **Session Vital Signs component › Sections › 1. Session summary**.

**Files:**

- Create: `frontend/src/lib/components/content/SessionVitals.svelte`

- [ ] **Step 1: Open the mockup's right-panel section**

In the mockup, find the `.vital`, `.v-section`, `.v-h`, `.stat-grid` rules and
the section labeled "Session". This is the layout target.

- [ ] **Step 2: Create the shell**

```svelte
<!-- ABOUTME: Session Vital Signs panel — replaces ActivityMinimap on the right. -->
<script lang="ts">
  import { sessionTiming } from "../../stores/sessionTiming.svelte.js";
  import { formatDuration } from "../../utils/duration.js";

  interface Props {
    sessionId: string;
  }

  let { sessionId }: Props = $props();

  $effect(() => {
    void sessionTiming.load(sessionId);
  });

  let timing = $derived(sessionTiming.timing);
</script>

<div class="vital">
  {#if timing}
    <section class="v-section">
      <header class="v-h">
        <span>Session</span>
        <span class="v-meta" class:live={timing.running}>
          {timing.running ? `running ${formatDuration(timing.total_duration_ms)}+` : formatDuration(timing.total_duration_ms)}
        </span>
      </header>
      <div class="stat-grid">
        <div>
          <div class="lbl">tool calls</div>
          <div class="val">{timing.tool_call_count}</div>
        </div>
        <div>
          <div class="lbl">tool time</div>
          <div class="val" class:live={timing.running}>{formatDuration(timing.tool_duration_ms)}{timing.running ? "+" : ""}</div>
        </div>
        <div>
          <div class="lbl">slowest call</div>
          <div class="val slow">
            {#if timing.slowest_call}
              {timing.slowest_call.tool_name} · {formatDuration(timing.slowest_call.duration_ms ?? 0)}
            {:else}
              —
            {/if}
          </div>
        </div>
        <div>
          <div class="lbl">turns</div>
          <div class="val">{timing.turn_count}</div>
        </div>
        <div>
          <div class="lbl">sub-agents</div>
          <div class="val">{timing.subagent_count}</div>
        </div>
      </div>
    </section>
  {:else if sessionTiming.error}
    <p class="v-error">{sessionTiming.error}</p>
  {/if}
</div>

<style>
  .vital {
    background: var(--bg-surface);
    border-left: 1px solid var(--border-muted);
    overflow-y: auto;
    height: 100%;
  }
  .v-section {
    padding: 12px 14px 14px;
    border-bottom: 1px solid var(--border-muted);
  }
  .v-section:last-child { border-bottom: 0; }
  /* ...copy remaining .v-h, .v-meta, .stat-grid styles from the mockup... */
</style>
```

Copy the rest of the section CSS verbatim from the mockup's `<style>` block —
`.v-h`, `.v-meta`, `.stat-grid .lbl/.val/.slow/.live`. Don't paraphrase.

- [ ] **Step 3: Visual check**

Temporarily mount `<SessionVitals sessionId={...}>` in `App.svelte` next to the
existing `ActivityMinimap` (don't replace yet — just visual smoke). Compare the
Session section to the mockup's Session block.

- [ ] **Step 4: Commit**

```bash
git add frontend/src/lib/components/content/SessionVitals.svelte
git commit -m "feat(frontend): add SessionVitals shell and Session summary section"
```

______________________________________________________________________

### Task 16: "Time spent" section + click-to-highlight state

Spec section: **Sections › 2. Time spent** and **Click behavior across the
panel**.

**Files:**

- Modify: `frontend/src/lib/components/content/SessionVitals.svelte`

- [ ] **Step 1: Add `categoryFilter` state**

In the script:

```ts
let categoryFilter = $state<string | null>(null);
function toggleCategory(cat: string) {
  categoryFilter = categoryFilter === cat ? null : cat;
}
```

- [ ] **Step 2: Add the section**

Inside the `<div class="vital">`, after the Session section:

```svelte
{#if timing && timing.by_category.length > 0}
  <section class="v-section">
    <header class="v-h">
      <span>Time spent</span>
      {#if categoryFilter}
        <button class="filter-chip" onclick={() => (categoryFilter = null)}>
          {categoryFilter}<span class="x">×</span>
        </button>
      {:else}
        <span class="v-meta">completed turns · click to highlight</span>
      {/if}
    </header>
    {#each timing.by_category as cat (cat.category)}
      {@const isActive = categoryFilter === cat.category}
      {@const isDimmed = categoryFilter && !isActive}
      <button
        class="agg-row"
        class:active={isActive}
        class:dimmed={isDimmed}
        onclick={() => toggleCategory(cat.category)}
      >
        <span class="agg-name">{cat.category}</span>
        <span class="agg-bar"><span class="agg-fill" style="width: {(cat.duration_ms / timing.tool_duration_ms) * 100}%; background: var(--cat-{cat.category.toLowerCase()})"></span></span>
        <span class="agg-val">{formatDuration(cat.duration_ms)}</span>
      </button>
    {/each}
  </section>
{/if}
```

- [ ] **Step 3: Add styles**

Copy the corresponding `.agg-row`, `.agg-name`, `.agg-bar`, `.agg-fill`,
`.agg-val`, `.filter-chip` rules from the mockup's `<style>` block.

- [ ] **Step 4: Map categories to color tokens**

The inline `style="background: var(--cat-{cat.category.toLowerCase()})"` only
works if categories exactly match `read|edit|bash|task|tool|other|mixed`. The
actual normalized values (`Read`, `Edit`, `Write`, `Bash`, `Grep`, `Glob`,
`Task`, `Tool`, `Other`) need a mapping function. Add a helper:

```ts
function categoryToken(cat: string): string {
  switch (cat) {
    case "Read":
    case "Grep":
    case "Glob":   return "var(--cat-read)";
    case "Edit":
    case "Write":  return "var(--cat-edit)";
    case "Bash":   return "var(--cat-bash)";
    case "Task":   return "var(--cat-task)";
    case "Tool":   return "var(--cat-tool)";
    case "Mixed":  return "var(--cat-mixed)";
    default:       return "var(--cat-other)";
  }
}
```

Use it in the inline `style` and elsewhere.

- [ ] **Step 5: Visual check**

Open dev server. Verify rows render with the right colors. Click a row — the
active row should tint and the chip appear. Click again or click × to clear.
Compare to the mockup's "Time spent" section.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/lib/components/content/SessionVitals.svelte
git commit -m "feat(frontend): add Time spent section with click-to-highlight"
```

______________________________________________________________________

### Task 17: Timeline lanes section

Spec section: **Sections › 3. Timeline lanes**.

**Files:**

- Modify: `frontend/src/lib/components/content/SessionVitals.svelte`

- [ ] **Step 1: Open the mockup's Timeline block**

Look at `.lane-row`, `.lane-label`, `.lane-track`, `.lane-mark`,
`.activity-bar`, `.legend` in the mockup. Note the structure: a `turns` lane on
top, per-category lanes, then an `activity` lane at the bottom.

- [ ] **Step 2: Compute per-lane marks**

Add helpers in script:

```ts
function laneMarksByCategory(timing: SessionTiming): Map<string, TurnTiming[]> {
  const m = new Map<string, TurnTiming[]>();
  for (const t of timing.turns) {
    const arr = m.get(t.primary_category) ?? [];
    arr.push(t);
    m.set(t.primary_category, arr);
  }
  return m;
}

function turnLeftPct(turn: TurnTiming, sessionStart: number, sessionEnd: number): number {
  const t = new Date(turn.started_at).getTime();
  return ((t - sessionStart) / (sessionEnd - sessionStart)) * 100;
}

function turnWidthPct(turn: TurnTiming, sessionStart: number, sessionEnd: number): number {
  if (turn.duration_ms == null) return 0.5;
  return (turn.duration_ms / (sessionEnd - sessionStart)) * 100;
}
```

- [ ] **Step 3: Render the lanes**

Add a section under "Time spent":

```svelte
{#if timing}
  {@const sessionStart = new Date(timing.turns[0]?.started_at ?? Date.now()).getTime()}
  {@const sessionEnd = sessionStart + timing.total_duration_ms}
  <section class="v-section">
    <header class="v-h">
      <span>Timeline</span>
      <span class="v-meta">click marks to scroll</span>
    </header>

    <!-- turns lane -->
    <div class="lane-row">
      <span class="lane-label">turns</span>
      <span class="lane-track">
        {#each timing.turns as t (t.message_id)}
          <button
            class="lane-mark"
            class:dimmed={categoryFilter && t.primary_category !== categoryFilter}
            style="left: {turnLeftPct(t, sessionStart, sessionEnd)}%; width: {turnWidthPct(t, sessionStart, sessionEnd)}%; background: {categoryToken(t.primary_category)}"
            title={`${t.primary_category} · ${formatDuration(t.duration_ms ?? 0)}`}
            onclick={() => ui.scrollToOrdinal(t.ordinal)}
          ></button>
        {/each}
      </span>
    </div>

    <!-- per-category lanes -->
    {#each timing.by_category as cat (cat.category)}
      <div class="lane-row" class:dimmed={categoryFilter && cat.category !== categoryFilter}>
        <span class="lane-label">{cat.category}</span>
        <span class="lane-track">
          {#each timing.turns.filter((t) => t.primary_category === cat.category) as t (t.message_id)}
            <button
              class="lane-mark"
              style="left: {turnLeftPct(t, sessionStart, sessionEnd)}%; width: {turnWidthPct(t, sessionStart, sessionEnd)}%; background: {categoryToken(cat.category)}"
              onclick={() => ui.scrollToOrdinal(t.ordinal)}
            ></button>
          {/each}
        </span>
      </div>
    {/each}

    <!-- activity lane (delegate to existing sessionActivity store) -->
    <ActivityLane sessionId={sessionId} />

    <!-- legend -->
    <div class="legend">
      {#each ["Task", "Bash", "Tool", "Edit", "Read"] as cat}
        <span><span class="legend-dot" style="background: {categoryToken(cat)}"></span>{cat}</span>
      {/each}
    </div>
  </section>
{/if}
```

`ui.scrollToOrdinal(ordinal: number, sessionId?: string)` is the existing UI
store method (`frontend/src/lib/stores/ui.svelte.ts:380`). It takes the
message's `ordinal`, not the database `id` — that's why `TurnTiming` carries
both fields.

`<ActivityLane>` is a tiny wrapper component that renders the activity bars from
`sessionActivity` store. Either inline it or extract — see next sub-step.

- [ ] **Step 4: Activity lane integration**

Either:

- (a) inline the activity-bar rendering (read what `ActivityMinimap.svelte`
  currently does — it uses `sessionActivity.buckets`), or
- (b) extract the activity-bar render block into a small `ActivityLane.svelte`
  component used by both old and new panels during transition.

Option (b) is cleaner because we still need the bars; pull them out into a
sibling component.

- [ ] **Step 5: Add styles**

Copy `.lane-row`, `.lane-label`, `.lane-track`, `.lane-mark`,
`.lane-mark.dimmed`, `.activity-bar`, `.legend`, `.legend-dot` from the mockup
`<style>`.

- [ ] **Step 6: Visual check**

Open the live app + mockup. Compare the Timeline section, especially:

- Turns lane bar positions and widths look correct against session timeline

- Per-category lanes only show that category's turns

- Activity lane bars render at the bottom

- Legend dots colored correctly

- Click a mark → scrolls the conversation

- With a category filter set, non-matching lanes dim

- [ ] **Step 7: Commit**

```bash
git add frontend/src/lib/components/content/SessionVitals.svelte frontend/src/lib/components/content/ActivityLane.svelte
git commit -m "feat(frontend): add Timeline lanes section with click-to-scroll"
```

______________________________________________________________________

### Task 18: `CallRow` and `CallGroup` components

Spec section: **CallRow / CallGroup**.

**Files:**

- Create: `frontend/src/lib/components/content/CallRow.svelte`

- Create: `frontend/src/lib/components/content/CallGroup.svelte`

- [ ] **Step 1: Open the mockup's `.call`, `.cgroup`, `.cg-rail`, `.cg-header`
  rules**

These are the visual contract. Note the grid layout, the bar styling (`.cbar`,
`.cbar.shared`, `.cbar.live`, `.cbar-wrap.slow`), the chevron behavior on
sub-agents, the duration label color states.

- [ ] **Step 2: Create `CallRow.svelte`**

```svelte
<script lang="ts">
  import type { CallTiming } from "../../api/types/timing.js";
  import { formatDuration } from "../../utils/duration.js";
  import { categoryToken } from "./categoryToken.js"; // export from SessionVitals or share

  interface Props {
    call: CallTiming;
    barWidthPct: number;       // bar width as % of session timeline
    isSlow?: boolean;
    isShared?: boolean;        // parallel non-sub-agent: striped bar
    isLive?: boolean;          // running call
    isSubagentExpanded?: boolean;
    onClick?: () => void;
    onChevronClick?: () => void;
  }

  let {
    call, barWidthPct, isSlow = false, isShared = false,
    isLive = false, isSubagentExpanded = false,
    onClick, onChevronClick,
  }: Props = $props();

  let durationLabel = $derived.by(() => {
    if (isLive) return `running ${formatDuration(call.duration_ms ?? 0)}+`;
    if (call.duration_ms == null && isShared) {
      return `≤${/* group's turn duration; passed in via barWidthPct context — for now, omit */ ""}`;
    }
    return formatDuration(call.duration_ms ?? 0);
  });

  let isSubagent = $derived(call.subagent_session_id != null);
</script>

<button class="call" class:slow={isSlow} class:expanded={isSubagentExpanded} onclick={onClick}>
  {#if isSubagent}
    <span class="chev" onclick={(e) => { e.stopPropagation(); onChevronClick?.(); }}>▸</span>
  {:else}
    <span class="chev spacer">▸</span>
  {/if}
  <span class="cn" style="color: {categoryToken(call.category)}">{call.tool_name}</span>
  <span class="ca">{call.input_preview}</span>
  <span class="cbar-wrap">
    <span class="cbar" class:shared={isShared} class:live={isLive}
          style="width: {barWidthPct}%; background: {categoryToken(call.category)}"></span>
  </span>
  <span class="cd" class:slow={isSlow} class:live={isLive} class:muted={!isSlow && !isLive}>
    {durationLabel}
  </span>
</button>

<style>
  /* Copy .call, .call.slow, .chev, .cn, .ca, .cbar-wrap, .cbar, .cbar.shared, .cbar.live, .cd, .cd.slow, .cd.live, .cd.muted from the mockup. */
</style>
```

- [ ] **Step 3: Create `CallGroup.svelte`**

```svelte
<script lang="ts">
  import type { CallTiming } from "../../api/types/timing.js";
  import { formatDuration } from "../../utils/duration.js";
  import CallRow from "./CallRow.svelte";

  interface Props {
    calls: CallTiming[];
    groupDurationMs: number | null;
    barScalePct: (call: CallTiming) => number;
    onCallClick: (call: CallTiming) => void;
    onSubagentExpand: (call: CallTiming) => void;
    expandedSubagentIds: Set<string>;
    isLive?: boolean;
  }

  let { calls, groupDurationMs, barScalePct, onCallClick, onSubagentExpand, expandedSubagentIds, isLive = false }: Props = $props();

  let groupBarPct = $derived.by(() => {
    if (groupDurationMs == null) return 0;
    // session-relative width — same as a single CallRow's bar
    return calls.length > 0 ? barScalePct(calls[0]!) : 0;
  });
</script>

<div class="cgroup">
  <div class="cg-rail"></div>
  <div class="cg-members">
    <div class="cg-header">
      <span class="cg-h-label">parallel · {calls.length} calls</span>
      <span class="cg-h-bar-wrap"><span class="cg-h-bar" style="width: {groupBarPct}%"></span></span>
      <span class="cg-h-dur">{groupDurationMs != null ? formatDuration(groupDurationMs) : "—"}</span>
    </div>
    {#each calls as call (call.tool_use_id)}
      <CallRow
        call={call}
        barWidthPct={barScalePct(call)}
        isShared={!call.subagent_session_id}
        isLive={isLive && /* last call */ false}
        isSubagentExpanded={expandedSubagentIds.has(call.subagent_session_id ?? "")}
        onClick={() => onCallClick(call)}
        onChevronClick={() => onSubagentExpand(call)}
      />
    {/each}
  </div>
</div>

<style>
  /* Copy .cgroup, .cg-rail, .cg-rail::before, .cg-members, .cg-header, .cg-h-label, .cg-h-bar-wrap, .cg-h-bar, .cg-h-dur from mockup. */
</style>
```

- [ ] **Step 4: Type-check**

```
nix shell nixpkgs#nodejs -c npm --prefix frontend run check
```

- [ ] **Step 5: Commit**

```bash
git add frontend/src/lib/components/content/CallRow.svelte frontend/src/lib/components/content/CallGroup.svelte
git commit -m "feat(frontend): add CallRow and CallGroup components"
```

______________________________________________________________________

### Task 19: Calls section + sub-agent inline expansion

Spec section: **Sections › 4. Calls** and **sub-agent inline expansion**.

**Files:**

- Modify: `frontend/src/lib/components/content/SessionVitals.svelte`

- [ ] **Step 1: Add Calls section state**

```ts
let expandedSubagentIds = $state(new Set<string>());
let subagentTimings = $state(new Map<string, SessionTiming>());

async function toggleSubagent(call: CallTiming) {
  if (!call.subagent_session_id) return;
  const sid = call.subagent_session_id;
  if (expandedSubagentIds.has(sid)) {
    expandedSubagentIds.delete(sid);
    expandedSubagentIds = new Set(expandedSubagentIds);
    return;
  }
  if (!subagentTimings.has(sid)) {
    const t = await fetchSessionTiming(sid);
    subagentTimings.set(sid, t);
    subagentTimings = new Map(subagentTimings);
  }
  expandedSubagentIds.add(sid);
  expandedSubagentIds = new Set(expandedSubagentIds);
}
```

- [ ] **Step 2: Render the Calls section**

```svelte
{#if timing}
  <section class="v-section">
    <header class="v-h">
      <span>Calls</span>
      <span class="v-meta">{timing.tool_call_count} call{timing.tool_call_count === 1 ? "" : "s"}{timing.running ? " · 1 running" : ""}</span>
    </header>
    <div class="scale-axis">
      <!-- N evenly-spaced ticks; last labeled `now` if running, else session end -->
      <span>0</span>
      <span>{formatDuration(timing.total_duration_ms / 4)}</span>
      <span>{formatDuration(timing.total_duration_ms / 2)}</span>
      <span>{formatDuration((3 * timing.total_duration_ms) / 4)}</span>
      <span class:now={timing.running}>{timing.running ? "now" : formatDuration(timing.total_duration_ms)}</span>
    </div>
    <div class="calls">
      {#each timing.turns as turn (turn.message_id)}
        {#if turn.calls.length === 1}
          <CallRow
            call={turn.calls[0]}
            barWidthPct={callBarPct(turn.calls[0], timing)}
            isSlow={isSlowCall(turn.calls[0], timing)}
            isLive={turn.duration_ms == null && isLastTurn(turn, timing)}
            isSubagentExpanded={expandedSubagentIds.has(turn.calls[0].subagent_session_id ?? "")}
            onClick={() => ui.scrollToOrdinal(turn.ordinal)}
            onChevronClick={() => toggleSubagent(turn.calls[0])}
          />
          {#if turn.calls[0].subagent_session_id && expandedSubagentIds.has(turn.calls[0].subagent_session_id)}
            <SubagentCalls timing={subagentTimings.get(turn.calls[0].subagent_session_id)!} />
          {/if}
        {:else}
          <CallGroup
            calls={turn.calls}
            groupDurationMs={turn.duration_ms}
            barScalePct={(c) => callBarPct(c, timing)}
            isLive={turn.duration_ms == null && isLastTurn(turn, timing)}
            onCallClick={() => ui.scrollToOrdinal(turn.ordinal)}
            onSubagentExpand={toggleSubagent}
            expandedSubagentIds={expandedSubagentIds}
          />
          {#each turn.calls.filter((c) => c.subagent_session_id && expandedSubagentIds.has(c.subagent_session_id)) as expandedCall (expandedCall.tool_use_id)}
            <SubagentCalls timing={subagentTimings.get(expandedCall.subagent_session_id!)!} />
          {/each}
        {/if}
      {/each}
    </div>
  </section>
{/if}
```

`<SubagentCalls>` is a small recursive component (or inline div) that renders
the child session's calls in a tinted nested block. Pattern matches the mockup's
`.sa-expand` styling.

- [ ] **Step 3: Compute slow threshold**

Per spec **Slow threshold**: top 10% of measurable durations. Add:

```ts
let slowCallThresholdMs = $derived.by(() => {
  if (!timing) return Infinity;
  const durations = timing.turns
    .flatMap((t) => t.calls)
    .map((c) => c.duration_ms)
    .filter((d): d is number => d != null);
  if (durations.length < 10) return durations.length > 0 ? Math.max(...durations) : Infinity;
  durations.sort((a, b) => b - a);
  return durations[Math.floor(durations.length * 0.1)] ?? Infinity;
});

function isSlowCall(c: CallTiming, _t: SessionTiming): boolean {
  return c.duration_ms != null && c.duration_ms >= slowCallThresholdMs;
}
```

- [ ] **Step 4: Add styles**

Copy `.scale-axis`, `.scale-axis .now`, `.calls`, `.sa-expand`,
`.sa-expand .sa-eh`, `.sa-expand .sa-eh-meta` from the mockup.

- [ ] **Step 5: Visual check**

Open the live app + mockup. Verify the Calls section:

- Calls render in chronological order

- Single-call turns render as a flat row

- Parallel turns render as a `CallGroup` with its rail and header

- Sub-agent rows expand inline when the chevron is clicked

- Slow rows get the red dot + tinted bar background

- Bar widths look proportional to the session timeline

- Click a row body → conversation scrolls

- [ ] **Step 6: Commit**

```bash
git add frontend/src/lib/components/content/SessionVitals.svelte frontend/src/lib/components/content/SubagentCalls.svelte
git commit -m "feat(frontend): add Calls section with sub-agent inline expansion"
```

______________________________________________________________________

### Task 20: Click-to-highlight wiring across panel

Spec section: **Click behavior across the panel**.

**Files:**

- Modify: `frontend/src/lib/components/content/SessionVitals.svelte`

- [ ] **Step 1: Apply `categoryFilter` to all sections**

In each render block, add the `dimmed` class based on `categoryFilter`:

- "Time spent" rows: dim if
  `categoryFilter && row.category !== categoryFilter` (already done in Task 16;
  verify)

- Timeline lanes: dim non-matching lanes (already done in Task 17; verify)

- Calls list: dim a `CallRow` if
  `categoryFilter && row.category !== categoryFilter`. For a `CallGroup`, dim
  the whole group iff its turn's `primary_category !== categoryFilter` (parallel
  groups dim as a unit per the spec attribution rule).

- [ ] **Step 2: Add the dim CSS rule**

```css
.call.dimmed,
.cgroup.dimmed,
.lane-row.dimmed,
.agg-row.dimmed {
  opacity: 0.30;
  transition: opacity 0.18s;
}
.lane-row.dimmed { opacity: 0.40; }  /* lanes dim less per mockup */
.agg-row.dimmed  { opacity: 0.40; }
```

- [ ] **Step 3: Visual check**

Live app: click "Bash" in "Time spent". Confirm:

- Bash row tints with the active background + ring
- Filter chip appears in the section header with the × close button
- Other agg rows dim to 40%
- Timeline: Bash lane stays full, others dim to 40%
- Calls list: Bash rows stay full, others dim to 30%
- Parallel groups dim if their primary category isn't Bash, even if some
  children are
- Click the chip × or Bash row again → all clears

Compare to the mockup's "Highlight on click" annotation page (visible in the
brainstorm session — also covered in the complete-design.html footnotes).

- [ ] **Step 4: Commit**

```bash
git add frontend/src/lib/components/content/SessionVitals.svelte
git commit -m "feat(frontend): wire click-to-highlight across panel sections"
```

______________________________________________________________________

### Task 21: Live state animations and SSE refresh

Spec section: **Live (running) sessions**.

**Files:**

- Modify: `frontend/src/lib/components/content/SessionVitals.svelte`

- Modify: `frontend/src/lib/components/content/CallRow.svelte` (live bar
  animation)

- [ ] **Step 1: Live grow animation**

Add to `CallRow.svelte` `<style>`:

```css
@keyframes live-grow-fallback {
  /* While SSE updates, grow linearly from current width.
     The component re-renders on each SSE event with a fresh width,
     so this keyframe just smooths the gap. */
  from { transform: scaleX(0.985); }
  to   { transform: scaleX(1.000); }
}
.cbar.live {
  background: linear-gradient(90deg, var(--running-fg), color-mix(in srgb, var(--running-fg) 70%, black));
  animation: duration-pulse 1.6s ease-in-out infinite, live-grow-fallback 1s linear infinite;
  transform-origin: left center;
}
```

- [ ] **Step 2: Live elapsed update tick**

In `SessionVitals.svelte`, when `timing.running`, set up a 1-second ticker so
the elapsed labels update between SSE events:

```ts
let liveTick = $state(0);
$effect(() => {
  if (!timing?.running) return;
  const id = setInterval(() => (liveTick += 1), 1000);
  return () => clearInterval(id);
});

// derived live elapsed (used by stat-grid `tool time` and others):
let liveElapsedMs = $derived.by(() => {
  void liveTick; // re-trigger
  if (!timing) return 0;
  return timing.tool_duration_ms;
});
```

(SSE events still drive truth-source refresh via `sessionTiming.applyEvent` from
Task 9; the ticker only smooths the gap.)

- [ ] **Step 3: Visual check (live session)**

Find or trigger a live session. Open the right panel and verify:

- Session header `running 4m 18s+` text in green, pulsing

- Stat grid has `tool time` with `+` suffix and `in flight` tile (read the
  mockup; you may need to add a new stat tile if the live session is currently
  active)

- Time spent: only completed turns contribute (in-flight turn excluded)

- Calls: the running row at the bottom has a green pulsing bar with the
  slow-grow animation; duration label pulses

- Scale axis last tick says `now`

- The SSE event triggers a real refresh — confirm by adding a temporary
  console.log in `applyEvent`

- [ ] **Step 4: Commit**

```bash
git add frontend/src/lib/components/content/SessionVitals.svelte frontend/src/lib/components/content/CallRow.svelte
git commit -m "feat(frontend): live state animations and SSE-driven refresh"
```

______________________________________________________________________

### Task 22: Visual checkpoint B — full right panel

This is a forced gate before wiring the panel into App.svelte.

- [ ] **Step 1: Open mockup + dev app side by side**

Open both. Pick a non-trivial session (parallel turns, sub-agents, ideally a
long one).

- [ ] **Step 2: Compare every section**

| Section                                                                                     | Match? |
| ------------------------------------------------------------------------------------------- | ------ |
| Session header (right meta, stat grid)                                                      |        |
| Time spent (bars, colors, click-to-highlight, filter chip)                             |        |
| Timeline (turns lane, per-category lanes, activity lane, legend)                            |        |
| Calls (chronological order, parallel groups, sub-agent expansion, slow tinting, scale axis) |        |
| Live session: running row, pulsing animations, `now` label, in-flight stat tile             |        |

- [ ] **Step 3: Fix divergences**

Before continuing.

- [ ] **Step 4: Capture screenshots**

Save:

- `docs/superpowers/specs/checkpoints/2026-04-26-right-panel-completed.png` (a
  completed session)
- `docs/superpowers/specs/checkpoints/2026-04-26-right-panel-live.png` (a live
  session)

```bash
git add docs/superpowers/specs/checkpoints/*.png
git commit -m "docs: checkpoint B — right panel matches mockup"
```

______________________________________________________________________

## Phase F — Wiring + cleanup

### Task 23: Mount `SessionVitals`, rename UI store members, delete `ActivityMinimap`

Spec section: **Routing & layout**.

**Files:**

- Modify: `frontend/src/App.svelte`

- Modify: `frontend/src/lib/stores/ui.svelte.ts`

- Modify: `frontend/src/lib/components/layout/SessionBreadcrumb.svelte`

- Delete: `frontend/src/lib/components/content/ActivityMinimap.svelte` (after
  grep)

- [ ] **Step 1: Replace `<ActivityMinimap>` in `App.svelte`**

Find the import (line 9) and the usage (line 426). Replace:

```svelte
<!-- before -->
import ActivityMinimap from "./lib/components/content/ActivityMinimap.svelte";
...
<ActivityMinimap sessionId={...} />

<!-- after -->
import SessionVitals from "./lib/components/content/SessionVitals.svelte";
...
<SessionVitals sessionId={...} />
```

Also widen the right column from ~200px to 320px in the layout grid (find the
grid-template-columns or width style in `App.svelte` and update).

- [ ] **Step 2: Rename `ui` store members**

In `frontend/src/lib/stores/ui.svelte.ts`, find `toggleActivityMinimap` (line
412\) and the corresponding visibility flag. Rename:

- `toggleActivityMinimap` → `toggleVitals`
- `activityMinimapVisible` (or whatever the flag is named) → `vitalsVisible`

Use a global rename in the file.

- [ ] **Step 3: Update `SessionBreadcrumb`**

Open `frontend/src/lib/components/layout/SessionBreadcrumb.svelte` (line 576).
Update the call site from `ui.toggleActivityMinimap()` to `ui.toggleVitals()`.
Also update any visible label/icon if it reads "minimap".

- [ ] **Step 4: Verify nothing else references `ActivityMinimap`**

```bash
rg -l "ActivityMinimap" frontend/src
```

Expected: only `App.svelte` (already removed) and the `ActivityMinimap.svelte`
file itself.

- [ ] **Step 5: Delete `ActivityMinimap.svelte`**

```bash
rm frontend/src/lib/components/content/ActivityMinimap.svelte
rg -l "ActivityMinimap" frontend/src   # should now be empty
```

- [ ] **Step 6: Build and visual check**

```
make build
make dev
make frontend-dev
```

Open the live app. The right column now shows `SessionVitals` directly. Click
around different sessions — verify the panel renders and updates correctly.
Compare to the mockup one final time.

- [ ] **Step 7: Type-check + lint**

```
nix shell nixpkgs#nodejs -c npm --prefix frontend run check
```

- [ ] **Step 8: Commit**

```bash
git add frontend/src/App.svelte frontend/src/lib/stores/ui.svelte.ts frontend/src/lib/components/layout/SessionBreadcrumb.svelte
git rm frontend/src/lib/components/content/ActivityMinimap.svelte
git commit -m "feat(frontend): mount SessionVitals; remove ActivityMinimap"
```

______________________________________________________________________

## Phase G — Testing and final review

### Task 24: Test fixture extensions

**Files:**

- Modify: `cmd/testfixture/main.go`

- [ ] **Step 1: Read the existing fixture generator**

Open `cmd/testfixture/main.go`. See what it currently generates. Identify how to
add a new session shape.

- [ ] **Step 2: Add a "duration UX showcase" fixture**

Add a generator that produces a session with:

- A solo Skill call (~2s)
- A parallel turn: 2 Reads + 1 Agent (Task) sub-agent that takes ~2m
- A solo Bash call that's slow (~28s)
- (Optional) a turn that's still running at fixture time

The generator should write the session JSONL plus the child sub-agent JSONL into
the fixture data dir.

```go
// fixtureDurationShowcase generates a session demonstrating the
// duration UX: solo, parallel, sub-agent, and slow tool calls.
func fixtureDurationShowcase(dir string) error {
	// ...
	return nil
}
```

Hook it into the fixture-generation entry point.

- [ ] **Step 3: Run the fixture**

```bash
go run ./cmd/testfixture --out testdata/fixtures/duration-showcase
```

Confirm it produces the expected files. Sync into a fresh DB and open in the dev
server to eyeball.

- [ ] **Step 4: Commit**

```bash
git add cmd/testfixture/main.go testdata/fixtures/duration-showcase
git commit -m "feat(testfixture): add duration UX showcase session"
```

______________________________________________________________________

### Task 25: Playwright E2E

**Files:**

- Create: `frontend/e2e/session-timing.spec.ts`

- [ ] **Step 1: Read existing Playwright tests**

Open `frontend/e2e/` to see the convention (page object pattern, fixture, base
URL setup).

- [ ] **Step 2: Write the E2E spec**

```ts
// frontend/e2e/session-timing.spec.ts
import { expect, test } from "@playwright/test";

test.describe("Session Vital Signs", () => {
  test.beforeEach(async ({ page }) => {
    // Use the duration-showcase fixture from Task 24.
    await page.goto("/sessions/duration-showcase");
  });

  test("renders the four sections", async ({ page }) => {
    await expect(page.getByRole("heading", { name: "Session" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Time spent" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Timeline" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Calls" })).toBeVisible();
  });

  test("clicking a Time spent row highlights matching rows", async ({ page }) => {
    await page.getByRole("button", { name: /^Bash/ }).click();
    // Filter chip appears
    await expect(page.locator(".filter-chip")).toContainText("Bash");
    // Non-Bash agg rows dim
    const taskRow = page.locator('.agg-row:has(.agg-name:text("Task"))');
    await expect(taskRow).toHaveClass(/dimmed/);
    // Bash calls remain full opacity in Calls list; non-Bash dim
    // (assert via opacity computed style or class)
    // Click chip × to clear
    await page.locator(".filter-chip .x").click();
    await expect(page.locator(".filter-chip")).toHaveCount(0);
  });

  test("clicking a call row scrolls the conversation", async ({ page }) => {
    const conversation = page.locator(".conv-body");
    const beforeScroll = await conversation.evaluate((el) => el.scrollTop);

    // Find a slow call row in the Calls section, click it.
    const slowRow = page.locator(".call.slow").first();
    await slowRow.click();

    const afterScroll = await conversation.evaluate((el) => el.scrollTop);
    expect(afterScroll).not.toBe(beforeScroll);
  });

  test("sub-agent expands inline when chevron clicked", async ({ page }) => {
    // The Agent call row in the parallel group has a clickable chevron.
    const agentRow = page.locator('.call:has(.cn:text("Agent"))').first();
    await agentRow.locator(".chev").click();
    // The sub-agent's calls are now visible inside `.sa-expand`.
    await expect(page.locator(".sa-expand").first()).toBeVisible();
  });
});
```

- [ ] **Step 3: Run E2E**

```
make e2e
```

Expected: PASS. Fix any selector or timing issues until green.

- [ ] **Step 4: Commit**

```bash
git add frontend/e2e/session-timing.spec.ts
git commit -m "test(e2e): add Playwright spec for Session Vital Signs"
```

______________________________________________________________________

### Task 26: Final visual review against mockup

Last gate before declaring the feature done.

- [ ] **Step 1: Open mockup + live app**

```bash
make build
make dev
```

Open `http://localhost:8080` (the built binary serves the embedded SPA).
Navigate to the duration-showcase fixture session.

- [ ] **Step 2: Whole-screen comparison**

Full-screen the live app. Open the mockup full-screen in a separate window.
Compare:

- Sidebar — unchanged
- Conversation column — turn summaries, ToolBlock badges, ParallelGroup,
  sub-agent inline
- Right panel — all four sections, in order, all interactions

For any discrepancy bigger than minor pixel offsets:

- Decide: is the mockup wrong (i.e. it set an unrealistic expectation), or is
  the implementation wrong?

- If the mockup is wrong, raise it explicitly in commit message and update the
  mockup.

- If the implementation is wrong, fix it before claiming done.

- [ ] **Step 3: Run all checks**

```
make vet
make lint
make test
make test-postgres
make e2e
nix shell nixpkgs#nodejs -c npm --prefix frontend test
nix shell nixpkgs#nodejs -c npm --prefix frontend run check
```

Expected: all green.

- [ ] **Step 4: Commit checkpoint screenshot**

Save `docs/superpowers/specs/checkpoints/2026-04-26-final.png` of the full
session view.

```bash
git add docs/superpowers/specs/checkpoints/2026-04-26-final.png
git commit -m "docs: final checkpoint — duration UX matches mockup"
```

______________________________________________________________________

## Self-review notes

The plan covers every section of the spec. Per-section coverage:

| Spec section                                         | Plan task                                                |
| ---------------------------------------------------- | -------------------------------------------------------- |
| Visual contract                                      | (referenced throughout, gated at Tasks 14, 22, 26)       |
| Duration semantics → Display rules                   | Tasks 10, 11, 12, 13 (frontend) + Tasks 5, 6 (backend)   |
| Aggregate attribution rule                           | Task 2 (frontend) + Tasks 5, 6 (backend)                 |
| Backend → Database, Per-message, Per-tool sub-agent  | Tasks 5, 6                                               |
| Backend → Session timing summary endpoint            | Task 7                                                   |
| Backend → Store interface and PG mirror              | Tasks 4, 6                                               |
| Backend → SSE updates                                | Task 8                                                   |
| Frontend → Inline duration badge in `ToolBlock`      | Task 10                                                  |
| Frontend → Parallel group rendering in `MessageList` | Tasks 12, 13                                             |
| Frontend → Turn summary on assistant message         | Task 11                                                  |
| Frontend → Session Vital Signs component             | Tasks 15, 16, 17, 18, 19                                 |
| Frontend → CallRow / CallGroup                       | Task 18                                                  |
| Frontend → Routing & layout                          | Task 23                                                  |
| Visual encoding                                      | Task 3 + per-component CSS in Tasks 10–21                |
| Live (running) sessions                              | Task 21                                                  |
| Performance                                          | (informational; required indexes already exist per spec) |
| Testing                                              | Tasks 5, 6, 7, 24, 25                                    |

Check passed. No placeholders found. Type names consistent (`SessionTiming`,
`CallTiming`, `TurnTiming`, `CategoryTotal`) across Go and TS. Method names
consistent (`GetSessionTiming`, `attributeTurn`, `formatDuration`,
`categoryToken`).
