package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const (
	compressedRolloutThread  = "019eb791-cf7d-75c1-8439-9ed74c122b05"
	compressedRolloutSession = "codex:" + compressedRolloutThread
	compressedRolloutPlain   = "rollout-2026-09-01T10-00-00-" + compressedRolloutThread + ".jsonl"
)

func compressedRolloutContent(entries ...string) string {
	lines := []string{
		testjsonl.CodexSessionMetaJSON(
			compressedRolloutThread, "/work/project", "codex_cli_rs",
			"2026-09-01T10:00:00Z",
		),
	}
	return testjsonl.JoinJSONL(append(lines, entries...)...)
}

func newCompressedRolloutEngine(t *testing.T, database *db.DB, root string, cfg EngineConfig) *Engine {
	t.Helper()
	cfg.AgentDirs = map[parser.AgentType][]string{parser.AgentCodex: {root}}
	cfg.Machine = "local"
	e := NewEngine(t.Context(), database, cfg)
	t.Cleanup(e.Close)
	return e
}

func compressedRolloutTexts(t *testing.T, database *db.DB, id string) []string {
	t.Helper()
	messages, err := database.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	texts := make([]string, 0, len(messages))
	for _, m := range messages {
		texts = append(texts, m.Content)
	}
	return texts
}

type compressedRolloutLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *compressedRolloutLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *compressedRolloutLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureCompressedRolloutLog sends the standard logger to a buffer until the test ends.
func captureCompressedRolloutLog(t *testing.T) *compressedRolloutLog {
	t.Helper()
	buf := &compressedRolloutLog{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

// Codex compresses a rollout that has not changed for a week to
// rollout-<ts>-<uuid>.jsonl.zst and removes the plain file, keeping the
// file's modification time. A resumed thread is unpacked back to the plain
// file in place. These tests pin that the archive follows the file through
// compression without losing, duplicating or re-parsing the session.

const codexCompressedName = compressedRolloutPlain + ".zst"

func zstdCompressString(t *testing.T, content string) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	out := enc.EncodeAll([]byte(content), nil)
	require.NoError(t, enc.Close())
	return out
}

func writeCompressedCodexRollout(t *testing.T, root, day, name, content string) string {
	t.Helper()
	dir := filepath.Join(root, "2026", "09", day)
	require.NoError(t, os.MkdirAll(dir, 0o755), "mkdir codex day dir")
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, zstdCompressString(t, content), 0o600), "write compressed rollout")
	return path
}

func countCodexSessions(t *testing.T, database *db.DB) int {
	t.Helper()
	var count int
	err := database.Reader().QueryRow(t.Context(),
		"SELECT COUNT(*) FROM sessions WHERE agent = ?", "codex",
	).Scan(&count)
	require.NoError(t, err)
	return count
}

func codexSessionFilePath(t *testing.T, database *db.DB, id string) string {
	t.Helper()
	stored, err := database.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, stored, "session %s must exist", id)
	require.NotNil(t, stored.FilePath)
	return *stored.FilePath
}

var codexCompressedRuns = []struct {
	name string
	run  func(t *testing.T, e *Engine)
}{
	{"sync", func(t *testing.T, e *Engine) {
		t.Helper()
		e.SyncAll(t.Context(), nil)
	}},
	{"rebuild", func(t *testing.T, e *Engine) {
		t.Helper()
		_, err := e.ResyncAllWithOptions(t.Context(), nil, RebuildOptions{})
		require.NoError(t, err)
	}},
}

