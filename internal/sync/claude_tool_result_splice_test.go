package sync

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const (
	claudeSpliceMainID     = "parent-session"
	claudeSpliceSubagentID = "agent-x"
)

func newClaudeSpliceEngine(t *testing.T, database *db.DB, root string) *Engine {
	t.Helper()
	return newClaudeSpliceEngineWith(t, database, root, EngineConfig{})
}

func newClaudeSpliceEngineWith(
	t *testing.T, database *db.DB, root string, cfg EngineConfig,
) *Engine {
	t.Helper()
	cfg.AgentDirs = map[parser.AgentType][]string{parser.AgentClaude: {root}}
	cfg.Machine = "local"
	e := NewEngine(t.Context(), database, cfg)
	t.Cleanup(e.Close)
	return e
}

// claudeSpliceLayout is the Claude agent directory used by these tests: a
// parent transcript, its tool-results side file, and a sub-agent transcript.
type claudeSpliceLayout struct {
	main, sideFile, subagent string
}

func claudeSpliceFiles(root string) claudeSpliceLayout {
	session := filepath.Join(root, "project", claudeSpliceMainID)
	return claudeSpliceLayout{
		main:     session + ".jsonl",
		sideFile: filepath.Join(session, "tool-results", "r1.txt"),
		subagent: filepath.Join(session, "subagents", claudeSpliceSubagentID+".jsonl"),
	}
}

// claudeTaggedPlaceholder is the placeholder Claude Code writes in place of a
// tool output of size bytes that it moved to resultPath.
func claudeTaggedPlaceholder(resultPath string, size int) string {
	return "<persisted-output>\nOutput too large (" + strconv.Itoa(size) +
		"B). Full output saved to: " + resultPath +
		"\n\nPreview (first 2KB):\npreview only\n</persisted-output>"
}

// claudePersistedOutputField is the toolUseResult Claude Code records beside
// that placeholder.
func claudePersistedOutputField(t *testing.T, resultPath string, size int) string {
	t.Helper()
	return `{"persistedOutputPath":` + mustSyncJSONString(t, resultPath) +
		`,"persistedOutputSize":` + strconv.Itoa(size) + `}`
}

type claudeSpliceResult struct {
	toolUseID, content string
}

// claudeEntries builds a linear Claude transcript on testjsonl.SessionBuilder:
// each entry has a fresh uuid, the previous entry's uuid as its parent, and a
// timestamp one second after the previous entry. The lines are written out in
// full so that rebuilding a transcript reproduces its earlier lines byte for
// byte, which the append-only sync relies on.
type claudeEntries struct {
	*testjsonl.SessionBuilder
	t *testing.T
	n int
}

func newClaudeEntries(t *testing.T) *claudeEntries {
	t.Helper()
	return &claudeEntries{SessionBuilder: testjsonl.NewSessionBuilder(), t: t}
}

func (c *claudeEntries) add(entryType, content, extra string) {
	parent := ""
	if c.n > 0 {
		parent = `,"parentUuid":"e` + strconv.Itoa(c.n-1) + `"`
	} else {
		extra += `,"cwd":"/tmp/project"`
	}
	ts := time.Date(2024, 1, 1, 0, 0, c.n, 0, time.UTC).Format(time.RFC3339)
	c.AddRaw(`{"type":"` + entryType + `","timestamp":"` + ts +
		`","uuid":"e` + strconv.Itoa(c.n) + `"` + parent +
		`,"message":{"content":` + content + `}` + extra + `}`)
	c.n++
}

func (c *claudeEntries) user(text string) {
	c.add("user", mustSyncJSONString(c.t, text), "")
}

func (c *claudeEntries) toolUses(ids ...string) {
	blocks := make([]string, 0, len(ids))
	for _, id := range ids {
		blocks = append(blocks, `{"type":"tool_use","id":`+mustSyncJSONString(c.t, id)+
			`,"name":"Bash","input":{"command":"make logs"}}`)
	}
	c.add("assistant", "["+strings.Join(blocks, ",")+"]", "")
}

func (c *claudeEntries) toolResults(toolUseResult string, results ...claudeSpliceResult) {
	blocks := make([]string, 0, len(results))
	for _, r := range results {
		blocks = append(blocks, `{"type":"tool_result","tool_use_id":`+
			mustSyncJSONString(c.t, r.toolUseID)+
			`,"content":`+mustSyncJSONString(c.t, r.content)+`,"is_error":false}`)
	}
	extra := ""
	if toolUseResult != "" {
		extra = `,"toolUseResult":` + toolUseResult
	}
	c.add("user", "["+strings.Join(blocks, ",")+"]", extra)
}

