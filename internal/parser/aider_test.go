package parser

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtureAider is the path to the golden fixture derived from a real
// .aider.chat.history.md file. It holds three runs: two with content and
// a trailing header-only run with no body.
func fixtureAider() string {
	return filepath.Join(
		"testdata", "aider", "myrepo", ".aider.chat.history.md",
	)
}

// TestParseAiderRunsPerRun asserts the per-run fan-out: the multi-run
// fixture yields one session per run that has parseable turns, each with
// its own StartedAt and FirstMessage. The header-only trailing run
// contributes no session.
func TestParseAiderRunsPerRun(t *testing.T) {
	results, err := parseAiderRuns(fixtureAider(), "testmachine")
	require.NoError(t, err)
	// Three runs in the file, but the trailing header-only run has no
	// turns, so only two sessions are emitted.
	require.Len(t, results, 2, "one session per content-bearing run")

	r0, r1 := results[0], results[1]

	// Both runs share project/machine/agent but are distinct sessions.
	for _, r := range results {
		assert.Equal(t, AgentAider, r.Session.Agent)
		assert.Equal(t, "testmachine", r.Session.Machine)
		assert.Equal(t, "myrepo", r.Session.Project)
		assert.Contains(t, r.Session.ID, "aider:")
	}
	assert.NotEqual(t, r0.Session.ID, r1.Session.ID,
		"distinct runs get distinct session IDs")

	// Run 0: header 14:01:00, first prompt "add a retry to the webhook",
	// two user prompts ("add a retry..." and "Step 1").
	assert.Equal(t,
		time.Date(2026, 6, 9, 14, 1, 0, 0, time.UTC), r0.Session.StartedAt)
	assert.Equal(t, r0.Session.StartedAt, r0.Session.EndedAt,
		"a run has no separate end time")
	assert.Contains(t, r0.Session.FirstMessage, "add a retry to the webhook")
	assert.Equal(t, 2, r0.Session.UserMessageCount)

	// Run 1: header 15:30:00, its own first prompt and message stream.
	assert.Equal(t,
		time.Date(2026, 6, 9, 15, 30, 0, 0, time.UTC), r1.Session.StartedAt)
	assert.Contains(t,
		r1.Session.FirstMessage, "make the timeout configurable")
	assert.Equal(t, 1, r1.Session.UserMessageCount)

	// The message streams are per-run, not flattened: run 1 must not carry
	// run 0's prompt.
	require.NotEmpty(t, r1.Messages)
	for _, m := range r1.Messages {
		assert.NotContains(t, m.Content, "add a retry to the webhook")
	}

	// Run 0 roles, in order. "#### Step 1" is an aider USER prompt (a fresh
	// user channel), not an assistant heading: aider only ever writes
	// "#### " for user input. The "> " lines are tool output, surfaced as
	// assistant transcript content (agentsview has no tool role).
	roles := make([]RoleType, len(r0.Messages))
	for i, m := range r0.Messages {
		roles[i] = m.Role
	}
	assert.Equal(t, []RoleType{
		RoleUser,      // add a retry to the webhook
		RoleAssistant, // I'll add exponential backoff ... Here is the plan:
		RoleUser,      // Step 1
		RoleAssistant, // Wrap the call in a loop.
		RoleAssistant, // > Applied edit ... (tool block, surfaced as assistant)
	}, roles)
	assert.Equal(t, "add a retry to the webhook", r0.Messages[0].Content)
	assert.Contains(t, r0.Messages[1].Content, "exponential backoff")
	assert.Contains(t, r0.Messages[1].Content, "Here is the plan:")

	// No per-message timestamps in aider's markdown format.
	for _, r := range results {
		for _, m := range r.Messages {
			assert.True(t, m.Timestamp.IsZero())
		}
	}
}

// TestParseAiderRunSingle parses one run out of a file by index.
func TestParseAiderRunSingle(t *testing.T) {
	sess, msgs, err := parseAiderRun(fixtureAider(), 1, "m")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotEmpty(t, msgs)
	assert.Contains(t, sess.FirstMessage, "make the timeout configurable")
	assert.Equal(t,
		time.Date(2026, 6, 9, 15, 30, 0, 0, time.UTC), sess.StartedAt)

	// The trailing header-only run (index 2) yields no session.
	sess2, msgs2, err := parseAiderRun(fixtureAider(), 2, "m")
	require.NoError(t, err)
	assert.Nil(t, sess2)
	assert.Empty(t, msgs2)

	// Out-of-range indices are tolerated, not errors.
	sess3, _, err := parseAiderRun(fixtureAider(), 99, "m")
	require.NoError(t, err)
	assert.Nil(t, sess3)
}