// TestCodexCompressedRolloutStoresSameMessagesAsPlain: a rollout that only
// ever existed compressed is archived exactly as the plain file would be,
// through an ordinary sync and through a rebuild.
func TestCodexCompressedRolloutStoresSameMessagesAsPlain(t *testing.T) {
	one := codexUserEntryJSON("2026-09-01T10:00:01Z", "one")
	two := codexAssistantEntryJSON("2026-09-01T10:00:02Z", "two")
	three := codexUserEntryJSON("2026-09-01T10:00:03Z", "three")
	content := compressedRolloutContent(one, two, three)

	plainRoot := t.TempDir()
	writeCodexRollout(t, plainRoot, "01", compressedRolloutPlain, content)
	plainDB := openTestDB(t)
	newCompressedRolloutEngine(t, plainDB, plainRoot, EngineConfig{}).SyncAll(t.Context(), nil)
	want := compressedRolloutTexts(t, plainDB, compressedRolloutSession)
	require.Equal(t, []string{"one", "two", "three"}, want)

	for _, run := range codexCompressedRuns {
		t.Run(run.name, func(t *testing.T) {
			root := t.TempDir()
			compressed := writeCompressedCodexRollout(t, root, "01", codexCompressedName, content)
			database := openTestDB(t)
			e := newCompressedRolloutEngine(t, database, root, EngineConfig{})
			run.run(t, e)
			assert.Equal(t, want, compressedRolloutTexts(t, database, compressedRolloutSession))
			assert.Equal(t, compressed, codexSessionFilePath(t, database, compressedRolloutSession))

			// A cold sync has no stored fingerprint, so the engine keeps the
			// hash the parse reports. It must be the compressed file's own,
			// the one Fingerprint computes for the path on later passes.
			raw, err := os.ReadFile(compressed)
			require.NoError(t, err)
			sum := sha256.Sum256(raw)
			stored, ok := database.GetFileHashByAgentPath(t.Context(), compressed, "codex")
			require.True(t, ok)
			assert.Equal(t, hex.EncodeToString(sum[:]), stored,
				"the stored hash must be of the compressed bytes, not the unpacked ones")
		})
	}
}

// TestCodexPlainRolloutWinsUntilCompressed: with both files present the
// plain one is the session's source; after Codex removes it, the session
// keeps its id and messages and its path moves to the compressed file.
func TestCodexPlainRolloutWinsUntilCompressed(t *testing.T) {
	one := codexUserEntryJSON("2026-09-01T10:00:01Z", "one")
	two := codexAssistantEntryJSON("2026-09-01T10:00:02Z", "two")
	content := compressedRolloutContent(one, two)
	root := t.TempDir()
	plain := writeCodexRollout(t, root, "01", compressedRolloutPlain, content)
	compressed := writeCompressedCodexRollout(t, root, "01", codexCompressedName, content)
	database := openTestDB(t)
	e := newCompressedRolloutEngine(t, database, root, EngineConfig{})

	e.SyncAll(t.Context(), nil)
	require.Equal(t, []string{"one", "two"}, compressedRolloutTexts(t, database, compressedRolloutSession))
	assert.Equal(t, plain, codexSessionFilePath(t, database, compressedRolloutSession))
	assert.Equal(t, 1, countCodexSessions(t, database), "one rollout in two forms is one session")

	require.NoError(t, os.Remove(plain))
	e.SyncAll(t.Context(), nil)
	assert.Equal(t, []string{"one", "two"}, compressedRolloutTexts(t, database, compressedRolloutSession))
	assert.Equal(t, compressed, codexSessionFilePath(t, database, compressedRolloutSession))
	assert.Equal(t, 1, countCodexSessions(t, database))
}

func TestCodexChangedPathPrefersPlainOverCompressed(t *testing.T) {
	one := codexUserEntryJSON("2026-09-01T10:00:01Z", "one")
	two := codexAssistantEntryJSON("2026-09-01T10:00:02Z", "new answer")
	root := t.TempDir()
	plain := writeCodexRollout(t, root, "01", compressedRolloutPlain, compressedRolloutContent(one, two))
	compressed := writeCompressedCodexRollout(t, root, "01", codexCompressedName, compressedRolloutContent(one))
	database := openTestDB(t)
	e := newCompressedRolloutEngine(t, database, root, EngineConfig{})
	e.SyncAll(t.Context(), nil)
	require.Equal(t, []string{"one", "new answer"}, compressedRolloutTexts(t, database, compressedRolloutSession))

	plan, err := e.PlanChangedPathsContext(t.Context(), []string{compressed})
	require.NoError(t, err)
	require.Len(t, plan.Files, 1)
	assert.Equal(t, plain, plan.Files[0].Path)
	_, err = e.SyncChangedPathPlanContext(t.Context(), plan, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"one", "new answer"}, compressedRolloutTexts(t, database, compressedRolloutSession))
	assert.Equal(t, plain, codexSessionFilePath(t, database, compressedRolloutSession))
}

