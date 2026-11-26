package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Codex compresses a rollout that has not changed for a week to
// rollout-<ts>-<uuid>.jsonl.zst and removes the plain file. These tests pin
// that AgentsView reads the compressed file as the same session.

const (
	compressedThreadA   = "019f1000-0000-7000-8000-00000000000a"
	compressedThreadB   = "019f1000-0000-7000-8000-00000000000b"
	compressedThreadC   = "019f1000-0000-7000-8000-00000000000c"
	compressedPageOfC   = "019f1000-0000-7000-8000-0000000000c1"
	compressedDayPrefix = "rollout-2026-06-11T12-44-06-"
)

func zstdCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	out := enc.EncodeAll(data, nil)
	require.NoError(t, enc.Close())
	return out
}

func codexRolloutContent(uuid, prompt string) string {
	return testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(uuid, "/home/user/code/api", "codex_cli_rs", tsEarly),
		testjsonl.CodexMsgJSON("user", prompt, tsEarlyS1),
		testjsonl.CodexMsgJSON("assistant", "answer to "+prompt, tsEarlyS5),
	)
}

func writeFileIn(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	return path
}

func codexDayDir(root string) string {
	return filepath.Join(root, "2026", "06", "11")
}

func TestCodexFilenameHelpersAcceptCompressedRollouts(t *testing.T) {
	plain := compressedDayPrefix + compressedThreadA + ".jsonl"
	compressed := plain + ".zst"
	page := compressedDayPrefix + compressedThreadC + "_" + compressedPageOfC + ".jsonl.zst"

	assert.True(t, isCodexSessionFilename(compressed))
	assert.Equal(t, compressedThreadA, CodexSessionUUIDFromFilename(compressed))
	assert.Equal(t, CodexSessionUUIDFromFilename(plain), CodexSessionUUIDFromFilename(compressed),
		"the compressed file must key as the plain file it replaced")
	assert.Equal(t, compressedThreadC+"_"+compressedPageOfC, CodexSessionUUIDFromFilename(page))
	assert.Empty(t, CodexSessionUUIDFromFilename(plain+".tmp"),
		"Codex's temporary files are not rollouts")
	assert.Empty(t, CodexSessionUUIDFromFilename("rollout-compress-abc123.tmp"))

	root := t.TempDir()
	layout, uuid, ok := CodexSessionPathInfo(root, filepath.Join(codexDayDir(root), compressed))
	require.True(t, ok)
	assert.Equal(t, CodexLayoutDated, layout)
	assert.Equal(t, compressedThreadA, uuid)
	layout, uuid, ok = CodexSessionPathInfo(root, filepath.Join(root, compressed))
	require.True(t, ok)
	assert.Equal(t, CodexLayoutArchivedFlat, layout)
	assert.Equal(t, compressedThreadA, uuid)
}

// TestCodexDiscoverIncludesCompressedRollouts: a day directory with one plain
// and three compressed rollouts, one of them an undo page, yields four
// sources with the keys the plain names would have had.
func TestCodexDiscoverIncludesCompressedRollouts(t *testing.T) {
	root := t.TempDir()
	day := codexDayDir(root)
	writeFileIn(t, day, compressedDayPrefix+compressedThreadA+".jsonl",
		[]byte(codexRolloutContent(compressedThreadA, "plain")))
	writeFileIn(t, day, compressedDayPrefix+compressedThreadB+".jsonl.zst",
		zstdCompress(t, []byte(codexRolloutContent(compressedThreadB, "compressed"))))
	writeFileIn(t, day, compressedDayPrefix+compressedThreadC+".jsonl.zst",
		zstdCompress(t, []byte(codexRolloutContent(compressedThreadC, "compressed thread"))))
	writeFileIn(t, day, compressedDayPrefix+compressedThreadC+"_"+compressedPageOfC+".jsonl.zst",
		zstdCompress(t, []byte(codexRolloutContent(compressedThreadC, "compressed page"))))

	sources := newCodexSourceSet(AgentCodex, []string{root})
	discovered, err := sources.Discover(t.Context())
	require.NoError(t, err)
	keys := make([]string, 0, len(discovered))
	for _, source := range discovered {
		keys = append(keys, source.Key)
	}
	assert.ElementsMatch(t, []string{
		"codex:" + compressedThreadA,
		"codex:" + compressedThreadB,
		"codex:" + compressedThreadC,
		"codex:" + compressedThreadC + "_" + compressedPageOfC,
	}, keys)

	var streamed []string
	require.NoError(t, sources.DiscoverEach(t.Context(), func(source SourceRef) error {
		streamed = append(streamed, source.Key)
		return nil
	}))
	assert.ElementsMatch(t, keys, streamed, "streaming discovery must see the same files")
}