// TestAiderSessionIDStableOnAppend is the core regression test for
// MUST-FIX 1's ID scheme: appending a new run to a file must not change
// any earlier run's session ID. A bare positional index would re-key
// later runs when an early run is removed; hashing the header plus an
// equal-header ordinal keeps each ID pinned to its own run.
func TestAiderSessionIDStableOnAppend(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "myrepo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	path := filepath.Join(repo, ".aider.chat.history.md")

	base := "# aider chat started at 2026-06-09 14:01:00\n" +
		"#### first prompt\nanswer one\n" +
		"# aider chat started at 2026-06-09 15:30:00\n" +
		"#### second prompt\nanswer two\n"
	require.NoError(t, os.WriteFile(path, []byte(base), 0o644))

	before, err := parseAiderRuns(path, "m")
	require.NoError(t, err)
	require.Len(t, before, 2)
	id0, id1 := before[0].Session.ID, before[1].Session.ID

	// Append a third run with a fresh timestamp.
	appended := base +
		"# aider chat started at 2026-06-09 16:45:00\n" +
		"#### third prompt\nanswer three\n"
	require.NoError(t, os.WriteFile(path, []byte(appended), 0o644))

	after, err := parseAiderRuns(path, "m")
	require.NoError(t, err)
	require.Len(t, after, 3)

	assert.Equal(t, id0, after[0].Session.ID,
		"appending a run must not re-key the first run")
	assert.Equal(t, id1, after[1].Session.ID,
		"appending a run must not re-key the second run")
	assert.NotEqual(t, id0, after[2].Session.ID)
	assert.NotEqual(t, id1, after[2].Session.ID)
}

// TestAiderSessionIDStableOnEarlyRemoval asserts that removing an early
// run does not re-key the runs that follow it (the bare-positional-index
// failure mode).
func TestAiderSessionIDStableOnEarlyRemoval(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "myrepo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	path := filepath.Join(repo, ".aider.chat.history.md")

	run0 := "# aider chat started at 2026-06-09 14:01:00\n" +
		"#### first prompt\nanswer one\n"
	run1 := "# aider chat started at 2026-06-09 15:30:00\n" +
		"#### second prompt\nanswer two\n"
	require.NoError(t, os.WriteFile(path, []byte(run0+run1), 0o644))

	before, err := parseAiderRuns(path, "m")
	require.NoError(t, err)
	require.Len(t, before, 2)
	secondID := before[1].Session.ID

	// Remove the first run; the second run is now positionally index 0 but
	// must keep its original ID.
	require.NoError(t, os.WriteFile(path, []byte(run1), 0o644))
	after, err := parseAiderRuns(path, "m")
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.Equal(t, secondID, after[0].Session.ID,
		"removing an earlier run must not re-key a later run")
}

// TestAiderEqualHeaderRunsGetStableDistinctIDs covers the rare case of two
// runs with identical header timestamps: they must get distinct, stable
// IDs disambiguated by their ordinal among equal-header runs.
func TestAiderEqualHeaderRunsGetStableDistinctIDs(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "myrepo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	path := filepath.Join(repo, ".aider.chat.history.md")

	content := "# aider chat started at 2026-06-09 14:01:00\n" +
		"#### prompt a\nanswer a\n" +
		"# aider chat started at 2026-06-09 14:01:00\n" +
		"#### prompt b\nanswer b\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	r1, err := parseAiderRuns(path, "m")
	require.NoError(t, err)
	require.Len(t, r1, 2)
	assert.NotEqual(t, r1[0].Session.ID, r1[1].Session.ID,
		"equal-header runs disambiguate by ordinal")

	// Stable across re-parse.
	r2, err := parseAiderRuns(path, "m")
	require.NoError(t, err)
	require.Len(t, r2, 2)
	assert.Equal(t, r1[0].Session.ID, r2[0].Session.ID)
	assert.Equal(t, r1[1].Session.ID, r2[1].Session.ID)
}

