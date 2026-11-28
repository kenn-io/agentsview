package remotesync

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// writeCodeBuddyRemoteHome lays out a CodeBuddy CLI/IDE 5.x home with one
// transcript that references one stored image, one unreferenced image, a
// settings file, and an IDE catalog with a pending WAL write.
func writeCodeBuddyRemoteHome(t *testing.T) (home string, catalogWriter *sql.DB) {
	t.Helper()
	home = t.TempDir()
	project := filepath.Join(home, "projects", "-work")
	require.NoError(t, os.MkdirAll(project, 0o755))
	used := strings.Repeat("ab", 32)
	unused := strings.Repeat("cd", 32)
	for _, blob := range []string{used, unused} {
		dir := filepath.Join(home, "blobs", blob[:2])
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, blob+".png"), []byte("png"), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{"token":"secret"}`), 0o600))
	transcript := `{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"sessionId":"run-1","content":[{"type":"input_text","text":"hello"},{"type":"image_blob_ref","blob_id":"` + used + `","mime":"image/png"}]}` + "\n" +
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225601000,"content":[{"type":"output_text","text":"hi"}]}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(project, "run-1.jsonl"), []byte(transcript), 0o600))

	catalogWriter, err := sql.Open("sqlite3", filepath.Join(home, parser.CodeBuddyCatalogDBName))
	require.NoError(t, err)
	t.Cleanup(func() { _ = catalogWriter.Close() })
	_, err = catalogWriter.ExecContext(t.Context(), `PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;
		CREATE TABLE conversations (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', custom_title TEXT, runtime_session_id TEXT);
		INSERT INTO conversations VALUES ('conv-1', 'Remote title', NULL, 'run-1');`)
	require.NoError(t, err)
	return home, catalogWriter
}

func TestCodeBuddyRemoteTargetsCarryCatalogAndReferencedImages(t *testing.T) {
	home, _ := writeCodeBuddyRemoteHome(t)
	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentCodeBuddy: {home},
	}})
	assert.Equal(t, []string{filepath.Join(home, "projects")}, targets.Dirs[parser.AgentCodeBuddy])

	var archive bytes.Buffer
	require.NoError(t, WriteArchive(t.Context(), &archive, targets))
	entries := archiveEntries(t, archive.Bytes())
	names := strings.Join(keys(entries), "\n")
	assert.Contains(t, names, "run-1.jsonl")
	assert.Contains(t, names, strings.Repeat("ab", 32)+".png", "a referenced image travels")
	assert.NotContains(t, names, strings.Repeat("cd", 32), "an unreferenced image stays home")
	assert.NotContains(t, names, "settings.json")
	assert.NotContains(t, names, parser.CodeBuddyCatalogDBName+"-wal", "the catalog travels as one snapshot")

	var snapshot []byte
	for name, body := range entries {
		if strings.HasSuffix(name, "/"+parser.CodeBuddyCatalogDBName) {
			snapshot = body
		}
	}
	require.NotEmpty(t, snapshot)
	path := filepath.Join(t.TempDir(), parser.CodeBuddyCatalogDBName)
	require.NoError(t, os.WriteFile(path, snapshot, 0o600))
	reader, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer reader.Close()
	var title string
	require.NoError(t, reader.QueryRowContext(t.Context(), "SELECT title FROM conversations").Scan(&title))
	assert.Equal(t, "Remote title", title, "the snapshot includes WAL-only writes")
}

// One unreadable project folder must not stop the readable conversations and
// their images from syncing.
func TestCodeBuddyRemoteTargetsSkipUnreadableProject(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory-permission read failures are not portable to Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	home, _ := writeCodeBuddyRemoteHome(t)
	locked := filepath.Join(home, "projects", "-locked")
	require.NoError(t, os.MkdirAll(locked, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(locked, "run-2.jsonl"), []byte("{}\n"), 0o600))
	require.NoError(t, os.Chmod(locked, 0))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	targets, err := ResolveTargets(t.Context(), config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentCodeBuddy: {home},
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join(home, "projects")}, targets.Dirs[parser.AgentCodeBuddy])
	extra := targets.ProviderExtraFiles[parser.AgentCodeBuddy]
	assert.Contains(t, extra, filepath.Join(home, parser.CodeBuddyCatalogDBName))
	used := strings.Repeat("ab", 32)
	assert.Contains(t, extra, filepath.Join(home, "blobs", used[:2], used+".png"),
		"the readable project's image still travels")

	var archive bytes.Buffer
	require.NoError(t, WriteArchive(t.Context(), &archive, targets))
	names := strings.Join(keys(archiveEntries(t, archive.Bytes())), "\n")
	assert.Contains(t, names, "run-1.jsonl")
	assert.Contains(t, names, used+".png")
}

// An unreadable projects folder still fails resolution, so the receiver does
// not evict its copy of every conversation over a read error.
func TestCodeBuddyRemoteTargetsFailOnUnreadableProjects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory-permission read failures are not portable to Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	home, _ := writeCodeBuddyRemoteHome(t)
	projects := filepath.Join(home, "projects")
	require.NoError(t, os.Chmod(projects, 0))
	t.Cleanup(func() { _ = os.Chmod(projects, 0o755) })

	_, err := ResolveTargets(t.Context(), config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentCodeBuddy: {home},
	}})
	require.Error(t, err)
}

// writeCodeBuddyRemoteHistory adds IDE 4.x history to a CodeBuddy home: one
// conversation only 4.x holds, and the 4.x request totals of a conversation
// the IDE converted to the CLI/5.x format. It also adds a cache directory.
func writeCodeBuddyRemoteHistory(t *testing.T, home string) {
	t.Helper()
	only4x := filepath.Join(home, "history", "ws", "conv-4x")
	require.NoError(t, os.MkdirAll(filepath.Join(only4x, "messages"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(only4x, "index.json"), []byte(`{"messages":[{"id":"u1"}]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(only4x, "messages", "u1.json"),
		[]byte(`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>from 4.x</user_query>"}]}}`), 0o600))

	converted := filepath.Join(home, "history", "ws", "conv-1")
	require.NoError(t, os.MkdirAll(filepath.Join(converted, "messages"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(converted, "index.json"), []byte(`{"messages":[{"id":"a1","role":"assistant"}]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(converted, "messages", "a1.json"),
		[]byte(`{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req-a1","modelId":"glm-x","statsSnapshot":{"inputTokens":300,"outputTokens":40}}}`), 0o600))
	transcript := `{"type":"session-meta","id":"m","sessionId":"conv-1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}` + "\n" +
		`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"old prompt"}]}` + "\n" +
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"old reply"}]}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, "projects", "-work", "conv-1.jsonl"), []byte(transcript), 0o600))

	require.NoError(t, os.MkdirAll(filepath.Join(home, "cache"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, "cache", "state.bin"), []byte("cache"), 0o600))
}

