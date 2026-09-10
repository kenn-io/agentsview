package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvenerProviderLifecycle(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "projects", "example", "sessions", "session-1.transcript.jsonl")
	writeSourceFile(t, path, "{}\n")
	writeSourceFile(t, filepath.Join(filepath.Dir(path), "session-1.meta.json"), "{}")
	writeSourceFile(t, filepath.Join(root, "logs", "request.transcript.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(filepath.Dir(path), "session-1.api.jsonl"), "{}\n")
	for _, roots := range [][]string{{root}, {filepath.Join(root, "projects", "example")}, {filepath.Dir(path)}, {root, filepath.Dir(path)}} {
		provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: roots})
		require.True(t, ok, "Evener provider must be registered")
		for range 2 {
			sources, err := provider.Discover(context.Background())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			assert.Equal(t, path, sources[0].DisplayPath)
		}
		found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: "session-1", RequireFreshSource: true})
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, path, found.DisplayPath)
		changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{Path: filepath.Join(filepath.Dir(path), "session-1.meta.json"), EventKind: "write"})
		require.NoError(t, err)
		require.Len(t, changed, 1)
		assert.Equal(t, path, changed[0].DisplayPath)
		plan, err := provider.WatchPlan(context.Background())
		require.NoError(t, err)
		require.NotEmpty(t, plan.Roots)
		assert.True(t, plan.Roots[0].Recursive)
		assert.Contains(t, plan.Roots[0].IncludeGlobs, "*.meta.json")
	}
}

func TestEvenerProviderMissingRootAndNewDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	assert.Empty(t, sources)
	path := filepath.Join(root, "projects", "created", "sessions", "new.transcript.jsonl")
	writeSourceFile(t, path, "{}\n")
	sources, err = provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, path, sources[0].DisplayPath)
}

func TestEvenerProviderLookupRejectsUnsafeAndMismatchedHints(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "good.transcript.jsonl")
	writeSourceFile(t, path, "{}\n")
	provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	for _, id := range []string{"../good", "a/b", "a\\b", "..", ".", "wrong"} {
		_, found, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: id, StoredFilePath: path, RequireFreshSource: true})
		require.NoError(t, err)
		assert.False(t, found, "ID %q must not borrow another source", id)
	}
	outside := filepath.Join(t.TempDir(), "good.transcript.jsonl")
	writeSourceFile(t, outside, "{}\n")
	require.NoError(t, os.Remove(path))
	_, found, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: "good", StoredFilePath: outside, RequireFreshSource: true})
	require.NoError(t, err)
	assert.False(t, found)
}

func TestEvenerProviderFingerprintTracksEachFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "good.transcript.jsonl")
	meta := filepath.Join(root, "sessions", "good.meta.json")
	writeSourceFile(t, path, "{\"one\":1}\n")
	writeSourceFile(t, meta, `{"id":"good","name":"one"}`)
	provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	before, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	hasher, ok := provider.(MultiFileStatHasher)
	require.True(t, ok)
	firstDigest := hasher.ComputeMultiFileStatHash(path)
	assert.NotZero(t, firstDigest)
	metaInfo, err := os.Stat(meta)
	require.NoError(t, err)
	beforeChangeTime, ok := codexIndexChangeTime(meta, metaInfo)
	require.True(t, ok)
	writeSourceFile(t, meta, `{"id":"good","name":"two"}`)
	// Preserve size and mtime, but establish a distinct ctime before checking
	// the stat digest. Rapid writes can share a filesystem timestamp tick.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.NoError(c, os.Chtimes(meta, metaInfo.ModTime(), metaInfo.ModTime()))
		info, err := os.Stat(meta)
		require.NoError(c, err)
		changeTime, ok := codexIndexChangeTime(meta, info)
		require.True(c, ok)
		assert.NotEqual(c, beforeChangeTime, changeTime)
	}, 2*time.Second, time.Millisecond)
	second, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	assert.Equal(t, before.Size, second.Size)
	assert.Equal(t, before.MTimeNS, second.MTimeNS)
	assert.NotEqual(t, before.Hash, second.Hash)
	assert.NotEqual(t, firstDigest, hasher.ComputeMultiFileStatHash(path))
	require.NoError(t, os.Remove(meta))
	third, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	assert.NotEqual(t, second.Hash, third.Hash)
	assert.Less(t, third.Size, second.Size)
	info, err := os.Stat(path)
	require.NoError(t, err)
	replacement := filepath.Join(root, "replacement")
	writeSourceFile(t, replacement, "{\"two\":2}\n")
	require.NoError(t, os.Chtimes(replacement, info.ModTime(), info.ModTime()))
	require.NoError(t, os.Rename(replacement, path))
	fourth, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	assert.NotEqual(t, third.Hash, fourth.Hash)
	require.NoError(t, os.Truncate(path, 0))
	fifth, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	assert.Zero(t, fifth.Size)
	assert.NotEqual(t, fourth.Hash, fifth.Hash)
	require.NoError(t, os.Remove(path))
	_, err = provider.Fingerprint(context.Background(), sources[0])
	require.Error(t, err)
}

