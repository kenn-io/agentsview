package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listSortedIDs returns the session IDs in the order ListSessions returns them.
func listSortedIDs(t *testing.T, d *DB, f SessionFilter) []string {
	t.Helper()
	page, err := d.ListSessions(context.Background(), f)
	require.NoError(t, err, "ListSessions")
	ids := make([]string, len(page.Sessions))
	for i, s := range page.Sessions {
		ids[i] = s.ID
	}
	return ids
}

// TestListSessions_SortAscDesc covers each sort key in both directions using an
// explicit absolute Descending so the default-direction logic is exercised
// separately (TestListSessions_SortDefaultDirection).
func TestListSessions_SortAscDesc(t *testing.T) {
	cases := []struct {
		name    string
		orderBy string
		setup   func(t *testing.T, d *DB)
		// wantAsc is the order with Descending=false; desc is its reverse.
		wantAsc []string
	}{
		{
			name:    "recent",
			orderBy: "recent",
			setup: func(t *testing.T, d *DB) {
				insertSession(t, d, "r-old", "p", func(s *Session) { s.EndedAt = Ptr("2024-01-01T00:00:00Z") })
				insertSession(t, d, "r-mid", "p", func(s *Session) { s.EndedAt = Ptr("2024-02-01T00:00:00Z") })
				insertSession(t, d, "r-new", "p", func(s *Session) { s.EndedAt = Ptr("2024-03-01T00:00:00Z") })
			},
			wantAsc: []string{"r-old", "r-mid", "r-new"},
		},
		{
			name:    "started",
			orderBy: "started",
			setup: func(t *testing.T, d *DB) {
				insertSession(t, d, "s-old", "p", func(s *Session) { s.StartedAt = Ptr("2024-01-01T00:00:00Z") })
				insertSession(t, d, "s-mid", "p", func(s *Session) { s.StartedAt = Ptr("2024-02-01T00:00:00Z") })
				insertSession(t, d, "s-new", "p", func(s *Session) { s.StartedAt = Ptr("2024-03-01T00:00:00Z") })
			},
			wantAsc: []string{"s-old", "s-mid", "s-new"},
		},
		{
			name:    "messages",
			orderBy: "messages",
			setup: func(t *testing.T, d *DB) {
				insertSession(t, d, "m1", "p", func(s *Session) { s.MessageCount = 1 })
				insertSession(t, d, "m5", "p", func(s *Session) { s.MessageCount = 5 })
				insertSession(t, d, "m9", "p", func(s *Session) { s.MessageCount = 9 })
			},
			wantAsc: []string{"m1", "m5", "m9"},
		},
		{
			name:    "user-messages",
			orderBy: "user-messages",
			setup: func(t *testing.T, d *DB) {
				insertSession(t, d, "u1", "p", func(s *Session) { s.UserMessageCount = 1 })
				insertSession(t, d, "u4", "p", func(s *Session) { s.UserMessageCount = 4 })
				insertSession(t, d, "u8", "p", func(s *Session) { s.UserMessageCount = 8 })
			},
			wantAsc: []string{"u1", "u4", "u8"},
		},
		{
			name:    "output-tokens",
			orderBy: "output-tokens",
			setup: func(t *testing.T, d *DB) {
				insertSession(t, d, "o100", "p", func(s *Session) { s.TotalOutputTokens = 100 })
				insertSession(t, d, "o500", "p", func(s *Session) { s.TotalOutputTokens = 500 })
				insertSession(t, d, "o900", "p", func(s *Session) { s.TotalOutputTokens = 900 })
			},
			wantAsc: []string{"o100", "o500", "o900"},
		},
		{
			name:    "peak-context",
			orderBy: "peak-context",
			setup: func(t *testing.T, d *DB) {
				insertSession(t, d, "pc1", "p", func(s *Session) { s.PeakContextTokens = 1000 })
				insertSession(t, d, "pc2", "p", func(s *Session) { s.PeakContextTokens = 2000 })
				insertSession(t, d, "pc3", "p", func(s *Session) { s.PeakContextTokens = 3000 })
			},
			wantAsc: []string{"pc1", "pc2", "pc3"},
		},
		{
			name:    "failures",
			orderBy: "failures",
			setup: func(t *testing.T, d *DB) {
				insertSession(t, d, "f0", "p")
				insertSession(t, d, "f3", "p")
				updateSignals(t, d, "f3", SessionSignalUpdate{ToolFailureSignalCount: 3})
				insertSession(t, d, "f7", "p")
				updateSignals(t, d, "f7", SessionSignalUpdate{ToolFailureSignalCount: 7})
			},
			wantAsc: []string{"f0", "f3", "f7"},
		},
		{
			name:    "retries",
			orderBy: "retries",
			setup: func(t *testing.T, d *DB) {
				insertSession(t, d, "rt0", "p")
				insertSession(t, d, "rt2", "p")
				updateSignals(t, d, "rt2", SessionSignalUpdate{ToolRetryCount: 2})
				insertSession(t, d, "rt6", "p")
				updateSignals(t, d, "rt6", SessionSignalUpdate{ToolRetryCount: 6})
			},
			wantAsc: []string{"rt0", "rt2", "rt6"},
		},
		{
			name:    "edit-churn",
			orderBy: "edit-churn",
			setup: func(t *testing.T, d *DB) {
				insertSession(t, d, "ec0", "p")
				insertSession(t, d, "ec4", "p")
				updateSignals(t, d, "ec4", SessionSignalUpdate{EditChurnCount: 4})
				insertSession(t, d, "ec8", "p")
				updateSignals(t, d, "ec8", SessionSignalUpdate{EditChurnCount: 8})
			},
			wantAsc: []string{"ec0", "ec4", "ec8"},
		},
		{
			name:    "compactions",
			orderBy: "compactions",
			setup: func(t *testing.T, d *DB) {
				insertSession(t, d, "c0", "p")
				insertSession(t, d, "c1", "p")
				updateSignals(t, d, "c1", SessionSignalUpdate{CompactionCount: 1})
				insertSession(t, d, "c5", "p")
				updateSignals(t, d, "c5", SessionSignalUpdate{CompactionCount: 5})
			},
			wantAsc: []string{"c0", "c1", "c5"},
		},
		{
			name:    "context-pressure",
			orderBy: "context-pressure",
			setup: func(t *testing.T, d *DB) {
				insertSession(t, d, "cp2", "p")
				updateSignals(t, d, "cp2", SessionSignalUpdate{ContextPressureMax: Ptr(0.2)})
				insertSession(t, d, "cp5", "p")
				updateSignals(t, d, "cp5", SessionSignalUpdate{ContextPressureMax: Ptr(0.5)})
				insertSession(t, d, "cp9", "p")
				updateSignals(t, d, "cp9", SessionSignalUpdate{ContextPressureMax: Ptr(0.9)})
			},
			wantAsc: []string{"cp2", "cp5", "cp9"},
		},
		{
			name:    "health",
			orderBy: "health",
			setup: func(t *testing.T, d *DB) {
				insertSession(t, d, "h20", "p")
				updateSignals(t, d, "h20", SessionSignalUpdate{HealthScore: Ptr(20)})
				insertSession(t, d, "h60", "p")
				updateSignals(t, d, "h60", SessionSignalUpdate{HealthScore: Ptr(60)})
				insertSession(t, d, "h95", "p")
				updateSignals(t, d, "h95", SessionSignalUpdate{HealthScore: Ptr(95)})
			},
			wantAsc: []string{"h20", "h60", "h95"},
		},
		{
			name:    "secrets",
			orderBy: "secrets",
			setup: func(t *testing.T, d *DB) {
				insertSession(t, d, "sec0", "p")
				insertSession(t, d, "sec2", "p")
				require.NoError(t, d.ReplaceSessionSecretFindings("sec2", nil, 2, "v1"))
				insertSession(t, d, "sec5", "p")
				require.NoError(t, d.ReplaceSessionSecretFindings("sec5", nil, 5, "v1"))
			},
			wantAsc: []string{"sec0", "sec2", "sec5"},
		},
		{
			name:    "id",
			orderBy: "id",
			setup: func(t *testing.T, d *DB) {
				insertSession(t, d, "id-a", "p")
				insertSession(t, d, "id-b", "p")
				insertSession(t, d, "id-c", "p")
			},
			wantAsc: []string{"id-a", "id-b", "id-c"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			tc.setup(t, d)

			gotAsc := listSortedIDs(t, d, filterWith(func(f *SessionFilter) {
				f.OrderBy = tc.orderBy
				f.Descending = Ptr(false)
			}))
			assert.Equal(t, tc.wantAsc, gotAsc, "ascending order")

			wantDesc := reversed(tc.wantAsc)
			gotDesc := listSortedIDs(t, d, filterWith(func(f *SessionFilter) {
				f.OrderBy = tc.orderBy
				f.Descending = Ptr(true)
			}))
			assert.Equal(t, wantDesc, gotDesc, "descending order")
		})
	}
}

