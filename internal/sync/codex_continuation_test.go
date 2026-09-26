package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Codex writes a long thread across more than one rollout file. The second
// file is named `rollout-<timestamp>-<thread>_<rollout>.jsonl`, its
// session_meta carries the thread's own id, `history_mode: "paginated"` and a
// `history_base` naming the thread and the ordinal the previous file stopped
// at, and its entries continue the thread's ordinal sequence.
//
// These tests pin that such a file is read and its messages are added to the
// thread it continues, rather than being dropped.
func codexPaginatedContinuationMetaJSON(
	threadID, cwd, timestamp string, baseOrdinal int,
) string {
	return testjsonl.CodexSessionMetaWithFieldsJSON(
		threadID, cwd, "codex_cli_rs", timestamp,
		map[string]any{
			"history_mode": "paginated",
			"history_base": map[string]any{
				"thread_id":             threadID,
				"end_ordinal_exclusive": baseOrdinal,
			},
		},
	)
}

func codexUserEntryJSON(timestamp, text string) string {
	return `{"type":"response_item","timestamp":"` + timestamp +
		`","payload":{"type":"message","role":"user","content":` +
		`[{"type":"input_text","text":"` + text + `"}]}}`
}

// writeCodexRollout writes one rollout file into the dated Codex layout and
// returns its path.
func writeCodexRollout(t *testing.T, root, day, name, content string) string {
	t.Helper()
	dir := filepath.Join(root, "2026", "09", day)
	require.NoError(t, os.MkdirAll(dir, 0o755), "mkdir codex day dir")
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600), "write rollout")
	return path
}

// TestCodexPaginatedContinuationJoinsItsThread pins the whole defect: a thread
// whose rollout continued in a second file was stored ending at the first
// file's last entry, and the continuation was not recorded as skipped either,
// so the missing stretch was invisible.
func TestCodexPaginatedContinuationJoinsItsThread(t *testing.T) {
	const (
		threadID  = "019eb791-cf7d-75c1-8439-9ed74c122b05"
		rolloutID = "019eb7a5-48f1-7283-99e7-f98e215e72b2"
		sessionID = "codex:" + threadID
	)
	root := t.TempDir()
	writeCodexRollout(t, root, "01",
		"rollout-2026-09-01T10-00-00-"+threadID+".jsonl",
		testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON(
				threadID, "/work/project", "codex_cli_rs",
				"2026-09-01T10:00:00Z"),
			codexUserEntryJSON("2026-09-01T10:00:01Z", "first file one"),
			codexUserEntryJSON("2026-09-01T10:00:02Z", "first file two"),
		))
	writeCodexRollout(t, root, "01",
		"rollout-2026-09-01T11-00-00-"+threadID+"_"+rolloutID+".jsonl",
		testjsonl.JoinJSONL(
			codexPaginatedContinuationMetaJSON(
				threadID, "/work/project", "2026-09-01T11:00:00Z", 3),
			codexUserEntryJSON("2026-09-01T11:00:01Z", "second file one"),
			codexUserEntryJSON("2026-09-01T11:30:00Z", "second file two"),
		))

	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)

	messages, err := database.GetMessages(t.Context(), sessionID, 0, 100, true)
	require.NoError(t, err, "GetMessages")
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		texts = append(texts, message.Content)
	}
	assert.Equal(t,
		[]string{
			"first file one", "first file two",
			"second file one", "second file two",
		},
		texts,
		"the continuation's messages belong to the thread it continues")

	session, err := database.GetSession(t.Context(), sessionID)
	require.NoError(t, err, "GetSession")
	require.NotNil(t, session, "the thread must be stored")
	require.NotNil(t, session.EndedAt, "ended_at")
	assert.Equal(t, "2026-09-01T11:30:00Z", *session.EndedAt,
		"the thread ends at the continuation's last entry")
	assert.Equal(t, 4, session.MessageCount,
		"the stored message count covers the whole chain")
	assert.Equal(t,
		filepath.Join(root, "2026", "09", "01",
			"rollout-2026-09-01T10-00-00-"+threadID+".jsonl"),
		database.GetSessionFilePath(t.Context(), sessionID),
		"the thread's stored file stays its own rollout")
}