// TestCodexPlainRolloutWinsOverCompressedSibling: while both forms exist the
// plain file is the source, because Codex appends to it; once it is gone the
// session's source moves to the compressed file.
func TestCodexPlainRolloutWinsOverCompressedSibling(t *testing.T) {
	root := t.TempDir()
	day := codexDayDir(root)
	content := codexRolloutContent(compressedThreadA, "both forms")
	plain := writeFileIn(t, day, compressedDayPrefix+compressedThreadA+".jsonl", []byte(content))
	compressed := writeFileIn(t, day, compressedDayPrefix+compressedThreadA+".jsonl.zst",
		zstdCompress(t, []byte(content)))

	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, plain, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "codex:" + compressedThreadA, RequireFreshSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, plain, found.DisplayPath)

	require.NoError(t, os.Remove(plain))
	discovered, err = provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, compressed, discovered[0].DisplayPath)
	assert.Equal(t, "codex:"+compressedThreadA, discovered[0].Key)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "codex:" + compressedThreadA, RequireFreshSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, compressed, found.DisplayPath)
}

// TestCodexParseCompressedRolloutMatchesPlain: the compressed file parses to
// the same session and messages as the plain file, records the compressed
// file's own size, and never earns a resumable checkpoint.
func TestCodexParseCompressedRolloutMatchesPlain(t *testing.T) {
	plainRoot := t.TempDir()
	compressedRoot := t.TempDir()
	content := codexRolloutContent(compressedThreadA, "same bytes")
	plain := writeFileIn(t, codexDayDir(plainRoot),
		compressedDayPrefix+compressedThreadA+".jsonl", []byte(content))
	compressed := writeFileIn(t, codexDayDir(compressedRoot),
		compressedDayPrefix+compressedThreadA+".jsonl.zst", zstdCompress(t, []byte(content)))

	parse := func(root, path string) (ParseResultOutcome, SourceFingerprint) {
		t.Helper()
		provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
		require.True(t, ok)
		source, ok := provider.(interface {
			SourceRefForPath(string) (SourceRef, bool)
		}).SourceRefForPath(path)
		require.True(t, ok)
		fingerprint, err := provider.Fingerprint(t.Context(), source)
		require.NoError(t, err)
		outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source, Fingerprint: fingerprint})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		return outcome.Results[0], fingerprint
	}
	plainResult, _ := parse(plainRoot, plain)
	compressedResult, compressedFingerprint := parse(compressedRoot, compressed)

	assert.Equal(t, plainResult.Result.Session.ID, compressedResult.Result.Session.ID)
	assert.Equal(t, plainResult.Result.Messages, compressedResult.Result.Messages)
	assert.Equal(t, plainResult.Result.Session.MessageCount, compressedResult.Result.Session.MessageCount)
	assert.Equal(t, plainResult.Result.Session.FirstMessage, compressedResult.Result.Session.FirstMessage)
	assert.Equal(t, compressed, compressedResult.Result.Session.File.Path)

	info, err := os.Stat(compressed)
	require.NoError(t, err)
	assert.Equal(t, info.Size(), compressedResult.Result.Session.File.Size,
		"the stored size is the compressed file's own, so an unchanged file is skipped")
	assert.Equal(t, compressedFingerprint.Hash, compressedResult.Result.Session.File.Hash)
	assert.Equal(t, compressedFingerprint.Size, compressedResult.Result.Session.File.Size)
	assert.Empty(t, compressedResult.Result.Checkpoint, "no checkpoint into a compressed file")
	assert.Empty(t, compressedResult.Result.CheckpointHashState)
	assert.NotEmpty(t, plainResult.Result.Checkpoint, "the plain file still checkpoints")

	// Without a fingerprint hash (a first sync at this path) the parse must
	// still report the compressed file's own hash, never the unpacked bytes'.
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{compressedRoot}, Machine: "devbox"})
	require.True(t, ok)
	source, ok := provider.(interface {
		SourceRefForPath(string) (SourceRef, bool)
	}).SourceRefForPath(compressed)
	require.True(t, ok)
	cold, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, cold.Results, 1)
	raw, err := os.ReadFile(compressed)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	assert.Equal(t, hex.EncodeToString(sum[:]), cold.Results[0].Result.Session.File.Hash)

	sizer, ok := provider.(ParseSourceSizer)
	require.True(t, ok, "the engine sizes its memory lease through ParseSourceSizer")
	size, err := sizer.ParseSourceSize(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), size,
		"memory admission uses decoded bytes while the fingerprint keeps compressed bytes")
}

