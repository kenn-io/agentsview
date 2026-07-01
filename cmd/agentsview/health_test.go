package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestGradeCell(t *testing.T) {
	a := "A"
	tests := []struct {
		name string
		in   *string
		want string
	}{
		{"nil grade renders dash", nil, "-"},
		{"empty grade renders dash", new(""), "-"},
		{"grade preserved", &a, "A"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, gradeCell(tc.in))
		})
	}
}

func TestFormatPressure(t *testing.T) {
	half := 0.5
	tests := []struct {
		name string
		in   *float64
		want string
	}{
		{"nil renders dash", nil, "-"},
		{"50% rounds correctly", &half, "50%"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, formatPressure(tc.in))
		})
	}
}

func TestFormatScore(t *testing.T) {
	score := 87
	assert.Empty(t, formatScore(nil), "nil score should be empty")
	assert.Equal(t, " (score 87)", formatScore(&score))
}

func TestFormatConfidence(t *testing.T) {
	tests := []struct {
		name      string
		conf      string
		endedWith string
		want      string
	}{
		{"both empty returns empty", "", "", ""},
		{
			name: "confidence only",
			conf: "high",
			want: " (high confidence)",
		},
		{
			name:      "ended-with only",
			endedWith: "user",
			want:      " (ended with user)",
		},
		{
			name:      "both joined",
			conf:      "low",
			endedWith: "assistant",
			want:      " (low confidence, ended with assistant)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, formatConfidence(tc.conf, tc.endedWith))
		})
	}
}

func TestShortDate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty renders dash", "", "-"},
		{
			name: "RFC3339Nano parsed",
			in:   "2026-04-15T20:48:24.123Z",
			want: parseLocalDate(t, "2026-04-15T20:48:24.123Z"),
		},
		{
			name: "garbage passes through",
			in:   "not-a-date",
			want: "not-a-date",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shortDate(tc.in),
				"shortDate(%q)", tc.in)
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"under limit unchanged", "hello", 10, "hello"},
		{"exact length unchanged", "hello", 5, "hello"},
		{"over limit ellipsized", "hello world", 5, "hell…"},
		{"single char limit", "abc", 1, "a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, truncate(tc.in, tc.n),
				"truncate(%q, %d)", tc.in, tc.n)
		})
	}
}

func TestShortID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain uuid trimmed", "abcdef1234567890", "abcdef12"},
		{"prefixed id stripped", "host~abcdef12345", "abcdef12"},
		{"short id preserved", "abc", "abc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shortID(tc.in),
				"shortID(%q)", tc.in)
		})
	}
}

func TestPrintHealthList(t *testing.T) {
	a := "A"
	d := "D"
	pressure := 0.62
	sessions := []db.Session{
		{
			ID:                 "abc12345-6789-0000",
			Project:            "agentsview",
			Agent:              "claude",
			MessageCount:       42,
			FinalFailureStreak: 0,
			Outcome:            "success",
			HealthGrade:        &a,
			ContextPressureMax: &pressure,
			EndedAt:            new("2026-04-15T20:48:24Z"),
		},
		{
			ID:                 "def67890",
			Project:            "roborev",
			Agent:              "codex",
			MessageCount:       18,
			FinalFailureStreak: 3,
			Outcome:            "failed",
			HealthGrade:        &d,
		},
	}

	var buf bytes.Buffer
	printHealthList(&buf, sessions)

	out := buf.String()
	for _, want := range []string{
		"DATE", "AGENT", "GRADE", "OUTCOME",
		"agentsview", "claude", "A", "success",
		"roborev", "codex", "D", "failed",
		"abc12345", "def67890",
	} {
		assert.Contains(t, out, want, "output missing %q", want)
	}
}

func TestPrintHealthDetail(t *testing.T) {
	a := "A"
	score := 92
	pressure := 0.45
	sess := db.Session{
		ID:                     "abc12345",
		Project:                "agentsview",
		Agent:                  "claude",
		StartedAt:              new("2026-04-15T20:48:24Z"),
		EndedAt:                new("2026-04-15T21:30:00Z"),
		MessageCount:           42,
		UserMessageCount:       12,
		HealthGrade:            &a,
		HealthScore:            &score,
		Outcome:                "success",
		OutcomeConfidence:      "high",
		EndedWithRole:          "assistant",
		ToolFailureSignalCount: 1,
		ToolRetryCount:         2,
		EditChurnCount:         3,
		ConsecutiveFailureMax:  4,
		FinalFailureStreak:     0,
		CompactionCount:        1,
		ContextPressureMax:     &pressure,
		GitBranch:              "main",
		SecretLeakCount:        5,
	}

	var buf bytes.Buffer
	printHealthDetail(&buf, sess)
	out := buf.String()

	for _, want := range []string{
		"Session:  abc12345",
		"Project:  agentsview",
		"Branch:   main",
		"Messages: 42 (12 user)",
		"Grade:   A (score 92)",
		"Outcome: success (high confidence, ended with assistant)",
		"Tool failures:        1",
		"Tool retries:         2",
		"Edit churn:           3",
		"Consecutive fails:    4",
		"Secret findings:      5",
		"Compactions:          1",
		"Context pressure:     45%",
	} {
		assert.Contains(t, out, want, "output missing %q", want)
	}
}

