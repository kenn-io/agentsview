# User-Defined Automation Patterns Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let users append their own prefix patterns to the automated-session
classifier via `~/.agentsview/config.toml`, with a content-addressed hash that
auto-triggers backfill whenever the classifier set changes.

**Architecture:** Seven units — TOML config schema, a `RWMutex`-guarded
user-prefix singleton in `internal/db`, a SHA-256 classifier hash, hash-driven
backfill drivers for SQLite and PostgreSQL, central wiring helper in
`cmd/agentsview` with a static AST guardrail test, and an
`agentsview classifier rebuild` recovery command. PG push switches to trusting
`sess.IsAutomated` so PG never recomputes locally.

**Tech Stack:** Go 1.22+, SQLite (cgo `mattn/go-sqlite3`, fts5 build tag),
PostgreSQL via `pgx`, `BurntSushi/toml`, `spf13/cobra`, `go/parser` + `go/ast`
for the static guardrail test.

**Spec:**
`docs/superpowers/specs/2026-04-22-user-defined-automation-patterns-design.md`

______________________________________________________________________

## Pre-flight

Verify the working tree is clean and on the feature branch before starting.

- [ ] **Pre-step 1: Confirm branch and clean state**

```bash
git status
git branch --show-current
```

Expected: branch is `feat/user-defined-automation-patterns`, working tree clean
(or only this plan file untracked).

- [ ] **Pre-step 2: Confirm baseline tests pass**

```bash
make test-short
```

Expected: PASS. If failing on `main`, stop and report — the failures are
pre-existing and would mask regressions.

______________________________________________________________________

## Task 1: Config schema

**Files:**

- Modify: `internal/config/config.go` — add `AutomatedConfig` struct and
  `Automated` field on `Config`; teach `loadFile` to read it.
- Modify: `internal/config/config_test.go` — TOML round-trip test.

**Context:** Config does *no* normalization. It only parses the raw
`[automated] prefixes` array into a `[]string`. All
trim/dedupe/length-cap/built-in-overlap rules live in
`internal/db.SetUserAutomationPrefixes` (Task 2). This keeps `internal/config`
free of any classifier coupling.

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:

```go
func TestAutomatedPrefixesRoundTrip(t *testing.T) {
	dir := setupTestEnv(t)
	writeConfig(t, dir, map[string]any{
		"automated": map[string]any{
			"prefixes": []string{
				"You are analyzing an essay",
				"You are grading quotes",
				"  ", // whitespace preserved here; normalization is db-side
				"You are analyzing an essay", // duplicate preserved here too
			},
		},
	})
	cfg, err := loadConfigFromPFlags(t)
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	got := cfg.Automated.Prefixes
	want := []string{
		"You are analyzing an essay",
		"You are grading quotes",
		"  ",
		"You are analyzing an essay",
	}
	if !slices.Equal(got, want) {
		t.Errorf("prefixes = %q, want %q", got, want)
	}
}

func TestAutomatedPrefixesAbsentIsNil(t *testing.T) {
	dir := setupTestEnv(t)
	writeConfig(t, dir, map[string]any{
		"public_url": "http://example.com",
	})
	cfg, err := loadConfigFromPFlags(t)
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	if cfg.Automated.Prefixes != nil {
		t.Errorf("expected nil, got %v", cfg.Automated.Prefixes)
	}
}
```

If `slices` is not yet imported in this test file, add it to the import block.

- [ ] **Step 2: Run test to verify it fails**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/config/ -run TestAutomatedPrefixes -v
```

Expected: FAIL — `cfg.Automated` undefined (compile error).

- [ ] **Step 3: Add struct and field**

In `internal/config/config.go`, add the `AutomatedConfig` type near the other
named config structs (e.g. just below `PGConfig`):

```go
// AutomatedConfig holds user-supplied additions to the
// automated-session classifier. Parse-only; all semantic
// normalization (trim, dedupe, length cap, built-in overlap
// drop) happens inside db.SetUserAutomationPrefixes.
type AutomatedConfig struct {
	Prefixes []string `toml:"prefixes" json:"prefixes,omitempty"`
}
```

Add the field to the `Config` struct (just below `PG PGConfig`):

```go
	Automated AutomatedConfig `json:"automated,omitempty" toml:"automated"`
```

- [ ] **Step 4: Wire `loadFile` to read the TOML section**

In the `var file struct { ... }` declaration inside `loadFile`, add:

```go
		Automated AutomatedConfig `toml:"automated"`
```

After the existing assignments (right before the agent-dirs raw decode loop),
add:

```go
	if file.Automated.Prefixes != nil {
		c.Automated.Prefixes = file.Automated.Prefixes
	}
```

- [ ] **Step 5: Run test to verify it passes**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/config/ -run TestAutomatedPrefixes -v
```

Expected: PASS.

- [ ] **Step 6: Run formatter and vet**

```bash
go fmt ./internal/config/...
go vet ./internal/config/...
```

Expected: no diff, no warnings.

- [ ] **Step 7: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): parse [automated] prefixes from config.toml"
```

______________________________________________________________________

## Task 2: Classifier singleton

**Files:**

- Modify: `internal/db/automated.go` — add `userPrefixes` singleton,
  `SetUserAutomationPrefixes`, `UserAutomationPrefixes`,
  `normalizeUserPrefixes`; extend `IsAutomatedSession` to walk user prefixes.
- Modify: `internal/db/automated_test.go` — table tests for
  `normalizeUserPrefixes`; extend `TestIsAutomatedSession` with a user-prefixes
  group.

**Context:** Singleton lives in `internal/db` so the built-in
`automatedPrefixes` slice stays unexported. `Set` is silent (no logging) — quiet
CLI paths like `usage --statusline` and `pg push` rely on this. Read path uses
`RWMutex` so it's lock-free under contention. `Set` writes a normalized copy;
`Get` returns a copy; both prevent mutation through the shared backing array.

- [ ] **Step 1: Write the failing test for `normalizeUserPrefixes`**

Append to `internal/db/automated_test.go`:

```go
func TestNormalizeUserPrefixes(t *testing.T) {
	long := strings.Repeat("a", 1025)
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"Nil", nil, nil},
		{"Empty", []string{}, nil},
		{"AllWhitespace", []string{"   ", "\t\n"}, nil},
		{"TrimsEachEntry", []string{"  hello  ", "world\n"}, []string{"hello", "world"}},
		{"DropEmpty", []string{"hello", "", "  ", "world"}, []string{"hello", "world"}},
		{"DropTooLong", []string{"hello", long}, []string{"hello"}},
		{"DropDuplicate", []string{"a", "b", "a"}, []string{"a", "b"}},
		{"DropDuplicateAfterTrim", []string{"a", " a "}, []string{"a"}},
		{
			"DropBuiltInOverlap",
			[]string{"You are a code reviewer.", "novel"},
			[]string{"novel"},
		},
		{
			"PreservesUserOrder",
			[]string{"zeta", "alpha", "mu"},
			[]string{"zeta", "alpha", "mu"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeUserPrefixes(tt.in)
			if !slices.Equal(got, tt.want) {
				t.Errorf("normalizeUserPrefixes(%q) = %q, want %q",
					tt.in, got, tt.want)
			}
		})
	}
}
```

If `slices` and `strings` are not imported in `automated_test.go`, add them.

- [ ] **Step 2: Write the failing test for `IsAutomatedSession` with user
  prefixes**

Append to `internal/db/automated_test.go` (after `TestIsAutomatedSession`):

```go
func TestIsAutomatedSessionWithUserPrefixes(t *testing.T) {
	t.Cleanup(func() { SetUserAutomationPrefixes(nil) })
	SetUserAutomationPrefixes([]string{
		"You are analyzing an essay",
		"Grade these Benn Stancil quotes",
	})

	tests := []struct {
		name         string
		firstMessage string
		want         bool
	}{
		{
			"UserPrefixMatchesEssayPrompt",
			"You are analyzing an essay about epistemology.",
			true,
		},
		{
			"UserPrefixMatchesGradeQuotes",
			"Grade these Benn Stancil quotes for me.",
			true,
		},
		{
			"UserPrefixDoesNotMatchUnrelated",
			"How do I fix this bug?",
			false,
		},
		{
			"BuiltInPrefixStillMatches",
			"You are a code reviewer. Review the diff.",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsAutomatedSession(tt.firstMessage)
			if got != tt.want {
				t.Errorf("IsAutomatedSession(%q) = %v, want %v",
					tt.firstMessage, got, tt.want)
			}
		})
	}
}

