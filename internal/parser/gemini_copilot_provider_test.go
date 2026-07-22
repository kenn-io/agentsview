package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestGeminiProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	sessionID := "gemini-provider"
	sourcePath := filepath.Join(
		root,
		"tmp",
		"my-project",
		geminiChatsDir,
		"session-2026-06-19T12-00-gemini-provider.json",
	)
	writeSourceFile(t, sourcePath, testjsonl.GeminiSessionJSON(
		sessionID,
		"my-project",
		tsEarly,
		tsEarlyS5,
		[]map[string]any{
			testjsonl.GeminiUserMsg("u1", tsEarly, "hello gemini"),
			testjsonl.GeminiAssistantMsg("a1", tsEarlyS5, "hi", nil),
		},
	))

	provider, ok := NewProvider(AgentGemini, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 2)
	assert.Equal(t, filepath.Join(root, "tmp"), plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"session-*.json", "session-*.jsonl"}, plan.Roots[0].IncludeGlobs)
	assert.Equal(t, root, plan.Roots[1].Path)
	assert.False(t, plan.Roots[1].Recursive)
	assert.Equal(t, []string{"projects.json", "trustedFolders.json"}, plan.Roots[1].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)
	assert.Equal(t, "my_project", discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~gemini:" + sessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "write", WatchRoot: filepath.Join(root, "tmp")},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)

	require.NoError(t, os.Remove(sourcePath))
	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: filepath.Join(root, "tmp")},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
	assert.Equal(t, "my_project", changed[0].ProjectHint)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.Error(t, err)
	require.Empty(t, fingerprint)
}

func TestGeminiProviderProjectMetadataChangesClassifyAndFingerprint(t *testing.T) {
	root := t.TempDir()
	sessionID := "gemini-project-metadata"
	projectsPath := filepath.Join(root, "projects.json")
	writeSourceFile(t, projectsPath, `{"projects":{"/Users/alice/code/one":"alias"}}`)
	sourcePath := filepath.Join(
		root,
		"tmp",
		"alias",
		geminiChatsDir,
		"session-2026-06-19T12-00-gemini-project-metadata.json",
	)
	writeSourceFile(t, sourcePath, testjsonl.GeminiSessionJSON(
		sessionID,
		"alias",
		tsEarly,
		tsEarlyS5,
		[]map[string]any{
			testjsonl.GeminiUserMsg("u1", tsEarly, "hello gemini"),
			testjsonl.GeminiAssistantMsg("a1", tsEarlyS5, "hi", nil),
		},
	))

	provider, ok := NewProvider(AgentGemini, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~gemini:" + sessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "one", found.ProjectHint)

	fingerprintOne, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)

	writeSourceFile(t, projectsPath, `{"projects":{"/Users/alice/code/two":"alias"}}`)
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: projectsPath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
	assert.Equal(t, "two", changed[0].ProjectHint)

	fingerprintTwo, err := provider.Fingerprint(context.Background(), changed[0])
	require.NoError(t, err)
	assert.NotEqual(t, fingerprintOne.Hash, fingerprintTwo.Hash)

	source := changed[0]

	// Adding an entry that does not change this session's resolution
	// must NOT change its fingerprint. Cover both metadata files:
	// an unrelated projects.json alias, and an unrelated
	// trustedFolders.json path.
	writeUnrelatedProjectsEntry(t, root)
	fingerprintThree := fingerprintSource(t, provider, source)
	assert.Equal(t, fingerprintTwo.Hash, fingerprintThree.Hash,
		"unrelated projects.json entries must not invalidate the session")
	writeUnrelatedTrustedFolder(t, root)
	fingerprintFour := fingerprintSource(t, provider, source)
	assert.Equal(t, fingerprintTwo.Hash, fingerprintFour.Hash,
		"unrelated trustedFolders.json entries must not invalidate the session")

	// A trustedFolders.json entry that DOES change this session's resolved
	// project (an old-layout hash directory that only trustedFolders.json,
	// not projects.json, ever resolves) must invalidate the fingerprint.
	hashSessionID := "gemini-hash-project"
	hashProjectPath := "/Users/erin/code/hash-project"
	hashDirName := geminiPathHash(hashProjectPath)
	hashSourcePath := filepath.Join(
		root,
		"tmp",
		hashDirName,
		geminiChatsDir,
		"session-2026-06-19T12-00-gemini-hash-project.json",
	)
	writeSourceFile(t, hashSourcePath, testjsonl.GeminiSessionJSON(
		hashSessionID,
		"hash-project",
		tsEarly,
		tsEarlyS5,
		[]map[string]any{
			testjsonl.GeminiUserMsg("u1", tsEarly, "hello gemini"),
			testjsonl.GeminiAssistantMsg("a1", tsEarlyS5, "hi", nil),
		},
	))

	hashFoundBefore, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~gemini:" + hashSessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "unknown", hashFoundBefore.ProjectHint)
	fingerprintHashBefore := fingerprintSource(t, provider, hashFoundBefore)

	writeSourceFile(t, filepath.Join(root, "trustedFolders.json"),
		fmt.Sprintf(`{"trustedFolders":["%s"]}`, hashProjectPath))

	hashFoundAfter, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~gemini:" + hashSessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "hash_project", hashFoundAfter.ProjectHint)
	fingerprintHashAfter := fingerprintSource(t, provider, hashFoundAfter)
	assert.NotEqual(t, fingerprintHashBefore.Hash, fingerprintHashAfter.Hash,
		"a trustedFolders.json entry that changes this session's resolution must invalidate it")
}

