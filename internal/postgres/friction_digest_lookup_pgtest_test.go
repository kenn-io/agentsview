//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func seedDigestPG(t *testing.T, d interface {
	SaveFrictionDigest(context.Context, db.FrictionDigest, []db.FrictionDigestSubject, []db.FrictionPatternUpdate) error
}, date string, subjects []db.FrictionDigestSubject, patterns []db.FrictionPatternUpdate,
) {
	t.Helper()
	require.NoError(t, d.SaveFrictionDigest(t.Context(), db.FrictionDigest{
		Date: date, Timezone: "UTC", RulesVersion: "friction-v1", BuiltAt: time.Now().UTC(), Revision: 1,
		SnapshotJSON: []byte("{}"), SummaryJSON: []byte("{}"), Markdown: []byte(""), RunID: "run-" + date,
	}, subjects, patterns))
}

func TestFrictionDigestLookupsPG(t *testing.T) {
	d := newLedgerTestStore(t)
	seedDigestPG(t, d, "2026-09-13",
		[]db.FrictionDigestSubject{{SubjectID: "run-1:ci", Date: "2026-09-13", SubjectKind: "diagnostic"}},
		[]db.FrictionPatternUpdate{{
			Fingerprint: "fl1:aa", Kind: "error", Title: "[friction/error] ci: run-1:ci: x",
			Date: "2026-09-13", SubjectID: "run-1:ci", Occurrences: 2,
		}})

	t.Run("digested_subjects", func(t *testing.T) {
		got, err := d.FrictionDigestedSubjects(t.Context(), []string{"run-1:ci", "run-2:ci"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"run-1:ci": "2026-09-13"}, got)
		empty, err := d.FrictionDigestedSubjects(t.Context(), nil)
		require.NoError(t, err)
		assert.Empty(t, empty)
	})

	t.Run("patterns_by_fingerprint", func(t *testing.T) {
		got, err := d.FrictionPatternsByFingerprint(t.Context(), []string{"fl1:aa", "fl1:zz"})
		require.NoError(t, err)
		require.Contains(t, got, "fl1:aa")
		assert.NotContains(t, got, "fl1:zz")
		p := got["fl1:aa"]
		assert.Equal(t, "2026-09-13", p.FirstSeenDate)
		assert.Equal(t, 2, p.OccurrenceCount)
	})

	t.Run("diagnostic_fingerprint_resolves_its_digest", func(t *testing.T) {
		got, err := d.DigestDatesForFingerprints(t.Context(), []string{"fl1:aa", "fl1:missing"})
		require.NoError(t, err)
		assert.Equal(t, []string{"2026-09-13"}, got)
	})

	t.Run("chunks_large_inputs", func(t *testing.T) {
		ids := make([]string, 1200)
		for i := range ids {
			ids[i] = "missing-" + string(rune('a'+i%26)) + time.Duration(i).String()
		}
		ids[1100] = "run-1:ci"
		got, err := d.FrictionDigestedSubjects(t.Context(), ids)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"run-1:ci": "2026-09-13"}, got)
	})
}