// claudeMainEntries is the transcript of
// TestParseClaudeSession_ResolvesPersistedToolResultOutput: a Bash call whose
// result is the tagged placeholder for a size-byte output saved to resultPath,
// with toolUseResult.persistedOutputPath.
func claudeMainEntries(t *testing.T, resultPath string, size int) *claudeEntries {
	t.Helper()
	c := newClaudeEntries(t)
	c.user("run it")
	c.toolUses("toolu_1")
	c.toolResults(claudePersistedOutputField(t, resultPath, size),
		claudeSpliceResult{"toolu_1", claudeTaggedPlaceholder(resultPath, size)})
	return c
}

func claudeMainTranscript(
	t *testing.T, resultPath string, size int, extraUser ...string,
) string {
	t.Helper()
	c := claudeMainEntries(t, resultPath, size)
	for _, text := range extraUser {
		c.user(text)
	}
	return c.String()
}

// claudeSubagentTranscript has the same shape, but the result is the bare
// "Full output saved to:" line. The entry keeps
// toolUseResult.persistedOutputPath: without it, or the <persisted-output>
// tag, the parser never reads the side file, so the first sync could not
// archive the full output.
func claudeSubagentTranscript(
	t *testing.T, resultPath string, size int, extraUser ...string,
) string {
	t.Helper()
	c := newClaudeEntries(t)
	c.user("run it")
	c.toolUses("toolu_1")
	c.toolResults(claudePersistedOutputField(t, resultPath, size),
		claudeSpliceResult{"toolu_1", "Full output saved to: " + resultPath})
	for _, text := range extraUser {
		c.user(text)
	}
	return c.String()
}

// claudeSpliceStoredResults returns every stored tool call's result, in
// order.
func claudeSpliceStoredResults(t *testing.T, database *db.DB, id string) []string {
	t.Helper()
	messages, err := database.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	var results []string
	for _, m := range messages {
		for _, call := range m.ToolCalls {
			results = append(results, call.ResultContent)
		}
	}
	return results
}

// claudeSpliceMessageTexts returns the content of every stored message, in
// order.
func claudeSpliceMessageTexts(t *testing.T, database *db.DB, id string) []string {
	t.Helper()
	messages, err := database.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	texts := make([]string, 0, len(messages))
	for _, m := range messages {
		texts = append(texts, m.Content)
	}
	return texts
}

// storedFirstToolCall returns the first stored tool call of a session.
func storedFirstToolCall(t *testing.T, database *db.DB, id string) db.ToolCall {
	t.Helper()
	messages, err := database.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	for _, m := range messages {
		if len(m.ToolCalls) > 0 {
			return m.ToolCalls[0]
		}
	}
	require.Fail(t, "no stored tool call", "session %s", id)
	return db.ToolCall{}
}

// assertBlockedClaudeResult checks a call stored under a blocked category:
// no text in the summary or any result event, with the result's length kept
// on the call and on each event.
func assertBlockedClaudeResult(t *testing.T, call db.ToolCall, length int) {
	t.Helper()
	assert.Empty(t, call.ResultContent)
	assert.Equal(t, length, call.ResultContentLength)
	require.NotEmpty(t, call.ResultEvents)
	for i, event := range call.ResultEvents {
		assert.Empty(t, event.Content, "event %d", i)
		assert.Equal(t, length, event.ContentLength, "event %d", i)
	}
}

// claudeSpliceLog is a log destination safe for the engine's goroutines.
type claudeSpliceLog struct {
	mu  gosync.Mutex
	buf bytes.Buffer
}

