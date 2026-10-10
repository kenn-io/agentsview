package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeAntigravitySummariesDB creates the IDE's shared title store at the
// given root. A nil title inserts SQL NULL, which must read as no signal.
func writeAntigravitySummariesDB(
	t *testing.T, root string, titles map[string]*string,
) {
	t.Helper()
	path := AntigravityTitleDatabasePath(root)
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()
	mustExec(t, db, `CREATE TABLE conversation_summaries (
		conversation_id text PRIMARY KEY, title text)`)
	for id, title := range titles {
		mustExec(t, db,
			`INSERT INTO conversation_summaries (conversation_id, title) VALUES (?, ?)`,
			id, title)
	}
}

func antigravityTitleTestFixture(
	t *testing.T,
) (root, dbPath, id string, provider *antigravityProvider) {
	t.Helper()
	root = t.TempDir()
	id = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath = filepath.Join(root, "conversations", id+".db")
	createAntigravityTestDB(t, dbPath)
	provider = newAntigravityTestProvider(t, root)
	return root, dbPath, id, provider
}

func antigravityTitleTestOutcome(
	t *testing.T, provider *antigravityProvider, root, dbPath string,
) ParseResultOutcome {
	t.Helper()
	source, ok := provider.sources.sourceRef(root, dbPath, true)
	require.True(t, ok)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	return outcome.Results[0]
}

// TestAntigravityParseReadsSummaryTitle covers the primary path: the title
// lives in the root's shared conversation_summaries.db, keyed by the bare
// conversation id, and reaches the parsed session.
func TestAntigravityParseReadsSummaryTitle(t *testing.T) {
	root, dbPath, id, provider := antigravityTitleTestFixture(t)
	title := "会话标题样例"
	writeAntigravitySummariesDB(t, root, map[string]*string{id: &title})

	result := antigravityTitleTestOutcome(t, provider, root, dbPath)
	assert.Equal(t, title, result.Result.Session.SessionName)
	assert.True(t, result.Result.Session.SessionNamePresent)
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Empty(t, result.RetryReason)
	assert.Empty(t, result.Result.TitleRetryReason)
}

// TestAntigravityParseTitleIsNoSignalWithoutRow covers the three no-signal
// shapes: the shared database is absent, the id has no row, and the row's
// value is SQL NULL. None may clear an existing title, and none is an error.
func TestAntigravityParseTitleIsNoSignalWithoutRow(t *testing.T) {
	t.Run("missing database", func(t *testing.T) {
		_, dbPath, _, provider := antigravityTitleTestFixture(t)
		result := antigravityTitleTestOutcome(t, provider, rootFor(t, dbPath), dbPath)
		assert.Empty(t, result.Result.Session.SessionName)
		assert.False(t, result.Result.Session.SessionNamePresent)
		assert.Equal(t, DataVersionCurrent, result.DataVersion)
	})

	t.Run("missing row", func(t *testing.T) {
		root, dbPath, _, provider := antigravityTitleTestFixture(t)
		other := "other-title"
		writeAntigravitySummariesDB(t, root, map[string]*string{
			"11111111-1111-4111-8111-111111111111": &other,
		})
		result := antigravityTitleTestOutcome(t, provider, root, dbPath)
		assert.Empty(t, result.Result.Session.SessionName)
		assert.False(t, result.Result.Session.SessionNamePresent)
		assert.Equal(t, DataVersionCurrent, result.DataVersion)
	})

	t.Run("sql null", func(t *testing.T) {
		root, dbPath, id, provider := antigravityTitleTestFixture(t)
		writeAntigravitySummariesDB(t, root, map[string]*string{id: nil})
		result := antigravityTitleTestOutcome(t, provider, root, dbPath)
		assert.Empty(t, result.Result.Session.SessionName)
		assert.False(t, result.Result.Session.SessionNamePresent)
		assert.Equal(t, DataVersionCurrent, result.DataVersion)
	})
}

// TestAntigravityParseExplicitBlankTitleClearsName pins the one case where a
// present-but-empty title is authoritative: the row exists and holds "", so
// the session must report a present title and the stored name may be cleared.
func TestAntigravityParseExplicitBlankTitleClearsName(t *testing.T) {
	root, dbPath, id, provider := antigravityTitleTestFixture(t)
	blank := ""
	writeAntigravitySummariesDB(t, root, map[string]*string{id: &blank})

	result := antigravityTitleTestOutcome(t, provider, root, dbPath)
	assert.Empty(t, result.Result.Session.SessionName)
	assert.True(t, result.Result.Session.SessionNamePresent)
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
}

// TestAntigravityParseTitleReadErrorRequestsRetry covers the fourth state: the
// shared database exists but cannot be read. The body still parses, no fatal
// error is returned, and the result is marked for retry so the engine does not
// record a clean skip.
func TestAntigravityParseTitleReadErrorRequestsRetry(t *testing.T) {
	root, dbPath, _, provider := antigravityTitleTestFixture(t)
	require.NoError(t, os.WriteFile(
		AntigravityTitleDatabasePath(root),
		[]byte("this is not a sqlite database"),
		0o644,
	))

	result := antigravityTitleTestOutcome(t, provider, root, dbPath)
	assert.Equal(t, DataVersionNeedsRetry, result.DataVersion)
	assert.NotEmpty(t, result.RetryReason)
	assert.NotEmpty(t, result.Result.TitleRetryReason)
	// The body result survives the title failure.
	require.NotEmpty(t, result.Result.Messages)
}

// TestAntigravityBrainTranscriptTitleReachesProviderParse is the entry
// coverage for a brain transcript: Discover and Parse, not a direct call to
// readAntigravityTitle. The summaries database lives at the IDE root.
func TestAntigravityBrainTranscriptTitleReachesProviderParse(t *testing.T) {
	root := t.TempDir()
	id := "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	path := writeAntigravityBrainTranscript(
		t, root, id, antigravityBrainTranscriptFixture,
	)
	title := "Synthetic conversation title"
	writeAntigravitySummariesDB(t, root, map[string]*string{id: &title})

	provider := newAntigravityProviderForRoots(t, root)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, path, sources[0].DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: sources[0], Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Empty(t, result.RetryReason)
	sess := result.Result.Session
	assert.Equal(t, "antigravity:"+id, sess.ID)
	assert.Equal(t, title, sess.SessionName)
	assert.True(t, sess.SessionNamePresent)
	require.NotEmpty(t, result.Result.Messages)
	assert.Equal(t, "list the files in the project", result.Result.Messages[0].Content)
}

// TestAntigravityTitleDatabasePathEmptyRoot ensures an empty root degrades to
// a no-signal read rather than opening a path relative to the process cwd.
func TestAntigravityTitleDatabasePathEmptyRoot(t *testing.T) {
	title, present, err := readAntigravityTitle(t.Context(), "", "some-id")
	require.NoError(t, err)
	assert.False(t, present)
	assert.Empty(t, title)
}

func rootFor(t *testing.T, dbPath string) string {
	t.Helper()
	return filepath.Dir(filepath.Dir(dbPath))
}