// A home holding both formats sends its 4.x history beside the CLI/5.x data
// and leaves settings, caches, and unreferenced images at home. The receiver
// imports both formats, and a converted conversation keeps its 4.x totals.
func TestCodeBuddyRemoteHomeWithHistorySendsOnlyConversations(t *testing.T) {
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	home, _ := writeCodeBuddyRemoteHome(t)
	writeCodeBuddyRemoteHistory(t, home)

	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentCodeBuddy: {home},
	}})
	assert.Equal(t, []string{filepath.Join(home, "projects"), filepath.Join(home, "history")},
		targets.Dirs[parser.AgentCodeBuddy])
	extra := targets.ProviderExtraFiles[parser.AgentCodeBuddy]
	assert.Contains(t, extra, filepath.Join(home, parser.CodeBuddyCatalogDBName))
	used := strings.Repeat("ab", 32)
	assert.Contains(t, extra, filepath.Join(home, "blobs", used[:2], used+".png"))

	var archive bytes.Buffer
	require.NoError(t, WriteArchive(t.Context(), &archive, targets))
	names := strings.Join(keys(archiveEntries(t, archive.Bytes())), "\n")
	assert.Contains(t, names, "run-1.jsonl")
	assert.Contains(t, names, "conv-4x/index.json")
	assert.Contains(t, names, "conv-4x/messages/u1.json")
	assert.Contains(t, names, used+".png")
	assert.NotContains(t, names, strings.Repeat("cd", 32), "an unreferenced image stays home")
	assert.NotContains(t, names, "settings.json")
	assert.NotContains(t, names, "state.bin")

	extracted := t.TempDir()
	_, err = ExtractTarStream(t.Context(), bytes.NewReader(archive.Bytes()), extracted)
	require.NoError(t, err)
	_, err = Importer{Host: "devbox", DB: database}.ImportExtracted(t.Context(), targets, extracted)
	require.NoError(t, err)

	for _, id := range []string{"run-1", "conv-4x", "conv-1"} {
		sess, err := database.GetSession(t.Context(), "devbox~codebuddy:"+id)
		require.NoError(t, err)
		require.NotNil(t, sess, id)
	}
	converted, err := database.GetSessionFull(t.Context(), "devbox~codebuddy:conv-1")
	require.NoError(t, err)
	require.NotNil(t, converted)
	require.NotNil(t, converted.FilePath)
	assert.Equal(t, "devbox:"+filepath.Join(home, "projects", "-work", "conv-1.jsonl"), *converted.FilePath,
		"the CLI/5.x copy owns the converted conversation")
	assert.Equal(t, 40, converted.TotalOutputTokens, "the mirrored 4.x history supplies the request totals")
}