func TestEvenerProviderChangedPathStaysLocal(t *testing.T) {
	for _, count := range []int{1, 250} {
		root := t.TempDir()
		for i := range count {
			writeSourceFile(t, filepath.Join(root, "projects", fmt.Sprintf("project-%d", i), "sessions", "other.transcript.jsonl"), "{}\n")
		}
		owner := filepath.Join(root, "projects", "target", "sessions", "good.transcript.jsonl")
		writeSourceFile(t, owner, "{}\n")
		provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: []string{root}})
		require.True(t, ok)
		sources, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{Path: filepath.Join(filepath.Dir(owner), "good.meta.json"), EventKind: "remove"})
		require.NoError(t, err)
		require.Len(t, sources, 1)
		assert.Equal(t, owner, sources[0].DisplayPath)
		for _, unrelated := range []string{filepath.Join(filepath.Dir(owner), "good.api.jsonl"), filepath.Join(root, "logs", "bad.transcript.jsonl")} {
			sources, err = provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{Path: unrelated, EventKind: "write"})
			require.NoError(t, err)
			assert.Empty(t, sources)
		}
	}
}

func TestEvenerProviderRawCaptureNamesOnlyTranscriptAndMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "projects", "demo", "sessions", "good.transcript.jsonl")
	meta := filepath.Join(filepath.Dir(path), "good.meta.json")
	writeSourceFile(t, path, "{}\n")
	writeSourceFile(t, meta, "{}")
	writeSourceFile(t, filepath.Join(filepath.Dir(path), "good.api.jsonl"), "{}\n")
	provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	planner, ok := provider.(RawCaptureProvider)
	require.True(t, ok)
	plan, err := planner.PlanRawCapture(context.Background(), sources[0])
	require.NoError(t, err)
	require.Len(t, plan.Entries, 2)
	assert.Equal(t, root, plan.CaptureRoot)
	assert.Equal(t, "projects/demo/sessions/good.transcript.jsonl", plan.Entries[0].Path)
	assert.Equal(t, "projects/demo/sessions/good.meta.json", plan.Entries[1].Path)
	assert.True(t, plan.Entries[0].Appendable)
	assert.False(t, plan.Entries[1].Appendable)
	require.NoError(t, os.Remove(meta))
	plan, err = planner.PlanRawCapture(context.Background(), sources[0])
	require.NoError(t, err)
	require.Len(t, plan.Entries, 1)
}

func TestEvenerProviderParseReplacementAndRetry(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "good.transcript.jsonl")
	header := `{"kind":"header","format_version":2,"session_id":"good","created_at":"2026-09-01T10:00:00Z","working_dir":"/workspace/example"}` + "\n"
	entry := `{"kind":"entry","seq":1,"turn":{"kind":"USER_INPUT","timestamp":"2026-09-01T10:00:01Z","message":{"role":"user","content":[{"kind":"text","text":"Hello"}]}}}` + "\n"
	writeSourceFile(t, path, header+entry)
	provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: []string{root}, Machine: "test-machine"})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	out, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	assert.True(t, out.ForceReplace)
	assert.True(t, out.ResultSetComplete)
	assert.Equal(t, DataVersionCurrent, out.Results[0].DataVersion)
	assert.Equal(t, "test-machine", out.Results[0].Result.Session.Machine)
	assert.Positive(t, out.Results[0].Result.Session.File.Size)
	assert.NotEmpty(t, out.Results[0].Result.Session.File.Hash)
	writeSourceFile(t, path, header+entry+`{"kind":"entry"`)
	out, err = provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	assert.Empty(t, out.Results)
	assert.False(t, out.ResultSetComplete)
	assert.False(t, out.ForceReplace)
	writeSourceFile(t, path, "broken\n")
	_, err = provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.Error(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = provider.Parse(ctx, ParseRequest{Source: sources[0]})
	assert.ErrorIs(t, err, context.Canceled)
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
			cfg := ProviderConfig{Roots: []string{root}}
			ctx := t.Context()
			if tc.remote {
				ctx = WithoutFilesystemProjectDiscovery(ctx)
			}
			provider, ok := NewProvider(AgentEvener, cfg)
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)

			originalStat := osStat
			t.Cleanup(func() { osStat = originalStat })
			var probedCwd bool
			osStat = func(path string) (os.FileInfo, error) {
				probedCwd = probedCwd || path == cwd
				return originalStat(path)
			}
			out, err := provider.Parse(ctx, ParseRequest{Source: sources[0]})
			require.NoError(t, err)
			require.Len(t, out.Results, 1)
			assert.Equal(t, !tc.remote, probedCwd)
			result := out.Results[0].Result
			assert.Equal(t, tc.project, result.Session.Project)
			assert.Equal(t, cwd, result.Session.Cwd)
			require.Len(t, result.Messages, 1)
			assert.Equal(t, "Hello", result.Messages[0].Content)
		})
	}
}