func reversed(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}

// TestListSessions_SortDefaultDirection verifies OrderBy with a nil Descending
// uses the sort key's canonical default: recent is descending (most recent
// first, the historical behavior), every other key is ascending.
func TestListSessions_SortDefaultDirection(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "a", "p", func(s *Session) {
		s.EndedAt = Ptr("2024-01-01T00:00:00Z")
		s.MessageCount = 1
	})
	insertSession(t, d, "b", "p", func(s *Session) {
		s.EndedAt = Ptr("2024-02-01T00:00:00Z")
		s.MessageCount = 9
	})

	// recent default: newest first.
	assert.Equal(t, []string{"b", "a"},
		listSortedIDs(t, d, filterWith(func(f *SessionFilter) { f.OrderBy = "recent" })))
	// empty OrderBy behaves like recent.
	assert.Equal(t, []string{"b", "a"},
		listSortedIDs(t, d, filterWith(func(f *SessionFilter) {})))
	// messages default: ascending (fewest first).
	assert.Equal(t, []string{"a", "b"},
		listSortedIDs(t, d, filterWith(func(f *SessionFilter) { f.OrderBy = "messages" })))
}

// TestListSessions_SortPaginationWalk paginates through a non-default sort and
// asserts the full ordered set is returned exactly once.
func TestListSessions_SortPaginationWalk(t *testing.T) {
	d := testDB(t)
	const n = 7
	for i := 1; i <= n; i++ {
		insertSession(t, d, fmt.Sprintf("p%02d", i), "p", func(s *Session) {
			s.MessageCount = i * 3
		})
	}

	for _, desc := range []bool{false, true} {
		t.Run(fmt.Sprintf("desc=%v", desc), func(t *testing.T) {
			var got []string
			seen := map[string]bool{}
			cursor := ""
			for pages := 0; ; pages++ {
				require.LessOrEqual(t, pages, n+1, "pagination did not terminate")
				page, err := d.ListSessions(context.Background(), SessionFilter{
					Limit:      2,
					OrderBy:    "messages",
					Descending: Ptr(desc),
					Cursor:     cursor,
				})
				require.NoError(t, err, "ListSessions page")
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

			want := make([]string, n)
			for i := 1; i <= n; i++ {
				want[i-1] = fmt.Sprintf("p%02d", i)
			}
			if desc {
				want = reversed(want)
			}
			assert.Equal(t, want, got, "walked order")
		})
	}
}

// TestListSessions_SortNullsLast verifies nullable sorts (health) place NULL
// rows last in both directions and that keyset pagination crosses the NULL
// boundary without dropping or duplicating rows.
func TestListSessions_SortNullsLast(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "h10", "p")
	updateSignals(t, d, "h10", SessionSignalUpdate{HealthScore: Ptr(10)})
	insertSession(t, d, "h90", "p")
	updateSignals(t, d, "h90", SessionSignalUpdate{HealthScore: Ptr(90)})
	insertSession(t, d, "hnull-a", "p") // health_score NULL
	insertSession(t, d, "hnull-b", "p") // health_score NULL

	// Ascending: non-null ascending, NULLs last (id ascending among NULLs).
	assert.Equal(t, []string{"h10", "h90", "hnull-a", "hnull-b"},
		listSortedIDs(t, d, filterWith(func(f *SessionFilter) {
			f.OrderBy = "health"
			f.Descending = Ptr(false)
		})))
	// Descending: non-null descending, NULLs still last (id descending among NULLs).
	assert.Equal(t, []string{"h90", "h10", "hnull-b", "hnull-a"},
		listSortedIDs(t, d, filterWith(func(f *SessionFilter) {
			f.OrderBy = "health"
			f.Descending = Ptr(true)
		})))

	// Paginate one row at a time to exercise the sentinel keyset across the
	// null boundary.
	var got []string
	cursor := ""
	for {
		page, err := d.ListSessions(context.Background(), SessionFilter{
			Limit: 1, OrderBy: "health", Descending: Ptr(false), Cursor: cursor,
		})
		require.NoError(t, err)
		for _, s := range page.Sessions {
			got = append(got, s.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	assert.Equal(t, []string{"h10", "h90", "hnull-a", "hnull-b"}, got, "paginated null walk")
}

// TestListSessions_CursorSortMismatch rejects a cursor reused under a different
// sort or direction, while accepting it under the same ordering.
func TestListSessions_CursorSortMismatch(t *testing.T) {
	d := testDB(t)
	for i := 1; i <= 4; i++ {
		insertSession(t, d, fmt.Sprintf("x%d", i), "p", func(s *Session) {
			s.MessageCount = i
		})
	}

	page, err := d.ListSessions(context.Background(), SessionFilter{
		Limit: 2, OrderBy: "messages", Descending: Ptr(false),
	})
	require.NoError(t, err)
	require.NotEmpty(t, page.NextCursor, "expected a next cursor")
	cursor := page.NextCursor

	// Same sort + direction: accepted.
	_, err = d.ListSessions(context.Background(), SessionFilter{
		Limit: 2, OrderBy: "messages", Descending: Ptr(false), Cursor: cursor,
	})
	require.NoError(t, err, "same sort should accept cursor")

	// Different sort key: rejected.
	_, err = d.ListSessions(context.Background(), SessionFilter{
		Limit: 2, OrderBy: "failures", Descending: Ptr(false), Cursor: cursor,
	})
	require.ErrorIs(t, err, ErrInvalidCursor, "cross-sort cursor")

	// Same sort, flipped direction: rejected.
	_, err = d.ListSessions(context.Background(), SessionFilter{
		Limit: 2, OrderBy: "messages", Descending: Ptr(true), Cursor: cursor,
	})
	require.ErrorIs(t, err, ErrInvalidCursor, "flipped-direction cursor")
}

// TestListSessions_LegacyCursorRecent confirms a cursor minted in the old shape
// (only EndedAt/ID/Total, no Sort/Desc/Value) still paginates the recent order.
// This exercises the cur.Sort == "" backward-compatibility path directly, rather
// than a cursor produced by the current implementation.
func TestListSessions_LegacyCursorRecent(t *testing.T) {
	d := testDB(t)
	for i := 1; i <= 4; i++ {
		insertSession(t, d, fmt.Sprintf("rc%d", i), "p", func(s *Session) {
			s.EndedAt = Ptr(fmt.Sprintf("2024-0%d-01T00:00:00Z", i))
		})
	}
	page1, err := d.ListSessions(context.Background(), SessionFilter{Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []string{"rc4", "rc3"}, idsOf(page1.Sessions))

	// Mint a legacy-shaped cursor by hand: no Sort/Desc/Value fields, just the
	// activity timestamp + id + total a pre-sort build would have produced.
	legacy := d.EncodeCursor(SessionCursor{
		EndedAt: "2024-03-01T00:00:00Z",
		ID:      "rc3",
		Total:   page1.Total,
	})
	cur, err := d.DecodeCursor(legacy)
	require.NoError(t, err)
	require.Empty(t, cur.Sort, "legacy cursor must carry no sort key")

	page2, err := d.ListSessions(context.Background(), SessionFilter{
		Limit: 2, Cursor: legacy,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"rc2", "rc1"}, idsOf(page2.Sessions))
}

func idsOf(sessions []Session) []string {
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.ID
	}
	return ids
}

// TestListSessions_SecretsSortVersionGated checks that the secrets sort matches
// the displayed (version-gated) count, and that gating flows through the keyset
// cursor on a paginated request.
func TestListSessions_SecretsSortVersionGated(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "sec-cur", "p")
	require.NoError(t, d.ReplaceSessionSecretFindings("sec-cur", nil, 5, "v1"))
	insertSession(t, d, "sec-stale", "p")
	require.NoError(t, d.ReplaceSessionSecretFindings("sec-stale", nil, 9, "old"))
	insertSession(t, d, "sec-none", "p")

	// With active version v1: the stale count (9) gates to 0, so the
	// current-version session (5) ranks first descending.
	gated := listSortedIDs(t, d, filterWith(func(f *SessionFilter) {
		f.OrderBy = "secrets"
		f.Descending = Ptr(true)
		f.SecretsRulesVersions = []string{"v1"}
	}))
	require.Len(t, gated, 3)
	assert.Equal(t, "sec-cur", gated[0], "current-version session ranks first")

	// Without active versions: raw counts, so the stale 9 ranks first.
	raw := listSortedIDs(t, d, filterWith(func(f *SessionFilter) {
		f.OrderBy = "secrets"
		f.Descending = Ptr(true)
	}))
	assert.Equal(t, "sec-stale", raw[0], "raw counts rank stale 9 first")

	// Paginated, gated: the cursor must carry the gated value so the
	// current-version session still leads and pages do not duplicate.
	var got []string
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := d.ListSessions(context.Background(), SessionFilter{
			Limit:                1,
			OrderBy:              "secrets",
			Descending:           Ptr(true),
			SecretsRulesVersions: []string{"v1"},
			Cursor:               cursor,
		})
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
	require.Len(t, got, 3)
	assert.Equal(t, "sec-cur", got[0], "paginated gated order leads with current-version session")
}

func TestValidSortKeyAndDefaultDirection(t *testing.T) {
	assert.True(t, ValidSortKey(""), "empty key is valid (default)")
	assert.True(t, ValidSortKey("failures"))
	assert.False(t, ValidSortKey("bogus"))

	assert.True(t, SortDefaultDescending("recent"), "recent defaults descending")
	assert.True(t, SortDefaultDescending(""), "empty key defaults to recent (descending)")
	assert.False(t, SortDefaultDescending("messages"), "non-recent keys default ascending")
}
