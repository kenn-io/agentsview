package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAutomationVerdictFromPrefix(t *testing.T) {
	SetUserAutomationPrefixes([]string{"Custom automation:"})
	t.Cleanup(func() { SetUserAutomationPrefixes(nil) })

	tests := []struct {
		name           string
		prefix         string
		fullByteLength int64
		wantMatched    bool
		wantConclusive bool
	}{
		{
			name: "built-in prefix", prefix: "You are a code reviewer. Review",
			fullByteLength: 200, wantMatched: true, wantConclusive: true,
		},
		{
			name: "user prefix", prefix: "Custom automation: inspect",
			fullByteLength: 100, wantMatched: true, wantConclusive: true,
		},
		{
			name: "substring visible", prefix: "This was invoked by roborev to perform this review",
			fullByteLength: 100, wantMatched: true, wantConclusive: true,
		},
		{
			name: "complete exact with Unicode whitespace", prefix: "\u2003Warmup\u00a0",
			fullByteLength: int64(len("\u2003Warmup\u00a0")),
			wantMatched:    true, wantConclusive: true,
		},
		{
			name: "exact followed by unseen suffix", prefix: "Warmup",
			fullByteLength: int64(len("Warmup and continue")),
			wantMatched:    false, wantConclusive: false,
		},
		{
			name: "complete non-match", prefix: "Explain this code",
			fullByteLength: int64(len("Explain this code")),
			wantMatched:    false, wantConclusive: true,
		},
		{
			name: "substring beyond prefix", prefix: strings.Repeat("x", 32),
			fullByteLength: 200, wantMatched: false, wantConclusive: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matched, conclusive := AutomationVerdictFromPrefix(
				[]byte(tt.prefix), tt.fullByteLength,
			)
			assert.Equal(t, tt.wantMatched, matched)
			assert.Equal(t, tt.wantConclusive, conclusive)
		})
	}
}