// A CodeBuddy CLI home that has not written a conversation yet holds only
// settings and caches, so remote sync sends nothing from it.
func TestCodeBuddyRemoteHomeWithoutConversationsSendsNothing(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{"token":"secret"}`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(home, "cache"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, "cache", "state.bin"), []byte("cache"), 0o600))

	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentCodeBuddy: {home},
	}})
	assert.Empty(t, targets.Dirs[parser.AgentCodeBuddy])
	assert.Empty(t, targets.ProviderExtraFiles[parser.AgentCodeBuddy])

	var archive bytes.Buffer
	require.NoError(t, WriteArchive(t.Context(), &archive, targets))
	names := strings.Join(keys(archiveEntries(t, archive.Bytes())), "\n")
	assert.NotContains(t, names, "settings.json")
	assert.NotContains(t, names, "state.bin")
}

// writeCodeBuddyLegacyDataRoot lays out an IDE 4.x data root with one
// conversation at the deepest layout CodeBuddy writes, and returns the root.
func writeCodeBuddyLegacyDataRoot(t *testing.T) (root, conversation string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "CodeBuddyExtension", "Data")
	conversation = filepath.Join(root, "user", "CodeBuddyIDE", "user", "history", "ws", "conv-4x")
	require.NoError(t, os.MkdirAll(filepath.Join(conversation, "messages"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(conversation, "index.json"), []byte(`{"messages":[{"id":"u1"}]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(conversation, "messages", "u1.json"),
		[]byte(`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>from 4.x</user_query>"}]}}`), 0o600))
	return root, conversation
}

// An IDE 4.x data root still travels whole: its conversations sit several
// directories below the configured root.
func TestCodeBuddyRemoteLegacyDataRootSendsWholeRoot(t *testing.T) {
	root, _ := writeCodeBuddyLegacyDataRoot(t)

	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentCodeBuddy: {root},
	}})
	assert.Equal(t, []string{root}, targets.Dirs[parser.AgentCodeBuddy])

	var archive bytes.Buffer
	require.NoError(t, WriteArchive(t.Context(), &archive, targets))
	names := strings.Join(keys(archiveEntries(t, archive.Bytes())), "\n")
	assert.Contains(t, names, "conv-4x/index.json")
	assert.Contains(t, names, "conv-4x/messages/u1.json")
}

// A 4.x data root that cannot be listed fails resolution, so the receiver
// does not evict its copy of every 4.x conversation over a read error.
func TestCodeBuddyRemoteLegacyDataRootFailsWhenUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory-permission read failures are not portable to Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root, _ := writeCodeBuddyLegacyDataRoot(t)
	require.NoError(t, os.Chmod(root, 0))
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	_, err := ResolveTargets(t.Context(), config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentCodeBuddy: {root},
	}})
	require.Error(t, err)
}

func TestImporterReadsCodeBuddyCatalogFromRemoteHome(t *testing.T) {
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	home, _ := writeCodeBuddyRemoteHome(t)
	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentCodeBuddy: {home},
	}})
	var archive bytes.Buffer
	require.NoError(t, WriteArchive(t.Context(), &archive, targets))
	extracted := t.TempDir()
	_, err = ExtractTarStream(t.Context(), bytes.NewReader(archive.Bytes()), extracted)
	require.NoError(t, err)

	stats, err := Importer{Host: "devbox", DB: database}.ImportExtracted(t.Context(), targets, extracted)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	sess, err := database.GetSessionFull(t.Context(), "devbox~codebuddy:run-1")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.SessionName)
	assert.Equal(t, "Remote title", *sess.SessionName, "the remote catalog supplies the title")
	require.NotNil(t, sess.FilePath)
	assert.Equal(t, "devbox:"+filepath.Join(home, "projects", "-work", "run-1.jsonl"), *sess.FilePath)
}
