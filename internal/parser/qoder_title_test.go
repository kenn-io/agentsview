package parser

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const qoderTitleTestID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

// writeQoderTitleDB creates a fixture of the Qoder application database's
// chat_sessions table. A nil title inserts SQL NULL.
func writeQoderTitleDB(
	t *testing.T, path string, titles map[string]*string,
) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()
	mustExec(t, db,
		`CREATE TABLE chat_sessions (session_id text PRIMARY KEY, title text)`)
	for id, title := range titles {
		mustExec(t, db,
			`INSERT INTO chat_sessions (session_id, title) VALUES (?, ?)`,
			id, title)
	}
}

// writeQoderTitleTestSession writes a minimal Qoder transcript under
// <root>/proj/<stem>.jsonl and returns its path.
func writeQoderTitleTestSession(t *testing.T, root, stem string) string {
	t.Helper()
	path := filepath.Join(root, "proj", stem+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := fmt.Sprintf(
		"{\"type\":\"user\",\"uuid\":\"u1\","+
			"\"timestamp\":\"2026-06-04T09:47:27.966Z\","+
			"\"message\":{\"role\":\"user\",\"content\":\"hello\"},"+
			"\"sessionId\":%q}\n", stem)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func qoderTitleTestProvider(
	t *testing.T, roots []string,
) Provider {
	t.Helper()
	provider, ok := NewProvider(AgentQoder, ProviderConfig{
		Roots: roots, Machine: "local",
	})
	require.True(t, ok)
	return provider
}

func qoderTitleTestParse(
	t *testing.T, provider Provider, root string,
) []ParseResultOutcome {
	t.Helper()
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	return outcome.Results
}

// withQoderTitleDBOverride points the Application Support lookup at a fixture
// database for the duration of one test.
func withQoderTitleDBOverride(t *testing.T, path string) {
	t.Helper()
	previous := qoderAppDatabasePathOverride
	qoderAppDatabasePathOverride = func(bool) string { return path }
	t.Cleanup(func() { qoderAppDatabasePathOverride = previous })
}

// TestQoderParsePrefersSiblingJSONTitle locks in the high-priority source: a
// non-empty title in the sibling -session.json wins, and the working_dir /
// fork_from metadata is still applied.
func TestQoderParsePrefersSiblingJSONTitle(t *testing.T) {
	root := t.TempDir()
	path := writeQoderTitleTestSession(t, root, qoderTitleTestID)
	require.NoError(t, os.WriteFile(
		strings.TrimSuffix(path, ".jsonl")+"-session.json",
		[]byte(`{"title":"JSON wins","working_dir":"/tmp/wd",`+
			`"fork_from":"11111111-1111-4111-8111-111111111111"}`),
		0o644,
	))
	intl := "database title"
	fixture := filepath.Join(t.TempDir(), "main.sqlite")
	writeQoderTitleDB(t, fixture, map[string]*string{qoderTitleTestID: &intl})
	withQoderTitleDBOverride(t, fixture)

	results := qoderTitleTestParse(t, qoderTitleTestProvider(t, []string{root}), root)
	sess := results[0].Result.Session
	assert.Equal(t, "JSON wins", sess.SessionName)
	assert.True(t, sess.SessionNamePresent)
	assert.Equal(t, "/tmp/wd", sess.Cwd)
	assert.Equal(t, "qoder:11111111-1111-4111-8111-111111111111", sess.ParentSessionID)
	assert.Equal(t, RelFork, sess.RelationshipType)
	assert.Empty(t, results[0].Result.TitleRetryReason)
	assert.Equal(t, DataVersionCurrent, results[0].DataVersion)
}

// TestQoderParseFallsBackToIDEDatabase covers the low-priority source: with no
// sibling JSON, the bound IDE database supplies the title.
func TestQoderParseFallsBackToIDEDatabase(t *testing.T) {
	root := t.TempDir()
	writeQoderTitleTestSession(t, root, qoderTitleTestID)
	title := "会话标题样例"
	fixture := filepath.Join(t.TempDir(), "main.sqlite")
	writeQoderTitleDB(t, fixture, map[string]*string{qoderTitleTestID: &title})
	withQoderTitleDBOverride(t, fixture)

	results := qoderTitleTestParse(t, qoderTitleTestProvider(t, []string{root}), root)
	sess := results[0].Result.Session
	assert.Equal(t, title, sess.SessionName)
	assert.True(t, sess.SessionNamePresent)
	assert.Equal(t, DataVersionCurrent, results[0].DataVersion)
}

// TestQoderParseExplicitBlankIDETitleClears pins that an explicitly empty TEXT
// is a present title (allowed to clear), unlike a missing row or SQL NULL.
func TestQoderParseExplicitBlankIDETitleClears(t *testing.T) {
	root := t.TempDir()
	writeQoderTitleTestSession(t, root, qoderTitleTestID)
	blank := ""
	fixture := filepath.Join(t.TempDir(), "main.sqlite")
	writeQoderTitleDB(t, fixture, map[string]*string{qoderTitleTestID: &blank})
	withQoderTitleDBOverride(t, fixture)

	results := qoderTitleTestParse(t, qoderTitleTestProvider(t, []string{root}), root)
	sess := results[0].Result.Session
	assert.Empty(t, sess.SessionName)
	assert.True(t, sess.SessionNamePresent)
}

// TestQoderParseNoSignalsWithoutBoundDatabase covers the affinity gate: a
// configured root that is not one of the two recognized Qoder IDE session
// directories has no bound title database, so no title is read, nothing is
// cleared, and no error is raised -- even on a machine that does have the
// client installed.
func TestQoderParseNoSignalsWithoutBoundDatabase(t *testing.T) {
	root := t.TempDir()
	writeQoderTitleTestSession(t, root, qoderTitleTestID)

	results := qoderTitleTestParse(t, qoderTitleTestProvider(t, []string{root}), root)
	sess := results[0].Result.Session
	assert.Empty(t, sess.SessionName)
	assert.False(t, sess.SessionNamePresent)
	assert.Equal(t, DataVersionCurrent, results[0].DataVersion)
	assert.Empty(t, results[0].Result.TitleRetryReason)

	// Binding is by root, so an empty root list binds nothing either.
	assert.Empty(t, resolveQoderAppSQLitePath(
		filepath.Join(root, "proj", qoderTitleTestID+".jsonl"), nil))
	// A plain directory named like neither client must not fall back to the
	// international database this machine may own.
	assert.Empty(t, resolveQoderAppSQLitePath(
		filepath.Join(root, "proj", qoderTitleTestID+".jsonl"), []string{root}))
}

// TestQoderParseTitleDatabaseReadErrorRequestsRetry keeps a title read failure
// out of the parse error path while still marking the result for retry.
func TestQoderParseTitleDatabaseReadErrorRequestsRetry(t *testing.T) {
	root := t.TempDir()
	writeQoderTitleTestSession(t, root, qoderTitleTestID)
	fixture := filepath.Join(t.TempDir(), "main.sqlite")
	require.NoError(t, os.WriteFile(fixture, []byte("not a database"), 0o644))
	withQoderTitleDBOverride(t, fixture)

	results := qoderTitleTestParse(t, qoderTitleTestProvider(t, []string{root}), root)
	assert.Equal(t, DataVersionNeedsRetry, results[0].DataVersion)
	assert.NotEmpty(t, results[0].RetryReason)
	assert.NotEmpty(t, results[0].Result.TitleRetryReason)
	assert.NotEmpty(t, results[0].Result.Messages)
}

// TestQoderParseCorruptJSONDoesNotFallThrough covers the ordering rule: a
// malformed sibling JSON cannot be shown to hold no title, so the database
// must not silently supply a lower-priority value.
func TestQoderParseCorruptJSONDoesNotFallThrough(t *testing.T) {
	root := t.TempDir()
	path := writeQoderTitleTestSession(t, root, qoderTitleTestID)
	require.NoError(t, os.WriteFile(
		strings.TrimSuffix(path, ".jsonl")+"-session.json",
		[]byte("{not json"),
		0o644,
	))
	dbTitle := "must not be applied"
	fixture := filepath.Join(t.TempDir(), "main.sqlite")
	writeQoderTitleDB(t, fixture, map[string]*string{qoderTitleTestID: &dbTitle})
	withQoderTitleDBOverride(t, fixture)

	results := qoderTitleTestParse(t, qoderTitleTestProvider(t, []string{root}), root)
	sess := results[0].Result.Session
	assert.Empty(t, sess.SessionName)
	assert.False(t, sess.SessionNamePresent)
	assert.Equal(t, DataVersionNeedsRetry, results[0].DataVersion)
	assert.NotEmpty(t, results[0].RetryReason)
}

// TestQoderParseSubagentNeverAdoptsTitle keeps the subagent boundary: a
// subagent transcript must not inherit the parent's title even when the shared
// database has a row for the parent id.
func TestQoderParseSubagentNeverAdoptsTitle(t *testing.T) {
	root := t.TempDir()
	subPath := filepath.Join(root, "proj", qoderTitleTestID, "subagents", "agent-123.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(subPath), 0o755))
	require.NoError(t, os.WriteFile(subPath, []byte(
		"{\"agentId\":\"123\",\"type\":\"user\",\"uuid\":\"u1\","+
			"\"timestamp\":\"2026-06-04T09:47:27.966Z\","+
			"\"message\":{\"role\":\"user\",\"content\":\"sub task\"},"+
			"\"sessionId\":\"child\"}\n"), 0o644))
	title := "parent title"
	fixture := filepath.Join(t.TempDir(), "main.sqlite")
	writeQoderTitleDB(t, fixture, map[string]*string{qoderTitleTestID: &title})
	withQoderTitleDBOverride(t, fixture)

	results := qoderTitleTestParse(t, qoderTitleTestProvider(t, []string{root}), root)
	sess := results[0].Result.Session
	assert.Equal(t, RelSubagent, sess.RelationshipType)
	assert.Empty(t, sess.SessionName)
	assert.False(t, sess.SessionNamePresent)
}

// TestResolveQoderAppSQLitePathBindsRoots covers root affinity against the
// roots AgentsView actually configures. The client directory is an ancestor
// segment of the root (<client>/projects), not the root's last segment, so a
// last-segment match resolves every default root to no database at all.
func TestResolveQoderAppSQLitePathBindsRoots(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	applicationSupport := filepath.Join(home, "Library", "Application Support")

	for _, tt := range []struct {
		name string
		root string
		want string
	}{
		{
			name: "CN default project root",
			root: filepath.Join(home, ".qoder-cn", "projects"),
			want: filepath.Join(applicationSupport, qoderCNClientApp, "main.sqlite"),
		},
		{
			name: "international default project root",
			root: filepath.Join(home, ".qoder", "projects"),
			want: filepath.Join(applicationSupport, qoderIntlClientApp, "main.sqlite"),
		},
		{
			name: "client directory itself",
			root: filepath.Join(home, ".qoder-cn"),
			want: filepath.Join(applicationSupport, qoderCNClientApp, "main.sqlite"),
		},
		{
			name: "nested below the project root",
			root: filepath.Join(home, ".qoder", "projects", "-Users-example-work"),
			want: filepath.Join(applicationSupport, qoderIntlClientApp, "main.sqlite"),
		},
		{
			// .qoderwork is a separate legacy export directory: matching it
			// must be exact, never a prefix match on ".qoder".
			name: "qoderwork is not qoder",
			root: filepath.Join(home, ".qoderwork", "projects"),
		},
		{
			name: "custom root",
			root: filepath.Join(home, "custom-qoder"),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveQoderAppSQLitePath(
				filepath.Join(tt.root, "proj", "s.jsonl"), []string{tt.root})
			assert.Equal(t, tt.want, got)
		})
	}

	// A root prefix must match on a path boundary, not a bare substring.
	assert.Empty(t, resolveQoderAppSQLitePath(
		home+"/.qoder-other/projects/p/s.jsonl",
		[]string{home + "/.qoder"},
	))
	assert.Empty(t, resolveQoderAppSQLitePath(
		filepath.Join(home, ".qoder", "projects", "p", "s.jsonl"), nil))
}

// TestQoderClientAppForPathMatchesWholeSegments pins the segment rule directly:
// the nearest matching ancestor wins, and a directory whose name merely starts
// with a client directory is not that client.
func TestQoderClientAppForPathMatchesWholeSegments(t *testing.T) {
	for _, tt := range []struct {
		path string
		want string
	}{
		{path: "/home/u/.qoder-cn/projects/p/s.jsonl", want: qoderCNClientApp},
		{path: "/home/u/.qoder/projects/p/s.jsonl", want: qoderIntlClientApp},
		{path: "/home/u/.qoder-cn", want: qoderCNClientApp},
		{path: "/home/u/.qoderwork/projects/p/s.jsonl"},
		{path: "/home/u/.qoder-cn-backup/projects/p/s.jsonl"},
		{path: "/home/u/qoder/projects/p/s.jsonl"},
		{path: "/home/u"},
		{path: ""},
	} {
		t.Run(tt.path, func(t *testing.T) {
			assert.Equal(t, tt.want, QoderClientAppForPath(tt.path))
		})
	}
}

// TestQoderParseBindsDefaultProjectRoots is the regression guard for the
// default root shape: with root = <client>/projects the parser must still read
// the client's application database, and must report the CN build as CN.
func TestQoderParseBindsDefaultProjectRoots(t *testing.T) {
	for _, tt := range []struct {
		root string
		isCN bool
	}{
		{root: filepath.Join(".qoder-cn", "projects"), isCN: true},
		{root: filepath.Join(".qoder", "projects"), isCN: false},
	} {
		t.Run(tt.root, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), tt.root)
			writeQoderTitleTestSession(t, root, qoderTitleTestID)
			title := "默认项目根目录下的标题"
			fixture := filepath.Join(t.TempDir(), "main.sqlite")
			writeQoderTitleDB(t, fixture, map[string]*string{
				qoderTitleTestID: &title,
			})

			var seenCN []bool
			previous := qoderAppDatabasePathOverride
			qoderAppDatabasePathOverride = func(isCN bool) string {
				seenCN = append(seenCN, isCN)
				return fixture
			}
			t.Cleanup(func() { qoderAppDatabasePathOverride = previous })

			results := qoderTitleTestParse(t,
				qoderTitleTestProvider(t, []string{root}), root)
			assert.Equal(t, title, results[0].Result.Session.SessionName)
			require.NotEmpty(t, seenCN,
				"the title lookup must consult the bound application database")
			assert.Equal(t, tt.isCN, seenCN[0])
		})
	}
}

// TestQoderTitleDatabasePathsForRootsDedupes keeps one watch root per
// application directory even when several configured roots share a client.
func TestQoderTitleDatabasePathsForRootsDedupes(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	intl := filepath.Join(home, ".qoder", "projects")
	cn := filepath.Join(home, ".qoder-cn", "projects")
	paths := qoderTitleDatabasePathsForRoots([]string{intl, intl, cn})
	require.Len(t, paths, 2)
	assert.Equal(t,
		filepath.Join(home, "Library", "Application Support",
			qoderIntlClientApp, "main.sqlite"),
		paths[0])
	assert.Equal(t,
		filepath.Join(home, "Library", "Application Support",
			qoderCNClientApp, "main.sqlite"),
		paths[1])
	assert.Empty(t, qoderTitleDatabasePathsForRoots([]string{
		filepath.Join(home, "other"),
		filepath.Join(home, ".qoderwork", "projects"),
	}), "a root that belongs to neither client binds nothing")
}