// TestAiderSessionIDStableAcrossExtractionDirs is the MEDIUM-1 regression
// test for SSH sync. During remote sync the history file is extracted to a
// RANDOM local temp dir, so hashing the on-disk path would re-key the run on
// every sync. Passing a canonical identity path (the remote physical path)
// to parseAiderRunsWithID must produce the SAME ID regardless of where the
// file physically lives, while the plain on-disk parse (local behavior)
// produces DIFFERENT IDs for the two locations.
func TestAiderSessionIDStableAcrossExtractionDirs(t *testing.T) {
	content := "# aider chat started at 2026-06-09 14:01:00\n" +
		"#### first prompt\nanswer one\n" +
		"# aider chat started at 2026-06-09 15:30:00\n" +
		"#### second prompt\nanswer two\n"

	// Two distinct on-disk locations standing in for two sync runs that
	// extract the same remote file under different random temp dirs.
	writeAt := func(t *testing.T) string {
		t.Helper()
		repo := filepath.Join(t.TempDir(), "myrepo")
		require.NoError(t, os.MkdirAll(repo, 0o755))
		p := filepath.Join(repo, ".aider.chat.history.md")
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
		return p
	}
	pathA := writeAt(t)
	pathB := writeAt(t)
	require.NotEqual(t, pathA, pathB, "the two extraction paths must differ")

	// The canonical identity is the remote physical path, the same for both
	// syncs regardless of the temp extraction dir.
	const identity = "host:/home/wes/myrepo/.aider.chat.history.md"

	withIDa, err := parseAiderRunsWithID(pathA, identity, "m")
	require.NoError(t, err)
	require.Len(t, withIDa, 2)
	withIDb, err := parseAiderRunsWithID(pathB, identity, "m")
	require.NoError(t, err)
	require.Len(t, withIDb, 2)

	assert.Equal(t, withIDa[0].Session.ID, withIDb[0].Session.ID,
		"identical identity path must yield a stable ID across temp dirs")
	assert.Equal(t, withIDa[1].Session.ID, withIDb[1].Session.ID,
		"identical identity path must yield a stable ID across temp dirs")

	// Sanity: the ID is derived from the identity path, not the temp path.
	// Without an identity path (local behavior), the two extraction paths
	// produce DIFFERENT IDs -- exactly the instability the identity path
	// fixes. parseAiderRuns is the empty-identity passthrough.
	localA, err := parseAiderRuns(pathA, "m")
	require.NoError(t, err)
	require.Len(t, localA, 2)
	localB, err := parseAiderRuns(pathB, "m")
	require.NoError(t, err)
	require.Len(t, localB, 2)
	assert.NotEqual(t, localA[0].Session.ID, localB[0].Session.ID,
		"without an identity path the temp path leaks into the ID")
	assert.NotEqual(t, withIDa[0].Session.ID, localA[0].Session.ID,
		"identity-path ID differs from on-disk-path ID")

	// parseAiderRunWithID (single-run) must agree with the fan-out variant.
	single, _, err := parseAiderRunWithID(pathB, identity, 0, "m")
	require.NoError(t, err)
	require.NotNil(t, single)
	assert.Equal(t, withIDa[0].Session.ID, single.ID,
		"single-run identity ID must match the fan-out identity ID")
}

// TestAiderSameHeaderEarlyRemovalRekeysSiblings pins the documented residual
// limitation of the ordinal disambiguator: when multiple runs share a
// byte-identical header timestamp, removing an earlier same-header run shifts
// the ordinals of the later same-header siblings and therefore re-keys their
// IDs. A same-second collision in one repo is rare; this test exists so the
// behavior is explicit and any future change to the scheme is a conscious one.
func TestAiderSameHeaderEarlyRemovalRekeysSiblings(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "myrepo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	path := filepath.Join(repo, ".aider.chat.history.md")

	hdr := "# aider chat started at 2026-06-09 14:01:00\n"
	runA := hdr + "#### prompt a\nanswer a\n"
	runB := hdr + "#### prompt b\nanswer b\n"
	runC := hdr + "#### prompt c\nanswer c\n"
	require.NoError(t, os.WriteFile(path, []byte(runA+runB+runC), 0o644))

	before, err := parseAiderRuns(path, "m")
	require.NoError(t, err)
	require.Len(t, before, 3)
	idB := before[1].Session.ID

	// Remove the first same-header run; runs b and c each shift down one
	// equal-header ordinal.
	require.NoError(t, os.WriteFile(path, []byte(runB+runC), 0o644))
	after, err := parseAiderRuns(path, "m")
	require.NoError(t, err)
	require.Len(t, after, 2)

	// Documented residual: run b, now ordinal 0, takes the former ordinal-0
	// ID, so its ID changes. A unique-header run keeps its ID instead
	// (see TestAiderSessionIDStableOnEarlyRemoval).
	assert.NotEqual(t, idB, after[0].Session.ID,
		"same-header early removal re-keys later siblings (accepted residual)")
}