func TestBackfillIsAutomatedMatchingHashBoundsTextAllocations(t *testing.T) {
	d := testDB(t)
	const (
		largeConclusiveBytes = 4 << 20
		unresolvedBytes      = 256 << 10
		allocationCeiling    = 6 << 20
	)

	prefixPrompt := "You are a code reviewer." +
		strings.Repeat("x", largeConclusiveBytes)
	insertSession(t, d, "prefix-first-message", "proj", func(s *Session) {
		s.FirstMessage = &prefixPrompt
		s.UserMessageCount = 1
	})

	title := "Generated title"
	insertSession(t, d, "prefix-first-user", "proj", func(s *Session) {
		s.FirstMessage = &title
		s.UserMessageCount = 1
	})
	require.NoError(t, d.ReplaceSessionMessages(
		"prefix-first-user",
		[]Message{userMsg("prefix-first-user", 0, prefixPrompt)},
	), "store large prefix-matching first user message")

	humanPrompt := strings.Repeat("h", unresolvedBytes)
	insertSession(t, d, "large-human", "proj", func(s *Session) {
		s.FirstMessage = &humanPrompt
		s.UserMessageCount = 1
	})

	lateSubstringPrompt := strings.Repeat("z", unresolvedBytes) +
		" invoked by roborev to perform this review"
	insertSession(t, d, "late-substring", "proj", func(s *Session) {
		s.FirstMessage = &lateSubstringPrompt
		s.UserMessageCount = 1
	})

	largeMultiTurn := strings.Repeat("m", largeConclusiveBytes)
	insertSession(t, d, "stale-multi-turn", "proj", func(s *Session) {
		s.FirstMessage = &largeMultiTurn
		s.UserMessageCount = 2
	})

	_, err := d.getWriter().Exec(`
		UPDATE sessions
		SET is_automated = CASE id
			WHEN 'prefix-first-message' THEN 0
			WHEN 'prefix-first-user' THEN 0
			WHEN 'late-substring' THEN 0
			WHEN 'stale-multi-turn' THEN 1
			ELSE is_automated
		END
		WHERE id IN (
			'prefix-first-message', 'prefix-first-user',
			'late-substring', 'stale-multi-turn'
		)`)
	require.NoError(t, err, "seed stale automation flags")
	_, err = d.getWriter().Exec(
		`INSERT INTO stats (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		ClassifierHashKey, ClassifierHash(),
	)
	require.NoError(t, err, "stamp current classifier hash")

	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			d.mu.Lock()
			err := d.backfillIsAutomatedLocked(d.getWriter())
			d.mu.Unlock()
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	t.Logf("matching-hash audit: %s, %d B/op", result, result.AllocedBytesPerOp())
	assert.Less(t, result.AllocedBytesPerOp(), int64(allocationCeiling),
		"matching-hash audit copied conclusive large prompts: %s", result.String())

	for id, want := range map[string]bool{
		"prefix-first-message": true,
		"prefix-first-user":    true,
		"large-human":          false,
		"late-substring":       true,
		"stale-multi-turn":     false,
	} {
		session, err := d.GetSession(context.Background(), id)
		require.NoError(t, err, "get %s", id)
		require.NotNil(t, session, "missing %s", id)
		assert.Equal(t, want, session.IsAutomated, id)
	}
}

func TestBackfillIsAutomatedBidirectional(t *testing.T) {
	d := testDB(t)

	// Seed a false negative: single-turn roborev session with
	// is_automated = 0 (simulates pre-migration data).
	insertSession(t, d, "missed", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	// Force is_automated to 0 to simulate pre-migration state.
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 0 WHERE id = 'missed'",
	)
	require.NoError(t, err, "force missed to 0")

	// Seed a stale false positive: multi-turn session that was
	// previously marked automated under old broad rules.
	insertSession(t, d, "stale", "proj", func(s *Session) {
		fm := "# Fix Request for login flow"
		s.FirstMessage = &fm
		s.MessageCount = 10
		s.UserMessageCount = 5
	})
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 1 WHERE id = 'stale'",
	)
	require.NoError(t, err, "force stale to 1")

	// Clear the marker so the backfill will run.
	_, err = d.getWriter().Exec(
		"DELETE FROM stats WHERE key = ?",
		ClassifierHashKey,
	)
	require.NoError(t, err, "clear marker")

	// Run backfill.
	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(d.getWriter())
	d.mu.Unlock()
	require.NoError(t, err, "first backfill run")

	ctx := context.Background()

	// False negative should now be set.
	missed, err := d.GetSession(ctx, "missed")
	require.NoError(t, err, "get missed")
	assert.True(t, missed.IsAutomated, "missed session should be automated after backfill")

	// Stale false positive should now be cleared.
	stale, err := d.GetSession(ctx, "stale")
	require.NoError(t, err, "get stale")
	assert.False(t, stale.IsAutomated, "stale session should not be automated after backfill")
}

func TestBackfillIsAutomatedMarkerDoesNotHideCorruption(t *testing.T) {
	d := testDB(t)

	// Seed a roborev session.
	insertSession(t, d, "review", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})

	// Clear the marker and run backfill.
	_, err := d.getWriter().Exec(
		"DELETE FROM stats WHERE key = ?",
		ClassifierHashKey,
	)
	require.NoError(t, err, "clear marker")

	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(d.getWriter())
	d.mu.Unlock()
	require.NoError(t, err, "first run")

	// Manually corrupt the session. Matching classifier hashes
	// cannot be trusted as a complete integrity marker because
	// other DB write paths can import or preserve stale flags.
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 0 WHERE id = 'review'",
	)
	require.NoError(t, err, "corrupt")

	// Second run should repair the inconsistent row even though
	// the current hash is already stored.
	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(d.getWriter())
	d.mu.Unlock()
	require.NoError(t, err, "second run")

	ctx := context.Background()
	review, err := d.GetSession(ctx, "review")
	require.NoError(t, err, "get review")
	assert.True(t, review.IsAutomated,
		"second run should repair stale is_automated=0")
}

func TestBackfillIsAutomatedRepairsFalseNegativeWithMatchingHash(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "stale-hash", "proj", func(s *Session) {
		fm := "You are combining multiple code review outputs into a single GitHub PR comment. Rules follow."
		s.FirstMessage = &fm
		s.MessageCount = 2
		s.UserMessageCount = 1
	})

	_, err := d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 0 WHERE id = 'stale-hash'",
	)
	require.NoError(t, err, "force stale is_automated=0")
	_, err = d.getWriter().Exec(
		`INSERT INTO stats (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		ClassifierHashKey, ClassifierHash(),
	)
	require.NoError(t, err, "stamp current classifier hash")

	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(d.getWriter())
	d.mu.Unlock()
	require.NoError(t, err, "backfill")

	got, err := d.GetSession(ctx, "stale-hash")
	require.NoError(t, err, "get stale-hash")
	assert.True(t, got.IsAutomated,
		"matching classifier hash must not hide stale is_automated=0")
}

func TestBackfillIsAutomatedUsesFirstUserMessageWhenFirstMessageIsTitle(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	title := "Generated review title"
	insertSession(t, d, "title-review", "proj", func(s *Session) {
		s.FirstMessage = &title
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	require.NoError(t, d.ReplaceSessionMessages("title-review", []Message{
		userMsg("title-review", 0,
			"You are a code reviewer. Review the code changes shown below."),
		asstMsg("title-review", 1, "Review complete."),
	}), "ReplaceSessionMessages")
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 0 WHERE id = 'title-review'",
	)
	require.NoError(t, err, "force stale is_automated=0")

	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(d.getWriter())
	d.mu.Unlock()
	require.NoError(t, err, "backfill")

	got, err := d.GetSession(ctx, "title-review")
	require.NoError(t, err, "get title-review")
	require.NotNil(t, got, "title-review")
	assert.True(t, got.IsAutomated,
		"backfill should classify automation from the first stored user message")
}

func TestOpenRepairsAutomatedFalseNegativeWithMatchingHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	require.NoError(t, err, "open")

	insertSession(t, d, "reload-stale", "proj", func(s *Session) {
		fm := "You are combining multiple code review outputs into a single GitHub PR comment. Rules follow."
		s.FirstMessage = &fm
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 0 WHERE id = 'reload-stale'",
	)
	require.NoError(t, err, "force stale is_automated=0")
	_, err = d.getWriter().Exec(
		`INSERT INTO stats (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		ClassifierHashKey, ClassifierHash(),
	)
	require.NoError(t, err, "stamp current classifier hash")
	require.NoError(t, d.Close(), "close")

	reopened, err := Open(path)
	require.NoError(t, err, "reopen")
	defer reopened.Close()

	got, err := reopened.GetSession(context.Background(), "reload-stale")
	require.NoError(t, err, "get reload-stale")
	assert.True(t, got.IsAutomated,
		"Open must repair stale is_automated=0 despite matching hash")
}

// TestIncrementalUpdateReclassifiesOnPatternChange covers the
// case where the classifier gained a new pattern after a row
// was originally inserted. The row took the incremental path on
// subsequent parses, so UpsertSession never ran again to set
// is_automated. UpdateSessionIncremental must re-evaluate.
func TestIncrementalUpdateReclassifiesOnPatternChange(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Simulate a pre-existing single-turn session whose
	// first_message now matches a classifier pattern but whose
	// stored is_automated is stale at 0.
	insertSession(t, d, "changelog-inc", "proj", func(s *Session) {
		fm := "You are generating a changelog for myrepo version 1.0.0."
		s.FirstMessage = &fm
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 0 WHERE id = 'changelog-inc'",
	)
	require.NoError(t, err, "force stale is_automated=0")

	// Incremental update with umc still <= 1.
	err = callUpdateSessionIncrementalCompat(
		t,
		d,
		"changelog-inc",
		nil,
		2,
		1,
		1024,
		100,
		0,
		"",
		0,
		0,
		false,
		false,
	)
	require.NoError(t, err, "incremental update")

	got, err := d.GetSession(ctx, "changelog-inc")
	require.NoError(t, err, "get changelog-inc")
	assert.True(t, got.IsAutomated,
		"is_automated should be re-set after incremental update")
}

// TestIncrementalUpdateClearsWhenCountGrows covers the existing
// guard: when user_message_count grows past 1, is_automated must
// be cleared even if first_message still matches a pattern.
func TestIncrementalUpdateClearsWhenCountGrows(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "grew-past-one", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	// After UpsertSession the row is correctly is_automated=1.
	pre, err := d.GetSession(ctx, "grew-past-one")
	require.NoError(t, err, "get pre")
	require.True(t, pre.IsAutomated,
		"precondition: expected is_automated=1 after upsert")

	// Incremental update pushes umc > 1 — must clear.
	err = callUpdateSessionIncrementalCompat(
		t,
		d,
		"grew-past-one",
		nil,
		7,
		3,
		2048,
		200,
		0,
		"",
		0,
		0,
		false,
		false,
	)
	require.NoError(t, err, "incremental update")

	got, err := d.GetSession(ctx, "grew-past-one")
	require.NoError(t, err, "get grew-past-one")
	assert.False(t, got.IsAutomated,
		"is_automated should be cleared when umc grows > 1")
}

// TestIncrementalUpdateLeavesNonMatching covers a non-automated
// single-turn session: re-evaluation must NOT set is_automated
// when first_message doesn't match any pattern.
func TestIncrementalUpdateLeavesNonMatching(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "normal-single", "proj", func(s *Session) {
		fm := "Fix the login bug please"
		s.FirstMessage = &fm
		s.MessageCount = 2
		s.UserMessageCount = 1
	})

	err := callUpdateSessionIncrementalCompat(
		t,
		d,
		"normal-single",
		nil,
		2,
		1,
		1024,
		100,
		0,
		"",
		0,
		0,
		false,
		false,
	)
	require.NoError(t, err, "incremental update")

	got, err := d.GetSession(ctx, "normal-single")
	require.NoError(t, err, "get normal-single")
	assert.False(t, got.IsAutomated,
		"is_automated should stay 0 for non-matching first_message")
}

// TestIncrementalUpdateClearsTerminationStatus verifies that an
// incremental sync resets termination_status to NULL. The classifier
// needs the full message slice to reach the right verdict, and the
// incremental path only sees the new tail. Leaving the previous
// classification in place would surface stale "tool_call_pending"
// or "awaiting_user" indicators in the UI for up to 15 minutes
// after the user appended a resolving result or a new prompt.
func TestIncrementalUpdateClearsTerminationStatus(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "stale-term", "proj", func(s *Session) {
		s.MessageCount = 2
		s.UserMessageCount = 1
		v := "tool_call_pending"
		s.TerminationStatus = &v
	})

	pre, err := d.GetSession(ctx, "stale-term")
	require.NoError(t, err, "get pre")
	require.NotNil(t, pre.TerminationStatus,
		"precondition: expected tool_call_pending")
	require.Equal(t, "tool_call_pending", *pre.TerminationStatus,
		"precondition: expected tool_call_pending")

	err = callUpdateSessionIncrementalCompat(
		t,
		d,
		"stale-term",
		nil,
		4,
		2,
		2048,
		200,
		0,
		"",
		0,
		0,
		false,
		false,
	)
	require.NoError(t, err, "incremental update")

	got, err := d.GetSession(ctx, "stale-term")
	require.NoError(t, err, "get stale-term")
	assert.Nil(t, got.TerminationStatus,
		"termination_status should be NULL after incremental update")
}

func TestBackfillIsAutomatedBumpsLocalModifiedAt(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Seed a single-turn roborev session that the new classifier
	// will flip to is_automated = 1.
	insertSession(t, d, "to-flip", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	// Force is_automated = 0 so the backfill has work to do.
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 0 WHERE id = 'to-flip'",
	)
	require.NoError(t, err, "force to-flip to 0")

	// Snapshot local_modified_at before the backfill.
	before, err := d.GetSessionFull(ctx, "to-flip")
	require.NoError(t, err, "get to-flip before")
	var beforeLM string
	if before.LocalModifiedAt != nil {
		beforeLM = *before.LocalModifiedAt
	}

	// SQLite's strftime('now') ticks at millisecond precision.
	// Sleep a few ms so a re-set produces a strictly later value.
	// (Mirrors internal/db/signals_test.go:164.)
	time.Sleep(5 * time.Millisecond)

	// Clear the marker so the backfill runs.
	_, err = d.getWriter().Exec(
		"DELETE FROM stats WHERE key = ?",
		ClassifierHashKey,
	)
	require.NoError(t, err, "clear marker")

	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(d.getWriter())
	d.mu.Unlock()
	require.NoError(t, err, "backfill run")

	after, err := d.GetSessionFull(ctx, "to-flip")
	require.NoError(t, err, "get to-flip after")
	require.True(t, after.IsAutomated, "to-flip should be automated after backfill")
	require.NotNil(t, after.LocalModifiedAt,
		"local_modified_at not set after backfill")
	require.NotEmpty(t, *after.LocalModifiedAt,
		"local_modified_at not set after backfill")
	assert.Greater(t, *after.LocalModifiedAt, beforeLM,
		"local_modified_at not bumped")
}

// TestBackfillIsAutomatedRerunsOnHashChange verifies that a
// classifier change (here, adding a user prefix) invalidates
// the stored hash and re-runs the backfill on next open,
// without any manual marker bump.
func TestBackfillIsAutomatedRerunsOnHashChange(t *testing.T) {
	t.Cleanup(func() {
		SetUserAutomationPrefixes(nil)
		SetUserAutomationSubstrings(nil)
		SetUserAutomationExactMatches(nil)
	})
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
	require.NoError(t, err, "get essay before")
	require.False(t, pre.IsAutomated,
		"precondition: essay should be is_automated=0")

	// Add a user prefix and re-run backfill. The new hash
	// should not equal the stored hash, so the backfill
	// runs and flips is_automated to 1.
	SetUserAutomationPrefixes([]string{"You are analyzing an essay"})
	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(d.getWriter())
	d.mu.Unlock()
	require.NoError(t, err, "backfill after prefix add")

	got, err := d.GetSession(ctx, "essay")
	require.NoError(t, err, "get essay after")
	assert.True(t, got.IsAutomated,
		"essay should be is_automated=1 after user prefix added")

	// A second backfill (no further classifier change) still
	// repairs inconsistent rows; the stored hash is not a
	// substitute for row integrity.
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 0 WHERE id = 'essay'",
	)
	require.NoError(t, err, "force back to 0")
	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(d.getWriter())
	d.mu.Unlock()
	require.NoError(t, err, "second backfill")
	got, err = d.GetSession(ctx, "essay")
	require.NoError(t, err, "get essay second")
	assert.True(t, got.IsAutomated,
		"second backfill must repair stale flag when hash unchanged")
}

// TestBackfillFixesOrphanCopyClassificationGap reproduces
// the production bug where ResyncAll's orphan-copied rows kept
// stale is_automated values forever. The sequence:
//
//  1. Old DB has a single-turn roborev session marked
//     is_automated=0 (e.g. it predates a classifier change, or
//     was synced from a remote with a stale binary).
//  2. ResyncAll opens an empty temp DB. Its at-Open backfill
//     runs on zero rows and stamps the *current* classifier
//     hash, so future Opens skip backfill.
//  3. CopyOrphanedDataFrom imports the orphan row verbatim,
//     including is_automated=0.
//  4. The stamped hash still matches. A regular backfill must
//     nevertheless audit rows and repair the imported flag.
//
// This guards against treating the classifier hash as a complete
// integrity marker.
func TestBackfillFixesOrphanCopyClassificationGap(t *testing.T) {
	dir := t.TempDir()

	// 1. Old DB with a misclassified single-turn roborev session.
	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(srcPath)
	require.NoError(t, err, "Open src")
	insertSession(t, srcDB, "stale-orphan", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	_, err = srcDB.getWriter().Exec(
		"UPDATE sessions SET is_automated = 0 WHERE id = 'stale-orphan'",
	)
	require.NoError(t, err, "force orphan to is_automated=0")
	srcDB.Close()

	// 2. Fresh dst DB (Open's at-Open backfill ran on an empty
	// table and stamped the current classifier hash).
	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(dstPath)
	require.NoError(t, err, "Open dst")
	defer dstDB.Close()

	// 3. Copy orphan rows (mirrors ResyncAll line ~954).
	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err, "CopyOrphanedDataFrom")
	require.Equal(t, 1, count, "expected 1 orphan")

	ctx := context.Background()
	got, err := dstDB.GetSession(ctx, "stale-orphan")
	require.NoError(t, err, "get orphan after copy")
	require.False(t, got.IsAutomated,
		"precondition: orphan row should carry stale is_automated=0")

	// 4. A regular backfill must still repair the row even
	// though the stored classifier hash already matches.
	dstDB.mu.Lock()
	err = dstDB.backfillIsAutomatedLocked(dstDB.getWriter())
	dstDB.mu.Unlock()
	require.NoError(t, err, "backfill")
	got, err = dstDB.GetSession(ctx, "stale-orphan")
	require.NoError(t, err, "get orphan after backfill")
	assert.True(t, got.IsAutomated,
		"backfill must reclassify orphan-copied rows so they don't keep stale is_automated values")
}

// TestForceBackfillIsAutomatedRunsDespiteMatchingHash verifies
// that ForceBackfillIsAutomated reclassifies even when the
// stored hash matches the current classifier hash. This is the
// safety net ResyncAll relies on after CopyOrphanedDataFrom
// imports rows whose is_automated values were computed against
// the old DB but are now stamped under the temp DB's hash.
func TestForceBackfillIsAutomatedRunsDespiteMatchingHash(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Seed a single-turn roborev session that should be
	// is_automated = 1 under the current classifier.
	insertSession(t, d, "stuck", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})

	// Force is_automated = 0 to simulate a row imported via
	// CopyOrphanedDataFrom from a DB whose classifier set was
	// stale at the time the flag was computed.
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 0 WHERE id = 'stuck'",
	)
	require.NoError(t, err, "force stuck to 0")

	// Stamp the current hash so a *plain* backfill would
	// short-circuit (mirrors ResyncAll's temp DB state after
	// at-Open backfill ran on an empty table).
	_, err = d.getWriter().Exec(
		`INSERT INTO stats (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		ClassifierHashKey, ClassifierHash(),
	)
	require.NoError(t, err, "stamp current hash")

	// The force path must reclassify regardless of the stored
	// hash and re-stamp the current classifier hash.
	require.NoError(t, d.ForceBackfillIsAutomated(), "force backfill")

	got, err := d.GetSession(ctx, "stuck")
	require.NoError(t, err, "get stuck after force")
	assert.True(t, got.IsAutomated,
		"ForceBackfillIsAutomated must flip stuck to is_automated=1")

	// And the hash must be re-stamped after the force run so
	// subsequent Opens don't re-do the work.
	var stored string
	err = d.getWriter().QueryRow(
		`SELECT value FROM stats WHERE key = ?`,
		ClassifierHashKey,
	).Scan(&stored)
	require.NoError(t, err, "read hash after force")
	assert.Equal(t, ClassifierHash(), stored,
		"stored hash not refreshed after force")
}
