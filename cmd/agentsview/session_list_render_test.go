package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

// renderNow is the reference clock the table tests render against.
var renderNow = time.Date(2026, 6, 19, 23, 18, 0, 0, time.UTC)

func TestHumanizeSessionAge(t *testing.T) {
	t.Parallel()
	mk := func(d time.Duration) db.Session {
		return db.Session{EndedAt: new(renderNow.Add(-d).Format(time.RFC3339))}
	}
	tests := []struct {
		name string
		sess db.Session
		want string
	}{
		{"seconds", mk(30 * time.Second), "30s"},
		{"minutes", mk(5 * time.Minute), "5m"},
		{"hours", mk(3 * time.Hour), "3h"},
		{"days", mk(2 * 24 * time.Hour), "2d"},
		{
			"absolute beyond a week",
			db.Session{EndedAt: new("2026-01-02T08:00:00Z")},
			"Jan 02",
		},
		{"no timestamp", db.Session{}, emDash},
		{
			"falls back to started_at",
			db.Session{StartedAt: new("2026-06-19T23:10:00Z")},
			"8m",
		},
		{
			"future clock skew reads as now",
			db.Session{EndedAt: new(renderNow.Add(5 * time.Second).Format(time.RFC3339))},
			"now",
		},
		{
			"RFC3339Nano timestamp parses",
			db.Session{EndedAt: new(renderNow.Add(-time.Hour).Format(time.RFC3339Nano))},
			"1h",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, humanizeSessionAge(tc.sess, renderNow))
		})
	}
}

func TestHumanizeAgeRelative(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		t       time.Time
		wantStr string
		wantOK  bool
	}{
		{"future skew reads as now", renderNow.Add(5 * time.Second), "now", true},
		{"seconds", renderNow.Add(-30 * time.Second), "30s", true},
		{"minutes", renderNow.Add(-5 * time.Minute), "5m", true},
		{"hours", renderNow.Add(-3 * time.Hour), "3h", true},
		{"days", renderNow.Add(-2 * 24 * time.Hour), "2d", true},
		{"a week or more returns not-ok", renderNow.Add(-7 * 24 * time.Hour), "", false},
		{"long past returns not-ok", renderNow.Add(-90 * 24 * time.Hour), "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := humanizeAgeRelative(tc.t, renderNow)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantStr, got)
		})
	}
}

func TestIsSessionRecentlyActive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sess db.Session
		want bool
	}{
		{
			"just now",
			db.Session{EndedAt: new(renderNow.Add(-time.Minute).Format(time.RFC3339))},
			true,
		},
		{
			"future clock skew counts as active",
			db.Session{EndedAt: new(renderNow.Add(5 * time.Second).Format(time.RFC3339))},
			true,
		},
		{
			"beyond the window",
			db.Session{EndedAt: new(renderNow.Add(-30 * time.Minute).Format(time.RFC3339))},
			false,
		},
		{"no timestamp", db.Session{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isSessionRecentlyActive(tc.sess, renderNow))
		})
	}
}

func TestSessionActivityTimeCreatedAtFallback(t *testing.T) {
	t.Parallel()
	// A session with only created_at (no ended_at/started_at) can be
	// returned by --resume because the backend active_since filter falls
	// back to created_at; AGE/the marker must agree by using it too.
	created := renderNow.Add(-2 * time.Minute)
	s := db.Session{CreatedAt: created.Format(time.RFC3339)}
	got := sessionActivityTime(s)
	assert.False(t, got.IsZero(), "created_at must be the final fallback")
	assert.WithinDuration(t, created, got, time.Second)
	assert.True(t, isSessionRecentlyActive(s, renderNow),
		"a recently-created session must render as active for --resume")

	// ended_at still takes precedence over created_at.
	ended := renderNow.Add(-time.Minute)
	s2 := db.Session{
		EndedAt:   new(ended.Format(time.RFC3339)),
		CreatedAt: renderNow.Add(-time.Hour).Format(time.RFC3339),
	}
	assert.WithinDuration(t, ended, sessionActivityTime(s2), time.Second)
}

func TestCollapseHome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, cwd, home, want string
	}{
		{"under home", "/home/u/p", "/home/u", "~/p"},
		{"exactly home", "/home/u", "/home/u", "~"},
		{"outside home", "/other/p", "/home/u", "/other/p"},
		{"empty becomes em dash", "", "/home/u", emDash},
		{"shared prefix only is not collapsed", "/home/user2/p", "/home/u", "/home/user2/p"},
		{"empty home left intact", "/srv/app", "", "/srv/app"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, collapseHome(tc.cwd, tc.home))
		})
	}
}

func TestTruncName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in string
		max      int
		want     string
	}{
		{"short kept", "hello", 10, "hello"},
		{"truncated with ellipsis", "abcdefghij", 5, "abcde…"},
		{"whitespace collapsed", "a  b\nc", 10, "a b c"},
		{"multibyte boundary", "héllo wörld", 5, "héllo…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, truncName(tc.in, tc.max))
		})
	}
}

