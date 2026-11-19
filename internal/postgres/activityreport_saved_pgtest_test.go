//go:build pgtest

package postgres

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
)

func TestPGActivityReportSavedRestart(t *testing.T) {
	for _, mode := range []string{"restart", "identity", "pricing", "candidate", "peer", "unavailable", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			cache := t.TempDir()
			t.Setenv("CACHE_DIRECTORY", cache)
			local, syncer, _ := newUsageJournalFixture(t)
			pgKeptSession(t, local, "a", "project-a", "2026-06-14")
			require.NoError(t, local.UpsertProjectIdentityObservation(t.Context(), pgKeptIdentity("a", 14)))
			if mode == "peer" {
				pgKeptSession(t, local, "peer", "project-b", "2026-06-14")
				for _, id := range []string{"a", "peer"} {
					require.NoError(t, local.ReplaceSessionMessages(t.Context(), id, []db.Message{{SessionID: id, Ordinal: 0, Role: "assistant", Timestamp: "2026-06-14T10:00:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":500}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"}}))
				}
			}
			pgKeptPush(t, syncer, false)
			target, directory := testPGURL(t), t.TempDir()
			q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
			if mode == "peer" {
				f.Project = "project-a"
			}
			first := openPGActivityInputStore(t, target, syncer.schema, directory)
			t.Cleanup(func() { require.NoError(t, first.Close()) })
			if mode == "unavailable" {
				first.pg = &activityIdentityError{storeExecutor: first.pg, denied: true}
			}
			if mode == "oversize" {
				first.runtimeState().activityKept.MaxBytes = 1
			}
			want := pgKeptBuild(t, first, f, q, true)
			files, err := filepath.Glob(filepath.Join(cache, "postgres-activity-reports", "*", "*.gob"))
			require.NoError(t, err)
			if mode == "unavailable" || mode == "oversize" {
				require.Empty(t, files)
			} else {
				require.Len(t, files, 1)
			}
			if mode == "identity" {
				f.IncludeSubagents, f.IncludeForks = true, true
				pricing, err := first.loadPricingMap(t.Context())
				require.NoError(t, err)
				digest, err := export.EffectivePricingDigest(pricing)
				require.NoError(t, err)
				version, err := first.activityKeptVersion(t.Context(), digest)
				require.NoError(t, err)
				identity, err := first.activitySourceIdentity(t.Context(), "")
				require.NoError(t, err)
				selection := kept.Selection(f, q)
				data, ok := first.activityReports.Get(selection, identity+"|"+version)
				require.True(t, ok)
				require.True(t, first.activityReports.Put(selection, "other|"+version, data))
			}
			if mode == "candidate" {
				pgKeptSession(t, local, "b", "project-a", "2026-06-14")
				pgKeptPush(t, syncer, false)
			}
			if mode == "peer" {
				require.NoError(t, local.DeleteSession(t.Context(), "peer"))
				pgKeptPush(t, syncer, true)
			}
			restarted := openPGActivityInputStore(t, target, syncer.schema, directory)
			t.Cleanup(func() { require.NoError(t, restarted.Close()) })
			reads := &activityInputReadCounter{storeExecutor: restarted.pg}
			restarted.pg = reads
			if mode == "pricing" {
				restarted.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 40_000_000}})
			}
			got := pgKeptBuild(t, restarted, f, q, mode != "restart")
			if mode == "restart" {
				requirePGKeptEqual(t, want, got)
				assert.Empty(t, reads.transcriptReads)
				assert.Empty(t, reads.usageReads)
			} else {
				assert.NotEmpty(t, reads.transcriptReads)
				assert.NotEmpty(t, reads.usageReads)
			}
		})
	}
}

func TestPGActivityReportSavedExpiryAndLimit(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("CACHE_DIRECTORY", cache)
	local, syncer, _ := newUsageJournalFixture(t)
	for day := 1; day <= 9; day++ {
		pgKeptSession(t, local, fmt.Sprintf("day-%d", day), "project-a", fmt.Sprintf("2026-06-%02d", day))
	}
	pgKeptPush(t, syncer, false)
	target, directory := testPGURL(t), t.TempDir()
	store := openPGActivityInputStore(t, target, syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	f := db.AnalyticsFilter{Timezone: "UTC"}
	for day := 1; day <= 9; day++ {
		pgKeptBuild(t, store, f, pgDayQuery(t, fmt.Sprintf("2026-06-%02d", day), "UTC"), true)
		files, err := filepath.Glob(filepath.Join(cache, "postgres-activity-reports", "*", "*.gob"))
		require.NoError(t, err)
		for _, file := range files {
			info, err := os.Stat(file)
			require.NoError(t, err)
			old := info.ModTime().Add(-time.Second)
			require.NoError(t, os.Chtimes(file, old, old))
		}
	}
	files, err := filepath.Glob(filepath.Join(cache, "postgres-activity-reports", "*", "*.gob"))
	require.NoError(t, err)
	require.Len(t, files, 8)
	path := func(day int) string {
		q := pgDayQuery(t, fmt.Sprintf("2026-06-%02d", day), "UTC")
		filter := f
		filter.IncludeSubagents, filter.IncludeForks = true, true
		hash := sha256.Sum256([]byte(kept.Selection(filter, q)))
		return filepath.Join(filepath.Dir(files[0]), fmt.Sprintf("%x.gob", hash[:16]))
	}
	_, err = os.Stat(path(1))
	require.ErrorIs(t, err, os.ErrNotExist)
	old := time.Now().Add(-31 * 24 * time.Hour)
	for _, day := range []int{2, 3, 4} {
		require.NoError(t, os.Chtimes(path(day), old, old))
	}
	pgKeptBuild(t, store, f, pgDayQuery(t, "2026-06-04", "UTC"), false)
	info, err := os.Stat(path(4))
	require.NoError(t, err)
	require.True(t, info.ModTime().After(old))
	store.runtimeState().activityKept.DeleteSlots(func(string) bool { return true })
	pgKeptBuild(t, store, f, pgDayQuery(t, "2026-06-05", "UTC"), false)
	_, err = os.Stat(path(2))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(path(3))
	require.ErrorIs(t, err, os.ErrNotExist)
	pgKeptBuild(t, store, f, pgDayQuery(t, "2026-06-02", "UTC"), true)
	pgKeptBuild(t, store, f, pgDayQuery(t, "2026-06-03", "UTC"), true)
}

func TestPGActivityReportSavedAfterLocalDayEnds(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("CACHE_DIRECTORY", cache)
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	store := openPGActivityInputStore(t, testPGURL(t), syncer.schema, t.TempDir())
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	now := time.Now().UTC()
	q, f := pgDayQuery(t, now.Format("2006-01-02"), "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	q.RangeStart, q.RangeEnd = now.Add(-2*time.Hour), now.Add(-time.Hour)
	q.EffectiveEnd = q.RangeEnd
	// Keep the selection fixed while moving its local midnight across the end.
	q.Loc = time.FixedZone("before midnight", (12-now.Hour())*60*60)
	want := pgKeptBuild(t, store, f, q, true)
	files, err := filepath.Glob(filepath.Join(cache, "postgres-activity-reports", "*", "*.gob"))
	require.NoError(t, err)
	require.Empty(t, files)
	q.Loc = time.FixedZone("after midnight", -now.Hour()*60*60)
	requirePGKeptEqual(t, want, pgKeptBuild(t, store, f, q, false))
	files, err = filepath.Glob(filepath.Join(cache, "postgres-activity-reports", "*", "*.gob"))
	require.NoError(t, err)
	require.Len(t, files, 1)
}