// TestCodexCompressionAfterSyncKeepsArchiveAndSkipsUnchangedFile: a rollout
// synced while plain and then compressed by Codex keeps its archived rows,
// follows the path change, and is not parsed again while it stays unchanged.
func TestCodexCompressionAfterSyncKeepsArchiveAndSkipsUnchangedFile(t *testing.T) {
	one := codexUserEntryJSON("2026-09-01T10:00:01Z", "one")
	two := codexAssistantEntryJSON("2026-09-01T10:00:02Z", "two")
	three := codexUserEntryJSON("2026-09-01T10:00:03Z", "three")
	content := compressedRolloutContent(one, two, three)
	root := t.TempDir()
	plain := writeCodexRollout(t, root, "01", compressedRolloutPlain, content)
	database := openTestDB(t)
	e := newCompressedRolloutEngine(t, database, root, EngineConfig{})

	e.SyncAll(t.Context(), nil)
	require.Equal(t, []string{"one", "two", "three"}, compressedRolloutTexts(t, database, compressedRolloutSession))

	info, err := os.Stat(plain)
	require.NoError(t, err)
	compressed := writeCompressedCodexRollout(t, root, "01", codexCompressedName, content)
	require.NoError(t, os.Chtimes(compressed, info.ModTime(), info.ModTime()), "Codex keeps the mtime")
	require.NoError(t, os.Remove(plain))

	stats := e.SyncAll(t.Context(), nil)
	assert.Equal(t, 1, stats.Synced, "the compressed file is parsed once")
	assert.Equal(t, []string{"one", "two", "three"}, compressedRolloutTexts(t, database, compressedRolloutSession))
	assert.Equal(t, compressed, codexSessionFilePath(t, database, compressedRolloutSession))

	stats = e.SyncAll(t.Context(), nil)
	assert.Equal(t, 0, stats.Synced, "an unchanged compressed file is not parsed again")
	assert.Equal(t, 1, stats.Skipped)
	assert.Equal(t, []string{"one", "two", "three"}, compressedRolloutTexts(t, database, compressedRolloutSession))
}

// TestCodexCompressedRolloutThatGrewAddsMessages: the compressed file holds
// turns the last sync of the plain file never saw; the archive gains them.
func TestCodexCompressedRolloutThatGrewAddsMessages(t *testing.T) {
	one := codexUserEntryJSON("2026-09-01T10:00:01Z", "one")
	two := codexAssistantEntryJSON("2026-09-01T10:00:02Z", "two")
	three := codexUserEntryJSON("2026-09-01T10:00:03Z", "three")
	four := codexAssistantEntryJSON("2026-09-01T10:00:04Z", "four")
	root := t.TempDir()
	plain := writeCodexRollout(t, root, "01", compressedRolloutPlain, compressedRolloutContent(one, two))
	database := openTestDB(t)
	e := newCompressedRolloutEngine(t, database, root, EngineConfig{})

	e.SyncAll(t.Context(), nil)
	require.Equal(t, []string{"one", "two"}, compressedRolloutTexts(t, database, compressedRolloutSession))

	writeCompressedCodexRollout(t, root, "01", codexCompressedName, compressedRolloutContent(one, two, three, four))
	require.NoError(t, os.Remove(plain))
	e.SyncAll(t.Context(), nil)
	assert.Equal(t, []string{"one", "two", "three", "four"},
		compressedRolloutTexts(t, database, compressedRolloutSession))
}

// TestCodexTruncatedCompressedRolloutKeepsArchive: a compressed file that
// cannot be unpacked fails as a read of that source, leaves the archived
// session as it was, and is logged once.
func TestCodexTruncatedCompressedRolloutKeepsArchive(t *testing.T) {
	one := codexUserEntryJSON("2026-09-01T10:00:01Z", "one")
	two := codexAssistantEntryJSON("2026-09-01T10:00:02Z", "two")
	three := codexUserEntryJSON("2026-09-01T10:00:03Z", "three")
	content := compressedRolloutContent(one, two, three)
	for _, run := range codexCompressedRuns {
		t.Run(run.name, func(t *testing.T) {
			root := t.TempDir()
			plain := writeCodexRollout(t, root, "01", compressedRolloutPlain, content)
			database := openTestDB(t)
			e := newCompressedRolloutEngine(t, database, root, EngineConfig{})
			e.SyncAll(t.Context(), nil)
			require.Equal(t, []string{"one", "two", "three"}, compressedRolloutTexts(t, database, compressedRolloutSession))

			whole := zstdCompressString(t, content)
			compressed := filepath.Join(filepath.Dir(plain), codexCompressedName)
			require.NoError(t, os.WriteFile(compressed, whole[:len(whole)/2], 0o600))
			require.NoError(t, os.Remove(plain))

			logs := captureCompressedRolloutLog(t)
			run.run(t, e)
			assert.Equal(t, []string{"one", "two", "three"}, compressedRolloutTexts(t, database, compressedRolloutSession),
				"a file that cannot be read must not touch the archive")
			assert.Equal(t, plain, codexSessionFilePath(t, database, compressedRolloutSession),
				"the path moves only when the compressed file was read")
			mentions := 0
			for line := range strings.SplitSeq(logs.String(), "\n") {
				if strings.Contains(line, codexCompressedName) {
					mentions++
				}
			}
			assert.Equal(t, 1, mentions, "the failure is logged once:\n%s", logs.String())
		})
	}
}