func TestSessionDisplayName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "display", sessionDisplayName(db.Session{
		DisplayName: new("display"), FirstMessage: new("first"),
	}))
	assert.Equal(t, "first", sessionDisplayName(db.Session{
		FirstMessage: new("first"),
	}))
	assert.Equal(t, "", sessionDisplayName(db.Session{}))
	// An empty display name falls through to the first message.
	assert.Equal(t, "first", sessionDisplayName(db.Session{
		DisplayName: new(""), FirstMessage: new("first"),
	}))
}

func TestOrEmDash(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "main", orEmDash("main"))
	assert.Equal(t, emDash, orEmDash(""))
	assert.Equal(t, emDash, orEmDash("   "))
}

// listFixture builds a SessionList with a live claude session, an older one
// in the home dir, and a codex session that (like real codex rows) has no
// cwd or branch.
func listFixture(home string) *service.SessionList {
	return &service.SessionList{
		Total: 619,
		Sessions: []db.Session{
			{
				ID: "s_live", Project: "monitoring", Agent: "claude",
				GitBranch:    "main",
				FirstMessage: new("build monitoring dashboards\nfor the homelab"),
				MessageCount: 87,
				StartedAt:    new("2026-06-19T23:00:00Z"),
				EndedAt:      new("2026-06-19T23:17:00Z"),
				Cwd:          "/srv/monitoring",
			},
			{
				ID: "s_codex", Project: "agentsview", Agent: "codex",
				FirstMessage: new("You are a code reviewer"),
				MessageCount: 28,
				StartedAt:    new("2026-06-19T22:00:00Z"),
				EndedAt:      new("2026-06-19T22:00:23Z"),
			},
			{
				ID: "s_old", Project: "vault", Agent: "claude",
				GitBranch:    "main",
				FirstMessage: new("refactor vault notes"),
				MessageCount: 26,
				StartedAt:    new("2026-06-19T19:50:00Z"),
				EndedAt:      new("2026-06-19T20:10:00Z"),
				Cwd:          home + "/vault",
			},
		},
	}
}

func TestPrintSessionListHuman(t *testing.T) {
	t.Parallel()
	const home = "/home/u"
	var out bytes.Buffer
	require.NoError(t, printSessionListHuman(
		&out, listFixture(home), renderNow, home))
	s := out.String()

	// Header carries the enriched columns, with ID restored as the first
	// data column so every row has a copyable handle.
	for _, col := range []string{"ID", "AGE", "AGENT", "PROJECT", "BRANCH", "MSGS", "NAME", "CWD"} {
		assert.Contains(t, s, col)
	}

	// Every row carries its full, untruncated session ID.
	for _, id := range []string{"s_live", "s_codex", "s_old"} {
		assert.Contains(t, s, id)
	}

	// The live session (1m ago) is flagged in-flight; the codex session
	// (~78m ago) and old session (~3h ago) are not.
	assert.Contains(t, s, activeMarker)
	assert.Equal(t, 1, strings.Count(s, activeMarker), "only the live row is in-flight")
	assert.Contains(t, s, "1m") // 23:18 - 23:17

	// Missing codex cwd/branch render as an em dash, not blank.
	assert.Contains(t, s, emDash)

	// Home-prefixed cwd collapses to ~; the newline in a name is collapsed.
	assert.Contains(t, s, "~/vault")
	assert.Contains(t, s, "build monitoring dashboards for the homelab")

	// Message counts are present.
	assert.Contains(t, s, "87")

	// Footer hint only appears when there is a next page.
	assert.NotContains(t, s, "--cursor")
}

func TestPrintSessionListHumanNextCursor(t *testing.T) {
	t.Parallel()
	list := listFixture("/home/u")
	list.NextCursor = "opaque-token"
	var out bytes.Buffer
	require.NoError(t, printSessionListHuman(&out, list, renderNow, "/home/u"))
	assert.Contains(t, out.String(), "More results: --cursor opaque-token")
}

func TestPrintSessionListHumanEmpty(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	require.NoError(t, printSessionListHuman(
		&out, &service.SessionList{}, renderNow, "/home/u"))
	assert.Equal(t, "(no sessions)\n", out.String())
}

func TestPrintSessionListHumanSanitizesUntrustedFields(t *testing.T) {
	t.Parallel()
	list := &service.SessionList{
		Sessions: []db.Session{{
			ID: "s1", Project: "p", Agent: "claude",
			GitBranch:    "ma\x1bin",
			FirstMessage: new("hi\x1b[31m there"),
			Cwd:          "/srv\x07/app",
		}},
	}
	var out bytes.Buffer
	require.NoError(t, printSessionListHuman(&out, list, renderNow, "/home/u"))
	s := out.String()
	assert.NotContains(t, s, "\x1b")
	assert.NotContains(t, s, "\x07")
	assert.Contains(t, s, "main")
	assert.Contains(t, s, "/srv/app")
}
