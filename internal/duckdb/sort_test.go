//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// syncedStoreFromWrites seeds a local SQLite DB with the given batch writes and
// returns a DuckDB store mirroring it, so sort/keyset SQL can be exercised
// against the DuckDB dialect (CAST placeholders, COALESCE sentinel).
func syncedStoreFromWrites(t *testing.T, writes []db.SessionBatchWrite) *Store {
	t.Helper()
	ctx := context.Background()
	local := newLocalDB(t)
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	return NewStoreFromDB(syncer.DB())
}

func sortSeedSession(id, project, ts string, signals db.SessionSignalUpdate) db.SessionBatchWrite {
	return db.SessionBatchWrite{
		Session: db.Session{
			ID:               id,
			Project:          project,
			Machine:          "test-machine",
			Agent:            "claude",
			StartedAt:        new(ts),
			EndedAt:          new(ts),
			MessageCount:     2,
			UserMessageCount: 2,
			RelationshipType: "root",
		},
		Signals:         signals,
		DataVersion:     1,
		ReplaceMessages: true,
	}
}

func duckWalk(t *testing.T, store *Store, f db.SessionFilter) []string {
	t.Helper()
	ctx := context.Background()
	var got []string
	seen := map[string]bool{}
	cursor := ""
	for {
		f.Cursor = cursor
		page, err := store.ListSessions(ctx, f)
		require.NoError(t, err)
		for _, s := range page.Sessions {
			require.False(t, seen[s.ID], "duplicate %s", s.ID)
			seen[s.ID] = true
			got = append(got, s.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return got
}

// TestDuckDBSort_SecretsVersioned mirrors the SQLite secrets-sort behavior on
// DuckDB, including the gating CASE inside ORDER BY and the keyset cursor.
func TestDuckDBSort_SecretsVersioned(t *testing.T) {
	store := syncedStoreFromWrites(t, []db.SessionBatchWrite{
		sortSeedSession("sec-cur", "sec", "2026-03-01T00:00:00Z", db.SessionSignalUpdate{
			SecretLeakCount: 5, SecretsRulesVersion: "v1",
		}),
		sortSeedSession("sec-stale", "sec", "2026-03-02T00:00:00Z", db.SessionSignalUpdate{
			SecretLeakCount: 9, SecretsRulesVersion: "old",
		}),
		sortSeedSession("sec-none", "sec", "2026-03-03T00:00:00Z", db.SessionSignalUpdate{}),
	})

	desc := true
	walked := duckWalk(t, store, db.SessionFilter{
		Project:              "sec",
		OrderBy:              "secrets",
		Descending:           &desc,
		SecretsRulesVersions: []string{"v1"},
		Limit:                1,
	})
	require.Len(t, walked, 3)
	require.Equal(t, "sec-cur", walked[0],
		"current-version session leads once the stale 9 is gated to 0")
}

func multiKeySeed(id string, msgs int, started string) db.SessionBatchWrite {
	return db.SessionBatchWrite{
		Session: db.Session{
			ID:               id,
			Project:          "mk",
			Machine:          "test-machine",
			Agent:            "claude",
			StartedAt:        new(started),
			MessageCount:     msgs,
			UserMessageCount: 2,
			RelationshipType: "root",
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}
}

// TestDuckDBSort_MultiKey mirrors the SQLite mixed-direction multi-key behavior
// on DuckDB (CAST cursor placeholders, lexicographic OR-expansion): messages
// ascending, then started descending, with the id tie-break following the last
// term's direction. The paginated walk must equal the full-listing order.
func TestDuckDBSort_MultiKey(t *testing.T) {
	store := syncedStoreFromWrites(t, []db.SessionBatchWrite{
		multiKeySeed("mk-a", 1, "2024-03-01T00:00:00Z"),
		multiKeySeed("mk-b", 1, "2024-01-01T00:00:00Z"),
		multiKeySeed("mk-c", 2, "2024-02-01T00:00:00Z"),
		multiKeySeed("mk-d", 2, "2024-05-01T00:00:00Z"),
		multiKeySeed("mk-e", 1, "2024-03-01T00:00:00Z"), // ties mk-a
	})
	asc, desc := false, true
	sortKeys := []db.SortKey{
		{Key: "messages", Descending: &asc},
		{Key: "started", Descending: &desc},
	}

	full, err := store.ListSessions(context.Background(), db.SessionFilter{
		Project: "mk", Sort: sortKeys, Limit: 100,
	})
	require.NoError(t, err)
	want := make([]string, len(full.Sessions))
	for i, s := range full.Sessions {
		want[i] = s.ID
	}
	require.Equal(t, []string{"mk-e", "mk-a", "mk-b", "mk-d", "mk-c"}, want)

	walked := duckWalk(t, store, db.SessionFilter{Project: "mk", Sort: sortKeys, Limit: 1})
	require.Equal(t, want, walked, "paginated walk matches full listing")
}

// TestDuckDBSort_NullsLast mirrors the nullable-sort (health) behavior on
// DuckDB: NULLs sort last and pagination crosses the sentinel boundary.
func TestDuckDBSort_NullsLast(t *testing.T) {
	store := syncedStoreFromWrites(t, []db.SessionBatchWrite{
		sortSeedSession("h20", "health", "2026-03-01T00:00:00Z", db.SessionSignalUpdate{
			HealthScore: new(20),
		}),
		sortSeedSession("h80", "health", "2026-03-02T00:00:00Z", db.SessionSignalUpdate{
			HealthScore: new(80),
		}),
		sortSeedSession("hnull", "health", "2026-03-03T00:00:00Z", db.SessionSignalUpdate{}),
	})

	walked := duckWalk(t, store, db.SessionFilter{
		Project: "health", OrderBy: "health", Limit: 1,
	})
	require.Equal(t, []string{"h20", "h80", "hnull"}, walked)
}