func TestHealthListFilterIncludesAllSessions(t *testing.T) {
	got := healthListFilter(7)

	assert.Equal(t, 7, got.Limit)
	assert.True(t, got.IncludeOneShot)
	assert.True(t, got.IncludeAutomated)
}

func TestResolveHealthSessionIDMatchesDisplayedShortID(t *testing.T) {
	database := dbtest.OpenTestDB(t)

	require.NoError(t, database.UpsertSession(db.Session{
		ID: "abcdef1234567890", Project: "p", Machine: "m",
		Agent: "claude", MessageCount: 1,
	}), "upsert one-shot session")

	got, err := resolveHealthSessionID(
		context.Background(),
		service.NewDirectBackend(database, nil),
		"abcdef12",
	)

	require.NoError(t, err)
	assert.Equal(t, "abcdef1234567890", got)
}

func TestResolveHealthSessionIDExactMatchCanBeOutsideHealthList(t *testing.T) {
	database := dbtest.OpenTestDB(t)

	parentID := "parent-session"
	require.NoError(t, database.UpsertSession(db.Session{
		ID: parentID, Project: "p", Machine: "m", Agent: "claude",
		MessageCount: 2, UserMessageCount: 2,
	}), "upsert parent session")
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "child-session", Project: "p", Machine: "m", Agent: "codex",
		MessageCount: 2, UserMessageCount: 2,
		ParentSessionID: &parentID, RelationshipType: "subagent",
	}), "upsert child session")

	got, err := resolveHealthSessionID(
		context.Background(),
		service.NewDirectBackend(database, nil),
		"child-session",
	)

	require.NoError(t, err)
	assert.Equal(t, "child-session", got)
}

func TestResolveHealthSessionIDPartialMatchCanBeOutsideHealthList(t *testing.T) {
	database := dbtest.OpenTestDB(t)

	writes := make([]db.SessionBatchWrite, 0, maxHealthLimit+1)
	for i := range maxHealthLimit {
		started := fmt.Sprintf("2026-04-15T12:%02d:%02dZ", i/60, i%60)
		writes = append(writes, db.SessionBatchWrite{Session: db.Session{
			ID:      fmt.Sprintf("newer-session-%03d", i),
			Project: "p", Machine: "m", Agent: "claude",
			MessageCount: 1, StartedAt: &started,
		}})
	}
	oldStarted := "2020-01-01T00:00:00Z"
	writes = append(writes, db.SessionBatchWrite{Session: db.Session{
		ID: "old-partial-target", Project: "p", Machine: "m",
		Agent: "codex", MessageCount: 1, StartedAt: &oldStarted,
	}})
	result, err := database.WriteSessionBatchAtomic(writes)
	require.NoError(t, err, "seed health sessions")
	require.Equal(t, maxHealthLimit+1, result.WrittenSessions)

	got, err := resolveHealthSessionID(
		context.Background(),
		service.NewDirectBackend(database, nil),
		"partial-target",
	)

	require.NoError(t, err)
	assert.Equal(t, "old-partial-target", got)
}

func TestResolveHealthSessionIDUsesDaemonPartialLookup(t *testing.T) {
	var gotQuery string
	ts := daemonRouteTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/session-ids/resolve": func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.Query().Get("partial")
			writeJSONResponse(w, `{"ids":["old-partial-target"]}`)
		},
	})

	got, err := resolveHealthSessionID(
		context.Background(),
		service.NewHTTPBackend(ts.URL, "", false),
		"partial-target",
	)

	require.NoError(t, err)
	assert.Equal(t, "partial-target", gotQuery)
	assert.Equal(t, "old-partial-target", got)
}

func TestResolveHealthSessionIDExactMatchStillChecksShortIDAmbiguity(
	t *testing.T,
) {
	database := dbtest.OpenTestDB(t)

	require.NoError(t, database.UpsertSession(db.Session{
		ID: "abcdef12", Project: "p", Machine: "m",
		Agent: "claude", MessageCount: 1,
	}), "upsert exact session")
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "abcdef1234567890", Project: "p", Machine: "m",
		Agent: "codex", MessageCount: 1,
	}), "upsert short-id collision")

	got, err := resolveHealthSessionID(
		context.Background(),
		service.NewDirectBackend(database, nil),
		"abcdef12",
	)

	require.Error(t, err)
	assert.Empty(t, got)
	assert.Contains(t, err.Error(), "ambiguous")
	assert.Contains(t, err.Error(), "abcdef1234567890")
}