func TestCodexS3PreferredFilesKeepsPlainOverCompressed(t *testing.T) {
	plain := "s3://bucket/host/raw/codex/sessions/2026/06/11/" + compressedDayPrefix + compressedThreadA + ".jsonl"
	archived := "s3://bucket/host/raw/codex/archived_sessions/" + compressedDayPrefix + compressedThreadA + ".jsonl"
	files := []DiscoveredFile{
		{Path: plain + ".zst"},
		{Path: "s3://bucket/host/raw/codex/sessions/2026/06/11/" + compressedDayPrefix + compressedThreadB + ".jsonl.zst"},
		{Path: plain},
		{Path: archived + ".zst"},
	}
	kept := codexS3PreferredFiles(files)
	paths := make([]string, 0, len(kept))
	for _, file := range kept {
		paths = append(paths, file.Path)
	}
	assert.Equal(t, []string{
		files[1].Path,
		plain,
		archived + ".zst",
	}, paths, "a compressed object beside its plain one is dropped; a lone compressed object "+
		"and another copy of the session at a different address stay")
}

func TestCodexIncrementalParseRefusesCompressedRollout(t *testing.T) {
	root := t.TempDir()
	content := codexRolloutContent(compressedThreadA, "no offsets")
	compressed := writeFileIn(t, codexDayDir(root),
		compressedDayPrefix+compressedThreadA+".jsonl.zst", zstdCompress(t, []byte(content)))
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, ok := provider.(interface {
		SourceRefForPath(string) (SourceRef, bool)
	}).SourceRefForPath(compressed)
	require.True(t, ok)
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)

	outcome, status, err := provider.ParseIncremental(t.Context(), IncrementalRequest{
		Source:      source,
		Fingerprint: fingerprint,
		SessionID:   "codex:" + compressedThreadA,
		Offset:      0,
	})
	require.NoError(t, err)
	assert.Equal(t, IncrementalNeedsFullParse, status)
	assert.True(t, outcome.ForceReplace)
}

func TestCodexParseTruncatedCompressedRolloutFails(t *testing.T) {
	root := t.TempDir()
	content := codexRolloutContent(compressedThreadA, "cut short")
	whole := zstdCompress(t, []byte(content))
	truncated := writeFileIn(t, codexDayDir(root),
		compressedDayPrefix+compressedThreadA+".jsonl.zst", whole[:len(whole)/2])
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, ok := provider.(interface {
		SourceRefForPath(string) (SourceRef, bool)
	}).SourceRefForPath(truncated)
	require.True(t, ok)

	_, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.Error(t, err)
	assert.Contains(t, err.Error(), filepath.Base(truncated))
}

func TestCodexExecCheckReadsCompressedRollout(t *testing.T) {
	root := t.TempDir()
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(compressedThreadA, "/home/user/code/api", "codex_exec", tsEarly),
		testjsonl.CodexMsgJSON("user", "exec", tsEarlyS1),
	)
	compressed := writeFileIn(t, codexDayDir(root),
		compressedDayPrefix+compressedThreadA+".jsonl.zst", zstdCompress(t, []byte(content)))
	assert.True(t, IsCodexExecSessionFile(compressed))
}

// TestCodexCompressedForkChildFindsItsReplayParent: a fork child that Codex
// compressed still names its parent, so raw capture carries the parent's
// rollout and the replayed parent turns can be recognised.
func TestCodexCompressedForkChildFindsItsReplayParent(t *testing.T) {
	root := t.TempDir()
	day := codexDayDir(root)
	parent := writeFileIn(t, day, compressedDayPrefix+compressedThreadA+".jsonl.zst",
		zstdCompress(t, []byte(codexRolloutContent(compressedThreadA, "parent task"))))
	childContent := `{"type":"session_meta","payload":{"id":"` + compressedThreadB +
		`","forked_from_id":"` + compressedThreadA + `"}}` + "\n"
	child := writeFileIn(t, day, compressedDayPrefix+compressedThreadB+".jsonl.zst",
		zstdCompress(t, []byte(childContent)))

	parentID, ok := CodexReplayParentID(child)
	require.True(t, ok, "a compressed fork child must still report its parent")
	assert.Equal(t, compressedThreadA, parentID)

	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	plan, supported, err := ResolveRawCapturePlan(t.Context(), provider,
		requireCodexProviderSource(t, provider, compressedThreadB))
	require.NoError(t, err)
	require.True(t, supported)
	var captured []string
	for _, entry := range plan.Entries {
		captured = append(captured, entry.LocalPath)
	}
	assert.ElementsMatch(t, []string{canonicalRawCaptureTestPath(t, child), canonicalRawCaptureTestPath(t, parent)}, captured)
}

