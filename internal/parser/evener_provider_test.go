package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvenerProviderLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "projects", "example", "sessions", "session-1.transcript.jsonl")
	writeSourceFile(t, path, "{}\n")
	writeSourceFile(t, filepath.Join(filepath.Dir(path), "session-1.meta.json"), "{}")
	writeSourceFile(t, filepath.Join(root, "logs", "request.transcript.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(filepath.Dir(path), "session-1.api.jsonl"), "{}\n")
	for _, roots := range [][]string{{root}, {filepath.Join(root, "projects", "example")}, {filepath.Dir(path)}, {root, filepath.Dir(path)}} {
		provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: roots})
		require.True(ok, "Evener provider must be registered")
		for range 2 {
			sources, err := provider.Discover(t.Context())
			require.NoError(err)
			require.Len(sources, 1)
			assert.Equal(path, sources[0].DisplayPath)
		}
		found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "session-1", RequireFreshSource: true})
		require.NoError(err)
		require.True(ok)
		assert.Equal(path, found.DisplayPath)
		changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: filepath.Join(filepath.Dir(path), "session-1.meta.json"), EventKind: "write"})
		require.NoError(err)
		require.Len(changed, 1)
		assert.Equal(path, changed[0].DisplayPath)
		plan, err := provider.WatchPlan(t.Context())
		require.NoError(err)
		require.NotEmpty(plan.Roots)
		assert.True(plan.Roots[0].Recursive)
		assert.Contains(plan.Roots[0].IncludeGlobs, "*.meta.json")
	}
}

func TestEvenerProviderMissingRootAndNewDirectories(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := filepath.Join(t.TempDir(), "state")
	provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: []string{root}})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	assert.Empty(sources)
	path := filepath.Join(root, "projects", "created", "sessions", "new.transcript.jsonl")
	writeSourceFile(t, path, "{}\n")
	sources, err = provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(path, sources[0].DisplayPath)
}

func TestEvenerProviderLookupRejectsUnsafeAndMismatchedHints(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "sessions", "good.transcript.jsonl")
	writeSourceFile(t, path, "{}\n")
	provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: []string{root}})
	require.True(ok)
	for _, id := range []string{"../good", "a/b", "a\\b", "..", ".", "wrong"} {
		_, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: id, StoredFilePath: path, RequireFreshSource: true})
		require.NoError(err)
		assert.False(found, "ID %q must not borrow another source", id)
	}
	outside := filepath.Join(t.TempDir(), "good.transcript.jsonl")
	writeSourceFile(t, outside, "{}\n")
	require.NoError(os.Remove(path))
	_, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "good", StoredFilePath: outside, RequireFreshSource: true})
	require.NoError(err)
	assert.False(found)
}