// TestDiscoverAiderFindsFilesAtMaxDepth pins the depth-cap fix: a history
// file whose parent directory is exactly aiderMaxWalkDepth (4) levels under
// the root must be discovered, while one a level deeper must not. A `>=`
// test skipped the max-depth directory before its files were seen.
func TestDiscoverAiderFindsFilesAtMaxDepth(t *testing.T) {
	root := t.TempDir()
	atCap := filepath.Join(root, "a", "b", "c", "d")        // parent depth 4
	tooDeep := filepath.Join(root, "a", "b", "c", "d", "e") // parent depth 5
	require.NoError(t, os.MkdirAll(atCap, 0o755))
	require.NoError(t, os.MkdirAll(tooDeep, 0o755))
	hist := "# aider chat started at 2026-06-09 14:01:00\n#### p\nans\n"
	atCapFile := filepath.Join(atCap, ".aider.chat.history.md")
	tooDeepFile := filepath.Join(tooDeep, ".aider.chat.history.md")
	require.NoError(t, os.WriteFile(atCapFile, []byte(hist), 0o644))
	require.NoError(t, os.WriteFile(tooDeepFile, []byte(hist), 0o644))

	var paths []string
	for _, f := range discoverAiderSessions(root) {
		paths = append(paths, f.Path)
	}
	assert.Contains(t, paths, atCapFile,
		"a history file at the max walk depth must be discovered")
	assert.NotContains(t, paths, tooDeepFile,
		"a history file below the max walk depth must not be discovered")
}

// TestAiderRawIDAtDetectsShiftedIndex pins that a stored positional
// "<history>#<idx>" path is validated by recomputed ID: after an earlier run
// is removed, the stale index no longer recomputes to a run, and the session
// re-resolves to the run's new index by raw ID (the engine fast-path
// correctness fix).
func TestAiderRawIDAtDetectsShiftedIndex(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "myrepo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	path := filepath.Join(repo, ".aider.chat.history.md")

	run0 := "# aider chat started at 2026-06-09 14:01:00\n#### first\nans1\n"
	run1 := "# aider chat started at 2026-06-09 15:30:00\n#### second\nans2\n"
	require.NoError(t, os.WriteFile(path, []byte(run0+run1), 0o644))

	// run1's raw ID, as stored under the virtual path "<path>#1".
	id1, ok := AiderRawIDAt(path, 1)
	require.True(t, ok)
	_, ok = AiderRawIDAt(path, 5)
	assert.False(t, ok, "out-of-range index returns false")

	// Remove the first run; index 1 is now out of range, so the stored
	// positional path can no longer be trusted by recomputed ID.
	require.NoError(t, os.WriteFile(path, []byte(run1), 0o644))
	_, ok = AiderRawIDAt(path, 1)
	assert.False(t, ok, "stale index 1 no longer recomputes to a run")

	// Re-resolution by raw ID finds run1 at its new index 0.
	resolved := findAiderSourceFile(dir, id1)
	assert.Equal(t, AiderVirtualPath(path, 0), resolved,
		"re-resolving by raw ID locates the run at its shifted index")
}