func TestEvenerProviderRejectsSymlinkCompanions(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "good.transcript.jsonl")
	writeSourceFile(t, path, "{}\n")
	outside := filepath.Join(t.TempDir(), "metadata")
	writeSourceFile(t, outside, "{}")
	require.NoError(t, os.Symlink(outside, filepath.Join(filepath.Dir(path), "good.meta.json")))
	provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	_, err = provider.Fingerprint(context.Background(), sources[0])
	require.Error(t, err)
	_, err = provider.(RawCaptureProvider).PlanRawCapture(context.Background(), sources[0])
	require.Error(t, err)
}

func TestEvenerProviderParentArrivalChangesFingerprint(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(dir, 0700))
	child := writeEvenerFixture(t, dir, "child", map[string]any{"parent_session_id": "parent"}, evenerTestTurn("USER_INPUT", "copied"), evenerTestTurn("USER_INPUT", "new"))
	writeEvenerMeta(t, child, map[string]any{"id": "child", "parent_session_id": "parent", "divergence_turn": 2})
	provider, ok := NewProvider(AgentType("evener"), ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: "child"})
	require.NoError(t, err)
	require.True(t, ok)
	first, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	digest := provider.(MultiFileStatHasher).ComputeMultiFileStatHash(child)
	writeEvenerFixture(t, dir, "parent", nil, evenerTestTurn("USER_INPUT", "copied"))
	second, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.NotEqual(t, first.Hash, second.Hash)
	assert.NotEqual(t, digest, provider.(MultiFileStatHasher).ComputeMultiFileStatHash(child))
	out, err := provider.Parse(context.Background(), ParseRequest{Source: source, Fingerprint: second})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	require.Len(t, out.Results[0].Result.Messages, 1)
	assert.Equal(t, "new", out.Results[0].Result.Messages[0].Content)
	require.NoError(t, os.Remove(filepath.Join(dir, "parent.transcript.jsonl")))
	third, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, first.Hash, third.Hash)
}