// writeUnrelatedProjectsEntry rewrites projects.json to keep the existing
// "alias" -> two resolution while adding an entry under a different alias
// name. It must not affect any session resolved via the "alias" directory.
func writeUnrelatedProjectsEntry(t *testing.T, root string) {
	t.Helper()

	writeSourceFile(t, filepath.Join(root, "projects.json"),
		`{"projects":{"/Users/alice/code/two":"alias","/Users/carol/code/three":"other-alias"}}`)
}

// writeUnrelatedTrustedFolder writes trustedFolders.json with a path whose
// hash does not match any session directory used in this test, so it must
// not affect existing resolutions.
func writeUnrelatedTrustedFolder(t *testing.T, root string) {
	t.Helper()

	writeSourceFile(t, filepath.Join(root, "trustedFolders.json"),
		`{"trustedFolders":["/Users/dave/code/unrelated"]}`)
}

// fingerprintSource fingerprints source via provider, failing the test on error.
func fingerprintSource(t *testing.T, provider Provider, source SourceRef) SourceFingerprint {
	t.Helper()

	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	return fingerprint
}

// TestGeminiProviderFingerprintMatchesReconstructedSource verifies that
// Fingerprint produces the same hash whether it is called with the
// discovery-built SourceRef (ProjectHint already populated) or with a bare
// SourceRef reconstructed from just a path (as happens when a caller looks
// up a source by stored file path without going through discovery). Both
// must resolve the same on-disk project so a metadata-scoped fingerprint
// cannot drift between the two callers.
func TestGeminiProviderFingerprintMatchesReconstructedSource(t *testing.T) {
	root := t.TempDir()
	sessionID := "gemini-reconstructed"
	writeSourceFile(t, filepath.Join(root, "projects.json"),
		`{"projects":{"/Users/alice/code/reconstructed":"alias"}}`)
	sourcePath := filepath.Join(
		root,
		"tmp",
		"alias",
		geminiChatsDir,
		"session-2026-06-19T12-00-gemini-reconstructed.json",
	)
	writeSourceFile(t, sourcePath, testjsonl.GeminiSessionJSON(
		sessionID,
		"alias",
		tsEarly,
		tsEarlyS5,
		[]map[string]any{
			testjsonl.GeminiUserMsg("u1", tsEarly, "hello gemini"),
			testjsonl.GeminiAssistantMsg("a1", tsEarlyS5, "hi", nil),
		},
	))

	provider, ok := NewProvider(AgentGemini, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~gemini:" + sessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "reconstructed", discovered.ProjectHint)
	discoveredFingerprint := fingerprintSource(t, provider, discovered)

	// A bare SourceRef with no Opaque and no ProjectHint, as a caller would
	// build from only a stored file path.
	reconstructed := SourceRef{
		Provider:       AgentGemini,
		Key:            sourcePath,
		DisplayPath:    sourcePath,
		FingerprintKey: sourcePath,
	}
	reconstructedFingerprint := fingerprintSource(t, provider, reconstructed)

	assert.Equal(t, discoveredFingerprint.Hash, reconstructedFingerprint.Hash,
		"discovery-built and reconstructed sources must resolve the same project")
}

func TestGeminiProviderParse(t *testing.T) {
	root := t.TempDir()
	sessionID := "gemini-provider"
	sourcePath := filepath.Join(
		root,
		"tmp",
		"my-project",
		geminiChatsDir,
		"session-2026-06-19T12-00-gemini-provider.json",
	)
	writeSourceFile(t, sourcePath, testjsonl.GeminiSessionJSON(
		sessionID,
		"my-project",
		tsEarly,
		tsEarlyS5,
		[]map[string]any{
			testjsonl.GeminiUserMsg("u1", tsEarly, "hello gemini"),
			testjsonl.GeminiAssistantMsg("a1", tsEarlyS5, "hi", nil),
		},
	))

	provider, ok := NewProvider(AgentGemini, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~gemini:" + sessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, sourcePath, fingerprint.Key)
	assert.Positive(t, fingerprint.Size)
	assert.Positive(t, fingerprint.MTimeNS)
	assert.NotEmpty(t, fingerprint.Hash)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      found,
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "gemini:"+sessionID, result.Result.Session.ID)
	assert.Equal(t, AgentGemini, result.Result.Session.Agent)
	assert.Equal(t, "my_project", result.Result.Session.Project)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Len(t, result.Result.Messages, 2)
}

// A followed project-directory symlink whose target cannot be resolved must
// surface incomplete streaming discovery rather than reading as absent:
// reconciliation treats a clean DiscoverEach as authoritative and would
// tombstone every session beneath the symlink.
func TestGeminiProviderStreamingDiscoveryPropagatesProjectSymlinkErrors(t *testing.T) {
	writeGeminiSession := func(t *testing.T, root, dir, id string) {
		t.Helper()
		path := filepath.Join(
			root, "tmp", dir, geminiChatsDir,
			"session-2026-06-19T12-00-"+id+".json",
		)
		writeSourceFile(t, path, testjsonl.GeminiSessionJSON(
			id, dir, tsEarly, tsEarlyS5,
			[]map[string]any{
				testjsonl.GeminiUserMsg("u1", tsEarly, "hello gemini"),
			},
		))
	}
	discoverEach := func(t *testing.T, root string) ([]string, error) {
		t.Helper()
		provider, ok := NewProvider(AgentGemini, ProviderConfig{
			Roots: []string{root},
		})
		require.True(t, ok)
		discoverer, ok := provider.(StreamingDiscoverer)
		require.True(t, ok)
		var yielded []string
		err := discoverer.DiscoverEach(t.Context(), func(source SourceRef) error {
			yielded = append(yielded, source.DisplayPath)
			return nil
		})
		return yielded, err
	}

	t.Run("dangling project symlink", func(t *testing.T) {
		root := t.TempDir()
		writeGeminiSession(t, root, "healthy-project", "healthy")
		target := filepath.Join(t.TempDir(), "linked-project")
		require.NoError(t, os.MkdirAll(target, 0o755))
		link := filepath.Join(root, "tmp", "linked")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		require.NoError(t, os.RemoveAll(target))

		_, err := discoverEach(t, root)

		require.Error(t, err)
		assert.ErrorIs(t, err, os.ErrNotExist)
		var incomplete DiscoveryIncompleteError
		assert.ErrorAs(t, err, &incomplete)

		require.NoError(t, os.Remove(link))
		yielded, err := discoverEach(t, root)
		require.NoError(t, err)
		assert.Len(t, yielded, 1)
	})

	t.Run("unstatable project symlink target", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("directory read permissions are not enforced on Windows")
		}
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permissions")
		}
		root := t.TempDir()
		writeGeminiSession(t, root, "healthy-project", "healthy")
		targetParent := t.TempDir()
		target := filepath.Join(targetParent, "linked-project")
		require.NoError(t, os.MkdirAll(target, 0o755))
		if err := os.Symlink(target, filepath.Join(root, "tmp", "linked")); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		require.NoError(t, os.Chmod(targetParent, 0o000))
		t.Cleanup(func() { _ = os.Chmod(targetParent, 0o755) })

		_, err := discoverEach(t, root)

		require.Error(t, err)
		assert.ErrorIs(t, err, os.ErrPermission)
		var incomplete DiscoveryIncompleteError
		assert.ErrorAs(t, err, &incomplete)

		require.NoError(t, os.Chmod(targetParent, 0o755))
		yielded, err := discoverEach(t, root)
		require.NoError(t, err)
		assert.Len(t, yielded, 1)
	})
}

func TestCopilotProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	barePath := filepath.Join(root, copilotStateDir, "copilot-provider.jsonl")
	dirEvents := filepath.Join(root, copilotStateDir, "copilot-provider", "events.jsonl")
	workspacePath := filepath.Join(root, copilotStateDir, "copilot-provider", "workspace.yaml")
	content := strings.Join([]string{
		`{"type":"session.start","data":{"sessionId":"copilot-provider","context":{"cwd":"/home/user/code/copilot-app","branch":"main"}},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"hello copilot"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"hi"},"timestamp":"2025-01-15T10:00:02Z"}`,
		`{"type":"session.shutdown","data":{"modelMetrics":{"gpt-5":{"usage":{"inputTokens":100,"outputTokens":20,"cacheReadTokens":30,"cacheWriteTokens":10,"reasoningTokens":5}}}},"timestamp":"2025-01-15T10:00:03Z"}`,
	}, "\n") + "\n"
	writeSourceFile(t, barePath, content)
	writeSourceFile(t, dirEvents, content)
	writeSourceFile(t, workspacePath, "name: Workspace title\n")

	provider, ok := NewProvider(AgentCopilot, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, filepath.Join(root, copilotStateDir), plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*.jsonl", "workspace.yaml"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, dirEvents, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "copilot-provider",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, dirEvents, found.DisplayPath)

	for _, path := range []string{dirEvents, workspacePath} {
		changed, err := provider.SourcesForChangedPath(
			context.Background(),
			ChangedPathRequest{Path: path, EventKind: "write", WatchRoot: filepath.Join(root, copilotStateDir)},
		)
		require.NoError(t, err)
		require.Len(t, changed, 1)
		assert.Equal(t, dirEvents, changed[0].DisplayPath)
	}

	require.NoError(t, os.Remove(dirEvents))
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: dirEvents, EventKind: "remove", WatchRoot: filepath.Join(root, copilotStateDir)},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, barePath, changed[0].DisplayPath)
	writeSourceFile(t, dirEvents, content)

	require.NoError(t, os.Remove(workspacePath))
	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: workspacePath, EventKind: "remove", WatchRoot: filepath.Join(root, copilotStateDir)},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, dirEvents, changed[0].DisplayPath)
	writeSourceFile(t, workspacePath, "name: Workspace title\n")

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, dirEvents, fingerprint.Key)
	assert.Positive(t, fingerprint.Size)
	assert.Positive(t, fingerprint.MTimeNS)
	assert.NotEmpty(t, fingerprint.Hash)

	writeSourceFile(t, workspacePath, "name: Workspace other\n")
	renamedFingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.NotEqual(t, fingerprint.Hash, renamedFingerprint.Hash)
	writeSourceFile(t, workspacePath, "name: Workspace title\n")
	fingerprint, err = provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      found,
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "copilot:copilot-provider", result.Result.Session.ID)
	assert.Equal(t, AgentCopilot, result.Result.Session.Agent)
	assert.Equal(t, "copilot_app", result.Result.Session.Project)
	assert.Equal(t, "Workspace title", result.Result.Session.FirstMessage)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Equal(t, fingerprint.Size, result.Result.Session.File.Size)
	assert.Equal(t, fingerprint.MTimeNS, result.Result.Session.File.Mtime)
	assert.Len(t, result.Result.Messages, 2)
	require.Len(t, result.Result.UsageEvents, 1)
	assert.Equal(t, "gpt-5", result.Result.UsageEvents[0].Model)
}