func (l *claudeSpliceLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *claudeSpliceLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureLog sends the standard logger to a buffer until the test ends.
func captureLog(t *testing.T) *claudeSpliceLog {
	t.Helper()
	buf := &claudeSpliceLog{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

// claudeSpliceRuns are the ways a changed transcript is synced again. Keep
// "append only" first: cases that need a full re-parse use claudeSpliceRuns[1:].
// "append only" pins existing behaviour: an incremental sync parses only the
// appended lines, never re-reads the placeholder, and kept the archived output
// before the splice existed.
var claudeSpliceRuns = []struct {
	name string
	run  func(t *testing.T, e *Engine)
}{
	{"append only", func(t *testing.T, e *Engine) {
		t.Helper()
		e.SyncAll(t.Context(), nil)
	}},
	{"full re-parse", func(t *testing.T, e *Engine) {
		t.Helper()
		e.SyncAllForceParse(t.Context(), nil)
	}},
	{"rebuild", func(t *testing.T, e *Engine) {
		t.Helper()
		_, err := e.ResyncAllWithOptions(t.Context(), nil, RebuildOptions{})
		require.NoError(t, err)
	}},
}

// TestClaudeMissingPersistedOutputKeepsArchivedResult: Claude Code moves a
// large tool output to a side file and later deletes it. A re-parse then
// holds only the placeholder, which must not replace the full output the
// archive holds, on append, full re-parse and rebuild, for a main session and
// a sub-agent. The new message still lands.
func TestClaudeMissingPersistedOutputKeepsArchivedResult(t *testing.T) {
	fullOutput := strings.Repeat("full output line\n", 64)
	sessions := []struct {
		name       string
		id         string
		transcript func(t *testing.T, resultPath string, size int, extraUser ...string) string
		path       func(files claudeSpliceLayout) string
	}{
		{
			"main session", claudeSpliceMainID, claudeMainTranscript,
			func(files claudeSpliceLayout) string { return files.main },
		},
		{
			"subagent", claudeSpliceSubagentID, claudeSubagentTranscript,
			func(files claudeSpliceLayout) string { return files.subagent },
		},
	}
	for _, s := range sessions {
		for _, run := range claudeSpliceRuns {
			t.Run(s.name+"/"+run.name, func(t *testing.T) {
				root := t.TempDir()
				files := claudeSpliceFiles(root)
				transcriptPath := s.path(files)
				dbtest.WriteTestFile(t, files.sideFile, []byte(fullOutput))
				dbtest.WriteTestFile(t, transcriptPath,
					[]byte(s.transcript(t, files.sideFile, len(fullOutput))))
				database := openTestDB(t)
				e := newClaudeSpliceEngine(t, database, root)
				e.SyncAll(t.Context(), nil)
				require.Equal(t, []string{fullOutput}, claudeSpliceStoredResults(t, database, s.id))
				texts := claudeSpliceMessageTexts(t, database, s.id)

				require.NoError(t, os.Remove(files.sideFile))
				dbtest.WriteTestFile(t, transcriptPath,
					[]byte(s.transcript(t, files.sideFile, len(fullOutput), "and again")))
				run.run(t, e)
				assert.Equal(t, []string{fullOutput}, claudeSpliceStoredResults(t, database, s.id),
					"a placeholder must not replace the archived result")
				call := storedFirstToolCall(t, database, s.id)
				assert.Equal(t, len(fullOutput), call.ResultContentLength)
				require.NotEmpty(t, call.ResultEvents)
				assert.Equal(t, fullOutput, call.ResultEvents[0].Content)
				assert.Len(t, claudeSpliceMessageTexts(t, database, s.id), len(texts)+1,
					"the new message must land")
			})
		}
	}
}

// TestClaudePresentPersistedOutputReplaces is the control: a side file that is
// present and changed replaces the archived result on a full re-parse.
func TestClaudePresentPersistedOutputReplaces(t *testing.T) {
	fullOutput := strings.Repeat("full output line\n", 64)
	root := t.TempDir()
	files := claudeSpliceFiles(root)
	dbtest.WriteTestFile(t, files.sideFile, []byte(fullOutput))
	dbtest.WriteTestFile(t, files.main,
		[]byte(claudeMainTranscript(t, files.sideFile, len(fullOutput))))
	database := openTestDB(t)
	e := newClaudeSpliceEngine(t, database, root)
	e.SyncAll(t.Context(), nil)
	require.Equal(t, []string{fullOutput}, claudeSpliceStoredResults(t, database, claudeSpliceMainID))

	// Only the side file changes; the transcript's earlier lines, including the
	// size its placeholder states, stay as written.
	corrected := fullOutput + "corrected tail\n"
	dbtest.WriteTestFile(t, files.sideFile, []byte(corrected))
	dbtest.WriteTestFile(t, files.main,
		[]byte(claudeMainTranscript(t, files.sideFile, len(fullOutput), "and again")))
	e.SyncAllForceParse(t.Context(), nil)
	assert.Equal(t, []string{corrected}, claudeSpliceStoredResults(t, database, claudeSpliceMainID))
}

// TestClaudeSpliceEdgeCases covers the inputs the splice must leave alone or
// match one by one.
func TestClaudeSpliceEdgeCases(t *testing.T) {
	fullOutput := strings.Repeat("full output line\n", 64)

	// The side file was already gone at the first sync, so the archive holds
	// the placeholder too: nothing is spliced.
	t.Run("archive also placeholder", func(t *testing.T) {
		root := t.TempDir()
		files := claudeSpliceFiles(root)
		dbtest.WriteTestFile(t, files.main,
			[]byte(claudeMainTranscript(t, files.sideFile, len(fullOutput))))
		database := openTestDB(t)
		e := newClaudeSpliceEngine(t, database, root)
		e.SyncAll(t.Context(), nil)

		dbtest.WriteTestFile(t, files.main,
			[]byte(claudeMainTranscript(t, files.sideFile, len(fullOutput), "and again")))
		logs := captureLog(t)
		e.SyncAllForceParse(t.Context(), nil)
		results := claudeSpliceStoredResults(t, database, claudeSpliceMainID)
		require.Len(t, results, 1)
		assert.Equal(t, claudeTaggedPlaceholder(files.sideFile, len(fullOutput)), results[0])
		assert.True(t, parser.IsPersistedToolResultPlaceholder(results[0]))
		assert.NotContains(t, logs.String(), "preserve claude")
	})

	// The blocked-category policy applies to spliced text as to parsed text.
	// An archive written before Bash was blocked holds the full output; a
	// re-parse under the block, with the side file gone, stores the result the
	// way a parsed full Bash result is stored: blank, with the full length.
	t.Run("blocked category", func(t *testing.T) {
		blocked := EngineConfig{BlockedResultCategories: []string{"Bash"}}

		// How a parsed full Bash result is stored under the block. This also
		// confirms that the parser files a Bash call under "Bash".
		parsedRoot := t.TempDir()
		parsedFiles := claudeSpliceFiles(parsedRoot)
		dbtest.WriteTestFile(t, parsedFiles.sideFile, []byte(fullOutput))
		dbtest.WriteTestFile(t, parsedFiles.main,
			[]byte(claudeMainTranscript(t, parsedFiles.sideFile, len(fullOutput))))
		parsedDB := openTestDB(t)
		newClaudeSpliceEngineWith(t, parsedDB, parsedRoot, blocked).SyncAll(t.Context(), nil)
		require.Equal(t, []string{""}, claudeSpliceStoredResults(t, parsedDB, claudeSpliceMainID))
		require.Equal(t, len(fullOutput),
			storedFirstToolCall(t, parsedDB, claudeSpliceMainID).ResultContentLength)

		root := t.TempDir()
		files := claudeSpliceFiles(root)
		dbtest.WriteTestFile(t, files.sideFile, []byte(fullOutput))
		dbtest.WriteTestFile(t, files.main,
			[]byte(claudeMainTranscript(t, files.sideFile, len(fullOutput))))
		database := openTestDB(t)
		newClaudeSpliceEngine(t, database, root).SyncAll(t.Context(), nil)
		require.Equal(t, []string{fullOutput}, claudeSpliceStoredResults(t, database, claudeSpliceMainID))

		require.NoError(t, os.Remove(files.sideFile))
		dbtest.WriteTestFile(t, files.main,
			[]byte(claudeMainTranscript(t, files.sideFile, len(fullOutput), "and again")))
		e := newClaudeSpliceEngineWith(t, database, root, blocked)
		e.SyncAllForceParse(t.Context(), nil)
		assert.Equal(t, []string{""}, claudeSpliceStoredResults(t, database, claudeSpliceMainID))
		assertBlockedClaudeResult(t,
			storedFirstToolCall(t, database, claudeSpliceMainID), len(fullOutput))
	})

	// An archive written under the block holds no text, only the result's
	// length. Once the side file is gone, a re-parse under the same block keeps
	// that length instead of the placeholder's, and the text stays blank.
	t.Run("blocked category, archive written blocked", func(t *testing.T) {
		blocked := EngineConfig{BlockedResultCategories: []string{"Bash"}}
		for _, run := range claudeSpliceRuns[1:] {
			t.Run(run.name, func(t *testing.T) {
				root := t.TempDir()
				files := claudeSpliceFiles(root)
				dbtest.WriteTestFile(t, files.sideFile, []byte(fullOutput))
				dbtest.WriteTestFile(t, files.main,
					[]byte(claudeMainTranscript(t, files.sideFile, len(fullOutput))))
				database := openTestDB(t)
				e := newClaudeSpliceEngineWith(t, database, root, blocked)
				e.SyncAll(t.Context(), nil)
				require.Equal(t, []string{""}, claudeSpliceStoredResults(t, database, claudeSpliceMainID))
				archived := storedFirstToolCall(t, database, claudeSpliceMainID)
				require.Equal(t, len(fullOutput), archived.ResultContentLength)
				require.Len(t, archived.ResultEvents, 1)

				require.NoError(t, os.Remove(files.sideFile))
				dbtest.WriteTestFile(t, files.main,
					[]byte(claudeMainTranscript(t, files.sideFile, len(fullOutput), "and again")))
				logs := captureLog(t)
				run.run(t, e)
				assert.Equal(t, []string{""}, claudeSpliceStoredResults(t, database, claudeSpliceMainID))
				call := storedFirstToolCall(t, database, claudeSpliceMainID)
				assertBlockedClaudeResult(t, call, len(fullOutput))
				assert.Equal(t, archived.ResultEvents[0].RawContentDigest,
					call.ResultEvents[0].RawContentDigest,
					"the event keeps the digest of the archived output, not the placeholder's")
				assert.Contains(t, logs.String(), "kept 1 archived tool results (1 as length only)")
			})
		}
	})

	// A new call in a resumed session whose side file is already missing has
	// no archived result: its placeholder is stored, the old call keeps its
	// archived output.
	t.Run("new call", func(t *testing.T) {
		root := t.TempDir()
		files := claudeSpliceFiles(root)
		dbtest.WriteTestFile(t, files.sideFile, []byte(fullOutput))
		dbtest.WriteTestFile(t, files.main,
			[]byte(claudeMainTranscript(t, files.sideFile, len(fullOutput))))
		database := openTestDB(t)
		e := newClaudeSpliceEngine(t, database, root)
		e.SyncAll(t.Context(), nil)
		require.Equal(t, []string{fullOutput}, claudeSpliceStoredResults(t, database, claudeSpliceMainID))

		require.NoError(t, os.Remove(files.sideFile))
		missing := filepath.Join(filepath.Dir(files.sideFile), "r2.txt")
		c := claudeMainEntries(t, files.sideFile, len(fullOutput))
		c.toolUses("toolu_2")
		c.toolResults(claudePersistedOutputField(t, missing, len(fullOutput)),
			claudeSpliceResult{"toolu_2", claudeTaggedPlaceholder(missing, len(fullOutput))})
		dbtest.WriteTestFile(t, files.main, []byte(c.String()))
		e.SyncAllForceParse(t.Context(), nil)
		assert.Equal(t, []string{fullOutput, claudeTaggedPlaceholder(missing, len(fullOutput))},
			claudeSpliceStoredResults(t, database, claudeSpliceMainID))
	})

	// Two placeholder results in one user entry are each matched to their own
	// archived call by tool_use_id, and the session is logged once.
	t.Run("two results", func(t *testing.T) {
		root := t.TempDir()
		files := claudeSpliceFiles(root)
		second := filepath.Join(filepath.Dir(files.sideFile), "r2.txt")
		secondOutput := strings.Repeat("second output line\n", 64)
		dbtest.WriteTestFile(t, files.sideFile, []byte(fullOutput))
		dbtest.WriteTestFile(t, second, []byte(secondOutput))
		transcript := func(extraUser ...string) string {
			c := newClaudeEntries(t)
			c.user("run them")
			c.toolUses("toolu_1", "toolu_2")
			c.toolResults("",
				claudeSpliceResult{"toolu_1", claudeTaggedPlaceholder(files.sideFile, len(fullOutput))},
				claudeSpliceResult{"toolu_2", claudeTaggedPlaceholder(second, len(secondOutput))})
			for _, text := range extraUser {
				c.user(text)
			}
			return c.String()
		}
		dbtest.WriteTestFile(t, files.main, []byte(transcript()))
		database := openTestDB(t)
		e := newClaudeSpliceEngine(t, database, root)
		e.SyncAll(t.Context(), nil)
		require.Equal(t, []string{fullOutput, secondOutput},
			claudeSpliceStoredResults(t, database, claudeSpliceMainID))

		require.NoError(t, os.Remove(files.sideFile))
		require.NoError(t, os.Remove(second))
		dbtest.WriteTestFile(t, files.main, []byte(transcript("and again")))
		logs := captureLog(t)
		e.SyncAllForceParse(t.Context(), nil)
		assert.Equal(t, []string{fullOutput, secondOutput},
			claudeSpliceStoredResults(t, database, claudeSpliceMainID))
		assert.Equal(t, 1, strings.Count(logs.String(), "preserve claude"))
		assert.Contains(t, logs.String(), "kept 2 archived tool results whose")
		assert.NotContains(t, logs.String(), "as length only")
	})

	// A real output may quote the placeholder's markers (a grep over
	// transcripts, a dump of the parser). It is not a placeholder, so once its
	// side file is gone the archived text is kept like any other.
	t.Run("output quoting the markers", func(t *testing.T) {
		quoting := "line 1\nfound <persisted-output> in transcript\n" +
			"Full output saved to: /somewhere/else.txt\n" + fullOutput
		for _, run := range claudeSpliceRuns[1:] {
			t.Run(run.name, func(t *testing.T) {
				root := t.TempDir()
				files := claudeSpliceFiles(root)
				dbtest.WriteTestFile(t, files.sideFile, []byte(quoting))
				dbtest.WriteTestFile(t, files.main,
					[]byte(claudeMainTranscript(t, files.sideFile, len(quoting))))
				database := openTestDB(t)
				e := newClaudeSpliceEngine(t, database, root)
				e.SyncAll(t.Context(), nil)
				require.Equal(t, []string{quoting}, claudeSpliceStoredResults(t, database, claudeSpliceMainID))

				require.NoError(t, os.Remove(files.sideFile))
				dbtest.WriteTestFile(t, files.main,
					[]byte(claudeMainTranscript(t, files.sideFile, len(quoting), "and again")))
				run.run(t, e)
				assert.Equal(t, []string{quoting}, claudeSpliceStoredResults(t, database, claudeSpliceMainID))
			})
		}
	})

	// A session with no placeholder results is written as parsed and never
	// logged as preserved.
	t.Run("no placeholders", func(t *testing.T) {
		root := t.TempDir()
		files := claudeSpliceFiles(root)
		transcript := func(extraUser ...string) string {
			c := newClaudeEntries(t)
			c.user("run it")
			c.toolUses("toolu_1")
			c.toolResults("", claudeSpliceResult{"toolu_1", "small inline result"})
			for _, text := range extraUser {
				c.user(text)
			}
			return c.String()
		}
		dbtest.WriteTestFile(t, files.main, []byte(transcript()))
		database := openTestDB(t)
		e := newClaudeSpliceEngine(t, database, root)
		e.SyncAll(t.Context(), nil)

		dbtest.WriteTestFile(t, files.main, []byte(transcript("and again")))
		logs := captureLog(t)
		e.SyncAllForceParse(t.Context(), nil)
		assert.Equal(t, []string{"small inline result"},
			claudeSpliceStoredResults(t, database, claudeSpliceMainID))
		assert.NotContains(t, logs.String(), "preserve claude")
	})

	// This pins existing behaviour: an unchanged transcript is not re-parsed, so
	// deleting only the side file kept the archived output before the splice
	// existed.
	t.Run("unchanged transcript", func(t *testing.T) {
		root := t.TempDir()
		files := claudeSpliceFiles(root)
		dbtest.WriteTestFile(t, files.sideFile, []byte(fullOutput))
		dbtest.WriteTestFile(t, files.main,
			[]byte(claudeMainTranscript(t, files.sideFile, len(fullOutput))))
		database := openTestDB(t)
		e := newClaudeSpliceEngine(t, database, root)
		e.SyncAll(t.Context(), nil)
		require.Equal(t, []string{fullOutput}, claudeSpliceStoredResults(t, database, claudeSpliceMainID))

		require.NoError(t, os.Remove(files.sideFile))
		e.SyncAll(t.Context(), nil)
		assert.Equal(t, []string{fullOutput}, claudeSpliceStoredResults(t, database, claudeSpliceMainID))
	})
}
