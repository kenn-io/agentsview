package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestAntigravityTitleCacheExpiresMatchingMarkers(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	priorNow := sqliteTitleNow
	sqliteTitleNow = func() time.Time { return now }
	t.Cleanup(func() { sqliteTitleNow = priorNow })
	store := filepath.Join(t.TempDir(), AntigravitySummariesDBName)
	conn, err := sql.Open("sqlite3", store)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	_, err = conn.ExecContext(t.Context(), testjsonl.AntigravityTitleSchema)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA journal_mode=WAL; INSERT INTO conversation_summaries VALUES ('conversation-a', 'First')`)
	require.NoError(t, err)
	checkpoint := func() {
		_, err := conn.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)")
		require.NoError(t, err)
		for _, path := range []string{store, store + "-wal"} {
			require.NoError(t, os.Chtimes(path, now, now))
		}
	}
	checkpoint()
	titles, _, err := AntigravityStoreTitles(t.Context(), store)
	require.NoError(t, err)
	require.Equal(t, "First", titles["conversation-a"])
	before, ok := StatSQLiteContainerState(store)
	require.True(t, ok)
	_, err = conn.ExecContext(t.Context(), "UPDATE conversation_summaries SET title = 'Later'")
	require.NoError(t, err)
	checkpoint()
	after, ok := StatSQLiteContainerState(store)
	require.True(t, ok)
	require.Equal(t, before, after)
	titles, _, err = AntigravityStoreTitles(t.Context(), store)
	require.NoError(t, err)
	assert.Equal(t, "First", titles["conversation-a"])
	now = now.Add(SQLiteContainerDigestVerifyInterval)
	titles, _, err = AntigravityStoreTitles(t.Context(), store)
	require.NoError(t, err)
	assert.Equal(t, "Later", titles["conversation-a"])
}

func TestAntigravityCLISessionRoot(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	root := filepath.Join(t.TempDir(), "profile", "antigravity-acp")
	tests := []struct {
		name     string
		path     string
		wantRoot string
		wantOK   bool
	}{
		{
			name:     "conversation db",
			path:     filepath.Join(root, "conversations", id+".db"),
			wantRoot: root,
			wantOK:   true,
		},
		{
			name:     "conversation pb",
			path:     filepath.Join(root, "conversations", id+".pb"),
			wantRoot: root,
			wantOK:   true,
		},
		{
			name:     "implicit pb",
			path:     filepath.Join(root, "implicit", id+".pb"),
			wantRoot: root,
			wantOK:   true,
		},
		{
			name:   "implicit db rejected",
			path:   filepath.Join(root, "implicit", id+".db"),
			wantOK: false,
		},
		{
			name:   "unrelated file",
			path:   filepath.Join(root, AntigravitySummariesDBName),
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotRoot, ok := AntigravityCLISessionRoot(tc.path)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantRoot, gotRoot)
		})
	}
}

func TestAntigravityCLITitleStoreOwner(t *testing.T) {
	base := t.TempDir()
	direct := filepath.Join(base, "direct")
	require.NoError(t, os.MkdirAll(filepath.Join(direct, "conversations"), 0o755))
	container := filepath.Join(base, ".t3", "userdata", "providers", "antigravity")
	singleProfile := filepath.Join(base, "single-profile")
	directContainer := filepath.Join(base, "harness-profiles")

	tests := []struct {
		name  string
		root  string
		store string
		want  bool
	}{
		{
			name:  "direct root store",
			root:  direct,
			store: filepath.Join(direct, AntigravitySummariesDBName),
			want:  true,
		},
		{
			name:  "nested instance store",
			root:  container,
			store: filepath.Join(container, "abc123", "antigravity-acp", AntigravitySummariesDBName),
			want:  true,
		},
		{
			name:  "single profile store",
			root:  singleProfile,
			store: filepath.Join(singleProfile, "antigravity-acp", AntigravitySummariesDBName),
			want:  true,
		},
		{
			name:  "direct instance container store",
			root:  directContainer,
			store: filepath.Join(directContainer, "work", AntigravitySummariesDBName),
			want:  true,
		},
		{
			name:  "nested scratch file rejected",
			root:  container,
			store: filepath.Join(container, "abc123", "antigravity-acp", "brain", "id", "scratch", AntigravitySummariesDBName),
			want:  false,
		},
		{
			name:  "unrelated filename rejected",
			root:  direct,
			store: filepath.Join(direct, "other.db"),
			want:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, AntigravityCLITitleStoreOwner(tc.root, tc.store))
		})
	}
}

func TestAntigravityCLIDirectRootWatchPlanIncludesTitleStore(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "conversations"), 0o755))
	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)

	var foundRoot bool
	for _, wr := range plan.Roots {
		if samePath(wr.Path, root) {
			foundRoot = true
			assert.Contains(t, wr.IncludeGlobs, AntigravitySummariesDBName)
			assert.Contains(t, wr.IncludeGlobs, AntigravitySummariesDBName+"-wal")
		}
	}
	assert.True(t, foundRoot, "expected watch root for %s", root)
}

func TestPreservesStoredSessionNameAntigravityCLI(t *testing.T) {
	assert.True(t, PreservesStoredSessionName(ParsedSession{
		Agent:              AgentAntigravityCLI,
		SessionNamePresent: false,
	}))
	assert.False(t, PreservesStoredSessionName(ParsedSession{
		Agent:              AgentAntigravityCLI,
		SessionName:        "Title",
		SessionNamePresent: true,
	}))
}