func TestResolveSessionID(t *testing.T) {
	database := dbtest.OpenTestDB(t)

	upsert := func(id string) {
		t.Helper()
		require.NoError(t, database.UpsertSession(db.Session{
			ID: id, Project: "p", Machine: "m",
			Agent: "claude", MessageCount: 1,
		}), "upsert %q", id)
	}

	// "abcdef12" is both a full session ID and the short-ID
	// (first 8 chars) of another session -- a real display
	// collision in `health` list output.
	upsert("abcdef12")
	upsert("abcdef1234567890")
	upsert("unique-session-id")
	// Host-prefixed remote ID where the full local ID is a
	// substring. Looking up by the full local ID must NOT
	// be ambiguous -- the user typed an exact ID and the
	// remote one displays as a different short ID.
	upsert("local-uuid-aaaa-bbbb")
	upsert("remotehost~local-uuid-aaaa-bbbb")

	ctx := context.Background()

	t.Run("unique substring resolves", func(t *testing.T) {
		got, err := resolveSessionID(ctx, database, "unique")
		require.NoError(t, err, "resolveSessionID")
		assert.Equal(t, "unique-session-id", got)
	})

	t.Run("exact full id matching another short id is ambiguous",
		func(t *testing.T) {
			_, err := resolveSessionID(ctx, database, "abcdef12")
			require.Error(t, err, "expected ambiguity error")
			assert.Contains(t, err.Error(), "ambiguous",
				"error lacks 'ambiguous'")
		})

	t.Run("no match returns empty", func(t *testing.T) {
		got, err := resolveSessionID(ctx, database, "zzznope")
		require.NoError(t, err, "resolveSessionID")
		assert.Empty(t, got)
	})

	t.Run("unique full id resolves", func(t *testing.T) {
		got, err := resolveSessionID(
			ctx, database, "abcdef1234567890",
		)
		require.NoError(t, err, "resolveSessionID")
		assert.Equal(t, "abcdef1234567890", got)
	})

	t.Run("exact id contained in host-prefixed id resolves",
		func(t *testing.T) {
			got, err := resolveSessionID(
				ctx, database, "local-uuid-aaaa-bbbb",
			)
			require.NoError(t, err, "resolveSessionID")
			assert.Equal(t, "local-uuid-aaaa-bbbb", got)
		})
}

// TestResolveSessionIDCollisionBeyondTopFew exercises the
// resolveLookupLimit bump: the previous limit of 5 could miss
// a short-ID collision when the colliding row sat outside the
// top-5 partial-match window. We seed many partial matches
// with timestamps that push the collider past position 5 and
// confirm ambiguity is still reported.
func TestResolveSessionIDCollisionBeyondTopFew(t *testing.T) {
	database := dbtest.OpenTestDB(t)

	upsert := func(id string, started string) {
		t.Helper()
		require.NoError(t, database.UpsertSession(db.Session{
			ID: id, Project: "p", Machine: "m",
			Agent: "claude", MessageCount: 1,
			StartedAt: &started,
		}), "upsert %q", id)
	}

	// shortID() truncates the segment after the last "~" to
	// 8 chars, so any input that can collide via shortID is
	// at most 8 chars long. Use an 8-char input, the exact
	// full ID, several distractor substring matches that
	// crowd the top of the result set, and one collider
	// whose first 8 chars equal the input -- pushed past
	// position 5 by an old timestamp.
	const partial = "abcdef12"
	upsert(partial, "2026-04-15T12:00:00Z")
	for i := range 10 {
		ts := "2026-04-15T10:00:0" + string(rune('0'+i)) + "Z"
		// Each distractor contains partial as a substring
		// but its own shortID starts with "x-" so it won't
		// trigger the ambiguity check on its own.
		upsert(
			"x-"+partial+"-"+string(rune('a'+i)), ts,
		)
	}
	// Collider: starts with partial, so shortID() == partial.
	// Old timestamp pushes it to the bottom of the partial
	// match result set (well beyond the previous limit of 5).
	upsert(partial+"-collide", "2020-01-01T00:00:00Z")

	ctx := context.Background()
	_, err := resolveSessionID(ctx, database, partial)
	require.Error(t, err, "expected ambiguity error")
	assert.Contains(t, err.Error(), "ambiguous",
		"error lacks 'ambiguous'")
}

func parseLocalDate(t *testing.T, ts string) string {
	t.Helper()
	return shortDate(ts)
}