func TestParseAiderTimestamp(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Time
		ok   bool
	}{
		{
			name: "valid naive local assumed UTC",
			in:   "2026-06-09 14:01:00",
			want: time.Date(2026, 6, 9, 14, 1, 0, 0, time.UTC),
			ok:   true,
		},
		{"trailing space tolerated", "2026-06-09 14:01:00 ",
			time.Date(2026, 6, 9, 14, 1, 0, 0, time.UTC), true},
		{"garbage rejected", "not a date", time.Time{}, false},
		{"empty rejected", "", time.Time{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseAiderTimestamp(c.in)
			assert.Equal(t, c.ok, ok)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestSplitAiderRuns(t *testing.T) {
	content := "junk before any header\n" +
		"# aider chat started at 2026-06-09 14:01:00\n" +
		"#### hi\n" +
		"answer\n" +
		"# aider chat started at 2026-06-09 15:00:00\n" +
		"# aider chat started at 2026-06-09 16:00:00\n" +
		"#### again\n"

	runs := splitAiderRuns(content)
	require.Len(t, runs, 3, "empty middle run keeps its slot")

	first, ok := parseAiderTimestamp("2026-06-09 14:01:00")
	require.True(t, ok)
	assert.Equal(t, first, runs[0].started)
	assert.Equal(t, "2026-06-09 14:01:00", runs[0].rawHeader)
	assert.Contains(t, runs[0].body, "#### hi")
	assert.Empty(t, runs[1].body, "header-only run has empty body")

	// Bytes before the first header are dropped.
	for _, r := range runs {
		assert.NotContains(t, r.body, "junk before any header")
	}
}

func TestParseAiderTurnsToolAndEditedFiles(t *testing.T) {
	body := "#### fix the bug\n" +
		"Here is the fix.\n" +
		"Some prose.\n" +
		"> Applied edit to src/a.py\n" +
		"> Applied edit to src/a.py\n" +
		"> Creating empty file src/b.py\n" +
		"> Did not apply edit to src/c.py (--dry-run)\n" +
		"> Skipping edits to src/d.py\n"

	msgs, touched := parseAiderTurns(body)

	roles := make([]RoleType, len(msgs))
	for i, m := range msgs {
		roles[i] = m.Role
	}
	// user, assistant prose, then the tool block surfaced as assistant.
	assert.Equal(t,
		[]RoleType{RoleUser, RoleAssistant, RoleAssistant}, roles)
	assert.Equal(t, "fix the bug", msgs[0].Content)
	assert.Contains(t, msgs[1].Content, "Here is the fix.")
	assert.Contains(t, msgs[2].Content, "Applied edit to src/a.py")

	// Dedup; dry-run and skip lines contribute nothing.
	assert.Equal(t, []string{"src/a.py", "src/b.py"}, touched)

	for _, m := range msgs {
		assert.True(t, m.Timestamp.IsZero())
	}
}

func TestParseAiderTurnsBlankLinesDoNotSplit(t *testing.T) {
	body := "#### q\n" +
		"para one\n" +
		"\n" +
		"para two\n"
	msgs, _ := parseAiderTurns(body)
	require.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Contains(t, msgs[1].Content, "para one")
	assert.Contains(t, msgs[1].Content, "para two")
}

func TestParseAiderRunsEmptyAndGarbage(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name      string
		content   string
		wantCount int
	}{
		{"empty file", "", 0},
		{"only preamble, no header", "some text\nwith no header\n", 0},
		{"header-only run, no body", "# aider chat started at 2026-06-09 14:01:00\n", 0},
		{
			name:      "garbage header timestamp still indexed",
			content:   "# aider chat started at not-a-real-date\n#### hello\nhi\n",
			wantCount: 1,
		},
		{
			name: "crlf line endings",
			content: "# aider chat started at 2026-06-09 14:01:00\r\n" +
				"#### crlf prompt\r\nanswer\r\n",
			wantCount: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := filepath.Join(dir, c.name)
			require.NoError(t, os.MkdirAll(repo, 0o755))
			path := filepath.Join(repo, ".aider.chat.history.md")
			require.NoError(t,
				os.WriteFile(path, []byte(c.content), 0o644))

			results, err := parseAiderRuns(path, "m")
			require.NoError(t, err) // never panics, never hard-errors
			assert.Len(t, results, c.wantCount)
		})
	}
}

func TestAiderVirtualPathRoundTrip(t *testing.T) {
	hist := filepath.Join("repo", ".aider.chat.history.md")
	vp := AiderVirtualPath(hist, 3)
	assert.Equal(t, hist+"#3", vp)

	gotPath, gotIdx, ok := ParseAiderVirtualPath(vp)
	require.True(t, ok)
	assert.Equal(t, hist, gotPath)
	assert.Equal(t, 3, gotIdx)

	// Non-virtual / invalid inputs are rejected.
	cases := []string{
		hist,                 // real path, no "#"
		"repo/other.md#0",    // wrong base name
		hist + "#",           // empty index
		hist + "#-1",         // negative index
		hist + "#notanumber", // non-numeric index
		"plain/path",         // no separator at all
	}
	for _, in := range cases {
		_, _, ok := ParseAiderVirtualPath(in)
		assert.False(t, ok, "should reject %q", in)
	}
}

func TestListAiderRunMetas(t *testing.T) {
	metas, err := ListAiderRunMetas(fixtureAider())
	require.NoError(t, err)
	// Three runs -> three metas (one per slot, including the header-only
	// trailing run, so positional indices stay stable).
	require.Len(t, metas, 3)
	assert.Equal(t, 0, metas[0].Idx)
	assert.Equal(t, 1, metas[1].Idx)
	assert.Equal(t, 2, metas[2].Idx)
	assert.Equal(t,
		time.Date(2026, 6, 9, 14, 1, 0, 0, time.UTC), metas[0].Started)
	assert.Equal(t,
		time.Date(2026, 6, 9, 15, 30, 0, 0, time.UTC), metas[1].Started)
	for i, m := range metas {
		_, idx, ok := ParseAiderVirtualPath(m.VirtualPath)
		require.True(t, ok)
		assert.Equal(t, i, idx)
	}
	// HasMessages flags content-bearing runs: the first two runs have turns,
	// the trailing header-only run does not. The engine's unchanged-check
	// uses this to avoid expecting a stored row for header-only runs.
	assert.True(t, metas[0].HasMessages, "run 0 has turns")
	assert.True(t, metas[1].HasMessages, "run 1 has turns")
	assert.False(t, metas[2].HasMessages, "header-only run produces no session")
}

// TestAiderRegistryOptInDiscovery pins that Aider is not discovered by
// default. Aider has no central store, and a rootless $HOME scan can trigger
// macOS privacy prompts during passive background refreshes, so users must opt
// in with AIDER_DIR or aider_dirs. ShallowWatch must stay true so a configured
// broad root is watched only at the root, relying on the periodic sync.
func TestAiderRegistryOptInDiscovery(t *testing.T) {
	def, ok := AgentByType(AgentAider)
	require.True(t, ok, "AgentAider missing from Registry")
	assert.Empty(t, def.DefaultDirs,
		"aider must not be discovered by default; opt in via AIDER_DIR/aider_dirs")
	assert.True(t, def.ShallowWatch,
		"aider must watch an opt-in broad root shallowly, not recurse all of it")
	// The shallow-watch contract relies on no static subdir or custom
	// watch-roots wiring overriding it.
	assert.Empty(t, def.WatchSubdirs)
	assert.Nil(t, def.WatchRootsFunc)
}

func TestDiscoverAiderSessions(t *testing.T) {
	root := t.TempDir()

	// A repo with a history file at its root.
	repo := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, ".aider.chat.history.md"),
		[]byte("# aider chat started at 2026-06-09 14:01:00\n"), 0o644))
	// A sibling non-matching file must never be picked up.
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "README.md"), []byte("x\n"), 0o644))

	// A history file buried in a skipped dir must be ignored.
	skip := filepath.Join(repo, "node_modules", "dep")
	require.NoError(t, os.MkdirAll(skip, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skip, ".aider.chat.history.md"),
		[]byte("# aider chat started at 2026-06-09 14:01:00\n"), 0o644))

	files := discoverAiderSessions(root)
	require.Len(t, files, 1, "found repo file, skipped node_modules")
	assert.Equal(t, AgentAider, files[0].Agent)
	assert.Equal(t, aiderHistoryFile, filepath.Base(files[0].Path))

	// Empty root is tolerated.
	assert.Empty(t, discoverAiderSessions(""))
}