func TestCodexReconciliationPrefersRestoredPlain(t *testing.T) {
	root := t.TempDir()
	content := compressedRolloutContent(codexUserEntryJSON("2026-09-01T10:00:01Z", "one"))
	compressed := writeCompressedCodexRollout(t, root, "01", codexCompressedName, content)
	database := openTestDB(t)
	e := newCompressedRolloutEngine(t, database, root, EngineConfig{})
	provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.Equal(t, compressed, sources[0].DisplayPath)
	candidate, ok := e.reconciliationCandidate(t.Context(), provider, sources[0], []string{root}, nil)
	require.True(t, ok)
	plain := writeCodexRollout(t, root, "01", compressedRolloutPlain, content)
	files, err := e.rehydrateReconciliationPage(
		t.Context(), []reconciliationCandidate{candidate},
		map[parser.AgentType]parser.Provider{parser.AgentCodex: provider}, false,
	)
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.NotNil(t, files[0].ProviderSource)
	assert.Equal(t, plain, files[0].ProviderSource.DisplayPath)
}

func TestCodexDuplicatePreferenceAcrossRoots(t *testing.T) {
	for _, tc := range []struct {
		name           string
		plainFlat      bool
		compressedFlat bool
		wantPlain      bool
	}{
		{name: "dated plain before compressed", wantPlain: true},
		{name: "flat plain before compressed", plainFlat: true, compressedFlat: true, wantPlain: true},
		{name: "dated compressed before flat plain", plainFlat: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			compressedRoot := filepath.Join(base, "a-compressed")
			plainRoot := filepath.Join(base, "z-plain")
			one := codexUserEntryJSON("2026-09-01T10:00:01Z", "one")
			two := codexAssistantEntryJSON("2026-09-01T10:00:02Z", "plain answer")
			compressed := writeCompressedCodexRollout(t, compressedRoot, "01", codexCompressedName, compressedRolloutContent(one))
			plain := writeCodexRollout(t, plainRoot, "01", compressedRolloutPlain, compressedRolloutContent(one, two))
			if tc.compressedFlat {
				flat := filepath.Join(compressedRoot, codexCompressedName)
				require.NoError(t, os.Rename(compressed, flat))
				compressed = flat
			}
			if tc.plainFlat {
				flat := filepath.Join(plainRoot, compressedRolloutPlain)
				require.NoError(t, os.Rename(plain, flat))
				plain = flat
			}
			wantPath := compressed
			wantTexts := []string{"one"}
			if tc.wantPlain {
				wantPath = plain
				wantTexts = []string{"one", "plain answer"}
			}
			database := openTestDB(t)
			roots := []string{compressedRoot, plainRoot}
			e := NewEngine(t.Context(), database, EngineConfig{
				Machine:   "local",
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: roots},
			})
			t.Cleanup(e.Close)
			require.Zero(t, e.SyncAll(t.Context(), nil).Failed)
			require.Equal(t, wantTexts, compressedRolloutTexts(t, database, compressedRolloutSession))
			require.Equal(t, wantPath, codexSessionFilePath(t, database, compressedRolloutSession))

			require.NoError(t, e.ReconcileWatchRoots(t.Context(), roots, false))
			assert.Equal(t, wantTexts, compressedRolloutTexts(t, database, compressedRolloutSession))
			assert.Equal(t, wantPath, codexSessionFilePath(t, database, compressedRolloutSession))

			for _, paths := range [][]string{{compressed}, {compressed, plain}, {plain, compressed}} {
				plan, err := e.PlanChangedPathsContext(t.Context(), paths)
				require.NoError(t, err)
				require.Len(t, plan.Files, 1)
				assert.Equal(t, wantPath, plan.Files[0].Path)
				_, err = e.SyncChangedPathPlanContext(t.Context(), plan, nil)
				require.NoError(t, err)
				assert.Equal(t, wantTexts, compressedRolloutTexts(t, database, compressedRolloutSession))
				assert.Equal(t, wantPath, codexSessionFilePath(t, database, compressedRolloutSession))
			}
		})
	}
}