// TestCodexContinuationFilenameResolvesToItsThread pins which resolver reads a
// continuation filename. The continuation names its thread through its own
// resolver, while CodexSessionUUIDFromFilename keeps resolving it to nothing:
// that function keys discovery and every stored-source lookup, and resolving
// both rollouts of one thread to the same UUID would collapse them onto one
// discovery key, where the dedupe drops one file by modification time.
func TestCodexContinuationFilenameResolvesToItsThread(t *testing.T) {
	const (
		threadID  = "019eb791-cf7d-75c1-8439-9ed74c122b05"
		rolloutID = "019eb7a5-48f1-7283-99e7-f98e215e72b2"
	)
	continuationName := "rollout-2026-09-01T11-00-00-" +
		threadID + "_" + rolloutID + ".jsonl"

	assert.Equal(t, threadID,
		parser.CodexContinuationThreadUUIDFromFilename(continuationName),
		"a continuation filename names the thread it continues")
	assert.Empty(t, parser.CodexContinuationThreadUUIDFromFilename(
		"rollout-2026-09-01T10-00-00-"+threadID+".jsonl"),
		"a thread's own rollout is not a continuation of anything")
	assert.Empty(t, parser.CodexSessionUUIDFromFilename(continuationName),
		"the discovery resolver still refuses a continuation filename, so the "+
			"two rollouts cannot collapse onto one discovery key")
	assert.Equal(t, threadID, parser.CodexSessionUUIDFromFilename(
		"rollout-2026-09-01T10-00-00-"+threadID+".jsonl"),
		"a thread's own rollout filename still resolves to itself")
	assert.Empty(t, parser.CodexSessionUUIDFromFilename(
		"rollout-2026-09-01T10-00-00-not-a-uuid.jsonl"),
		"a name carrying no UUID still resolves to nothing")
}

// TestCodexContinuationKeepsADiscoveryKeyOfItsOwn pins the discovery key both
// rollouts of one thread must keep: distinct keys, so
// dedupeDiscoveredFilesByPreference can never drop one of them and leave a
// stretch of the thread unread.
func TestCodexContinuationKeepsADiscoveryKeyOfItsOwn(t *testing.T) {
	const (
		threadID  = "019eb791-cf7d-75c1-8439-9ed74c122b05"
		rolloutID = "019eb7a5-48f1-7283-99e7-f98e215e72b2"
	)
	head := parser.DiscoveredFile{
		Agent: parser.AgentCodex,
		Path: "/codex/2026/09/01/rollout-2026-09-01T10-00-00-" +
			threadID + ".jsonl",
	}
	continuation := parser.DiscoveredFile{
		Agent: parser.AgentCodex,
		Path: "/codex/2026/09/01/rollout-2026-09-01T11-00-00-" +
			threadID + "_" + rolloutID + ".jsonl",
	}
	assert.NotEqual(t,
		discoveredFileKey(head), discoveredFileKey(continuation),
		"a thread's rollout and its continuation keep separate discovery keys")
}

// TestCodexContinuationWithoutItsThreadStaysItsOwnSource pins the guard's
// refusal. A continuation whose thread's own rollout is not beside it is not
// joined on the strength of its filename: it keeps its own source, so its
// content is still read and the file stays visible rather than being silently
// dropped or recorded as skipped.
func TestCodexContinuationWithoutItsThreadStaysItsOwnSource(t *testing.T) {
	const (
		threadID  = "019eb791-cf7d-75c1-8439-9ed74c122b05"
		rolloutID = "019eb7a5-48f1-7283-99e7-f98e215e72b2"
	)
	root := t.TempDir()
	orphan := writeCodexRollout(t, root, "01",
		"rollout-2026-09-01T11-00-00-"+threadID+"_"+rolloutID+".jsonl",
		testjsonl.JoinJSONL(
			codexPaginatedContinuationMetaJSON(
				threadID, "/work/project", "2026-09-01T11:00:00Z", 3),
			codexUserEntryJSON("2026-09-01T11:00:01Z", "orphan continuation"),
		))

	provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{
		Roots: []string{root}, Machine: "local",
	})
	require.True(t, ok, "construct codex provider")
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err, "Discover")

	paths := make([]string, 0, len(sources))
	for _, source := range sources {
		paths = append(paths, source.DisplayPath)
	}
	assert.Contains(t, paths, orphan,
		"a continuation with no thread beside it keeps its own source")
}