func TestAiderShouldSkipProtectedHomeDirsOnlyOnDarwinHomeRoot(t *testing.T) {
	home := filepath.Join(string(os.PathSeparator), "home", "user")

	assert.True(t, aiderShouldSkipProtectedHomeDirs(home, home, "darwin"))
	assert.False(t, aiderShouldSkipProtectedHomeDirs(home, home, "linux"))
	assert.False(t, aiderShouldSkipProtectedHomeDirs(home, home, "windows"))
	assert.False(t,
		aiderShouldSkipProtectedHomeDirs(filepath.Join(home, "Documents"), home, "darwin"),
		"explicit protected roots are user-scoped opt-ins")
}

func TestAiderProtectedHomeDirsCoversMacOSTCCPrompts(t *testing.T) {
	for _, name := range []string{
		"Desktop",
		"Documents",
		"Downloads",
		"Movies",
		"Music",
		"Photos",
		"Pictures",
	} {
		_, ok := aiderProtectedHomeDirs[name]
		assert.True(t, ok, "%s should be pruned from broad home discovery", name)
	}
}

func TestAiderBroadHomeWalkRootsExcludeMacOSProtectedDirs(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{"Code", "Documents", "Downloads"} {
		require.NoError(t, os.Mkdir(filepath.Join(home, name), 0o755))
	}

	roots := aiderDiscoveryWalkRoots(home, home, "darwin")

	assert.Contains(t, roots, filepath.Join(home, "Code"))
	assert.NotContains(t, roots, home,
		"broad Aider home discovery must not recursively walk $HOME on macOS")
	assert.NotContains(t, roots, filepath.Join(home, "Documents"))
	assert.NotContains(t, roots, filepath.Join(home, "Downloads"))
}

