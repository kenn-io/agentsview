package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSortSpec(t *testing.T) {
	asc, desc := false, true
	cases := []struct {
		name    string
		spec    string
		want    []SortKey
		wantErr string
	}{
		{name: "empty yields no terms", spec: "", want: nil},
		{name: "whitespace only", spec: "  ", want: nil},
		{
			name: "single bare key",
			spec: "messages",
			want: []SortKey{{Key: "messages"}},
		},
		{
			name: "single key with desc",
			spec: "messages:desc",
			want: []SortKey{{Key: "messages", Descending: &desc}},
		},
		{
			name: "multi key mixed directions",
			spec: "messages:desc,started:asc",
			want: []SortKey{
				{Key: "messages", Descending: &desc},
				{Key: "started", Descending: &asc},
			},
		},
		{
			name: "mixed suffixed and bare with spaces",
			spec: " messages , started:asc ",
			want: []SortKey{
				{Key: "messages"},
				{Key: "started", Descending: &asc},
			},
		},
		{name: "unknown key", spec: "bogus", wantErr: "unknown sort key"},
		{name: "unknown key in list", spec: "messages,bogus", wantErr: "unknown sort key"},
		{name: "bad direction", spec: "messages:up", wantErr: "invalid sort direction"},
		{name: "duplicate key", spec: "messages,messages:asc", wantErr: "duplicate sort key"},
		{name: "empty term", spec: "messages,,started", wantErr: "empty sort term"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSortSpec(tc.spec)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestFormatSortSpec(t *testing.T) {
	asc, desc := false, true
	got := FormatSortSpec([]SortKey{
		{Key: "recent"},
		{Key: "messages", Descending: &desc},
		{Key: "started", Descending: &asc},
	})
	assert.Equal(t, "recent,messages:desc,started:asc", got)

	// ParseSortSpec(FormatSortSpec(x)) round-trips a parsed spec.
	orig := "messages:desc,started:asc,id"
	parsed, err := ParseSortSpec(orig)
	require.NoError(t, err)
	assert.Equal(t, orig, FormatSortSpec(parsed))
}

func TestApplyFallbackDirection(t *testing.T) {
	asc, desc := false, true

	// nil fallback leaves terms untouched.
	in := []SortKey{{Key: "messages"}, {Key: "started", Descending: &asc}}
	assert.Equal(t, in, ApplyFallbackDirection(in, nil))

	// A fallback fills only the terms without an explicit direction.
	out := ApplyFallbackDirection(
		[]SortKey{{Key: "messages"}, {Key: "started", Descending: &asc}},
		&desc,
	)
	require.Len(t, out, 2)
	require.NotNil(t, out[0].Descending)
	assert.True(t, *out[0].Descending, "bare term takes the fallback")
	require.NotNil(t, out[1].Descending)
	assert.False(t, *out[1].Descending, "explicit term is untouched")

	// The input slice is not mutated.
	assert.Nil(t, in[0].Descending)
}

func TestResolveSort(t *testing.T) {
	asc, desc := false, true
	keys := func(rs []ResolvedSort) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			d := "asc"
			if r.Desc {
				d = "desc"
			}
			out[i] = r.Sort.key + ":" + d
		}
		return out
	}

	t.Run("empty defaults to recent desc", func(t *testing.T) {
		assert.Equal(t, []string{"recent:desc"}, keys(ResolveSort(SessionFilter{})))
	})

	t.Run("structured Sort wins over OrderBy", func(t *testing.T) {
		rs := ResolveSort(SessionFilter{
			Sort:    []SortKey{{Key: "messages", Descending: &desc}},
			OrderBy: "started",
		})
		assert.Equal(t, []string{"messages:desc"}, keys(rs))
	})

	t.Run("structured multi-key with per-key direction", func(t *testing.T) {
		rs := ResolveSort(SessionFilter{Sort: []SortKey{
			{Key: "messages", Descending: &desc},
			{Key: "started", Descending: &asc},
		}})
		assert.Equal(t, []string{"messages:desc", "started:asc"}, keys(rs))
	})

	t.Run("bare term uses canonical default", func(t *testing.T) {
		rs := ResolveSort(SessionFilter{Sort: []SortKey{
			{Key: "recent"},   // canonical desc
			{Key: "messages"}, // canonical asc
		}})
		assert.Equal(t, []string{"recent:desc", "messages:asc"}, keys(rs))
	})

	t.Run("legacy OrderBy string + Descending fallback", func(t *testing.T) {
		rs := ResolveSort(SessionFilter{
			OrderBy:    "messages,started:asc",
			Descending: &desc,
		})
		// messages is bare -> fallback desc; started is explicit asc.
		assert.Equal(t, []string{"messages:desc", "started:asc"}, keys(rs))
	})

	t.Run("unknown and duplicate keys dropped defensively", func(t *testing.T) {
		rs := ResolveSort(SessionFilter{Sort: []SortKey{
			{Key: "messages"},
			{Key: "bogus"},
			{Key: "messages", Descending: &desc},
		}})
		assert.Equal(t, []string{"messages:asc"}, keys(rs))
	})

	t.Run("all-invalid falls back to default", func(t *testing.T) {
		rs := ResolveSort(SessionFilter{Sort: []SortKey{{Key: "bogus"}}})
		assert.Equal(t, []string{"recent:desc"}, keys(rs))
	})

	t.Run("legacy descending applies to implicit default recent", func(t *testing.T) {
		// descending=false with no order_by means recent ascending (the
		// single-key behavior before multi-key sorting).
		rs := ResolveSort(SessionFilter{Descending: &asc})
		assert.Equal(t, []string{"recent:asc"}, keys(rs))
		rs = ResolveSort(SessionFilter{Descending: &desc})
		assert.Equal(t, []string{"recent:desc"}, keys(rs))
	})
}

// TestListSessions_MultiKeySort verifies a mixed-direction multi-key sort
// (messages ascending, then started descending) orders rows correctly, with the
// id tie-breaker following the last term's direction. Both the structured Sort
// field and the OrderBy string spell out the same ordering and must agree.
func TestListSessions_MultiKeySort(t *testing.T) {
	asc, desc := false, true
	seed := func(t *testing.T) *DB {
		d := testDB(t)
		insertSession(t, d, "mk-a", "p", func(s *Session) {
			s.MessageCount = 1
			s.StartedAt = Ptr("2024-03-01T00:00:00Z")
		})
		insertSession(t, d, "mk-b", "p", func(s *Session) {
			s.MessageCount = 1
			s.StartedAt = Ptr("2024-01-01T00:00:00Z")
		})
		insertSession(t, d, "mk-c", "p", func(s *Session) {
			s.MessageCount = 2
			s.StartedAt = Ptr("2024-02-01T00:00:00Z")
		})
		insertSession(t, d, "mk-d", "p", func(s *Session) {
			s.MessageCount = 2
			s.StartedAt = Ptr("2024-05-01T00:00:00Z")
		})
		insertSession(t, d, "mk-e", "p", func(s *Session) {
			s.MessageCount = 1
			s.StartedAt = Ptr("2024-03-01T00:00:00Z") // ties mk-a on (messages, started)
		})
		return d
	}

	// messages asc, started desc, id desc (tie-breaker follows last term):
	//   msgs=1: started desc -> {a,e}(03), b(01); a/e tie -> id desc -> e,a
	//   msgs=2: started desc -> d(05), c(02)
	want := []string{"mk-e", "mk-a", "mk-b", "mk-d", "mk-c"}

	t.Run("structured Sort", func(t *testing.T) {
		d := seed(t)
		got := listSortedIDs(t, d, filterWith(func(f *SessionFilter) {
			f.Sort = []SortKey{
				{Key: "messages", Descending: &asc},
				{Key: "started", Descending: &desc},
			}
		}))
		assert.Equal(t, want, got)
	})

	t.Run("OrderBy string", func(t *testing.T) {
		d := seed(t)
		got := listSortedIDs(t, d, filterWith(func(f *SessionFilter) {
			f.OrderBy = "messages:asc,started:desc"
		}))
		assert.Equal(t, want, got)
	})
}

// TestListSessions_MultiKeyPaginationWalk is the core keyset-correctness test:
// paginating one row at a time through a mixed-direction multi-key sort with
// ties must return the full ordered set exactly once.
func TestListSessions_MultiKeyPaginationWalk(t *testing.T) {
	asc, desc := false, true
	d := testDB(t)
	// 12 sessions across 3 message buckets with repeated started timestamps so
	// every keyset level (value compare, equality + next compare, id tie-break)
	// is exercised during the walk.
	type row struct {
		id      string
		msgs    int
		started string
	}
	rows := []row{
		{"w01", 1, "2024-01-01T00:00:00Z"},
		{"w02", 1, "2024-01-01T00:00:00Z"},
		{"w03", 1, "2024-02-01T00:00:00Z"},
		{"w04", 2, "2024-01-01T00:00:00Z"},
		{"w05", 2, "2024-03-01T00:00:00Z"},
		{"w06", 2, "2024-03-01T00:00:00Z"},
		{"w07", 2, "2024-03-01T00:00:00Z"},
		{"w08", 3, "2024-02-01T00:00:00Z"},
		{"w09", 3, "2024-02-01T00:00:00Z"},
		{"w10", 3, "2024-04-01T00:00:00Z"},
		{"w11", 3, "2024-01-01T00:00:00Z"},
		{"w12", 1, "2024-02-01T00:00:00Z"},
	}
	for _, r := range rows {
		insertSession(t, d, r.id, "p", func(s *Session) {
			s.MessageCount = r.msgs
			s.StartedAt = Ptr(r.started)
		})
	}

	sortKeys := []SortKey{
		{Key: "messages", Descending: &asc},
		{Key: "started", Descending: &desc},
	}

	// Full-listing order is the reference; the paginated walk must reproduce it.
	want := listSortedIDs(t, d, filterWith(func(f *SessionFilter) { f.Sort = sortKeys }))
	require.Len(t, want, len(rows))

	var got []string
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		require.LessOrEqual(t, pages, len(rows)+1, "pagination did not terminate")
		page, err := d.ListSessions(context.Background(), SessionFilter{
			Limit:  1,
			Sort:   sortKeys,
			Cursor: cursor,
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
	assert.Equal(t, want, got, "paginated walk matches full listing")
}

// TestListSessions_MultiKeyCursorMismatch rejects a multi-key cursor reused under
// any different sort list (fewer keys, different order, or a flipped direction)
// and accepts it under the identical list.
func TestListSessions_MultiKeyCursorMismatch(t *testing.T) {
	asc, desc := false, true
	d := testDB(t)
	for i := 1; i <= 4; i++ {
		insertSession(t, d, fmt.Sprintf("mm%d", i), "p", func(s *Session) {
			s.MessageCount = i
			s.StartedAt = Ptr(fmt.Sprintf("2024-0%d-01T00:00:00Z", i))
		})
	}
	base := []SortKey{
		{Key: "messages", Descending: &asc},
		{Key: "started", Descending: &desc},
	}
	page, err := d.ListSessions(context.Background(), SessionFilter{Limit: 2, Sort: base})
	require.NoError(t, err)
	require.NotEmpty(t, page.NextCursor)
	cursor := page.NextCursor

	cases := []struct {
		name string
		sort []SortKey
		ok   bool
	}{
		{name: "identical", sort: base, ok: true},
		{name: "fewer keys", sort: []SortKey{{Key: "messages", Descending: &asc}}},
		{name: "swapped order", sort: []SortKey{
			{Key: "started", Descending: &desc},
			{Key: "messages", Descending: &asc},
		}},
		{name: "flipped direction", sort: []SortKey{
			{Key: "messages", Descending: &asc},
			{Key: "started", Descending: &asc},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.ListSessions(context.Background(), SessionFilter{
				Limit: 2, Sort: tc.sort, Cursor: cursor,
			})
			if tc.ok {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrInvalidCursor)
		})
	}
}

// TestNextSessionCursor_LegacyFields confirms a single-key cursor still
// populates the legacy Sort/Desc/Value (and EndedAt for recent) fields for
// pre-multi-key readers, while a multi-key cursor populates only Keys.
func TestNextSessionCursor_LegacyFields(t *testing.T) {
	last := &Session{ID: "z", EndedAt: Ptr("2024-05-01T00:00:00Z"), MessageCount: 7}

	t.Run("single key recent populates legacy + EndedAt", func(t *testing.T) {
		rs := ResolveSort(SessionFilter{}) // recent desc
		cur := NextSessionCursor(last, rs, 3, SessionFilter{})
		require.Len(t, cur.Keys, 1)
		assert.Equal(t, "recent", cur.Keys[0].Sort)
		assert.Equal(t, "recent", cur.Sort)
		assert.True(t, cur.Desc)
		assert.Equal(t, cur.Keys[0].Value, cur.Value)
		assert.Equal(t, cur.Keys[0].Value, cur.EndedAt, "recent populates legacy EndedAt")
	})

	t.Run("single key non-recent leaves EndedAt empty", func(t *testing.T) {
		asc := false
		rs := ResolveSort(SessionFilter{Sort: []SortKey{{Key: "messages", Descending: &asc}}})
		cur := NextSessionCursor(last, rs, 3, SessionFilter{})
		require.Len(t, cur.Keys, 1)
		assert.Equal(t, "messages", cur.Sort)
		assert.Equal(t, "7", cur.Value)
		assert.Empty(t, cur.EndedAt, "non-recent single key does not set EndedAt")
	})

	t.Run("multi key populates only Keys", func(t *testing.T) {
		asc, desc := false, true
		rs := ResolveSort(SessionFilter{Sort: []SortKey{
			{Key: "messages", Descending: &asc},
			{Key: "recent", Descending: &desc},
		}})
		cur := NextSessionCursor(last, rs, 3, SessionFilter{})
		require.Len(t, cur.Keys, 2)
		assert.Empty(t, cur.Sort, "multi-key leaves legacy Sort empty")
		assert.Empty(t, cur.Value)
		assert.Empty(t, cur.EndedAt)
	})
}