func TestUserAutomationPrefixesReturnsCopy(t *testing.T) {
	t.Cleanup(func() { SetUserAutomationPrefixes(nil) })
	SetUserAutomationPrefixes([]string{"alpha", "beta"})
	got := UserAutomationPrefixes()
	if len(got) > 0 {
		got[0] = "MUTATED"
	}
	again := UserAutomationPrefixes()
	if len(again) == 0 || again[0] != "alpha" {
		t.Errorf("singleton mutated through returned slice: got %q", again)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run "TestNormalizeUserPrefixes|TestIsAutomatedSessionWithUserPrefixes|TestUserAutomationPrefixesReturnsCopy" -v
```

Expected: FAIL — `normalizeUserPrefixes` / `SetUserAutomationPrefixes` /
`UserAutomationPrefixes` undefined.

- [ ] **Step 4: Implement the singleton**

In `internal/db/automated.go`:

Add `sync` to the import block (alphabetical order keeps it between `slices` and
`strings`):

```go
import (
	"slices"
	"strings"
	"sync"
)
```

After the `automatedExactMatches` slice, add:

```go
const userPrefixMaxLen = 1024

var (
	userPrefixesMu sync.RWMutex
	userPrefixes   []string
)

// SetUserAutomationPrefixes replaces the user-pattern slice
// with a normalized copy of the input. Normalization (trim,
// drop empty, length cap, dedupe within input, drop entries
// that equal a built-in prefix) happens here so callers can
// pass the raw list straight from config. Pass nil to clear.
// Idempotent and silent — safe to call from quiet CLI paths
// (statusline, JSON output). Callers that want a startup
// summary should read len(UserAutomationPrefixes()).
func SetUserAutomationPrefixes(prefixes []string) {
	cleaned := normalizeUserPrefixes(prefixes)
	userPrefixesMu.Lock()
	defer userPrefixesMu.Unlock()
	userPrefixes = cleaned
}

// UserAutomationPrefixes returns a copy of the current
// user-prefix slice. Used by ClassifierHash and tests; the
// copy prevents callers from mutating singleton state.
func UserAutomationPrefixes() []string {
	userPrefixesMu.RLock()
	defer userPrefixesMu.RUnlock()
	return append([]string(nil), userPrefixes...)
}

// normalizeUserPrefixes applies the validation rules from the
// design spec ("Validation behavior" section). Built-in
// overlap is checked against the package-private
// automatedPrefixes directly.
func normalizeUserPrefixes(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if s == "" || len(s) > userPrefixMaxLen {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		if slices.Contains(automatedPrefixes, s) {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
```

- [ ] **Step 5: Extend `IsAutomatedSession` to walk user prefixes**

Replace the body of `IsAutomatedSession` with:

```go
func IsAutomatedSession(firstMessage string) bool {
	for _, prefix := range automatedPrefixes {
		if strings.HasPrefix(firstMessage, prefix) {
			return true
		}
	}
	userPrefixesMu.RLock()
	for _, prefix := range userPrefixes {
		if strings.HasPrefix(firstMessage, prefix) {
			userPrefixesMu.RUnlock()
			return true
		}
	}
	userPrefixesMu.RUnlock()
	for _, sub := range automatedSubstrings {
		if strings.Contains(firstMessage, sub) {
			return true
		}
	}
	trimmed := strings.TrimSpace(firstMessage)
	return slices.Contains(automatedExactMatches, trimmed)
}
```

- [ ] **Step 6: Run all tests in this file to verify pass**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run "TestNormalizeUserPrefixes|TestIsAutomatedSession" -v
```

Expected: PASS for all subtests, including the existing `TestIsAutomatedSession`
cases.

- [ ] **Step 7: Run formatter and vet**

```bash
go fmt ./internal/db/...
go vet ./internal/db/...
```

Expected: no diff, no warnings.

- [ ] **Step 8: Commit**

```bash
git add internal/db/automated.go internal/db/automated_test.go
git commit -m "feat(db): user-prefix classifier singleton with normalization"
```

______________________________________________________________________

## Task 3: Classifier hash

**Files:**

- Create: `internal/db/classifier_hash.go` — `ClassifierHash()` and the
  `classifierAlgorithmVersion` constant.
- Create: `internal/db/classifier_hash_test.go` — stability and sensitivity
  tests.

**Context:** Hash inputs are tagged (`P`/`S`/`E`/`U`) and length-prefixed so two
different pattern sets can never collide by splicing across slice boundaries.
`classifierAlgorithmVersion` lives next to the function that consumes it so a
logic-change reviewer sees both at once.

- [ ] **Step 1: Write the failing tests**

Create `internal/db/classifier_hash_test.go`:

```go
package db

import (
	"testing"
)

func TestClassifierHashStable(t *testing.T) {
	t.Cleanup(func() { SetUserAutomationPrefixes(nil) })
	SetUserAutomationPrefixes([]string{"foo", "bar"})
	a := ClassifierHash()
	b := ClassifierHash()
	if a != b {
		t.Errorf("hash unstable: %s vs %s", a, b)
	}
}

func TestClassifierHashChangesWithUserPrefixes(t *testing.T) {
	t.Cleanup(func() { SetUserAutomationPrefixes(nil) })
	SetUserAutomationPrefixes(nil)
	base := ClassifierHash()
	SetUserAutomationPrefixes([]string{"You are analyzing an essay"})
	with := ClassifierHash()
	if base == with {
		t.Errorf("hash did not change when user prefixes changed: %s", base)
	}
}

func TestClassifierHashOrderIndependent(t *testing.T) {
	t.Cleanup(func() { SetUserAutomationPrefixes(nil) })
	SetUserAutomationPrefixes([]string{"alpha", "beta", "gamma"})
	a := ClassifierHash()
	SetUserAutomationPrefixes([]string{"gamma", "alpha", "beta"})
	b := ClassifierHash()
	if a != b {
		t.Errorf("hash not order-independent: %s vs %s", a, b)
	}
}

// TestClassifierHashTagSeparation guards against the case
// where two different categorizations produce the same hash
// because the tag prefix was dropped from the encoding.
func TestClassifierHashTagSeparation(t *testing.T) {
	t.Cleanup(func() { SetUserAutomationPrefixes(nil) })
	SetUserAutomationPrefixes([]string{"Warmup"})
	got := ClassifierHash()
	SetUserAutomationPrefixes(nil)
	bareBuiltins := ClassifierHash()
	if got == bareBuiltins {
		t.Errorf(
			"user prefix 'Warmup' collided with built-in exact-match 'Warmup': %s",
			got,
		)
	}
}

// TestClassifierHashCurrentAlgoVersion is a forced-bump
// guard: it pins the algorithm version at construction time.
// If a future change to the matching logic forgets to bump
// classifierAlgorithmVersion, this test still passes (false
// negative) — but if someone bumps the version intentionally
// the test must be updated to match. The check exists to
// surface accidental version-constant edits during review.
func TestClassifierHashCurrentAlgoVersion(t *testing.T) {
	if classifierAlgorithmVersion != 1 {
		t.Fatalf(
			"classifierAlgorithmVersion changed to %d; "+
				"update this test and confirm matching "+
				"semantics actually changed (not just "+
				"pattern edits, which the hash already "+
				"detects)",
			classifierAlgorithmVersion,
		)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run TestClassifierHash -v
```

Expected: FAIL — `ClassifierHash` undefined.

- [ ] **Step 3: Implement the hash**

Create `internal/db/classifier_hash.go`:

```go
package db

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"
)

// classifierAlgorithmVersion bumps when the matching *logic*
// changes (e.g. a future case-insensitivity flag). Pattern
// edits do NOT bump this — those are detected automatically
// by including the pattern slices in the hash. Bumping this
// constant invalidates every stored hash and forces a
// backfill on next open of any DB.
const classifierAlgorithmVersion = 1

// ClassifierHash returns a stable hex-encoded SHA-256 over
// the algorithm version, all built-in pattern slices, and the
// currently configured user prefixes. Inputs are sorted
// before hashing so config order doesn't affect the result.
// Tagged + length-prefixed encoding prevents splice
// collisions between slice boundaries (e.g. moving an entry
// from substrings to exact-matches must change the hash).
func ClassifierHash() string {
	h := sha256.New()
	fmt.Fprintf(h, "v%d\n", classifierAlgorithmVersion)
	writeSorted(h, "P", automatedPrefixes)
	writeSorted(h, "S", automatedSubstrings)
	writeSorted(h, "E", automatedExactMatches)
	writeSorted(h, "U", UserAutomationPrefixes())
	return hex.EncodeToString(h.Sum(nil))
}

func writeSorted(h hash.Hash, tag string, items []string) {
	sorted := append([]string(nil), items...)
	sort.Strings(sorted)
	for _, s := range sorted {
		fmt.Fprintf(h, "%s\t%d\t%s\n", tag, len(s), s)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run TestClassifierHash -v
```

Expected: PASS.

- [ ] **Step 5: Run formatter and vet**

```bash
go fmt ./internal/db/...
go vet ./internal/db/...
```

Expected: no diff, no warnings.

- [ ] **Step 6: Commit**

```bash
git add internal/db/classifier_hash.go internal/db/classifier_hash_test.go
git commit -m "feat(db): classifier hash for backfill change detection"
```

______________________________________________________________________

## Task 4: SQLite backfill driver — marker → hash

**Files:**

- Modify: `internal/db/automated.go` — remove the exported
  `IsAutomatedBackfillMarker`.
- Modify: `internal/db/db.go` — replace the marker-based gate in
  `backfillIsAutomatedLocked` with a hash compare; add the
  `classifierHashStatsKey` constant.
- Modify: `internal/db/automated_backfill_test.go` — update existing tests to
  use the new key; add a hash-change test.

**Context:** The backfill keeps its existing scan/set/clear loop unchanged. Only
the gate (run vs skip) and the post-pass write change. Legacy `_v2`/`_v3` keys
stay in `stats` as dead data — no destructive migration needed because `db.Open`
handles "no hash stored yet" by writing one after the first pass.

- [ ] **Step 1: Update existing backfill tests to use the new key**

In `internal/db/automated_backfill_test.go`, the four existing tests reference
`IsAutomatedBackfillMarker`. Add a new exported (or unexported, see Step 3) name
that the tests will use. For now, change every occurrence of
`IsAutomatedBackfillMarker` in this file to `classifierHashStatsKey`. There are
5 occurrences (one per test except `TestBackfillIsAutomatedBumpsLocalModifiedAt`
has one too — verify with grep below).

```bash
grep -n IsAutomatedBackfillMarker internal/db/automated_backfill_test.go
```

Expected: 5 matches. Edit each to `classifierHashStatsKey`.

- [ ] **Step 2: Add a new test that exercises hash-driven re-runs**

Append to `internal/db/automated_backfill_test.go`:

```go
// TestBackfillIsAutomatedRerunsOnHashChange verifies that a
// classifier change (here, adding a user prefix) invalidates
// the stored hash and re-runs the backfill on next open,
// without any manual marker bump.
func TestBackfillIsAutomatedRerunsOnHashChange(t *testing.T) {
	t.Cleanup(func() { SetUserAutomationPrefixes(nil) })
	d := testDB(t)

	// Seed a session whose first_message would match a
	// user prefix once added. With the empty user-prefix
	// list it should be is_automated=0.
	insertSession(t, d, "essay", "proj", func(s *Session) {
		fm := "You are analyzing an essay about epistemology."
		s.FirstMessage = &fm
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	ctx := context.Background()
	pre, err := d.GetSession(ctx, "essay")
	requireNoError(t, err, "get essay before")
	if pre.IsAutomated {
		t.Fatalf("precondition: essay should be is_automated=0")
	}

	// Add a user prefix and re-run backfill. The new hash
	// should not equal the stored hash, so the backfill
	// runs and flips is_automated to 1.
	SetUserAutomationPrefixes([]string{"You are analyzing an essay"})
	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(d.getWriter())
	d.mu.Unlock()
	requireNoError(t, err, "backfill after prefix add")

	got, err := d.GetSession(ctx, "essay")
	requireNoError(t, err, "get essay after")
	if !got.IsAutomated {
		t.Error("essay should be is_automated=1 after user prefix added")
	}

	// A second backfill (no further classifier change) is a
	// no-op: stored hash now matches.
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 0 WHERE id = 'essay'",
	)
	requireNoError(t, err, "force back to 0")
	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(d.getWriter())
	d.mu.Unlock()
	requireNoError(t, err, "second backfill")
	got, err = d.GetSession(ctx, "essay")
	requireNoError(t, err, "get essay second")
	if got.IsAutomated {
		t.Error("second backfill must be a no-op when hash unchanged")
	}
}
```

- [ ] **Step 3: Run tests to verify failures**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run TestBackfillIsAutomated -v
```

Expected: FAIL — `classifierHashStatsKey` undefined and the new test references
missing behavior.

- [ ] **Step 4: Remove the old constant**

In `internal/db/automated.go`, delete the `IsAutomatedBackfillMarker`
declaration:

```go
// IsAutomatedBackfillMarker is the stats/sync_metadata key that
// gates the one-time is_automated re-classification. Bump the
// suffix whenever the classifier patterns change so existing
// databases re-run the backfill on next open.
const IsAutomatedBackfillMarker = "is_automated_backfill_v3"
```

- [ ] **Step 5: Replace the backfill gate with a hash check**

In `internal/db/db.go`, near the other stats-key constants (just below
`tokenCoverageRepairStatsKey` on line 38), add:

```go
const classifierHashStatsKey = "is_automated_classifier_hash"
```

Replace the body of `backfillIsAutomatedLocked` so the gate compares hashes and
the success path writes the new hash:

```go
func (db *DB) backfillIsAutomatedLocked(w *sql.DB) error {
	current := ClassifierHash()
	var stored string
	err := w.QueryRow(
		`SELECT value FROM stats WHERE key = ?`,
		classifierHashStatsKey,
	).Scan(&stored)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf(
			"probing classifier hash: %w", err,
		)
	}
	if err == nil && stored == current {
		return nil
	}

	rows, err := w.Query(
		`SELECT id, first_message, user_message_count,
			is_automated
		 FROM sessions
		 WHERE first_message IS NOT NULL`,
	)
	if err != nil {
		return fmt.Errorf(
			"querying automated backfill candidates: %w", err,
		)
	}
	defer rows.Close()

	var setIDs, clearIDs []string
	for rows.Next() {
		var id, fm string
		var umc int
		var rowAutomated bool
		if err := rows.Scan(
			&id, &fm, &umc, &rowAutomated,
		); err != nil {
			return fmt.Errorf(
				"scanning backfill candidate: %w", err,
			)
		}
		want := umc <= 1 && IsAutomatedSession(fm)
		if want && !rowAutomated {
			setIDs = append(setIDs, id)
		} else if !want && rowAutomated {
			clearIDs = append(clearIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if err := batchUpdateAutomated(
		w, setIDs, 1,
	); err != nil {
		return err
	}
	if err := batchUpdateAutomated(
		w, clearIDs, 0,
	); err != nil {
		return err
	}

	if len(setIDs) > 0 || len(clearIDs) > 0 {
		log.Printf(
			"migration: recomputed is_automated"+
				" (set %d, cleared %d)",
			len(setIDs), len(clearIDs),
		)
	}

	if _, err := w.Exec(
		`INSERT INTO stats (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		classifierHashStatsKey, current,
	); err != nil {
		return fmt.Errorf(
			"storing classifier hash: %w", err,
		)
	}
	return nil
}
```

Note the renamed local variable `rowAutomated` (was `current`) — `current` is
now the hash string at the top of the function.

- [ ] **Step 6: Run all db tests to verify pass**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -v
```

Expected: PASS — including the four pre-existing backfill tests and the new
hash-change test.

- [ ] **Step 7: Run formatter and vet**

```bash
go fmt ./internal/db/...
go vet ./internal/db/...
```

Expected: no diff, no warnings.

- [ ] **Step 8: Commit**

```bash
git add internal/db/automated.go internal/db/automated_backfill_test.go internal/db/db.go
git commit -m "feat(db): hash-driven is_automated backfill (replaces marker)"
```

______________________________________________________________________

## Task 5: PostgreSQL backfill driver + push.go change

**Files:**

- Modify: `internal/postgres/schema.go` — replace
  `isAutomatedBackfillMetadataKey = "is_automated_backfill_v3"` with
  `classifierHashMetadataKey = "is_automated_classifier_hash"`; rewrite
  `backfillIsAutomatedPG` body to compare/store hashes.
- Modify: `internal/postgres/push.go` — `pushSession` uses `sess.IsAutomated`
  directly instead of recomputing.
- Create: `internal/postgres/automated_pgtest_test.go` — backfill parity test +
  push trust test (under `pgtest` build tag).

**Context:** Both stores see the same `db.ClassifierHash()` because they run
inside the same agentsview process. PG-side recomputation in `pushSession` is
removed: the SQLite row already holds the correct value (set by `UpsertSession`
and `UpdateSessionIncremental`), and PG's own backfill (`backfillIsAutomatedPG`)
handles the rare "DB rehosted from a different machine" case.

- [ ] **Step 1: Write the failing pgtest tests**

Create `internal/postgres/automated_pgtest_test.go`:

```go
//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/wesm/agentsview/internal/db"
)

// TestPushSessionTrustsLocalIsAutomated verifies that
// pushSession copies sess.IsAutomated verbatim instead of
// re-running db.IsAutomatedSession on the first_message.
// Achieved by setting a user prefix locally, upserting a
// matching session (so IsAutomated=1), then clearing the
// user prefix BEFORE push and confirming the PG row stays
// IsAutomated=1.
func TestPushSessionTrustsLocalIsAutomated(t *testing.T) {
	t.Cleanup(func() { db.SetUserAutomationPrefixes(nil) })
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)

	// Set a user prefix BEFORE inserting so UpsertSession
	// sets is_automated=1 on the SQLite row.
	db.SetUserAutomationPrefixes([]string{"You are analyzing an essay"})
	fm := "You are analyzing an essay about epistemology."
	if err := local.UpsertSession(db.Session{
		ID:               "essay-1",
		Project:          "proj",
		Machine:          "local",
		Agent:            "claude",
		FirstMessage:     &fm,
		MessageCount:     2,
		UserMessageCount: 1,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Clear the user prefix so a recompute in pushSession
	// would now classify this row as is_automated=0. If
	// pushSession trusts the local value, PG sees =1 anyway.
	db.SetUserAutomationPrefixes(nil)

	ps, err := New(
		pgURL, "agentsview", local,
		"trust-test-machine", true,
		SyncOptions{},
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx, cancel := context.WithTimeout(
		context.Background(), 30*time.Second,
	)
	defer cancel()

	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	if _, err := ps.Push(ctx, false, nil); err != nil {
		t.Fatalf("push: %v", err)
	}

	var got bool
	if err := ps.DB().QueryRowContext(ctx,
		`SELECT is_automated FROM sessions WHERE id = $1`,
		"essay-1",
	).Scan(&got); err != nil {
		t.Fatalf("query pg: %v", err)
	}
	if !got {
		t.Error("pushSession recomputed is_automated; expected to trust local value")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails for the right reason**

```bash
make test-postgres
```

Or, if a PG container is already running:

```bash
TEST_PG_URL="postgres://agentsview:agentsview@localhost:5432/agentsview?sslmode=disable" \
  CGO_ENABLED=1 go test -tags "fts5,pgtest" ./internal/postgres/ -run TestPushSessionTrustsLocalIsAutomated -v
```

Expected: FAIL — `pushSession` currently recomputes via
`db.IsAutomatedSession(*sess.FirstMessage)`, so PG ends up with
`is_automated=false`. The test asserts `true`.

- [ ] **Step 3: Update `pushSession` to trust `sess.IsAutomated`**

In `internal/postgres/push.go`, replace lines 720-723 (the `isAutomated := ...`
computation):

```go
	createdAt, _ := ParseSQLiteTimestamp(sess.CreatedAt)
	isAutomated := sess.UserMessageCount <= 1 &&
		sess.FirstMessage != nil &&
		db.IsAutomatedSession(*sess.FirstMessage)
```

with:

```go
	createdAt, _ := ParseSQLiteTimestamp(sess.CreatedAt)
	isAutomated := sess.IsAutomated
```

- [ ] **Step 4: Run the trust test to verify it passes**

```bash
TEST_PG_URL="..." \
  CGO_ENABLED=1 go test -tags "fts5,pgtest" ./internal/postgres/ -run TestPushSessionTrustsLocalIsAutomated -v
```

Expected: PASS.

- [ ] **Step 5: Replace the PG backfill marker with a hash check**

In `internal/postgres/schema.go`, replace the constant declaration on line 561:

```go
const isAutomatedBackfillMetadataKey = "is_automated_backfill_v3"
```

with:

```go
const classifierHashMetadataKey = "is_automated_classifier_hash"
```

Replace the body of `backfillIsAutomatedPG` (lines 568-648) so the gate compares
hashes and the success path writes the new hash:

```go
func backfillIsAutomatedPG(
	ctx context.Context, pg *sql.DB,
) error {
	current := db.ClassifierHash()
	var stored string
	err := pg.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = $1`,
		classifierHashMetadataKey,
	).Scan(&stored)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf(
			"probing PG classifier hash: %w", err,
		)
	}
	if err == nil && stored == current {
		return nil
	}

	rows, err := pg.QueryContext(ctx,
		`SELECT id, first_message, user_message_count,
			is_automated
		 FROM sessions
		 WHERE first_message IS NOT NULL`)
	if err != nil {
		return fmt.Errorf(
			"querying PG automated backfill candidates: %w",
			err,
		)
	}
	defer rows.Close()

	var setIDs, clearIDs []string
	for rows.Next() {
		var id, fm string
		var umc int
		var rowAutomated bool
		if err := rows.Scan(
			&id, &fm, &umc, &rowAutomated,
		); err != nil {
			return fmt.Errorf(
				"scanning PG backfill candidate: %w", err,
			)
		}
		want := umc <= 1 && db.IsAutomatedSession(fm)
		if want && !rowAutomated {
			setIDs = append(setIDs, id)
		} else if !want && rowAutomated {
			clearIDs = append(clearIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if err := batchUpdateAutomatedPG(
		ctx, pg, setIDs, true,
	); err != nil {
		return err
	}
	if err := batchUpdateAutomatedPG(
		ctx, pg, clearIDs, false,
	); err != nil {
		return err
	}

	if len(setIDs) > 0 || len(clearIDs) > 0 {
		log.Printf(
			"pg migration: recomputed is_automated"+
				" (set %d, cleared %d)",
			len(setIDs), len(clearIDs),
		)
	}

	if _, err := pg.ExecContext(ctx,
		`INSERT INTO sync_metadata (key, value)
		 VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE
		 SET value = EXCLUDED.value`,
		classifierHashMetadataKey, current,
	); err != nil {
		return fmt.Errorf(
			"storing PG classifier hash: %w", err,
		)
	}
	return nil
}
```

- [ ] **Step 6: Add a PG backfill parity test**

Append to `internal/postgres/automated_pgtest_test.go`:

```go
// TestBackfillIsAutomatedPGRerunsOnHashChange exercises the
// PG-side hash-driven backfill: after a classifier change
// (here, adding a user prefix), EnsureSchema re-runs the
// backfill and flips matching rows to is_automated=true.
func TestBackfillIsAutomatedPGRerunsOnHashChange(t *testing.T) {
	t.Cleanup(func() { db.SetUserAutomationPrefixes(nil) })
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	fm := "You are analyzing an essay about epistemology."
	if err := local.UpsertSession(db.Session{
		ID:               "essay-pg",
		Project:          "proj",
		Machine:          "local",
		Agent:            "claude",
		FirstMessage:     &fm,
		MessageCount:     2,
		UserMessageCount: 1,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	ps, err := New(
		pgURL, "agentsview", local,
		"backfill-test-machine", true,
		SyncOptions{},
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx, cancel := context.WithTimeout(
		context.Background(), 30*time.Second,
	)
	defer cancel()

	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	if _, err := ps.Push(ctx, false, nil); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Confirm the PG row starts as is_automated=false.
	var pre bool
	if err := ps.DB().QueryRowContext(ctx,
		`SELECT is_automated FROM sessions WHERE id = $1`,
		"essay-pg",
	).Scan(&pre); err != nil {
		t.Fatalf("query pre: %v", err)
	}
	if pre {
		t.Fatalf("precondition: PG row should be is_automated=false")
	}

	// Add a user prefix so the classifier hash changes,
	// then re-run the PG backfill directly (bypassing
	// Sync.EnsureSchema's memo so the second pass actually
	// executes). The matching row should flip to true.
	db.SetUserAutomationPrefixes([]string{"You are analyzing an essay"})
	if err := backfillIsAutomatedPG(ctx, ps.DB()); err != nil {
		t.Fatalf("backfill after prefix add: %v", err)
	}

	var got bool
	if err := ps.DB().QueryRowContext(ctx,
		`SELECT is_automated FROM sessions WHERE id = $1`,
		"essay-pg",
	).Scan(&got); err != nil {
		t.Fatalf("query post: %v", err)
	}
	if !got {
		t.Error("PG row should be is_automated=true after backfill on hash change")
	}
}
```

- [ ] **Step 7: Run all PG tests to verify pass**

```bash
TEST_PG_URL="..." \
  CGO_ENABLED=1 go test -tags "fts5,pgtest" ./internal/postgres/ -v
```

Expected: PASS — both new tests, plus the existing PG suite (the marker rename
doesn't affect existing tests because they don't reference
`isAutomatedBackfillMetadataKey` by name).

- [ ] **Step 8: Verify non-PG tests still pass**

```bash
make test-short
```

Expected: PASS.

- [ ] **Step 9: Run formatter and vet**

```bash
go fmt ./internal/postgres/...
go vet ./internal/postgres/...
```

Expected: no diff, no warnings.

- [ ] **Step 10: Commit**

```bash
git add internal/postgres/schema.go internal/postgres/push.go internal/postgres/automated_pgtest_test.go
git commit -m "feat(postgres): hash-driven is_automated backfill, trust local in push"
```

______________________________________________________________________

## Task 6: Wiring — central helper + entry-point plumbing

**Files:**

- Create: `cmd/agentsview/classifier_wiring.go` — `applyClassifierConfig`
  helper.
- Modify: 12 entry-point files to call the helper after `config.Load*` and
  before `db.Open` / `postgres.*`.

**Context:** User prefixes must reach the singleton in every process that opens
SQLite or PostgreSQL. The static guardrail test in Task 7 will catch
regressions, but for this task we manually plumb the helper through every
existing entry point. The helper is silent — `serve` and `classifier rebuild`
log the count themselves; quiet paths (statusline, JSON output) stay quiet.

- [ ] **Step 1: Add the helper**

Create `cmd/agentsview/classifier_wiring.go`:

```go
// ABOUTME: applyClassifierConfig installs user-defined
// ABOUTME: classifier prefixes into the db package singleton.
package main

import (
	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
)

// applyClassifierConfig installs user-defined classifier
// prefixes into the db package singleton. Every command that
// loads config and may open SQLite or PostgreSQL must call
// this BEFORE db.Open / postgres.Open / postgres.NewStore /
// postgres.New / postgres.EnsureSchema. Silent by design so
// it's safe to call from quiet CLI paths (statusline, JSON
// output, etc.); see db.SetUserAutomationPrefixes for
// rationale. The static guardrail test in
// classifier_wiring_test.go (Task 7) enforces this rule.
func applyClassifierConfig(cfg config.Config) {
	db.SetUserAutomationPrefixes(cfg.Automated.Prefixes)
}
```

- [ ] **Step 2: Wire `runServe`**

In `cmd/agentsview/main.go`, in `runServe`, insert the call right before
`mustOpenDB` (line 80) so the singleton is loaded before SQLite opens:

```go
	applyClassifierConfig(cfg)
	database := mustOpenDB(cfg)
```

Then add a startup log line for visibility (the design says `serve` startup logs
the count — this is the only place that does). Insert just after
`database := mustOpenDB(cfg)`:

```go
	if n := len(db.UserAutomationPrefixes()); n > 0 {
		log.Printf("loaded %d user automation prefix(es) from config", n)
	}
```

- [ ] **Step 3: Wire `runSync`**

In `cmd/agentsview/sync.go`, in `runSync`, insert just before `db.Open` (line
39):

```go
	applyClassifierConfig(appCfg)
	database, err := db.Open(appCfg.DBPath)
```

- [ ] **Step 4: Wire `runImport`**

In `cmd/agentsview/import.go`, in `runImport`, insert just before `db.Open`
(line 27):

```go
	applyClassifierConfig(appCfg)
	database, err := db.Open(appCfg.DBPath)
```

- [ ] **Step 5: Wire `runHealth`**

In `cmd/agentsview/health.go`, in `runHealth`, insert just before `db.Open`
(line 33):

```go
	applyClassifierConfig(appCfg)
	database, err := db.Open(appCfg.DBPath)
```

- [ ] **Step 6: Wire `runPrune`**

In `cmd/agentsview/prune.go`, in `runPrune`, insert just before `db.Open` (line
249):

```go
	applyClassifierConfig(appCfg)
	database, err := db.Open(appCfg.DBPath)
```

- [ ] **Step 7: Wire `runProjects`**

In `cmd/agentsview/projects.go`, in `runProjects`, insert just before `db.Open`
(line 20):

```go
	applyClassifierConfig(appCfg)
	database, err := db.Open(appCfg.DBPath)
```

- [ ] **Step 8: Wire `openStatsService`**

In `cmd/agentsview/stats.go`, in `openStatsService`, insert just before
`db.Open` (line 140):

```go
	applyClassifierConfig(cfg)
	d, err := db.Open(cfg.DBPath)
```

- [ ] **Step 9: Wire `openUsageDB`**

In `cmd/agentsview/usage.go`, in `openUsageDB`, insert just before `db.Open`
(line 154):

```go
	applyClassifierConfig(cfg)
	database, err := db.Open(cfg.DBPath)
```

- [ ] **Step 10: Wire `tokenUse`**

In `cmd/agentsview/token_use.go`, in `tokenUse`, insert just before `db.Open`
(line 231):

```go
	applyClassifierConfig(appCfg)
	database, err := db.Open(appCfg.DBPath)
```

- [ ] **Step 11: Wire `runPGPush`**

In `cmd/agentsview/pg.go`, in `runPGPush`, insert just before `db.Open` (line
81):

```go
	applyClassifierConfig(appCfg)
	database, err := db.Open(appCfg.DBPath)
```

(The same call covers both `db.Open` and the later `postgres.New` because
they're in the same function body.)

- [ ] **Step 12: Wire `runPGServe`**

In `cmd/agentsview/pg.go`, in `runPGServe`, insert just before
`postgres.NewStore` (line 236):

```go
	applyClassifierConfig(appCfg)
	store, err := postgres.NewStore(
```

- [ ] **Step 13: Wire `newService` (transport.go)**

In `cmd/agentsview/transport.go`, in `newService`, insert in the `default:`
(direct mode) branch just before `db.Open` (line 112):

```go
	default:
		applyClassifierConfig(cfg)
		d, err := db.Open(cfg.DBPath)
```

- [ ] **Step 14: Wire `syncService` (session_sync.go)**

In `cmd/agentsview/session_sync.go`, in `syncService`, insert in the direct
branch just before `db.Open` (line 92):

```go
	applyClassifierConfig(cfg)
	d, err := db.Open(cfg.DBPath)
```

- [ ] **Step 15: Build and run all CLI tests**

```bash
make build
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview/ -v
```

Expected: build succeeds, tests pass. If a test relies on `is_automated` being
recomputed inside `pushSession`, surface the failure and fix by calling
`applyClassifierConfig` earlier in the test setup.

- [ ] **Step 16: Smoke test the binary**

```bash
./agentsview --help
./agentsview version
```

Expected: both commands run without error and without printing
classifier-related warnings.

- [ ] **Step 17: Run formatter and vet**

```bash
go fmt ./cmd/agentsview/...
go vet ./cmd/agentsview/...
```

Expected: no diff, no warnings.

- [ ] **Step 18: Commit**

```bash
git add cmd/agentsview/classifier_wiring.go \
	cmd/agentsview/main.go cmd/agentsview/sync.go \
	cmd/agentsview/import.go cmd/agentsview/health.go \
	cmd/agentsview/prune.go cmd/agentsview/projects.go \
	cmd/agentsview/stats.go cmd/agentsview/usage.go \
	cmd/agentsview/token_use.go cmd/agentsview/pg.go \
	cmd/agentsview/transport.go cmd/agentsview/session_sync.go
git commit -m "feat(cli): wire user automation prefixes into every store-open path"
```

______________________________________________________________________

## Task 7: Static guardrail test

**Files:**

- Create: `cmd/agentsview/classifier_wiring_test.go` — AST scan that fails the
  build if a function or `*ast.FuncLit` in `cmd/agentsview/` calls a trigger
  function (`db.Open`, `postgres.Open`, `postgres.NewStore`, `postgres.New`,
  `postgres.EnsureSchema`) without an earlier `applyClassifierConfig` call in
  the same enclosing body.

**Context:** This test runs in unit-test time, doesn't execute any command, and
locks in the wiring done in Task 6. It must recurse into `*ast.FuncLit` so cobra
`RunE: func(...) error { ... }` closures are checked the same as named
functions. A trigger inside a `RunE` closure does NOT satisfy the guard via a
helper call in the surrounding builder function.

- [ ] **Step 1: Sanity-check what the test should detect**

Verify (without running anything yet) that every entry point wired in Task 6 has
the `applyClassifierConfig(...)` call lexically *before* the trigger call in the
same function body. Quick grep:

```bash
grep -nE "applyClassifierConfig|db\.Open|postgres\.(Open|NewStore|New|EnsureSchema)" \
  cmd/agentsview/*.go | grep -v _test.go
```

Expected: each function body that uses a trigger has an earlier
`applyClassifierConfig(...)` line. If any entry point is missing the call, fix
Task 6 before continuing.

- [ ] **Step 2: Write the static guardrail test**

Create `cmd/agentsview/classifier_wiring_test.go`:

```go
// ABOUTME: static AST scan that prevents new commands from
// ABOUTME: opening a store without first wiring the
// ABOUTME: user-prefix classifier singleton.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// triggerCalls names the qualified function calls that read
// the classifier singleton (directly or indirectly via
// backfill). Every function or function literal in
// cmd/agentsview/ that contains one of these calls must
// contain an EARLIER call to applyClassifierConfig in the
// same enclosing body.
var triggerCalls = map[string]struct{}{
	"db.Open":               {},
	"postgres.Open":         {},
	"postgres.NewStore":     {},
	"postgres.New":          {},
	"postgres.EnsureSchema": {},
}

const wiringHelper = "applyClassifierConfig"

// TestEveryStoreOpenPathIsWired enforces the rule documented
// in the design spec: every code path in cmd/agentsview that
// opens or initializes a store must first call
// applyClassifierConfig so user-defined prefixes reach the
// db package singleton.
func TestEveryStoreOpenPathIsWired(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("listing cmd/agentsview: %v", err)
	}

	fset := token.NewFileSet()
	var violations []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(
			fset, filepath.Join(".", name), nil,
			parser.ParseComments,
		)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		violations = append(
			violations, scanFile(fset, f)...,
		)
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf(
			"functions or closures missing %s before "+
				"opening a store:\n  %s",
			wiringHelper,
			strings.Join(violations, "\n  "),
		)
	}
}

// scanFile walks every function declaration and function
// literal in f, returning a violation string for each body
// that contains a trigger call without an earlier
// applyClassifierConfig call.
func scanFile(
	fset *token.FileSet, f *ast.File,
) []string {
	var violations []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch fn := n.(type) {
		case *ast.FuncDecl:
			if fn.Body == nil {
				return true
			}
			if v := checkBody(
				fset, fn.Body, funcLabel(fset, fn),
			); v != "" {
				violations = append(violations, v)
			}
		case *ast.FuncLit:
			if v := checkBody(
				fset, fn.Body, litLabel(fset, fn),
			); v != "" {
				violations = append(violations, v)
			}
		}
		return true
	})
	return violations
}

// checkBody walks body's statements in source order. If a
// trigger call appears before the helper call (or the helper
// call never appears), it returns a violation string. Helper
// and trigger searches descend into nested expressions but
// stop at nested function literals — those have their own
// scope and are checked separately by ast.Inspect.
func checkBody(
	fset *token.FileSet,
	body *ast.BlockStmt,
	label string,
) string {
	var (
		seenHelper  bool
		earlyTrig   string
		earlyTrigAt token.Pos
	)
	ast.Inspect(body, func(n ast.Node) bool {
		// Don't descend into nested func literals — they
		// carry their own scope and are visited by the
		// outer ast.Inspect in scanFile.
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == wiringHelper {
				seenHelper = true
			}
		case *ast.SelectorExpr:
			pkg, ok := fn.X.(*ast.Ident)
			if !ok {
				return true
			}
			qname := pkg.Name + "." + fn.Sel.Name
			if _, isTrigger := triggerCalls[qname]; isTrigger {
				if !seenHelper && earlyTrig == "" {
					earlyTrig = qname
					earlyTrigAt = call.Pos()
				}
			}
		}
		return true
	})
	if earlyTrig == "" {
		return ""
	}
	pos := fset.Position(earlyTrigAt)
	return label + ": calls " + earlyTrig +
		" at " + pos.Filename + ":" +
		itoa(pos.Line) + " without earlier " +
		wiringHelper
}

func funcLabel(fset *token.FileSet, fn *ast.FuncDecl) string {
	pos := fset.Position(fn.Pos())
	return fn.Name.Name + " (" + pos.Filename + ":" +
		itoa(pos.Line) + ")"
}

func litLabel(fset *token.FileSet, fn *ast.FuncLit) string {
	pos := fset.Position(fn.Pos())
	return "anonymous func at " + pos.Filename + ":" +
		itoa(pos.Line)
}

// itoa avoids importing strconv just for line numbers.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
```

- [ ] **Step 3: Run the test to verify it passes**

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview/ -run TestEveryStoreOpenPathIsWired -v
```

Expected: PASS — every wired entry point from Task 6 has the helper call before
the trigger.

- [ ] **Step 4: Negative-test the test**

Temporarily remove the `applyClassifierConfig(appCfg)` line from `runProjects`
in `cmd/agentsview/projects.go` and re-run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview/ -run TestEveryStoreOpenPathIsWired -v
```

Expected: FAIL with a clear message naming `runProjects` and `db.Open`.

Restore the helper call. Re-run to confirm PASS again.

- [ ] **Step 5: Run formatter and vet**

```bash
go fmt ./cmd/agentsview/...
go vet ./cmd/agentsview/...
```

Expected: no diff, no warnings.

- [ ] **Step 6: Commit**

```bash
git add cmd/agentsview/classifier_wiring_test.go
git commit -m "test(cli): static guardrail for classifier-singleton wiring"
```

______________________________________________________________________

## Task 8: `agentsview classifier rebuild` CLI command

**Files:**

- Create: `cmd/agentsview/classifier.go` — `classifier` group root + `rebuild`
  subcommand.
- Create: `cmd/agentsview/classifier_test.go` — rebuild behavior tests.
- Modify: `cmd/agentsview/cli.go` — register the `classifier` group on the root
  command.

**Context:** Forces the next backfill on next open by deleting the stored hash
from SQLite `stats` and (if PG is configured) PG `sync_metadata`. Refuses to run
when a daemon owns the SQLite write lock (any of `transportHTTP` or
`transportDirect && DirectReadOnly`). PG delete failure is a hard error when PG
is configured. Prints the post-normalization user-prefix list so users can diff
against `[automated] prefixes`.

- [ ] **Step 1: Write the failing tests**

Create `cmd/agentsview/classifier_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
)

// classifierTestEnv prepares a temp data dir and writes a
// minimal config.toml with the given user prefixes.
func classifierTestEnv(t *testing.T, prefixes []string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_VIEWER_DATA_DIR", dir)

	tomlBuf := &bytes.Buffer{}
	tomlBuf.WriteString("[automated]\nprefixes = [")
	for i, p := range prefixes {
		if i > 0 {
			tomlBuf.WriteString(", ")
		}
		tomlBuf.WriteString("\"" + p + "\"")
	}
	tomlBuf.WriteString("]\n")
	if err := os.WriteFile(
		filepath.Join(dir, "config.toml"),
		tomlBuf.Bytes(), 0o600,
	); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Cleanup(func() { db.SetUserAutomationPrefixes(nil) })
	return dir
}

// seedHash opens the DB at cfg.DBPath, runs the backfill so
// a hash gets stored, then closes.
func seedHash(t *testing.T, cfg config.Config) {
	t.Helper()
	d, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	// Opening already runs backfill; the hash is now stored.
	_ = d
}

// readStoredHash returns the stored classifier hash from the
// stats table, or "" if none.
func readStoredHash(t *testing.T, dbPath string) string {
	t.Helper()
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	return d.GetStat("is_automated_classifier_hash")
}

func TestClassifierRebuildClearsSQLiteHash(t *testing.T) {
	dir := classifierTestEnv(t, []string{"You are analyzing an essay"})
	cfg, err := config.LoadMinimal()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.DBPath = filepath.Join(dir, "sessions.db")
	applyClassifierConfig(cfg)
	seedHash(t, cfg)
	if got := readStoredHash(t, cfg.DBPath); got == "" {
		t.Fatalf("precondition: expected stored hash, got empty")
	}

	if err := runClassifierRebuild(
		context.Background(), cfg, &bytes.Buffer{},
	); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	if got := readStoredHash(t, cfg.DBPath); got != "" {
		t.Errorf("expected hash cleared, got %q", got)
	}
}

func TestClassifierRebuildPrintsLoadedPrefixes(t *testing.T) {
	prefixes := []string{
		"You are analyzing an essay",
		"You are grading quotes",
	}
	dir := classifierTestEnv(t, prefixes)
	cfg, err := config.LoadMinimal()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.DBPath = filepath.Join(dir, "sessions.db")
	applyClassifierConfig(cfg)
	seedHash(t, cfg)

	out := &bytes.Buffer{}
	if err := runClassifierRebuild(
		context.Background(), cfg, out,
	); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	got := out.String()
	for _, p := range prefixes {
		if !strings.Contains(got, p) {
			t.Errorf("output missing %q:\n%s", p, got)
		}
	}
	if !strings.Contains(got, "loaded 2 user automation prefix") {
		t.Errorf("output missing count line:\n%s", got)
	}
	if !strings.Contains(got, "restart") {
		t.Errorf("output missing restart reminder:\n%s", got)
	}
}

func TestClassifierRebuildRefusesOnHTTPTransport(t *testing.T) {
	dir := classifierTestEnv(t, nil)
	cfg, err := config.LoadMinimal()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.DBPath = filepath.Join(dir, "sessions.db")

	tr := transport{Mode: transportHTTP, URL: "http://127.0.0.1:8080"}
	err = guardClassifierRebuild(tr)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "daemon") {
		t.Errorf("error should mention daemon, got: %v", err)
	}
}

func TestClassifierRebuildRefusesOnDirectReadOnly(t *testing.T) {
	tr := transport{Mode: transportDirect, DirectReadOnly: true}
	err := guardClassifierRebuild(tr)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "daemon") {
		t.Errorf("error should mention daemon, got: %v", err)
	}
}

func TestClassifierRebuildAllowsDirectWritable(t *testing.T) {
	tr := transport{Mode: transportDirect, DirectReadOnly: false}
	if err := guardClassifierRebuild(tr); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
```

This test file references three not-yet-implemented functions:

- `runClassifierRebuild(ctx, cfg, out)` — the real rebuild driver, exported in
  test scope as a package function.

- `guardClassifierRebuild(tr)` — pure transport-mode check, factored out so it's
  testable without spinning up an HTTP listener.

- `db.GetStat(key)` — a tiny accessor on `*db.DB` that reads `stats.value`. If
  this doesn't exist yet, add it in Step 2.

- [ ] **Step 2: Add `GetStat` accessor on `*db.DB`**

Check whether `internal/db/db.go` already has a stats accessor:

```bash
grep -nE "GetStat|stats\.value" internal/db/db.go internal/db/*.go
```

If a `GetStat` accessor does not exist, add this near the other stats helpers in
`internal/db/db.go`:

```go
// GetStat returns the value stored at key in the stats
// table, or "" if absent. Used by tests and by the
// classifier rebuild command.
func (db *DB) GetStat(key string) string {
	w := db.getReader()
	var v string
	if err := w.QueryRow(
		`SELECT value FROM stats WHERE key = ?`, key,
	).Scan(&v); err != nil {
		return ""
	}
	return v
}
```

(If the codebase already has `getReader` / `getWriter` accessors — which it does
per the existing tests — use whichever is appropriate. `getReader` is correct
for a SELECT.)

- [ ] **Step 3: Run the tests to verify they fail**

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview/ -run TestClassifierRebuild -v
```

Expected: FAIL — `runClassifierRebuild` and `guardClassifierRebuild` undefined.

- [ ] **Step 4: Implement the command**

Create `cmd/agentsview/classifier.go`:

```go
// ABOUTME: `agentsview classifier rebuild` — clears the
// ABOUTME: stored classifier hash so the next db.Open runs a
// ABOUTME: full backfill. Recovery path for stale flags.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"

	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"
	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/postgres"
)

const classifierHashKey = "is_automated_classifier_hash"

func newClassifierCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "classifier",
		Short:        "Manage the automated-session classifier",
		GroupID:      groupMeta,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newClassifierRebuildCommand())
	return cmd
}

func newClassifierRebuildCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "rebuild",
		Short: "Force is_automated re-backfill on next open",
		Long: "Clears the stored classifier hash so the next " +
			"db.Open runs a full is_automated backfill. " +
			"Use after editing [automated] prefixes in " +
			"config.toml or after a downgrade-then-upgrade " +
			"cycle that left flags stale.",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadPFlags(cmd.Flags())
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			applyClassifierConfig(cfg)
			tr, err := detectTransport(cfg.DataDir, 0)
			if err != nil {
				return err
			}
			if err := guardClassifierRebuild(tr); err != nil {
				return err
			}
			return runClassifierRebuild(
				cmd.Context(), cfg, cmd.OutOrStdout(),
			)
		},
	}
}

// guardClassifierRebuild rejects when the SQLite write lock
// is owned by a daemon we don't control. Pure function for
// testability.
func guardClassifierRebuild(tr transport) error {
	if tr.Mode == transportHTTP {
		return errors.New(
			"local daemon is serving on " + tr.URL +
				"; stop 'agentsview serve' (or 'pg serve') " +
				"before running 'classifier rebuild'",
		)
	}
	if tr.Mode == transportDirect && tr.DirectReadOnly {
		return errors.New(
			"local daemon is active but not responding; " +
				"refusing to rebuild to avoid competing for " +
				"write ownership. Stop the daemon first.",
		)
	}
	return nil
}

// runClassifierRebuild prints the loaded user-prefix list,
// deletes the classifier hash from SQLite stats, and (if PG
// is configured) deletes it from PG sync_metadata. Returns
// an error on PG delete failure when PG is configured.
func runClassifierRebuild(
	ctx context.Context, cfg config.Config, out io.Writer,
) error {
	prefixes := db.UserAutomationPrefixes()
	fmt.Fprintf(out,
		"loaded %d user automation prefix(es) from config:\n",
		len(prefixes),
	)
	for _, p := range prefixes {
		fmt.Fprintf(out, "  - %s\n", p)
	}

	if err := clearSQLiteClassifierHash(cfg.DBPath); err != nil {
		return fmt.Errorf("clearing SQLite hash: %w", err)
	}

	pgCfg, err := cfg.ResolvePG()
	if err != nil {
		return fmt.Errorf("resolving pg config: %w", err)
	}
	if pgCfg.URL != "" {
		if err := clearPGClassifierHash(ctx, pgCfg); err != nil {
			return fmt.Errorf("clearing PG hash: %w", err)
		}
	}

	fmt.Fprintln(out,
		"classifier hash cleared. Next db.Open will run "+
			"the is_automated backfill.")
	fmt.Fprintln(out,
		"restart any running 'agentsview serve' so write "+
			"paths use the updated prefixes")
	return nil
}

func clearSQLiteClassifierHash(dbPath string) error {
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		// Nothing to clear; first open will write the hash.
		return nil
	}
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Exec(
		`DELETE FROM stats WHERE key = ?`,
		classifierHashKey,
	)
	return err
}

func clearPGClassifierHash(
	ctx context.Context, pgCfg config.PGConfig,
) error {
	pg, err := postgres.Open(
		pgCfg.URL, pgCfg.Schema, pgCfg.AllowInsecure,
	)
	if err != nil {
		return err
	}
	defer pg.Close()
	_, err = pg.ExecContext(ctx,
		`DELETE FROM sync_metadata WHERE key = $1`,
		classifierHashKey,
	)
	return err
}
```

Note: this command opens its own raw `*sql.DB` connections rather than going
through `db.Open` so it doesn't trigger the just-cleared backfill on the same
call. The next "real" `db.Open` (e.g. when the user starts the daemon again)
does the backfill.

The `postgres.Open` signature is `(dsn, schema string, allowInsecure bool)` —
see `internal/postgres/connect.go:144`.

- [ ] **Step 5: Register the classifier group on the root command**

In `cmd/agentsview/cli.go`, in `newRootCommand`, add the classifier subcommand
registration alongside the others (anywhere in the `root.AddCommand(...)` block,
e.g. just before `root.AddCommand(newVersionCommand())`):

```go
	root.AddCommand(newClassifierCommand())
```

- [ ] **Step 6: Add a PG-error hard-failure test**

The spec testing table requires this command to treat PG delete failure as a
hard error when PG is configured (covering both reachable-but-error and
unreachable cases). Append to `cmd/agentsview/classifier_test.go`:

```go
// TestClassifierRebuildHardFailsOnPGUnreachable confirms
// that when PG is configured (pg.url non-empty) and the
// connection fails, runClassifierRebuild returns an error
// instead of silently skipping the PG delete.
func TestClassifierRebuildHardFailsOnPGUnreachable(t *testing.T) {
	dir := classifierTestEnv(t, nil)
	cfg, err := config.LoadMinimal()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.DBPath = filepath.Join(dir, "sessions.db")
	// Point at a deliberately-unreachable PG URL. Use port 1
	// (commonly closed) so Open returns quickly without
	// blocking the test.
	cfg.PG.URL = "postgres://nobody:nobody@127.0.0.1:1/nonexistent?sslmode=disable&connect_timeout=2"
	cfg.PG.AllowInsecure = true
	applyClassifierConfig(cfg)
	seedHash(t, cfg)

	err = runClassifierRebuild(
		context.Background(), cfg, &bytes.Buffer{},
	)
	if err == nil {
		t.Fatal("expected error for unreachable PG, got nil")
	}
	if !strings.Contains(err.Error(), "PG") &&
		!strings.Contains(err.Error(), "pg") {
		t.Errorf("error should mention PG, got: %v", err)
	}
}

// TestClassifierRebuildSkipsPGWhenNotConfigured verifies the
// silent-skip path: when pg.url is empty, the command does
// NOT attempt PG cleanup and returns nil even if PG would
// otherwise be unreachable.
func TestClassifierRebuildSkipsPGWhenNotConfigured(t *testing.T) {
	dir := classifierTestEnv(t, nil)
	cfg, err := config.LoadMinimal()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.DBPath = filepath.Join(dir, "sessions.db")
	cfg.PG.URL = ""
	applyClassifierConfig(cfg)
	seedHash(t, cfg)

	if err := runClassifierRebuild(
		context.Background(), cfg, &bytes.Buffer{},
	); err != nil {
		t.Fatalf("unexpected error when PG unconfigured: %v", err)
	}
}
```

Note: depending on how quickly `postgres.Open` fails when the host is closed,
the unreachable test may take a few seconds. The `connect_timeout=2` parameter
caps it.

The "reachable-but-error" case (PG up, DELETE fails due to permissions) shares
the exact same `if err != nil { return ... }` branch in `clearPGClassifierHash`,
so the unreachable test exercises the same error-propagation code path. A
separate reachable-but-error test would require pgtest infrastructure and is
omitted in favor of the simpler unreachable test; if pgtest is later added to
this command, extend `internal/postgres/automated_pgtest_test.go` with a
`REVOKE DELETE` case rather than reaching into `cmd/agentsview` from a pgtest
file.

- [ ] **Step 7: Run all the new tests**

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview/ -run TestClassifier -v
```

Expected: PASS for all `TestClassifier*` tests, including the two PG cases.

- [ ] **Step 8: Re-run the static guardrail test**

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview/ -run TestEveryStoreOpenPathIsWired -v
```

Expected: PASS. The new `classifier.go` calls `applyClassifierConfig` inside the
`RunE` closure before any store-open trigger (note: it uses raw `sql.Open` and
`postgres.Open`, so the trigger that matters is `postgres.Open`).

- [ ] **Step 9: Smoke test the new CLI**

```bash
make build
./agentsview classifier --help
./agentsview classifier rebuild --help
```

Expected: help renders without error.

- [ ] **Step 10: Run formatter and vet**

```bash
go fmt ./cmd/agentsview/... ./internal/db/...
go vet ./cmd/agentsview/... ./internal/db/...
```

Expected: no diff, no warnings.

- [ ] **Step 11: Commit**

```bash
git add cmd/agentsview/classifier.go cmd/agentsview/classifier_test.go \
	cmd/agentsview/cli.go internal/db/db.go
git commit -m "feat(cli): agentsview classifier rebuild for hash recovery"
```

______________________________________________________________________

## Task 9: Final integration verification

**Files:**

- No code changes — only tests and lint.

- [ ] **Step 1: Full Go test suite**

```bash
make test
```

Expected: PASS.

- [ ] **Step 2: Full lint**

```bash
make lint
make vet
```

Expected: no warnings.

- [ ] **Step 3: PG integration tests**

```bash
make test-postgres
```

Expected: PASS, including the two new `automated_pgtest_test.go` cases.

- [ ] **Step 4: End-to-end smoke test**

```bash
make build
mkdir -p /tmp/agentsview-smoke
cat > /tmp/agentsview-smoke/config.toml <<'EOF'
[automated]
prefixes = ["You are analyzing an essay", "Grade these Benn Stancil quotes"]
EOF
AGENT_VIEWER_DATA_DIR=/tmp/agentsview-smoke ./agentsview classifier rebuild
```

Expected output: two prefix lines, the count line, and the restart reminder. No
errors. (PG section is skipped silently because `pg.url` is unset.)

- [ ] **Step 5: Verify the hash was written on next open**

```bash
AGENT_VIEWER_DATA_DIR=/tmp/agentsview-smoke ./agentsview health 2>&1 | head -5
sqlite3 /tmp/agentsview-smoke/sessions.db \
  "SELECT key, value FROM stats WHERE key = 'is_automated_classifier_hash'"
```

Expected: `health` runs without error (empty session list is fine), and the
SELECT returns one row with a 64-character hex hash.

- [ ] **Step 6: Cleanup smoke test artifacts**

```bash
rm -rf /tmp/agentsview-smoke
```

- [ ] **Step 7: Final commit if anything was changed during verification**

```bash
git status
```

If clean: nothing more to commit. If any test data file (e.g. `testdata/`) was
added, commit it now with `chore: add test data` or similar.

______________________________________________________________________

## Done checklist

When every task above is complete:

- [ ] Branch builds with `make build`

- [ ] `make test` passes (Go unit + integration)

- [ ] `make test-postgres` passes (PG integration)

- [ ] `make lint` and `make vet` are clean

- [ ] `agentsview classifier rebuild --help` renders

- [ ] `agentsview classifier rebuild` round-trips on a fresh data dir with a
  configured `[automated] prefixes`

- [ ] Static guardrail (`TestEveryStoreOpenPathIsWired`) is green

- [ ] No `IsAutomatedBackfillMarker` references remain in the tree:

  ```bash
  grep -r IsAutomatedBackfillMarker .
  ```

  Expected: no matches.

Then:

- [ ] Open a PR with the spec link in the description, no test plan section (per
  CLAUDE.md PR conventions).