func TestEvenerProviderFingerprintTracksEachFile(t *testing.T) {
	parentAssert := assert.New(t)
	parentRequire := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "sessions", "good.transcript.jsonl")
	meta := filepath.Join(root, "sessions", "good.meta.json")
	writeSourceFile(t, path, "{\"one\":1}\n")
	writeSourceFile(t, meta, `{"id":"good","name":"one"}`)
	provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: []string{root}})
	parentRequire.True(ok)
	sources, err := provider.Discover(t.Context())
	parentRequire.NoError(err)
	parentRequire.Len(sources, 1)
	before, err := provider.Fingerprint(t.Context(), sources[0])
	parentRequire.NoError(err)
	hasher, ok := provider.(MultiFileStatHasher)
	parentRequire.True(ok)
	firstDigest := hasher.ComputeMultiFileStatHash(path)
	parentAssert.NotZero(firstDigest)
	metaInfo, err := os.Stat(meta)
	parentRequire.NoError(err)
	beforeChangeTime, ok := codexIndexChangeTime(meta, metaInfo)
	parentRequire.True(ok)
	writeSourceFile(t, meta, `{"id":"good","name":"two"}`)
	// Preserve size and mtime, but establish a distinct ctime before checking
	// the stat digest. Rapid writes can share a filesystem timestamp tick.
	parentRequire.EventuallyWithT(func(c *assert.CollectT) {
		require.NoError(c, os.Chtimes(meta, metaInfo.ModTime(), metaInfo.ModTime()))
		info, err := os.Stat(meta)
		require.NoError(c, err)
		changeTime, ok := codexIndexChangeTime(meta, info)
		require.True(c, ok)
		assert.NotEqual(c, beforeChangeTime, changeTime)
	}, 2*time.Second, time.Millisecond)
	second, err := provider.Fingerprint(t.Context(), sources[0])
	parentRequire.NoError(err)
	parentAssert.Equal(before.Size, second.Size)
	parentAssert.Equal(before.MTimeNS, second.MTimeNS)
	parentAssert.NotEqual(before.Hash, second.Hash)
	parentAssert.NotEqual(firstDigest, hasher.ComputeMultiFileStatHash(path))
	parentRequire.NoError(os.Remove(meta))
	third, err := provider.Fingerprint(t.Context(), sources[0])
	parentRequire.NoError(err)
	parentAssert.NotEqual(second.Hash, third.Hash)
	parentAssert.Less(third.Size, second.Size)
	info, err := os.Stat(path)
	parentRequire.NoError(err)
	replacement := filepath.Join(root, "replacement")
	writeSourceFile(t, replacement, "{\"two\":2}\n")
	parentRequire.NoError(os.Chtimes(replacement, info.ModTime(), info.ModTime()))
	parentRequire.NoError(os.Rename(replacement, path))
	fourth, err := provider.Fingerprint(t.Context(), sources[0])
	parentRequire.NoError(err)
	parentAssert.NotEqual(third.Hash, fourth.Hash)
	parentRequire.NoError(os.Truncate(path, 0))
	fifth, err := provider.Fingerprint(t.Context(), sources[0])
	parentRequire.NoError(err)
	parentAssert.Zero(fifth.Size)
	parentAssert.NotEqual(fourth.Hash, fifth.Hash)
	parentRequire.NoError(os.Remove(path))
	_, err = provider.Fingerprint(t.Context(), sources[0])
	parentRequire.Error(err)
}

func TestEvenerProviderChangedPathStaysLocal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	for _, count := range []int{1, 250} {
		root := t.TempDir()
		for i := range count {
			writeSourceFile(t, filepath.Join(root, "projects", fmt.Sprintf("project-%d", i), "sessions", "other.transcript.jsonl"), "{}\n")
		}
		owner := filepath.Join(root, "projects", "target", "sessions", "good.transcript.jsonl")
		writeSourceFile(t, owner, "{}\n")
		provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: []string{root}})
		require.True(ok)
		sources, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: filepath.Join(filepath.Dir(owner), "good.meta.json"), EventKind: "remove"})
		require.NoError(err)
		require.Len(sources, 1)
		assert.Equal(owner, sources[0].DisplayPath)
		for _, unrelated := range []string{filepath.Join(filepath.Dir(owner), "good.api.jsonl"), filepath.Join(root, "logs", "bad.transcript.jsonl")} {
			sources, err = provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: unrelated, EventKind: "write"})
			require.NoError(err)
			assert.Empty(sources)
		}
	}
}

func TestEvenerProviderRawCaptureNamesOnlyTranscriptAndMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "projects", "demo", "sessions", "good.transcript.jsonl")
	meta := filepath.Join(filepath.Dir(path), "good.meta.json")
	writeSourceFile(t, path, "{}\n")
	writeSourceFile(t, meta, "{}")
	writeSourceFile(t, filepath.Join(filepath.Dir(path), "good.api.jsonl"), "{}\n")
	provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: []string{root}})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	planner, ok := provider.(RawCaptureProvider)
	require.True(ok)
	plan, err := planner.PlanRawCapture(t.Context(), sources[0])
	require.NoError(err)
	require.Len(plan.Entries, 2)
	assert.Equal(root, plan.CaptureRoot)
	assert.Equal("projects/demo/sessions/good.transcript.jsonl", plan.Entries[0].Path)
	assert.Equal("projects/demo/sessions/good.meta.json", plan.Entries[1].Path)
	assert.True(plan.Entries[0].Appendable)
	assert.False(plan.Entries[1].Appendable)
	require.NoError(os.Remove(meta))
	plan, err = planner.PlanRawCapture(t.Context(), sources[0])
	require.NoError(err)
	require.Len(plan.Entries, 1)
}

func TestEvenerProviderParseReplacementAndRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "sessions", "good.transcript.jsonl")
	header := `{"kind":"header","format_version":2,"session_id":"good","created_at":"2026-09-01T10:00:00Z","working_dir":"/workspace/example"}` + "\n"
	entry := `{"kind":"entry","seq":1,"turn":{"kind":"USER_INPUT","timestamp":"2026-09-01T10:00:01Z","message":{"role":"user","content":[{"kind":"text","text":"Hello"}]}}}` + "\n"
	writeSourceFile(t, path, header+entry)
	provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: []string{root}, Machine: "test-machine"})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	out, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(err)
	require.Len(out.Results, 1)
	assert.True(out.ForceReplace)
	assert.True(out.ResultSetComplete)
	assert.Equal(DataVersionCurrent, out.Results[0].DataVersion)
	assert.Equal("test-machine", out.Results[0].Result.Session.Machine)
	assert.Positive(out.Results[0].Result.Session.File.Size)
	assert.NotEmpty(out.Results[0].Result.Session.File.Hash)
	writeSourceFile(t, path, header+entry+`{"kind":"entry"`)
	out, err = provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(err)
	assert.Empty(out.Results)
	assert.False(out.ResultSetComplete)
	assert.False(out.ForceReplace)
	writeSourceFile(t, path, "broken\n")
	_, err = provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.Error(err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = provider.Parse(ctx, ParseRequest{Source: sources[0]})
	assert.ErrorIs(err, context.Canceled)
}

func TestEvenerProviderParseHonorsFilesystemProjectDiscoveryPolicy(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repository")
	cwd := filepath.Join(repo, "nested")
	// A plain .git directory exercises the filesystem walker without invoking Git.
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	sessions := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessions, 0o755))
	writeEvenerFixture(t, sessions, "session", map[string]any{"working_dir": cwd},
		evenerTestTurn("USER_INPUT", "Hello"))

	for _, tc := range []struct {
		name    string
		remote  bool
		project string
	}{
		{"local", false, "repository"},
		{"remote", true, "nested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			cfg := ProviderConfig{Roots: []string{root}}
			ctx := t.Context()
			if tc.remote {
				ctx = WithoutFilesystemProjectDiscovery(ctx)
			}
			provider, ok := NewProvider(AgentEvener, cfg)
			require.True(ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(err)
			require.Len(sources, 1)

			originalStat := osStat
			t.Cleanup(func() { osStat = originalStat })
			var probedCwd bool
			osStat = func(path string) (os.FileInfo, error) {
				probedCwd = probedCwd || path == cwd
				return originalStat(path)
			}
			out, err := provider.Parse(ctx, ParseRequest{Source: sources[0]})
			require.NoError(err)
			require.Len(out.Results, 1)
			assert.Equal(!tc.remote, probedCwd)
			result := out.Results[0].Result
			assert.Equal(tc.project, result.Session.Project)
			assert.Equal(cwd, result.Session.Cwd)
			require.Len(result.Messages, 1)
			assert.Equal("Hello", result.Messages[0].Content)
		})
	}
}

func TestEvenerProviderRejectsSymlinkCompanions(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "sessions", "good.transcript.jsonl")
	writeSourceFile(t, path, "{}\n")
	outside := filepath.Join(t.TempDir(), "metadata")
	writeSourceFile(t, outside, "{}")
	require.NoError(os.Symlink(outside, filepath.Join(filepath.Dir(path), "good.meta.json")))
	provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: []string{root}})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	_, err = provider.Fingerprint(t.Context(), sources[0])
	require.Error(err)
	_, err = provider.(RawCaptureProvider).PlanRawCapture(t.Context(), sources[0])
	require.Error(err)
}