func TestCodexCompressedParseAdmissionAndStaging(t *testing.T) {
	for _, tc := range []struct {
		name        string
		outputSize  int
		knownSize   bool
		staged      bool
		s3          bool
		truncated   bool
		cancelFetch bool
		idPrefix    string
	}{
		{name: "small known size", outputSize: 32, knownSize: true},
		{name: "large known size", outputSize: 512 << 10, knownSize: true, staged: true},
		{name: "unknown size", outputSize: 32, staged: true},
		{name: "S3 small known size", outputSize: 32, knownSize: true, s3: true},
		{name: "S3 large known size", outputSize: 512 << 10, knownSize: true, staged: true, s3: true},
		{name: "S3 unknown size", outputSize: 32, staged: true, s3: true},
		{name: "S3 prefixed unknown size", outputSize: 32, staged: true, s3: true, idPrefix: "mirror~"},
		{name: "S3 truncated frame", outputSize: 512 << 10, knownSize: true, s3: true, truncated: true},
		{name: "S3 canceled download", outputSize: 32, s3: true, cancelFetch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := strings.Repeat("x", tc.outputSize)
			content := compressedRolloutContent(
				codexUserEntryJSON("2026-09-01T10:00:01Z", "run command"),
				testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call-a", nil, "2026-09-01T10:00:02Z"),
				testjsonl.CodexFunctionCallOutputJSON("call-a", output, "2026-09-01T10:00:03Z"),
				testjsonl.CodexFunctionCallWithCallIDJSON("wait", "call-b", map[string]any{
					"ids": []string{"agent-1"},
				}, "2026-09-01T10:00:04Z"),
				testjsonl.CodexFunctionCallOutputJSON("call-b", map[string]any{
					"status": map[string]any{"agent-1": map[string]any{"completed": "finished"}},
				}, "2026-09-01T10:00:05Z"),
			)
			var compressed []byte
			if tc.knownSize {
				compressed = zstdCompressString(t, content)
			} else {
				var buf bytes.Buffer
				encoder, err := zstd.NewWriter(&buf)
				require.NoError(t, err)
				_, err = encoder.Write([]byte(content))
				require.NoError(t, err)
				require.NoError(t, encoder.Flush())
				require.NoError(t, encoder.Close())
				compressed = buf.Bytes()
			}
			var header zstd.Header
			require.NoError(t, header.Decode(compressed))
			require.Equal(t, tc.knownSize, header.HasFCS, "fixture must exercise the intended frame size")
			if tc.truncated {
				compressed = compressed[:len(compressed)-4]
			}

			root := t.TempDir()
			path := filepath.Join(root, codexCompressedName)
			require.NoError(t, os.WriteFile(path, compressed, 0o600))
			database := openTestDB(t)
			stagingDir := t.TempDir()
			engine := newCompressedRolloutEngine(t, database, root, EngineConfig{
				StagedCodexParseMinBytes: 32 << 10,
				CodexStagingDir:          stagingDir,
				IDPrefix:                 tc.idPrefix,
			})
			assertReleased := func() {
				t.Helper()
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				lease, err := engine.retentionBudget().acquire(ctx, -1)
				defer lease.Release()
				require.NoError(t, err, "the full budget must be available without waiting")
				entries, err := os.ReadDir(stagingDir)
				require.NoError(t, err)
				assert.Empty(t, entries, "scratch files must be removed after use")
			}
			file := parser.DiscoveredFile{
				Agent: parser.AgentCodex, Path: path, SourceSize: int64(len(compressed)),
			}
			sessionID := compressedRolloutSession
			subagentID := "codex:agent-1"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.s3 {
				file.Path = "s3://bucket/laptop/raw/codex/2026/09/01/" + codexCompressedName
				file.Machine = "laptop"
				file.SourceMtime = 1788256800000000000
				file.SourceFingerprint = "s3:compressed-fixture"
				sessionID = "laptop~" + sessionID
				subagentID = "laptop~" + subagentID
				oldFetch, oldStat := fetchS3Object, statS3Object
				t.Cleanup(func() { fetchS3Object, statS3Object = oldFetch, oldStat })
				fetchS3Object = func(uri string) (io.ReadCloser, error) {
					if uri != file.Path {
						return nil, missingS3ObjectError()
					}
					if tc.cancelFetch {
						cancel()
					}
					return io.NopCloser(bytes.NewReader(compressed)), nil
				}
				statS3Object = func(string) (parser.S3Object, error) {
					return parser.S3Object{}, missingS3ObjectError()
				}
			}
			sessionID = tc.idPrefix + sessionID
			subagentID = tc.idPrefix + subagentID
			result := engine.processFile(ctx, file)
			defer func() { result.releaseStaged(); result.retentionLease.Release() }()
			if tc.truncated || tc.cancelFetch {
				require.Error(t, result.err)
				if tc.cancelFetch {
					require.ErrorIs(t, result.err, context.Canceled)
				}
				assert.Empty(t, result.results, "corrupt input must not publish a partial transcript")
				result.releaseStaged()
				result.retentionLease.Release()
				assertReleased()
				return
			}
			require.NoError(t, result.err)
			require.Len(t, result.results, 1)
			messages := result.results[0].Messages
			require.Len(t, messages, 3)
			require.Len(t, messages[1].ToolCalls, 1)
			events := messages[1].ToolCalls[0].ResultEvents
			require.Len(t, events, 1)
			if tc.staged {
				require.NotNil(t, result.staged, "decoded size must select scratch storage")
				assert.NotEqual(t, output, events[0].Content, "the parsed result must not retain the tool output")
				var stored string
				require.NoError(t, result.staged.scratch.QueryRowContext(t.Context(),
					"SELECT content FROM stage_events WHERE tool_use_id = ?", "call-a").Scan(&stored))
				assert.Equal(t, output, stored)
			} else {
				assert.Nil(t, result.staged, "a small declared size does not need staging")
				assert.Equal(t, output, events[0].Content)
			}
			if tc.knownSize {
				assert.Equal(t, int64(len(content)), result.sourceBytes)
				require.NotNil(t, result.retentionLease)
				assert.GreaterOrEqual(t, result.retentionLease.retainedBytes, int64(len(content)), "pending results must account for decoded data")
			}
			if !tc.knownSize {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				second, err := engine.retentionBudget().acquire(ctx, 1)
				defer second.Release()
				require.ErrorIs(t, err, context.Canceled, "an unknown size must hold the whole budget")
			}

			jobs := make(chan syncJob, 1)
			jobs <- syncJob{
				path: file.Path, agent: file.Agent, machine: file.Machine,
				processResult: result, retentionLease: result.retentionLease,
			}
			result = processResult{} // The collector owns the lease and scratch storage now.
			close(jobs)
			stats := engine.collectAndBatch(t.Context(), jobs, 1, 1, nil, syncWriteDefault)
			require.Zero(t, stats.Failed)
			require.Equal(t, 1, stats.Synced)
			assertReleased()
			storedMessages, err := database.GetAllMessages(t.Context(), sessionID)
			require.NoError(t, err)
			require.Len(t, storedMessages, 3)
			require.Len(t, storedMessages[1].ToolCalls, 1)
			require.Len(t, storedMessages[1].ToolCalls[0].ResultEvents, 1)
			assert.Equal(t, output, storedMessages[1].ToolCalls[0].ResultEvents[0].Content)
			require.Len(t, storedMessages[2].ToolCalls, 1)
			require.Len(t, storedMessages[2].ToolCalls[0].ResultEvents, 1)
			assert.Equal(t, subagentID, storedMessages[2].ToolCalls[0].ResultEvents[0].SubagentSessionID)
			assert.Equal(t, file.Path, database.GetSessionFilePath(t.Context(), sessionID))
			storedSize, _, ok := database.GetSessionFileInfo(t.Context(), sessionID)
			require.True(t, ok)
			assert.Equal(t, int64(len(compressed)), storedSize)
			if tc.s3 {
				storedHash, ok := database.GetSessionFileHash(t.Context(), sessionID)
				require.True(t, ok)
				assert.Equal(t, file.SourceFingerprint, storedHash)
			}
		})
	}
}