func TestDiscoverAiderSessionsSkipsMacOSProtectedDirs(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS TCC protected-directory pruning is Darwin-only")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	for _, name := range []string{
		"Desktop",
		"Documents",
		"Downloads",
		"Movies",
		"Music",
		"Photos",
		"Pictures",
	} {
		protectedRepo := filepath.Join(root, name, "proj")
		require.NoError(t, os.MkdirAll(protectedRepo, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(protectedRepo, ".aider.chat.history.md"),
			[]byte("# aider chat started at 2026-06-09 14:01:00\n"), 0o644))
	}

	files := discoverAiderSessions(root)
	assert.Empty(t, files, "default home discovery must not enter macOS TCC-protected folders")
}

func TestDiscoverAiderSessionsAllowsExplicitProtectedRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	documentsRoot := filepath.Join(home, "Documents")
	repo := filepath.Join(documentsRoot, "proj")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, ".aider.chat.history.md"),
		[]byte("# aider chat started at 2026-06-09 14:01:00\n"), 0o644))

	files := discoverAiderSessions(documentsRoot)
	require.Len(t, files, 1, "explicit Aider roots should still be scanned")
	assert.Equal(t, filepath.Join(repo, ".aider.chat.history.md"), files[0].Path)
}

// TestAiderWalkBudget documents the wall-clock budget (MUST-FIX 3,
// ported from the Rust adapter's WALK_BUDGET_SECS) and confirms a normal
// discovery walk completes well within it. The budget is checked inside
// the WalkDir callback and returns filepath.SkipAll once exceeded.
func TestAiderWalkBudget(t *testing.T) {
	assert.Equal(t, 2*time.Second, aiderWalkBudget,
		"budget mirrors the Rust adapter's WALK_BUDGET_SECS")

	root := t.TempDir()
	// A small but multi-level tree with one history file.
	deep := filepath.Join(root, "a", "b", "c")
	require.NoError(t, os.MkdirAll(deep, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(deep, ".aider.chat.history.md"),
		[]byte("# aider chat started at 2026-06-09 14:01:00\n#### hi\nok\n"),
		0o644))

	start := time.Now()
	files := discoverAiderSessions(root)
	elapsed := time.Since(start)
	assert.Less(t, elapsed, aiderWalkBudget,
		"a normal walk finishes well under budget")
	require.Len(t, files, 1)
}

func TestFindAiderSourceFile(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "myrepo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	hist := filepath.Join(repo, ".aider.chat.history.md")
	content := "# aider chat started at 2026-06-09 14:01:00\n" +
		"#### first\nanswer one\n" +
		"# aider chat started at 2026-06-09 15:30:00\n" +
		"#### second\nanswer two\n"
	require.NoError(t, os.WriteFile(hist, []byte(content), 0o644))

	// Parse the runs to learn the real per-run raw IDs.
	results, err := parseAiderRuns(hist, "m")
	require.NoError(t, err)
	require.Len(t, results, 2)

	for i, r := range results {
		rawID := r.Session.ID[len(aiderIDPrefix):]
		found := findAiderSourceFile(root, rawID)
		require.NotEmpty(t, found, "run %d should resolve", i)
		gotPath, gotIdx, ok := ParseAiderVirtualPath(found)
		require.True(t, ok)
		assert.Equal(t, aiderHistoryFile, filepath.Base(gotPath))
		assert.Equal(t, i, gotIdx, "run %d resolves to run index %d", i, i)
	}

	assert.Empty(t, findAiderSourceFile(root, "nonexistent-id"))
	assert.Empty(t, findAiderSourceFile("", "anything"))
}