func TestEvenerProviderParentArrivalChangesFingerprint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	dir := filepath.Join(root, "sessions")
	require.NoError(os.MkdirAll(dir, 0700))
	child := writeEvenerFixture(t, dir, "child", map[string]any{"parent_session_id": "parent"}, evenerTestTurn("USER_INPUT", "copied"), evenerTestTurn("USER_INPUT", "new"))
	writeEvenerMeta(t, child, map[string]any{"id": "child", "parent_session_id": "parent", "divergence_turn": 2})
	provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "child"})
	require.NoError(err)
	require.True(ok)
	first, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	digest := provider.(MultiFileStatHasher).ComputeMultiFileStatHash(child)
	writeEvenerFixture(t, dir, "parent", nil, evenerTestTurn("USER_INPUT", "copied"))
	second, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.NotEqual(first.Hash, second.Hash)
	assert.NotEqual(digest, provider.(MultiFileStatHasher).ComputeMultiFileStatHash(child))
	out, err := provider.Parse(t.Context(), ParseRequest{Source: source, Fingerprint: second})
	require.NoError(err)
	require.Len(out.Results, 1)
	require.Len(out.Results[0].Result.Messages, 1)
	assert.Equal("new", out.Results[0].Result.Messages[0].Content)
	require.NoError(os.Remove(filepath.Join(dir, "parent.transcript.jsonl")))
	third, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.Equal(first.Hash, third.Hash)
}

func TestEvenerProviderMetadataRemovalClearsSourceTitle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	dir := filepath.Join(root, "sessions")
	require.NoError(os.MkdirAll(dir, 0700))
	path := writeEvenerFixture(t, dir, "session", nil, evenerTestTurn("USER_INPUT", "hello"))
	writeEvenerMeta(t, path, map[string]any{"id": "session", "name": "Source title"})
	provider, ok := NewProvider(AgentEvener, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "session"})
	require.NoError(err)
	require.True(found)
	before, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(err)
	require.Len(before.Results, 1)
	assert.Equal("Source title", before.Results[0].Result.Session.SessionName)
	require.NoError(os.Remove(filepath.Join(dir, "session.meta.json")))
	after, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(err)
	require.Len(after.Results, 1)
	assert.Empty(after.Results[0].Result.Session.SessionName)
	assert.True(after.Results[0].Result.Session.SessionNamePresent, "absence is an authoritative empty provider title")
}

func TestEvenerProviderUnavailableParentRetainsChild(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			root := t.TempDir()
			dir := filepath.Join(root, "sessions")
			require.NoError(os.MkdirAll(dir, 0700))
			copied := evenerTestTurn("USER_INPUT", "copied")
			child := writeEvenerFixture(t, dir, "child", map[string]any{"parent_session_id": "parent"}, copied, evenerTestTurn("USER_INPUT", "new"))
			writeEvenerMeta(t, child, map[string]any{"id": "child", "parent_session_id": "parent", "divergence_turn": 2})
			parent := filepath.Join(dir, "parent.transcript.jsonl")
			target := ""
			switch kind {
			case "symlink":
				target = writeEvenerFixture(t, t.TempDir(), "parent", nil, copied)
				require.NoError(os.Symlink(target, parent))
			case "directory":
				require.NoError(os.Mkdir(parent, 0700))
			case "unreadable":
				writeEvenerFixture(t, dir, "parent", nil, copied)
				require.NoError(os.Chmod(parent, 0000))
				probe, err := os.Open(parent)
				if err == nil {
					require.NoError(probe.Close())
					t.Skip("process can read permission-denied files")
				}
				require.True(os.IsPermission(err))
			}
			provider, ok := NewProvider(AgentEvener, ProviderConfig{Roots: []string{root}})
			require.True(ok)
			source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "child"})
			require.NoError(err)
			require.True(found)
			before, err := provider.Parse(t.Context(), ParseRequest{Source: source})
			require.NoError(err)
			require.Len(before.Results, 1)
			require.Len(before.Results[0].Result.Messages, 2)
			fingerprint, err := provider.Fingerprint(t.Context(), source)
			require.NoError(err)
			hasher := provider.(MultiFileStatHasher)
			digest := hasher.ComputeMultiFileStatHash(child)
			if target != "" {
				writeSourceFile(t, target, "outside changed\n")
				after, err := provider.Fingerprint(t.Context(), source)
				require.NoError(err)
				assert.Equal(fingerprint, after)
				assert.Equal(digest, hasher.ComputeMultiFileStatHash(child))
			}
			parentInfo, err := os.Lstat(parent)
			require.NoError(err)
			require.NoError(os.Remove(parent))
			writeEvenerFixture(t, dir, "parent", nil, copied)
			// Recreating an equal-size parent need not advance filesystem time.
			// Give the replacement a distinct mtime for the stat-digest check.
			replacementTime := parentInfo.ModTime().Add(2 * time.Second)
			require.NoError(os.Chtimes(parent, replacementTime, replacementTime))
			available, err := provider.Fingerprint(t.Context(), source)
			require.NoError(err)
			assert.NotEqual(fingerprint.Hash, available.Hash)
			assert.NotEqual(digest, hasher.ComputeMultiFileStatHash(child))
			after, err := provider.Parse(t.Context(), ParseRequest{Source: source})
			require.NoError(err)
			require.Len(after.Results, 1)
			require.Len(after.Results[0].Result.Messages, 1)
			assert.Equal("new", after.Results[0].Result.Messages[0].Content)
		})
	}
}