func TestEvenerProviderMetadataRemovalClearsSourceTitle(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(dir, 0700))
	path := writeEvenerFixture(t, dir, "session", nil, evenerTestTurn("USER_INPUT", "hello"))
	writeEvenerMeta(t, path, map[string]any{"id": "session", "name": "Source title"})
	provider, ok := NewProvider(AgentEvener, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: "session"})
	require.NoError(t, err)
	require.True(t, found)
	before, err := provider.Parse(context.Background(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, before.Results, 1)
	assert.Equal(t, "Source title", before.Results[0].Result.Session.SessionName)
	require.NoError(t, os.Remove(filepath.Join(dir, "session.meta.json")))
	after, err := provider.Parse(context.Background(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, after.Results, 1)
	assert.Empty(t, after.Results[0].Result.Session.SessionName)
	assert.True(t, after.Results[0].Result.Session.SessionNamePresent, "absence is an authoritative empty provider title")
}

func TestEvenerProviderUnavailableParentRetainsChild(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "sessions")
			require.NoError(t, os.MkdirAll(dir, 0700))
			copied := evenerTestTurn("USER_INPUT", "copied")
			child := writeEvenerFixture(t, dir, "child", map[string]any{"parent_session_id": "parent"}, copied, evenerTestTurn("USER_INPUT", "new"))
			writeEvenerMeta(t, child, map[string]any{"id": "child", "parent_session_id": "parent", "divergence_turn": 2})
			parent := filepath.Join(dir, "parent.transcript.jsonl")
			target := ""
			switch kind {
			case "symlink":
				target = writeEvenerFixture(t, t.TempDir(), "parent", nil, copied)
				require.NoError(t, os.Symlink(target, parent))
			case "directory":
				require.NoError(t, os.Mkdir(parent, 0700))
			case "unreadable":
				writeEvenerFixture(t, dir, "parent", nil, copied)
				require.NoError(t, os.Chmod(parent, 0000))
				probe, err := os.Open(parent)
				if err == nil {
					require.NoError(t, probe.Close())
					t.Skip("process can read permission-denied files")
				}
				require.True(t, os.IsPermission(err))
			}
			provider, ok := NewProvider(AgentEvener, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "child"})
			require.NoError(t, err)
			require.True(t, found)
			before, err := provider.Parse(t.Context(), ParseRequest{Source: source})
			require.NoError(t, err)
			require.Len(t, before.Results, 1)
			require.Len(t, before.Results[0].Result.Messages, 2)
			fingerprint, err := provider.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			hasher := provider.(MultiFileStatHasher)
			digest := hasher.ComputeMultiFileStatHash(child)
			if target != "" {
				writeSourceFile(t, target, "outside changed\n")
				after, err := provider.Fingerprint(t.Context(), source)
				require.NoError(t, err)
				assert.Equal(t, fingerprint, after)
				assert.Equal(t, digest, hasher.ComputeMultiFileStatHash(child))
			}
			parentInfo, err := os.Lstat(parent)
			require.NoError(t, err)
			require.NoError(t, os.Remove(parent))
			writeEvenerFixture(t, dir, "parent", nil, copied)
			// Recreating an equal-size parent need not advance filesystem time.
			// Give the replacement a distinct mtime for the stat-digest check.
			replacementTime := parentInfo.ModTime().Add(2 * time.Second)
			require.NoError(t, os.Chtimes(parent, replacementTime, replacementTime))
			available, err := provider.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			assert.NotEqual(t, fingerprint.Hash, available.Hash)
			assert.NotEqual(t, digest, hasher.ComputeMultiFileStatHash(child))
			after, err := provider.Parse(t.Context(), ParseRequest{Source: source})
			require.NoError(t, err)
			require.Len(t, after.Results, 1)
			require.Len(t, after.Results[0].Result.Messages, 1)
			assert.Equal(t, "new", after.Results[0].Result.Messages[0].Content)
		})
	}
}