// TestCodexAllSourcePathsHideCompressedBesidePlain: the quick sync's duplicate
// expansion must never offer the compressed copy of a rollout whose plain
// file still exists, whatever their modification times.
func TestCodexAllSourcePathsHideCompressedBesidePlain(t *testing.T) {
	root := t.TempDir()
	day := codexDayDir(root)
	content := codexRolloutContent(compressedThreadA, "both forms")
	plain := writeFileIn(t, day, compressedDayPrefix+compressedThreadA+".jsonl", []byte(content))
	compressed := writeFileIn(t, day, compressedDayPrefix+compressedThreadA+".jsonl.zst",
		zstdCompress(t, []byte(content)))
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	lister := provider.(interface{ AllSourcePathsForUUID(string) []string })

	assert.Equal(t, []string{plain}, lister.AllSourcePathsForUUID(compressedThreadA))

	require.NoError(t, os.Remove(plain))
	assert.Equal(t, []string{compressed}, lister.AllSourcePathsForUUID(compressedThreadA))
}

func TestCodexCompressedSiblingHidden(t *testing.T) {
	plain := compressedDayPrefix + compressedThreadA + ".jsonl"
	listed := map[string]struct{}{plain: {}, plain + ".zst": {}}
	assert.True(t, codexCompressedSiblingHidden(plain+".zst", listed),
		"the compressed copy is hidden while the plain file is listed")
	assert.False(t, codexCompressedSiblingHidden(plain, listed))
	assert.False(t, codexCompressedSiblingHidden(plain+".zst", map[string]struct{}{plain + ".zst": {}}),
		"alone, the compressed copy is the thread's file")
}

// Resolving a known rollout must not inspect unrelated archive entries.
func TestCodexKnownRolloutLookupWorkDoesNotGrowWithArchive(t *testing.T) {
	for _, reconciliation := range []bool{false, true} {
		t.Run(fmt.Sprintf("reconciliation_%t", reconciliation), func(t *testing.T) {
			root := t.TempDir()
			path := writeFileIn(t, root, compressedDayPrefix+compressedThreadA+".jsonl.zst",
				zstdCompress(t, []byte(codexRolloutContent(compressedThreadA, "known rollout"))))
			provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			lookup := func() {
				if reconciliation {
					source, found, err := provider.(ReconciliationSourceResolver).SourceForReconciliation(
						t.Context(), path, "codex:"+compressedThreadA,
					)
					require.NoError(t, err)
					require.True(t, found)
					require.Equal(t, path, source.DisplayPath)
					return
				}
				sources, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
					Path: path, EventKind: "write",
				})
				require.NoError(t, err)
				require.Len(t, sources, 1)
				require.Equal(t, path, sources[0].DisplayPath)
			}
			small := testing.AllocsPerRun(3, lookup)
			for i := range 256 {
				name := fmt.Sprintf("rollout-2026-06-11T12-44-06-019f1000-0000-7000-8001-%012d.jsonl.zst", i)
				require.NoError(t, os.WriteFile(filepath.Join(root, name), nil, 0o644))
			}
			large := testing.AllocsPerRun(3, lookup)
			// Leave room for runtime variation while rejecting per-entry work.
			assert.Less(t, large, 2*small,
				"known-path lookup work must stay bounded as the archive grows")
		})
	}
}

func TestCodexCompressedChangedPathFindsDatedPlainByFilename(t *testing.T) {
	flat, live := t.TempDir(), t.TempDir()
	name := compressedDayPrefix + compressedThreadA + ".jsonl"
	content := []byte(codexRolloutContent(compressedThreadA, "same rollout"))
	compressed := writeFileIn(t, flat, name+".zst", zstdCompress(t, content))
	plain := writeFileIn(t, codexDayDir(live), name, content)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{flat, live}})
	require.True(t, ok)
	sources, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: compressed, EventKind: "write",
	})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, plain, sources[0].DisplayPath)
}