func TestEvenerProviderTruncatedTranscriptDefersReplacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	dir := filepath.Join(root, "sessions")
	require.NoError(os.MkdirAll(dir, 0700))
	path := writeEvenerFixture(t, dir, "session", nil, evenerTestTurn("USER_INPUT", "first"), evenerTestTurn("ASSISTANT", "answer"))
	provider, ok := NewProvider(AgentEvener, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "session"})
	require.NoError(err)
	require.True(found)
	complete, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(err)
	require.Len(complete.Results, 1)
	require.Len(complete.Results[0].Result.Messages, 2)
	writeEvenerFixture(t, dir, "session", nil, evenerTestTurn("USER_INPUT", "replacement"))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(err)
	_, err = f.WriteString(`{"kind":"entry","turn":`)
	require.NoError(err)
	require.NoError(f.Close())
	partial, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(err)
	assert.Empty(partial.Results, "no partial rows may replace existing history")
	assert.False(partial.ResultSetComplete, "an incomplete source must be retried")
	assert.False(partial.ForceReplace)
}

func TestEvenerProviderRemoteChangesRequestReconciliation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "session.transcript.jsonl")
	writeSourceFile(t, path, "{}\n")
	for _, tc := range []struct {
		name             string
		remote, metadata bool
		want             int
	}{
		{"local transcript", false, false, 1},
		{"remote transcript", true, false, 0},
		{"remote metadata", true, true, 0},
		{"local metadata", false, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ProviderConfig{Roots: []string{root}}
			if tc.remote {
				cfg.PathRewriter = func(path string) string { return "remote:" + path }
			}
			provider, ok := NewProvider(AgentEvener, cfg)
			require.True(t, ok)
			changedPath := path
			if tc.metadata {
				changedPath = evenerMetadataPath(path)
			}
			sources, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: changedPath, EventKind: "write"})
			require.NoError(t, err)
			assert.Len(t, sources, tc.want)
		})
	}
}