func TestEvenerProviderTruncatedTranscriptDefersReplacement(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(dir, 0700))
	path := writeEvenerFixture(t, dir, "session", nil, evenerTestTurn("USER_INPUT", "first"), evenerTestTurn("ASSISTANT", "answer"))
	provider, ok := NewProvider(AgentEvener, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "session"})
	require.NoError(t, err)
	require.True(t, found)
	complete, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, complete.Results, 1)
	require.Len(t, complete.Results[0].Result.Messages, 2)
	writeEvenerFixture(t, dir, "session", nil, evenerTestTurn("USER_INPUT", "replacement"))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"kind":"entry","turn":`)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	partial, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(t, err)
	assert.Empty(t, partial.Results, "no partial rows may replace existing history")
	assert.False(t, partial.ResultSetComplete, "an incomplete source must be retried")
	assert.False(t, partial.ForceReplace)
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
	root := t.TempDir()
	dir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(dir, 0700))
	child := writeEvenerFixture(t, dir, "child", map[string]any{"parent_session_id": "parent"}, evenerTestTurn("USER_INPUT", "copied"))
	writeEvenerMeta(t, child, map[string]any{"id": "child", "parent_session_id": "parent", "divergence_turn": 2})
	writeEvenerFixture(t, dir, "parent", nil, evenerTestTurn("USER_INPUT", "copied"))
	provider, ok := NewProvider(AgentEvener, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "child"})
	require.NoError(t, err)
	require.True(t, found)
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	hasher := provider.(MultiFileStatHasher)
	digest := hasher.ComputeMultiFileStatHash(child)
	meta := filepath.Join(dir, "parent.meta.json")
	mtime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, content := range []string{`{"id":"broken"}`, `{"id":"parent"}`, `{bad}`} {
		writeSourceFile(t, meta, content)
		// Equal-size revisions can share a filesystem timestamp tick.
		// Give each revision a distinct mtime before checking its stat digest.
		require.NoError(t, os.Chtimes(meta, mtime, mtime))
		mtime = mtime.Add(2 * time.Second)
		changed, err := provider.Fingerprint(t.Context(), source)
		require.NoError(t, err)
		assert.NotEqual(t, fingerprint.Hash, changed.Hash)
		assert.NotEqual(t, digest, hasher.ComputeMultiFileStatHash(child))
		assert.Equal(t, fingerprint.Size, changed.Size, "parent metadata is a dependency, not child source bytes")
		fingerprint = changed
		digest = hasher.ComputeMultiFileStatHash(child)
	}
	require.NoError(t, os.Remove(meta))
	missing, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.NotEqual(t, fingerprint.Hash, missing.Hash)
	for _, kind := range []string{"symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "symlink" {
				target := filepath.Join(t.TempDir(), "metadata")
				writeSourceFile(t, target, `{"id":"parent"}`)
				require.NoError(t, os.Symlink(target, meta))
			} else {
				require.NoError(t, os.Mkdir(meta, 0700))
			}
			blocked, err := provider.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			assert.NotEqual(t, missing.Hash, blocked.Hash, "absent metadata and invalid metadata change parent eligibility")
			assert.Zero(t, hasher.ComputeMultiFileStatHash(child), "nonregular parent metadata must not be followed by stat freshness")
			require.NoError(t, os.Remove(meta))
			restored, err := provider.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			assert.Equal(t, missing.Hash, restored.Hash)
		})
	}
}

func TestEvenerRawDiscoveryStreamsWithProgress(t *testing.T) {
	for _, count := range []int{1, 200} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			root := t.TempDir()
			sessions := filepath.Join(root, "projects", "demo", "sessions")
			for i := range count {
				writeSourceFile(t, filepath.Join(sessions, fmt.Sprintf("session-%d.transcript.jsonl", i)), "{}\n")
			}
			writeSourceFile(t, filepath.Join(sessions, "ignored.api.jsonl"), "{}\n")
			provider, ok := NewProvider(AgentEvener, ProviderConfig{Roots: []string{root, sessions}})
			require.True(t, ok)
			steps, largestBatch, yielded := 0, 0, 0
			ctx := WithRawCaptureDiscoveryProgress(t.Context(), func() error { steps++; return nil })
			ctx = WithStreamingDiscoveryBufferObserver(ctx, func(n int) { largestBatch = max(largestBatch, n) })
			complete, err := StreamRawCaptureSources(ctx, provider, func(source SourceRef) error {
				yielded++
				plan, err := provider.(RawCaptureProvider).PlanRawCapture(ctx, source)
				require.NoError(t, err)
				require.Len(t, plan.Entries, 1)
				return nil
			})
			require.NoError(t, err)
			assert.True(t, complete)
			assert.Equal(t, count, yielded)
			assert.GreaterOrEqual(t, steps, count)
			assert.Positive(t, largestBatch)
			assert.LessOrEqual(t, largestBatch, streamingDirectoryBatchSize)
			stop := fmt.Errorf("pause audit")
			ctx = WithRawCaptureDiscoveryProgress(t.Context(), func() error { return stop })
			complete, err = StreamRawCaptureSources(ctx, provider, func(SourceRef) error { t.Fatal("yield after pause"); return nil })
			assert.ErrorIs(t, err, stop)
			assert.False(t, complete)
			steps = 0
			ctx = WithRawCaptureDiscoveryProgress(t.Context(), func() error {
				steps++
				if steps == 3 {
					return stop
				}
				return nil
			})
			complete, err = StreamRawCaptureSources(ctx, provider, func(SourceRef) error { return nil })
			assert.ErrorIs(t, err, stop)
			assert.False(t, complete)
			assert.Equal(t, 3, steps)
			cancelled, cancel := context.WithCancel(t.Context())
			cancel()
			complete, err = StreamRawCaptureSources(cancelled, provider, func(SourceRef) error { return nil })
			assert.ErrorIs(t, err, context.Canceled)
			assert.False(t, complete)
			complete, err = StreamRawCaptureSources(t.Context(), provider, func(SourceRef) error { return stop })
			assert.ErrorIs(t, err, stop)
			assert.False(t, complete)
		})
	}
}