func TestEvenerProviderParentMetadataFingerprint(t *testing.T) {
	parentAssert := assert.New(t)
	parentRequire := require.New(t)

	root := t.TempDir()
	dir := filepath.Join(root, "sessions")
	parentRequire.NoError(os.MkdirAll(dir, 0700))
	child := writeEvenerFixture(t, dir, "child", map[string]any{"parent_session_id": "parent"}, evenerTestTurn("USER_INPUT", "copied"))
	writeEvenerMeta(t, child, map[string]any{"id": "child", "parent_session_id": "parent", "divergence_turn": 2})
	writeEvenerFixture(t, dir, "parent", nil, evenerTestTurn("USER_INPUT", "copied"))
	provider, ok := NewProvider(AgentEvener, ProviderConfig{Roots: []string{root}})
	parentRequire.True(ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "child"})
	parentRequire.NoError(err)
	parentRequire.True(found)
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	parentRequire.NoError(err)
	hasher := provider.(MultiFileStatHasher)
	digest := hasher.ComputeMultiFileStatHash(child)
	meta := filepath.Join(dir, "parent.meta.json")
	mtime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, content := range []string{`{"id":"broken"}`, `{"id":"parent"}`, `{bad}`} {
		writeSourceFile(t, meta, content)
		// Equal-size revisions can share a filesystem timestamp tick.
		// Give each revision a distinct mtime before checking its stat digest.
		parentRequire.NoError(os.Chtimes(meta, mtime, mtime))
		mtime = mtime.Add(2 * time.Second)
		changed, err := provider.Fingerprint(t.Context(), source)
		parentRequire.NoError(err)
		parentAssert.NotEqual(fingerprint.Hash, changed.Hash)
		parentAssert.NotEqual(digest, hasher.ComputeMultiFileStatHash(child))
		parentAssert.Equal(fingerprint.Size, changed.Size, "parent metadata is a dependency, not child source bytes")
		fingerprint = changed
		digest = hasher.ComputeMultiFileStatHash(child)
	}
	parentRequire.NoError(os.Remove(meta))
	missing, err := provider.Fingerprint(t.Context(), source)
	parentRequire.NoError(err)
	parentAssert.NotEqual(fingerprint.Hash, missing.Hash)
	for _, kind := range []string{"symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			if kind == "symlink" {
				target := filepath.Join(t.TempDir(), "metadata")
				writeSourceFile(t, target, `{"id":"parent"}`)
				require.NoError(os.Symlink(target, meta))
			} else {
				require.NoError(os.Mkdir(meta, 0700))
			}
			blocked, err := provider.Fingerprint(t.Context(), source)
			require.NoError(err)
			assert.NotEqual(missing.Hash, blocked.Hash, "absent metadata and invalid metadata change parent eligibility")
			assert.Zero(hasher.ComputeMultiFileStatHash(child), "nonregular parent metadata must not be followed by stat freshness")
			require.NoError(os.Remove(meta))
			restored, err := provider.Fingerprint(t.Context(), source)
			require.NoError(err)
			assert.Equal(missing.Hash, restored.Hash)
		})
	}
}

func TestEvenerRawDiscoveryStreamsWithProgress(t *testing.T) {
	for _, count := range []int{1, 200} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			root := t.TempDir()
			sessions := filepath.Join(root, "projects", "demo", "sessions")
			for i := range count {
				writeSourceFile(t, filepath.Join(sessions, fmt.Sprintf("session-%d.transcript.jsonl", i)), "{}\n")
			}
			writeSourceFile(t, filepath.Join(sessions, "ignored.api.jsonl"), "{}\n")
			provider, ok := NewProvider(AgentEvener, ProviderConfig{Roots: []string{root, sessions}})
			require.True(ok)
			steps, largestBatch, yielded := 0, 0, 0
			ctx := WithRawCaptureDiscoveryProgress(t.Context(), func() error { steps++; return nil })
			ctx = WithStreamingDiscoveryBufferObserver(ctx, func(n int) { largestBatch = max(largestBatch, n) })
			complete, err := StreamRawCaptureSources(ctx, provider, func(source SourceRef) error {
				yielded++
				plan, err := provider.(RawCaptureProvider).PlanRawCapture(ctx, source)
				require.NoError(err)
				require.Len(plan.Entries, 1)
				return nil
			})
			require.NoError(err)
			assert.True(complete)
			assert.Equal(count, yielded)
			assert.GreaterOrEqual(steps, count)
			assert.Positive(largestBatch)
			assert.LessOrEqual(largestBatch, streamingDirectoryBatchSize)
			stop := errors.New("pause audit")
			ctx = WithRawCaptureDiscoveryProgress(t.Context(), func() error { return stop })
			complete, err = StreamRawCaptureSources(ctx, provider, func(SourceRef) error { t.Fatal("yield after pause"); return nil })
			assert.ErrorIs(err, stop)
			assert.False(complete)
			steps = 0
			ctx = WithRawCaptureDiscoveryProgress(t.Context(), func() error {
				steps++
				if steps == 3 {
					return stop
				}
				return nil
			})
			complete, err = StreamRawCaptureSources(ctx, provider, func(SourceRef) error { return nil })
			assert.ErrorIs(err, stop)
			assert.False(complete)
			assert.Equal(3, steps)
			cancelled, cancel := context.WithCancel(t.Context())
			cancel()
			complete, err = StreamRawCaptureSources(cancelled, provider, func(SourceRef) error { return nil })
			assert.ErrorIs(err, context.Canceled)
			assert.False(complete)
			complete, err = StreamRawCaptureSources(t.Context(), provider, func(SourceRef) error { return stop })
			assert.ErrorIs(err, stop)
			assert.False(complete)
		})
	}
}
