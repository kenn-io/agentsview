package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeHybridStreamingDiscoveryReportsIncompleteSQLiteFailure(
	t *testing.T,
) {
	root := t.TempDir()
	storagePath := writeOpenCodeProviderStorageSession(
		t, root, "session", "ses_storage", "project", "Storage",
	)
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "opencode.db"), []byte("not sqlite"), 0o600,
	))
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	requireSourcePathsMatch(t, discovered, []string{storagePath})
	var streamed []SourceRef
	err = provider.(StreamingDiscoverer).DiscoverEach(
		t.Context(), func(source SourceRef) error {
			streamed = append(streamed, source)
			return nil
		},
	)
	require.Error(t, err)
	var incomplete DiscoveryIncompleteError
	require.ErrorAs(t, err, &incomplete)
	assert.Equal(t, AgentOpenCode, incomplete.Provider)
	assert.ErrorContains(t, err, "SQLite")
	requireSourcePathsMatch(t, streamed, []string{storagePath})
	assert.Equal(t, discovered, streamed,
		"incomplete streaming discovery must still expose valid storage sources")
}

func TestOpenCodeHybridStreamingIncompleteRootContinuesLaterRoots(t *testing.T) {
	setup := func(t *testing.T) (Provider, string, string) {
		t.Helper()
		incompleteRoot := t.TempDir()
		storagePath := writeOpenCodeProviderStorageSession(
			t, incompleteRoot, "session", "ses_storage", "project", "Storage",
		)
		require.NoError(t, os.WriteFile(
			filepath.Join(incompleteRoot, "opencode.db"), []byte("not sqlite"), 0o600,
		))
		healthyRoot := t.TempDir()
		dbPath, seeder, db := newTestDBAt(
			t, filepath.Join(healthyRoot, "opencode.db"),
		)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		seeder.AddProject("prj_1", "/workspace/healthy")
		seeder.AddSession(
			"ses_healthy", "prj_1", "", "Healthy", 1700000000000, 1700000010000,
		)
		provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
			Roots: []string{incompleteRoot, healthyRoot},
		})
		require.True(t, ok)
		return provider, storagePath,
			OpenCodeSQLiteVirtualPath(dbPath, "ses_healthy")
	}

	t.Run("returns accumulated incomplete error after later success", func(t *testing.T) {
		provider, storagePath, healthyPath := setup(t)
		var paths []string

		err := provider.(StreamingDiscoverer).DiscoverEach(
			t.Context(), func(source SourceRef) error {
				paths = append(paths, source.DisplayPath)
				return nil
			},
		)

		require.Error(t, err)
		var incomplete DiscoveryIncompleteError
		require.ErrorAs(t, err, &incomplete)
		assert.Equal(t, []string{storagePath, healthyPath}, paths)
	})

	t.Run("later callback error takes precedence", func(t *testing.T) {
		provider, storagePath, healthyPath := setup(t)
		sentinel := errors.New("stop on later root")
		var paths []string

		err := provider.(StreamingDiscoverer).DiscoverEach(
			t.Context(), func(source SourceRef) error {
				paths = append(paths, source.DisplayPath)
				if source.DisplayPath == healthyPath {
					return sentinel
				}
				return nil
			},
		)

		assert.Equal(t, sentinel, err,
			"a later callback error must replace accumulated incompleteness")
		assert.Equal(t, []string{storagePath, healthyPath}, paths)
	})
}

func TestOpenCodeStreamingPartialSQLiteFailureContinuesLaterRoots(t *testing.T) {
	partialRoot := t.TempDir()
	partialDB := filepath.Join(partialRoot, "opencode.db")
	require.NoError(t, os.WriteFile(partialDB, []byte("streamed by test"), 0o600))
	healthyRoot := t.TempDir()
	healthyDB := filepath.Join(healthyRoot, "opencode.db")
	require.NoError(t, os.WriteFile(healthyDB, []byte("streamed by test"), 0o600))
	partialPath := OpenCodeSQLiteVirtualPath(partialDB, "ses_partial")
	healthyPath := OpenCodeSQLiteVirtualPath(healthyDB, "ses_healthy")
	sentinel := errors.New("SQLite row stream failed")
	var streamedDBs []string
	spec := openCodeProviderSpecForAgent(AgentOpenCode)
	spec.streamSQLite = func(
		_ context.Context,
		dbPath string,
		yield func(OpenCodeSessionMeta) error,
	) error {
		streamedDBs = append(streamedDBs, dbPath)
		switch {
		case samePath(dbPath, partialDB):
			if err := yield(OpenCodeSessionMeta{
				SessionID: "ses_partial", VirtualPath: partialPath,
			}); err != nil {
				return err
			}
			return sentinel
		case samePath(dbPath, healthyDB):
			return yield(OpenCodeSessionMeta{
				SessionID: "ses_healthy", VirtualPath: healthyPath,
			})
		default:
			return fmt.Errorf("unexpected SQLite path %q", dbPath)
		}
	}
	sources := newOpenCodeFormatSourceSet(
		[]string{partialRoot, healthyRoot}, spec, nil,
	)
	var paths []string

	err := sources.DiscoverEach(t.Context(), func(source SourceRef) error {
		paths = append(paths, source.DisplayPath)
		return nil
	})

	require.Error(t, err)
	require.ErrorIs(t, err, sentinel)
	var incomplete DiscoveryIncompleteError
	require.ErrorAs(t, err, &incomplete)
	assert.Equal(t, AgentOpenCode, incomplete.Provider)
	assert.Equal(t, []string{partialPath, healthyPath}, paths,
		"a partial row stream must retain its yield and continue later roots")
	assert.Equal(t, []string{partialDB, healthyDB}, streamedDBs,
		"the later configured root must still be traversed")
}

func TestOpenCodeStreamingStorageFailureContinuesLaterRoots(t *testing.T) {
	failedRoot := t.TempDir()
	writeOpenCodeProviderStorageSession(
		t, failedRoot, "session", "ses_failed", "project", "Failed",
	)
	failedStorageRoot := filepath.Join(failedRoot, "storage", "session")
	healthyRoot := t.TempDir()
	dbPath, seeder, db := newTestDBAt(
		t, filepath.Join(healthyRoot, "opencode.db"),
	)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	seeder.AddProject("prj_1", "/workspace/healthy")
	seeder.AddSession(
		"ses_healthy", "prj_1", "", "Healthy", 1700000000000, 1700000010000,
	)
	healthyPath := OpenCodeSQLiteVirtualPath(dbPath, "ses_healthy")
	discoveryErr := errors.New("read failed storage root")
	ctx := withStreamingDirectoryReader(t.Context(), func(
		ctx context.Context, dir string, yield func(os.DirEntry) error,
	) error {
		if samePath(dir, failedStorageRoot) {
			return discoveryErr
		}
		return streamDirectoryEntriesDirect(ctx, dir, yield)
	})
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
		Roots: []string{failedRoot, healthyRoot},
	})
	require.True(t, ok)
	var paths []string

	err := provider.(StreamingDiscoverer).DiscoverEach(
		ctx, func(source SourceRef) error {
			paths = append(paths, source.DisplayPath)
			return nil
		},
	)

	require.ErrorIs(t, err, discoveryErr)
	var incomplete DiscoveryIncompleteError
	require.ErrorAs(t, err, &incomplete)
	assert.Equal(t, AgentOpenCode, incomplete.Provider)
	assert.Equal(t, []string{healthyPath}, paths,
		"a root-local storage failure must not starve later roots")
}

func TestOpenCodeStreamingSQLiteOnlyFailureContinuesLaterRoots(t *testing.T) {
	failedRoot := t.TempDir()
	failedDB := filepath.Join(failedRoot, "opencode.db")
	require.NoError(t, os.WriteFile(failedDB, []byte("not sqlite"), 0o600))
	healthyRoot := t.TempDir()
	dbPath, seeder, db := newTestDBAt(
		t, filepath.Join(healthyRoot, "opencode.db"),
	)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	seeder.AddProject("prj_1", "/workspace/healthy")
	seeder.AddSession(
		"ses_healthy", "prj_1", "", "Healthy", 1700000000000, 1700000010000,
	)
	healthyPath := OpenCodeSQLiteVirtualPath(dbPath, "ses_healthy")
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
		Roots: []string{failedRoot, healthyRoot},
	})
	require.True(t, ok)
	var paths []string

	err := provider.(StreamingDiscoverer).DiscoverEach(
		t.Context(), func(source SourceRef) error {
			paths = append(paths, source.DisplayPath)
			return nil
		},
	)

	require.Error(t, err)
	assert.ErrorContains(t, err, "file is not a database")
	var incomplete DiscoveryIncompleteError
	require.ErrorAs(t, err, &incomplete)
	assert.Equal(t, AgentOpenCode, incomplete.Provider)
	assert.Equal(t, []string{healthyPath}, paths,
		"a root-local SQLite failure must not starve later roots")
}

func TestOpenCodeSQLiteOnlyStreamingDiscoveryPropagatesSQLiteFailure(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "opencode.db"), []byte("not sqlite"), 0o600,
	))
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	_, err := provider.Discover(t.Context())
	require.Error(t, err)
	err = provider.(StreamingDiscoverer).DiscoverEach(
		t.Context(), func(SourceRef) error { return nil },
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SQLite")
}

func TestOpenCodeHybridStreamingDiscoveryPropagatesNestedStorageFailure(t *testing.T) {
	root := t.TempDir()
	writeOpenCodeProviderStorageSession(
		t, root, "session", "ses_storage", "project", "Storage",
	)
	projectDir := filepath.Join(root, "storage", "session", "global")
	injected := errors.New("nested storage read failed")
	ctx := withStreamingDirectoryReader(t.Context(), func(
		ctx context.Context, dir string, yield func(os.DirEntry) error,
	) error {
		if samePath(dir, projectDir) {
			return injected
		}
		return streamDirectoryEntriesDirect(ctx, dir, yield)
	})
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	err := provider.(StreamingDiscoverer).DiscoverEach(
		ctx, func(SourceRef) error { return nil },
	)

	assert.ErrorIs(t, err, injected)
}

// A followed project-directory symlink whose target cannot be resolved must
// surface incomplete streaming discovery rather than reading as absent:
// reconciliation treats a clean DiscoverEach as authoritative and would
// tombstone every session beneath the symlink.
func TestOpenCodeStorageStreamingDiscoveryPropagatesProjectSymlinkErrors(t *testing.T) {
	discoverEach := func(t *testing.T, root string) ([]string, error) {
		t.Helper()
		provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
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
		healthy := writeOpenCodeProviderStorageSession(
			t, root, "session", "ses_healthy", "project", "Healthy",
		)
		target := filepath.Join(t.TempDir(), "linked-project")
		require.NoError(t, os.MkdirAll(target, 0o755))
		link := filepath.Join(root, "storage", "session", "linked")
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
		assert.Equal(t, []string{healthy}, yielded)
	})

	t.Run("unstatable project symlink target", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("directory read permissions are not enforced on Windows")
		}
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permissions")
		}
		root := t.TempDir()
		healthy := writeOpenCodeProviderStorageSession(
			t, root, "session", "ses_healthy", "project", "Healthy",
		)
		targetParent := t.TempDir()
		target := filepath.Join(targetParent, "linked-project")
		require.NoError(t, os.MkdirAll(target, 0o755))
		link := filepath.Join(root, "storage", "session", "linked")
		if err := os.Symlink(target, link); err != nil {
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
		assert.Equal(t, []string{healthy}, yielded)
	})
}

func TestOpenCodeProviderStorageSourceMethods(t *testing.T) {

	root := t.TempDir()
	sessionPath := writeOpenCodeProviderStorageSession(
		t, root, "session", "ses_provider", "opencode-app", "Provider Session",
	)
	projectPath := filepath.Join(root, "storage", "project", "global.json")
	writeOpenCodeStorageFile(t, projectPath, map[string]any{
		"id": "global", "worktree": "/home/user/code/opencode-app",
	})

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	for i := range 64 {
		writeOpenCodeStorageFile(t, filepath.Join(
			root, "storage", "session", "global",
			fmt.Sprintf("ses_concrete_%02d.json", i),
		), map[string]any{
			"id":        fmt.Sprintf("ses_concrete_%02d", i),
			"directory": fmt.Sprintf("/work/concrete-%02d", i),
		})
	}

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 2)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.False(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{
		"opencode.db", "opencode.db-wal",
	}, plan.Roots[0].IncludeGlobs)
	assert.Equal(t, filepath.Join(root, "storage"), plan.Roots[1].Path)
	assert.True(t, plan.Roots[1].Recursive)
	assert.Equal(t, []string{"*.json"}, plan.Roots[1].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 65)
	var source SourceRef
	for _, candidate := range discovered {
		if candidate.DisplayPath == sessionPath {
			source = candidate
			break
		}
	}
	require.NotEmpty(t, source.DisplayPath)
	assert.Equal(t, AgentOpenCode, source.Provider)
	assert.Equal(t, sessionPath, source.DisplayPath)
	assert.Equal(t, sessionPath, source.FingerprintKey)
	assert.Equal(t, "opencode_app", source.ProjectHint)
	legacySessionPath := filepath.Join(
		root, "storage", "session", "global", "ses_legacy.json",
	)
	writeOpenCodeStorageFile(t, legacySessionPath, map[string]any{
		"id": "ses_legacy", "title": "Legacy project session",
		"time": map[string]any{"created": int64(1700000000000)},
	})
	legacyChanged, err := provider.SourcesForChangedPath(
		context.Background(), ChangedPathRequest{
			Path: legacySessionPath, EventKind: "write",
			WatchRoot: filepath.Join(root, "storage"),
		},
	)
	require.NoError(t, err)
	require.Len(t, legacyChanged, 1)
	assert.Equal(t, legacySessionPath, legacyChanged[0].DisplayPath)
	otherSessionPath := filepath.Join(
		root, "storage", "session", "other-project", "ses_other.json",
	)
	writeOpenCodeStorageFile(t, otherSessionPath, map[string]any{
		"id": "ses_other", "directory": "/home/user/code/other-app",
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "project", "other-project.json",
	), map[string]any{
		"id": "other-project", "worktree": "/home/user/code/other-app",
	})

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "remote~opencode:ses_provider",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sessionPath, found.DisplayPath)

	messagePath := filepath.Join(
		root, "storage", "message", "ses_provider", "msg_1.json",
	)
	partPath := filepath.Join(root, "storage", "part", "msg_1", "prt_1.json")
	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "session", path: sessionPath},
		{name: "message", path: messagePath},
		{name: "part", path: partPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, err := provider.SourcesForChangedPath(
				context.Background(),
				ChangedPathRequest{
					Path:      tc.path,
					EventKind: "write",
					WatchRoot: filepath.Join(root, "storage"),
				},
			)
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, sessionPath, changed[0].DisplayPath)
		})
	}
	changed, err := provider.SourcesForChangedPath(
		context.Background(), ChangedPathRequest{
			Path: projectPath, EventKind: "write",
			WatchRoot: filepath.Join(root, "storage"),
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, legacySessionPath, changed[0].DisplayPath)
	assert.Equal(t, "opencode_app", changed[0].ProjectHint)
	assert.NotEqual(t, sessionPath, changed[0].DisplayPath,
		"project changes must not fan out to sessions with concrete directories")
	assert.NotEqual(t, otherSessionPath, changed[0].DisplayPath)
	t.Logf("project event routed sources=%d ProjectHint=%q unrelated=%q excluded", len(changed), changed[0].ProjectHint, otherSessionPath)
	relevance, err := ResolveChangedPathRelevance(
		context.Background(), provider, ChangedPathRequest{
			Path: projectPath, EventKind: "write",
			WatchRoot: filepath.Join(root, "storage"),
		},
	)
	require.NoError(t, err)
	assert.Equal(t, ChangedPathDataBearing, relevance)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, sessionPath, fingerprint.Key)
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
	assert.Equal(t, "opencode:ses_provider", result.Result.Session.ID)
	assert.Equal(t, AgentOpenCode, result.Result.Session.Agent)
	assert.Equal(t, "opencode_app", result.Result.Session.Project)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.True(t,
		HasOpenCodeStorageFingerprint(result.Result.Session.File.Hash),
		"Parse must retain the provider content fingerprint")
	assert.Equal(t, fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Len(t, result.Result.Messages, 1)

	priorHash := fingerprint.Hash
	updatedPartPath := filepath.Join(
		root, "storage", "part", "msg_1", "prt_1.json",
	)
	rawPart, err := os.ReadFile(updatedPartPath)
	require.NoError(t, err)
	partInfo, err := os.Stat(updatedPartPath)
	require.NoError(t, err)
	updatedPart := strings.Replace(
		string(rawPart), "Hello from storage", "Changed in storage", 1,
	)
	require.NotEqual(t, string(rawPart), updatedPart)
	require.NoError(t, os.WriteFile(
		updatedPartPath, []byte(updatedPart), 0o644,
	))
	require.NoError(t, os.Chtimes(
		updatedPartPath, partInfo.ModTime(), partInfo.ModTime(),
	))
	laterFingerprint, err := provider.Fingerprint(
		context.Background(), found,
	)
	require.NoError(t, err)
	require.NotEqual(t, priorHash, laterFingerprint.Hash)
	staleRequestOutcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: found, Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, staleRequestOutcome.Results, 1)
	assert.Equal(t, "Changed in storage",
		staleRequestOutcome.Results[0].Result.Messages[0].Content,
		"file-backed Parse must materialize the later storage snapshot")
	assert.Equal(t, laterFingerprint.Hash,
		staleRequestOutcome.Results[0].Result.Session.File.Hash,
		"file-backed Parse must retain the hash from the snapshot it parsed")

	require.NoError(t, os.Remove(sessionPath), "remove storage session")
	removed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      sessionPath,
			EventKind: "remove",
			WatchRoot: filepath.Join(root, "storage"),
		},
	)
	require.NoError(t, err)
	require.Len(t, removed, 1)
	assert.Equal(t, sessionPath, removed[0].DisplayPath)
	assert.Equal(t, "global", removed[0].ProjectHint)
}

func TestOpenCodeProviderProjectIndexSurvivesProviderRecreation(t *testing.T) {
	root := t.TempDir()
	projectID := "project-index"
	projectPath := filepath.Join(
		root, "storage", "project", projectID+".json",
	)
	writeOpenCodeStorageFile(t, projectPath, map[string]any{
		"id": projectID, "worktree": "/home/user/code/indexed",
	})
	for i := range 256 {
		id := fmt.Sprintf("ses_concrete_%03d", i)
		writeOpenCodeStorageFile(t, filepath.Join(
			root, "storage", "session", projectID, id+".json",
		), map[string]any{
			"id": id, "directory": "/home/user/code/concrete",
		})
	}
	legacyPath := filepath.Join(
		root, "storage", "session", projectID, "ses_legacy.json",
	)
	writeOpenCodeStorageFile(t, legacyPath, map[string]any{
		"id": "ses_legacy",
	})

	factory := newOpenCodeProviderFactory(AgentDef{
		Type: AgentOpenCode, IDPrefix: "opencode",
	})
	config := ProviderConfig{Roots: []string{root}}
	primingProvider := factory.NewProvider(config)
	_, err := primingProvider.Discover(t.Context())
	require.NoError(t, err)

	// Changed-path classification creates fresh providers, so the factory-owned index must persist.
	changedProvider := factory.NewProvider(config)
	changed, err := changedProvider.SourcesForChangedPath(
		t.Context(), ChangedPathRequest{
			Path: projectPath, EventKind: "write",
			WatchRoot: filepath.Join(root, "storage"),
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, legacyPath, changed[0].DisplayPath)
}

func TestOpenCodeProviderReturnsSkipNoSessionForEmptyStorageSession(t *testing.T) {
	root := t.TempDir()
	const sessionID = "ses_empty"
	sessionPath := filepath.Join(
		root, "storage", "session", "global", sessionID+".json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id": sessionID, "title": "New session - 2026-01-01T00:00:00Z",
		"time": map[string]any{"created": int64(1700000000000)},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "project", "global.json",
	), map[string]any{"id": "global", "worktree": "/"})

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: sources[0], Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
	assert.Empty(t, outcome.Results)
}

func TestOpenCodeProviderProjectEventRetainsMalformedSession(t *testing.T) {
	root := t.TempDir()
	projectID := "malformed-project"
	projectPath := filepath.Join(root, "storage", "project", projectID+".json")
	sessionPath := filepath.Join(
		root, "storage", "session", projectID, "ses_malformed.json",
	)
	writeOpenCodeStorageFile(t, projectPath, map[string]any{
		"id": projectID, "worktree": "/work/malformed-app",
	})
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte("{"), 0o644))

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: projectPath, EventKind: "write",
		WatchRoot: filepath.Join(root, "storage"),
	})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, sessionPath, sources[0].DisplayPath)
}

func TestOpenCodeProviderClearsResolvedMalformedSessionsIndividually(t *testing.T) {
	root := t.TempDir()
	projectID := "malformed-project"
	projectPath := filepath.Join(root, "storage", "project", projectID+".json")
	sessionDir := filepath.Join(root, "storage", "session", projectID)
	repairedPath := filepath.Join(sessionDir, "ses_repaired.json")
	removedPath := filepath.Join(sessionDir, "ses_removed.json")
	writeOpenCodeStorageFile(t, projectPath, map[string]any{
		"id": projectID, "worktree": "/work/malformed-app",
	})
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	for _, path := range []string{repairedPath, removedPath} {
		require.NoError(t, os.WriteFile(path, []byte("{"), 0o644))
	}

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	projectSources := func() ([]SourceRef, error) {
		return provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
			Path: projectPath, EventKind: "write",
			WatchRoot: filepath.Join(root, "storage"),
		})
	}
	sources, err := projectSources()
	require.NoError(t, err)
	require.Len(t, sources, 2)
	assert.Equal(t, removedPath, sources[0].DisplayPath)
	assert.Equal(t, repairedPath, sources[1].DisplayPath)

	writeOpenCodeStorageFile(t, repairedPath, map[string]any{
		"id": "ses_repaired", "directory": "/work/repaired-app",
	})
	_, err = provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: repairedPath, EventKind: "write",
		WatchRoot: filepath.Join(root, "storage"),
	})
	require.NoError(t, err)
	sources, err = projectSources()
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, removedPath, sources[0].DisplayPath,
		"an unresolved malformed session must keep the fallback active")

	require.NoError(t, os.Remove(removedPath))
	_, err = provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: removedPath, EventKind: "remove",
		WatchRoot: filepath.Join(root, "storage"),
	})
	require.NoError(t, err)
	require.NoError(t, os.Remove(repairedPath))
	require.NoError(t, os.Remove(sessionDir))
	require.NoError(t, os.WriteFile(sessionDir, []byte("not a directory"), 0o644))

	sources, err = projectSources()
	require.NoError(t, err,
		"resolved malformed sessions must not leave the fallback scan active")
	assert.Empty(t, sources)
}

func TestOpenCodeProviderProjectMetadataRefreshAndMalformedReturnsError(
	t *testing.T,
) {
	root := t.TempDir()
	projectID := "legacy-project"
	sessionID := "ses_refresh"
	sessionPath := filepath.Join(
		root, "storage", "session", projectID, sessionID+".json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id": sessionID, "title": "Refresh", "time": map[string]any{
			"created": int64(1700000000000), "updated": int64(1700000060000),
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", sessionID, "msg_1.json",
	), map[string]any{
		"id": "msg_1", "sessionID": sessionID, "role": "user",
		"time": map[string]any{"created": int64(1700000000000)},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_1", "part_1.json",
	), map[string]any{
		"id": "part_1", "sessionID": sessionID, "messageID": "msg_1",
		"type": "text", "text": "hello",
		"time": map[string]any{"created": int64(1700000000000)},
	})
	projectPath := filepath.Join(
		root, "storage", "project", projectID+".json",
	)
	writeOpenCodeStorageFile(t, projectPath, map[string]any{
		"id": projectID, "worktree": "/home/user/code/old-app",
	})

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	parse := func(source SourceRef) (SourceFingerprint, ParsedSession) {
		fingerprint, err := provider.Fingerprint(t.Context(), source)
		require.NoError(t, err)
		outcome, err := provider.Parse(t.Context(), ParseRequest{
			Source: source, Fingerprint: fingerprint,
		})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		return fingerprint, outcome.Results[0].Result.Session
	}

	priorFingerprint, prior := parse(discovered[0])
	assert.Equal(t, "/home/user/code/old-app", prior.Cwd)
	projectInfo, err := os.Stat(projectPath)
	require.NoError(t, err)

	writeOpenCodeStorageFile(t, projectPath, map[string]any{
		"id": projectID, "worktree": "/home/user/code/new-app",
	})
	afterProjectInfo, err := os.Stat(projectPath)
	require.NoError(t, err)
	require.Equal(t, projectInfo.Size(), afterProjectInfo.Size())
	require.NoError(t, os.Chtimes(
		projectPath, projectInfo.ModTime(), projectInfo.ModTime(),
	))
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: projectPath, EventKind: "write",
		WatchRoot: filepath.Join(root, "storage"),
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	refreshedFingerprint, refreshed := parse(changed[0])
	assert.Equal(t, "/home/user/code/new-app", refreshed.Cwd)
	assert.Equal(t, "new_app", refreshed.Project)
	assert.NotEqual(t, priorFingerprint.Hash, refreshedFingerprint.Hash)
	assert.Equal(t, refreshedFingerprint.Hash, refreshed.File.Hash)
	t.Logf(
		"metadata rewrite changed fingerprint and refreshed Cwd=%q Project=%q",
		refreshed.Cwd, refreshed.Project,
	)

	require.NoError(t, os.WriteFile(projectPath, []byte("{"), 0o644))
	_, err = provider.Fingerprint(t.Context(), changed[0])
	assert.Error(t, err)
}

func TestOpenCodeProviderProjectMetadataChangeReportsSessionDirectoryError(
	t *testing.T,
) {
	root := t.TempDir()
	projectID := "broken-project"
	projectPath := filepath.Join(root, "storage", "project", projectID+".json")
	require.NoError(t, os.MkdirAll(filepath.Dir(projectPath), 0o755))
	writeOpenCodeStorageFile(t, projectPath, map[string]any{
		"id": projectID, "worktree": "/home/user/code/broken-app",
	})
	require.NoError(t, os.MkdirAll(filepath.Join(root, "storage", "session"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "storage", "session", projectID),
		[]byte("not a directory"), 0o644,
	))

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	formatProvider, ok := provider.(*openCodeFormatProvider)
	require.True(t, ok)
	_, err := formatProvider.sources.sourcesForProject(root, projectID)
	assert.Error(t, err)
	_, publicErr := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: projectPath, EventKind: "write",
		WatchRoot: filepath.Join(root, "storage"),
	})
	assert.Error(t, publicErr)
}

func TestOpenCodeProviderSQLiteSourceMethods(t *testing.T) {

	fixture := openCodeSQLiteProviderReadFixture(t)
	root := fixture.Root
	dbPath := fixture.DBPath
	virtualPath := fixture.SQLiteVirtualPath

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	relevance, err := ResolveChangedPathRelevance(
		context.Background(), provider, ChangedPathRequest{
			Path:      filepath.Join(root, "storage", "project", "global.json"),
			EventKind: "write",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	assert.Equal(t, ChangedPathUnclassified, relevance,
		"SQLite-only roots must ignore file-backed project metadata events")

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	// A SQLite-layout root has no storage tree, so it keeps the single
	// recursive unit and plans no watch root that does not exist.
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{
		"*.json", "opencode.db", "opencode.db-wal",
	}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	requireSourcePathsMatch(t, discovered, fixture.AllVirtualPaths)
	requireContainsSourcePath(t, discovered, virtualPath)
	maxBuffered := 0
	streamed := make([]SourceRef, 0, len(fixture.AllVirtualPaths))
	streamCtx := WithStreamingDiscoveryBufferObserver(
		context.Background(),
		func(buffered int) { maxBuffered = max(maxBuffered, buffered) },
	)
	require.NoError(t, provider.(StreamingDiscoverer).DiscoverEach(
		streamCtx,
		func(source SourceRef) error {
			streamed = append(streamed, source)
			return nil
		},
	))
	requireSourcePathsMatch(t, streamed, fixture.AllVirtualPaths)
	assert.Equal(t, 1, maxBuffered,
		"SQLite discovery must expose one rows.Next source at a time")

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: dbPath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	requireSourcePathsMatch(t, changed, fixture.AllVirtualPaths)

	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: virtualPath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	requireSourcePathsMatch(t, changed, []string{virtualPath})

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~opencode:" + fixture.TargetSessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtualPath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, virtualPath, fingerprint.Key)
	assert.Equal(t, int64(1700000060000)*1_000_000, fingerprint.MTimeNS)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      found,
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "opencode:ses_sqlite", result.Result.Session.ID)
	assert.Equal(t, "sqlite_app", result.Result.Session.Project)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, "Hello from sqlite", result.Result.Messages[0].Content)

	removedRoot, removedDBPath := newRemovedOpenCodeDBPath(t)
	removedProvider, ok := NewProvider(AgentOpenCode, ProviderConfig{
		Roots: []string{removedRoot},
	})
	require.True(t, ok)
	removed, err := removedProvider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: removedDBPath, EventKind: "remove", WatchRoot: removedRoot},
	)
	require.NoError(t, err)
	assert.Empty(t, removed, "removed sqlite DBs have no stateless virtual source list")
}

func TestOpenCodeProviderIgnoresNonDataSQLiteSidecars(t *testing.T) {
	tests := []struct {
		name      string
		suffix    string
		create    bool
		size      int
		remove    bool
		eventKind string
	}{
		{name: "missing WAL", suffix: "-wal", eventKind: "remove"},
		{name: "empty WAL", suffix: "-wal", create: true, eventKind: "write"},
		{name: "partial WAL", suffix: "-wal", create: true, size: 3, eventKind: "write"},
		{name: "header-only WAL", suffix: "-wal", create: true, size: 32, eventKind: "write"},
		{name: "removed WAL", suffix: "-wal", create: true, size: 64, remove: true, eventKind: "remove"},
		{name: "SHM", suffix: "-shm", create: true, size: 32 * 1024, eventKind: "write"},
		{name: "unknown sidecar", suffix: "-backup", create: true, size: 64, eventKind: "write"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := openCodeSQLiteProviderReadFixture(t)
			path := fixture.DBPath + tc.suffix
			if tc.create {
				require.NoError(t, os.WriteFile(path, make([]byte, tc.size), 0o600))
			}
			if tc.remove {
				require.NoError(t, os.Remove(path))
			}

			provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
				Roots: []string{fixture.Root},
			})
			require.True(t, ok)
			changed, err := provider.SourcesForChangedPath(
				context.Background(),
				ChangedPathRequest{
					Path:      path,
					EventKind: tc.eventKind,
					WatchRoot: fixture.Root,
				},
			)
			require.NoError(t, err)
			assert.Empty(t, changed)
		})
	}
}

func TestOpenCodeFormatWatchPathRelevance(t *testing.T) {
	tests := []struct {
		name  string
		agent AgentType
		db    string
	}{
		{name: "OpenCode", agent: AgentOpenCode, db: "opencode.db"},
		{name: "Kilo", agent: AgentKilo, db: "kilo.db"},
		{name: "MiMoCode", agent: AgentMiMoCode, db: "mimocode.db"},
		{name: "Icodemate", agent: AgentIcodemate, db: "icodemate.db"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			provider, ok := NewProvider(tc.agent, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)

			check := func(path string) ChangedPathRelevance {
				relevance, err := ResolveChangedPathRelevance(
					t.Context(), provider, ChangedPathRequest{
						Path: path, WatchRoot: root,
					},
				)
				require.NoError(t, err)
				return relevance
			}

			assert.Equal(t, ChangedPathNonData,
				check(filepath.Join(root, tc.db+"-shm")))
			assert.Equal(t, ChangedPathNonData,
				check(filepath.Join(root, tc.db+"-wal")),
				"missing WAL is non-data")
			walPath := filepath.Join(root, tc.db+"-wal")
			require.NoError(t, os.WriteFile(walPath, make([]byte, 32), 0o600))
			assert.Equal(t, ChangedPathNonData, check(walPath),
				"32-byte WAL header has no transaction frame")
			require.NoError(t, os.WriteFile(walPath, make([]byte, 33), 0o600))
			assert.Equal(t, ChangedPathDataBearing, check(walPath),
				"WAL data begins after the 32-byte header")
			assert.Equal(t, ChangedPathDataBearing,
				check(filepath.Join(root, tc.db)),
				"the main database remains push-worthy even when absent")
			assert.Equal(t, ChangedPathUnclassified,
				check(filepath.Join(root, tc.db+"-backup")))
			assert.Equal(t, ChangedPathUnclassified,
				check(filepath.Join(t.TempDir(), tc.db+"-shm")),
				"the same basename outside the configured root is unclaimed")
		})
	}
}

func TestOpenCodeFormatWatchPathRelevanceFailsOpen(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory-permission stat failures are not portable to this runner")
	}
	root := t.TempDir()
	walDir := filepath.Join(root, "locked")
	require.NoError(t, os.Mkdir(walDir, 0o700))
	walPath := filepath.Join(walDir, "opencode.db-wal")
	require.NoError(t, os.WriteFile(walPath, make([]byte, 64), 0o600))
	require.NoError(t, os.Chmod(walDir, 0o000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(walDir, 0o700)) })

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{walDir}})
	require.True(t, ok)
	relevance, err := ResolveChangedPathRelevance(
		t.Context(), provider, ChangedPathRequest{Path: walPath, WatchRoot: walDir},
	)
	require.NoError(t, err)
	assert.Equal(t, ChangedPathDataBearing, relevance,
		"unexpected WAL stat failures must retain the push")
}

func TestSQLiteWALHasFramesFailsOpenOnStatError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory-permission stat failures are not portable to Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	locked := filepath.Join(t.TempDir(), "locked")
	require.NoError(t, os.Mkdir(locked, 0o700))
	walPath := filepath.Join(locked, "opencode.db-wal")
	require.NoError(t, os.WriteFile(walPath, make([]byte, 64), 0o600))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() {
		require.NoError(t, os.Chmod(locked, 0o700))
	})

	assert.True(t, sqliteWALHasFrames(walPath),
		"stat errors other than not-exist must fail open so real WAL updates are not dropped")
}

func TestOpenCodeProviderReadsLiveSQLiteWAL(t *testing.T) {
	dbPath, seeder, writer := newTestDB(t)
	defer writer.Close()

	var journalMode string
	require.NoError(t, writer.QueryRow("PRAGMA journal_mode=WAL").Scan(&journalMode))
	require.Equal(t, "wal", journalMode)
	_, err := writer.Exec("PRAGMA wal_autocheckpoint=0")
	require.NoError(t, err)
	seedStandardSession(t, seeder)

	walPath := dbPath + "-wal"
	walInfo, err := os.Stat(walPath)
	require.NoError(t, err)
	require.Greater(t, walInfo.Size(), sqliteWALHeaderSize)

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
		Roots:   []string{filepath.Dir(dbPath)},
		Machine: "devbox",
	})
	require.True(t, ok)
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      walPath,
			EventKind: "write",
			WatchRoot: filepath.Dir(dbPath),
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: changed[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "opencode:ses_abc", outcome.Results[0].Result.Session.ID)
	assert.Equal(t, "Sure, I can help with Go.",
		outcome.Results[0].Result.Messages[1].Content)
}

// TestOpenCodeProviderSQLiteDiscoversAllListedSessions guards the refactor that
// builds SourceRefs directly from the listed SQLite metadata instead of
// reopening the DB per row via OpenCodeSQLiteSessionExists. Every row read from
// the DB must surface as a discoverable source with its dbPath#id virtual path.
func TestOpenCodeProviderSQLiteDiscoversAllListedSessions(t *testing.T) {

	fixture := openCodeSQLiteProviderReadFixture(t)
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
		Roots:   []string{fixture.Root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	requireSourcePathsMatch(t, discovered, fixture.AllVirtualPaths)
	for _, src := range discovered {
		assert.Equal(t, src.DisplayPath, src.FingerprintKey)
	}
}

// TestOpenCodeProviderSQLiteFingerprintUsesDiscoveryMeta pins that
// fingerprinting a discovered SQLite-backed session reuses the time_updated
// already listed during discovery instead of reopening the shared DB once per
// session. Replacing the DB with unreadable bytes after discovery makes any
// reopen fail, so a successful fingerprint proves the metadata was carried on
// the source.
func TestOpenCodeProviderSQLiteFingerprintUsesDiscoveryMeta(t *testing.T) {

	root := t.TempDir()
	dbPath, seeder, db := newTestDBAt(t, filepath.Join(root, "opencode.db"))
	seeder.AddProject("prj_1", "/home/user/code/sqlite-app")
	seeder.AddSession(
		"ses_meta", "prj_1", "", "Meta", 1700000000000, 1700000010000,
	)
	require.NoError(t, db.Close())

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)

	garbage := []byte("not a sqlite database")
	require.NoError(t, os.WriteFile(dbPath, garbage, 0o644))

	fp, err := provider.Fingerprint(context.Background(), discovered[0])
	require.NoError(t, err,
		"fingerprint must not reopen the SQLite DB for a discovered source")
	assert.Equal(t, OpenCodeSQLiteVirtualPath(dbPath, "ses_meta"), fp.Key)
	assert.Equal(t, int64(1700000010000000000), fp.MTimeNS,
		"fingerprint mtime must be the discovered composite in ns")
	assert.Zero(t, fp.Size,
		"a per-session fingerprint must not carry the shared container's "+
			"size: every session in the root shares one opencode.db, so any "+
			"one session's write would change every other session's "+
			"fingerprint and drop its freshness skip")
}

func TestOpenCodeProviderHybridDiscoveryFiltersSQLiteDuplicate(t *testing.T) {

	root := t.TempDir()
	storagePath := writeOpenCodeProviderStorageSession(
		t, root, "session", "ses_dup", "storage-app", "Storage Session",
	)
	dbPath, seeder, db := newTestDBAt(t, filepath.Join(root, "opencode.db"))
	defer db.Close()
	seeder.AddProject("prj_1", "/home/user/code/sqlite-app")
	seeder.AddSession("ses_dup", "prj_1", "", "Duplicate", 1700000000000, 1700000010000)
	seeder.AddSession("ses_db_only", "prj_1", "", "DB Only", 1700000000000, 1700000020000)
	virtualOnly := OpenCodeSQLiteVirtualPath(dbPath, "ses_db_only")

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	wantPaths := []string{storagePath, virtualOnly}
	requireSourcePathsMatch(t, discovered, wantPaths)
	var streamed []SourceRef
	require.NoError(t, provider.(StreamingDiscoverer).DiscoverEach(
		t.Context(), func(source SourceRef) error {
			streamed = append(streamed, source)
			return nil
		},
	))
	requireSourcePathsMatch(t, streamed, wantPaths)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: OpenCodeSQLiteVirtualPath(dbPath, "ses_dup"),
		FullSessionID:  "opencode:ses_dup",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, storagePath, found.DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      OpenCodeSQLiteVirtualPath(dbPath, "ses_dup"),
			EventKind: "write",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, storagePath, changed[0].DisplayPath,
		"a storage source that appears before rehydration remains canonical")
}

func TestOpenCodeHybridStreamingDedupUsesStorageTraversalSnapshot(t *testing.T) {
	root := t.TempDir()
	storagePath := writeOpenCodeProviderStorageSession(
		t, root, "session", "ses_storage", "storage-app", "Storage Session",
	)
	dbPath, seeder, db := newTestDBAt(t, filepath.Join(root, "opencode.db"))
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	seeder.AddProject("prj_1", "/home/user/code/sqlite-app")
	seeder.AddSession(
		"ses_sqlite", "prj_1", "", "SQLite", 1700000000000, 1700000010000,
	)
	virtualPath := OpenCodeSQLiteVirtualPath(dbPath, "ses_sqlite")
	lateStoragePath := filepath.Join(
		root, "storage", "session", "global", "ses_sqlite.json",
	)
	storageRoot := filepath.Join(root, "storage", "session")
	lateAdds := 0
	ctx := withStreamingDirectoryReader(t.Context(), func(
		ctx context.Context, dir string, yield func(os.DirEntry) error,
	) error {
		if err := streamDirectoryEntriesDirect(ctx, dir, yield); err != nil {
			return err
		}
		if !samePath(dir, storageRoot) {
			return nil
		}
		lateAdds++
		return os.WriteFile(lateStoragePath, []byte("{}"), 0o600)
	})
	provider, ok := NewProvider(
		AgentOpenCode, ProviderConfig{Roots: []string{root}},
	)
	require.True(t, ok)
	var paths []string

	err := provider.(StreamingDiscoverer).DiscoverEach(
		ctx, func(source SourceRef) error {
			paths = append(paths, source.DisplayPath)
			return nil
		},
	)

	require.NoError(t, err)
	assert.Equal(t, 1, lateAdds,
		"the late storage file must be added after the storage snapshot completes")
	assert.Equal(t, []string{storagePath, virtualPath}, paths,
		"deduplication must use the same storage snapshot that was yielded")
}

func TestOpenCodeHybridStreamingRetainedHeapDoesNotScaleWithStorageArchive(
	t *testing.T,
) {
	measureRetainedHeap := func(storageSessions int) uint64 {
		t.Helper()
		root := t.TempDir()
		sessionDir := filepath.Join(root, "storage", "session", "global")
		require.NoError(t, os.MkdirAll(sessionDir, 0o755))
		for i := range storageSessions {
			name := fmt.Sprintf(
				"ses_%05d_%s.json", i, strings.Repeat("x", 160),
			)
			require.NoError(t, os.WriteFile(
				filepath.Join(sessionDir, name), []byte("{}"), 0o600,
			))
		}
		dbPath, seeder, db := newTestDBAt(t, filepath.Join(root, "opencode.db"))
		seeder.AddProject("prj_1", "/home/user/code/sqlite-app")
		seeder.AddSession(
			"ses_sqlite", "prj_1", "", "SQLite", 1700000000000, 1700000010000,
		)
		provider, ok := NewProvider(
			AgentOpenCode, ProviderConfig{Roots: []string{root}},
		)
		require.True(t, ok)
		virtualPath := OpenCodeSQLiteVirtualPath(dbPath, "ses_sqlite")

		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		var retained uint64
		measured := false
		err := provider.(StreamingDiscoverer).DiscoverEach(
			t.Context(), func(source SourceRef) error {
				if source.DisplayPath != virtualPath {
					return nil
				}
				measured = true
				runtime.GC()
				var during runtime.MemStats
				runtime.ReadMemStats(&during)
				if during.HeapAlloc > before.HeapAlloc {
					retained = during.HeapAlloc - before.HeapAlloc
				}
				return nil
			},
		)
		require.NoError(t, err)
		require.True(t, measured,
			"retained heap must be sampled at the SQLite virtual source")
		require.NoError(t, db.Close())
		return retained
	}

	const retainedGrowthLimit = 256 * 1024
	smallRetained := measureRetainedHeap(16)
	largeRetained := measureRetainedHeap(4096)
	assert.LessOrEqual(t, largeRetained, smallRetained+retainedGrowthLimit,
		"hybrid deduplication must not retain storage IDs on the Go heap")
}

func TestOpenCodeHybridStreamingDiskMembershipFailuresPropagate(t *testing.T) {
	tests := []struct {
		name   string
		inject func(context.Context, error) context.Context
	}{
		{name: "query", inject: WithDiscoveryDiskMapQueryError},
		{name: "cleanup", inject: WithDiscoveryDiskMapCleanupError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeOpenCodeProviderStorageSession(
				t, root, "session", "ses_storage", "storage-app", "Storage Session",
			)
			_, seeder, db := newTestDBAt(t, filepath.Join(root, "opencode.db"))
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			seeder.AddProject("prj_1", "/home/user/code/sqlite-app")
			seeder.AddSession(
				"ses_sqlite", "prj_1", "", "SQLite", 1700000000000, 1700000010000,
			)
			provider, ok := NewProvider(
				AgentOpenCode, ProviderConfig{Roots: []string{root}},
			)
			require.True(t, ok)
			injected := errors.New("injected disk membership " + tt.name)

			err := provider.(StreamingDiscoverer).DiscoverEach(
				tt.inject(t.Context(), injected), func(SourceRef) error { return nil },
			)

			require.ErrorIs(t, err, injected)
		})
	}
}

func TestOpenCodeHybridStreamingDiscoveryPropagatesSQLiteCallbackError(
	t *testing.T,
) {
	root := t.TempDir()
	storagePath := writeOpenCodeProviderStorageSession(
		t, root, "session", "ses_storage", "storage-app", "Storage Session",
	)
	dbPath, seeder, db := newTestDBAt(t, filepath.Join(root, "opencode.db"))
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	seeder.AddProject("prj_1", "/home/user/code/sqlite-app")
	seeder.AddSession(
		"ses_sqlite", "prj_1", "", "SQLite", 1700000000000, 1700000010000,
	)
	virtualPath := OpenCodeSQLiteVirtualPath(dbPath, "ses_sqlite")
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sentinel := errors.New("stop after SQLite source")
	var yielded []string

	err := provider.(StreamingDiscoverer).DiscoverEach(
		t.Context(), func(source SourceRef) error {
			yielded = append(yielded, source.DisplayPath)
			if source.DisplayPath == virtualPath {
				return sentinel
			}
			return nil
		},
	)

	require.ErrorIs(t, err, sentinel)
	assert.Equal(t, []string{storagePath, virtualPath}, yielded)
}

func TestOpenCodeProviderDiscoveryToleratesCorruptSQLiteDB(t *testing.T) {

	root := t.TempDir()
	storagePath := writeOpenCodeProviderStorageSession(
		t, root, "session", "ses_valid", "storage-app", "Valid Session",
	)
	// A present-but-corrupt optional DB must not abort discovery of the valid
	// storage-backed session that lives in the same root.
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "opencode.db"), []byte("not a sqlite database"), 0o644,
	))

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, storagePath, discovered[0].DisplayPath)
}

func TestOpenCodeFamilyProviderRelabelsForks(t *testing.T) {

	for _, tc := range []struct {
		agent         AgentType
		sessionSubdir string
		prefix        string
		project       string
	}{
		{agent: AgentKilo, sessionSubdir: "session", prefix: "kilo:", project: "kilo-app"},
		{agent: AgentMiMoCode, sessionSubdir: "session_diff", prefix: "mimocode:", project: "mimo-app"},
	} {
		t.Run(string(tc.agent), func(t *testing.T) {

			root := t.TempDir()
			sessionPath := writeOpenCodeProviderStorageSession(
				t, root, tc.sessionSubdir, "ses_provider", tc.project, "Provider Session",
			)
			provider, ok := NewProvider(tc.agent, ProviderConfig{
				Roots:   []string{root},
				Machine: "devbox",
			})
			require.True(t, ok)
			source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
				FullSessionID: "host~" + tc.prefix + "ses_provider",
			})
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, sessionPath, source.DisplayPath)

			outcome, err := provider.Parse(context.Background(), ParseRequest{
				Source: source,
			})
			require.NoError(t, err)
			require.True(t, outcome.ResultSetComplete)
			require.Len(t, outcome.Results, 1)
			result := outcome.Results[0].Result
			assert.Equal(t, tc.prefix+"ses_provider", result.Session.ID)
			assert.Equal(t, tc.agent, result.Session.Agent)
			assert.Equal(t, strings.ReplaceAll(tc.project, "-", "_"), result.Session.Project)

			require.NoError(t, os.Remove(sessionPath), "remove storage session")
			removed, err := provider.SourcesForChangedPath(
				context.Background(),
				ChangedPathRequest{
					Path:      sessionPath,
					EventKind: "rename",
					WatchRoot: filepath.Join(root, "storage"),
				},
			)
			require.NoError(t, err)
			require.Len(t, removed, 1)
			assert.Equal(t, sessionPath, removed[0].DisplayPath)
		})
	}
}

func TestOpenCodeFamilyProviderFallsBackToProjectMetadata(t *testing.T) {
	for _, tc := range []struct {
		agent         AgentType
		sessionSubdir string
		prefix        string
	}{
		{agent: AgentKilo, sessionSubdir: "session", prefix: "kilo:"},
		{agent: AgentMiMoCode, sessionSubdir: "session_diff", prefix: "mimocode:"},
		{agent: AgentIcodemate, sessionSubdir: "session_diff", prefix: "icodemate:"},
	} {
		t.Run(string(tc.agent), func(t *testing.T) {
			root := t.TempDir()
			const projectID = "global"
			const sessionID = "ses-project-fallback"
			sessionPath := writeOpenCodeProviderStorageSession(
				t, root, tc.sessionSubdir, sessionID, "fork-app", "Fallback",
			)
			writeOpenCodeStorageFile(t, sessionPath, map[string]any{
				"id": sessionID, "projectID": projectID, "title": "Fallback",
				"time": map[string]any{
					"created": int64(1700000000000),
					"updated": int64(1700000060000),
				},
			})
			writeOpenCodeStorageFile(t, filepath.Join(
				root, "storage", "project", projectID+".json",
			), map[string]any{
				"id": projectID, "worktree": "/home/user/code/fork-app",
			})

			provider, ok := NewProvider(tc.agent, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			discovered, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, discovered, 1)
			source := discovered[0]
			assert.Equal(t, "fork_app", source.ProjectHint)
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			result := outcome.Results[0].Result
			assert.Equal(t, tc.agent, result.Session.Agent)
			assert.Equal(t, tc.prefix+sessionID, result.Session.ID)
			assert.Equal(t, "/home/user/code/fork-app", result.Session.Cwd)
			assert.Equal(t, "fork_app", result.Session.Project)
		})
	}
}

func writeOpenCodeProviderStorageSession(
	t *testing.T,
	root, sessionSubdir, sessionID, project, title string,
) string {
	t.Helper()
	sessionPath := filepath.Join(
		root, "storage", sessionSubdir, "global", sessionID+".json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        sessionID,
		"directory": filepath.Join("/home/user/code", project),
		"title":     title,
		"time": map[string]any{
			"created": int64(1700000000000),
			"updated": int64(1700000060000),
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", sessionID, "msg_1.json",
	), map[string]any{
		"id":        "msg_1",
		"sessionID": sessionID,
		"role":      "user",
		"time": map[string]any{
			"created": int64(1700000000000),
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_1", "prt_1.json",
	), map[string]any{
		"id":        "prt_1",
		"sessionID": sessionID,
		"messageID": "msg_1",
		"type":      "text",
		"text":      "Hello from storage",
		"time": map[string]any{
			"created": int64(1700000000000),
		},
	})
	return sessionPath
}

func newTestDBAt(
	t *testing.T,
	dbPath string,
) (string, *OpenCodeSeeder, *sql.DB) {
	t.Helper()
	copyOpenCodeSchemaTemplate(t, dbPath)
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "open test db")
	// Close before TempDir cleanup: Windows cannot delete a database file
	// that still has an open handle. Close is idempotent, so tests that
	// close the writer themselves are unaffected.
	t.Cleanup(func() { _ = db.Close() })
	return dbPath, &OpenCodeSeeder{db: db, t: t}, db
}

// TestOpenCodeSingleSessionMtimeDoesNotScanContainer pins the query shape of
// the single-session composite lookup. Reusing the streaming form's grouped
// subqueries here materializes an aggregate over every message and part in the
// container before the outer WHERE narrows to one session, so each per-session
// lookup would scan the whole archive. Assert the plan touches the child tables
// through their session_id indexes rather than a full scan.
func TestOpenCodeSingleSessionMtimeDoesNotScanContainer(t *testing.T) {
	root := t.TempDir()
	_, seeder, db := newTestDBAt(t, filepath.Join(root, "opencode.db"))
	seeder.AddProject("prj_1", "/home/user/code/app")
	seeder.AddSession(
		"ses_a", "prj_1", "", "A", 1700000000000, 1700000010000,
	)
	t.Cleanup(func() { _ = db.Close() })

	query := "SELECT " + openCodeSessionCompositeMtimeExpr +
		" FROM session s" + openCodeSessionCompositeMtimeJoins +
		" WHERE s.id = ?"
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, "ses_a")
	require.NoError(t, err)
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notUsed, &detail))
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	require.NoError(t, rows.Err())

	got := plan.String()
	for _, table := range []string{"message", "part"} {
		assert.NotContains(t, got, "SCAN "+table,
			"single-session composite mtime must not full-scan %s; plan:\n%s",
			table, got)
		// SEARCH alone is not proof of a seek: SQLite reports SEARCH for some
		// aggregate plans without an index, so require the index explicitly.
		assert.Regexp(t,
			`(?s)(SEARCH|SCAN) `+table+`[^\n]*USING (COVERING )?INDEX`,
			got,
			"single-session composite mtime must reach %s through an index; "+
				"plan:\n%s", table, got)
	}
}

// TestOpenCodeWatermarkOnlyQuerySkipsDigestScans pins that the mtime-only path
// does not compute the digest aggregates. OpenCodeSourceMtime backs the session
// watcher's 1.5s poll, so pulling the eight child COUNT/SUM/MIN/MAX subqueries
// in there would burn child-range scans per tick for a discarded value.
func TestOpenCodeWatermarkOnlyQuerySkipsDigestScans(t *testing.T) {
	watermarkOnly := "SELECT " + openCodeSessionCompositeMtimeExpr +
		" FROM session s" + openCodeSessionCompositeMtimeJoins +
		" WHERE s.id = ?"
	full := "SELECT " + openCodeSessionCompositeMtimeExpr + ", " +
		openCodeSessionCompositeCountsExpr +
		" FROM session s" + openCodeSessionCompositeMtimeJoins +
		" WHERE s.id = ?"

	assert.NotContains(t, watermarkOnly, "COUNT(",
		"the mtime-only query must not compute child counts")
	assert.NotContains(t, watermarkOnly, "group_concat(",
		"the mtime-only query must not build child identities")
	assert.Contains(t, full, "COUNT(",
		"the fingerprint query must still compute the digest aggregates")

	// Both must be executable, not merely string-shaped: assert against a real
	// container so a query that only looks right still fails here.
	root := t.TempDir()
	_, seeder, db := newTestDBAt(t, filepath.Join(root, "opencode.db"))
	seeder.AddProject("prj_1", "/home/user/code/app")
	seeder.AddSession(
		"ses_a", "prj_1", "", "A", 1700000000000, 1700000010000,
	)
	t.Cleanup(func() { _ = db.Close() })

	var watermark int64
	require.NoError(t,
		db.QueryRow(watermarkOnly, "ses_a").Scan(&watermark),
		"the mtime-only query must execute")
	assert.Equal(t, int64(1700000010000), watermark)

	var (
		w, st, pt, mn, pn int64
		mIdent, pIdent    string
	)
	require.NoError(t,
		db.QueryRow(full, "ses_a").Scan(
			&w, &st, &pt, &mn, &pn, &mIdent, &pIdent,
		),
		"the fingerprint query must execute")
	assert.Equal(t, watermark, w,
		"both queries must agree on the watermark")
}

// TestOpenCodeChangedPathWatermarkMergeEmitsOnlyUncovered pins the bounded
// changed-path listing: with a stored-freshness pager supplied, the provider
// merges the streamed watermark listing against paged stored authority and
// emits only members whose watermark advanced. The pager here returns one
// row per page, so the assertion that every member still resolves correctly
// also proves the merge consumes the stored side incrementally instead of
// materializing the container's membership.
func TestOpenCodeChangedPathWatermarkMergeEmitsOnlyUncovered(t *testing.T) {
	dbPath, seeder, _ := newTestDB(t)
	seeder.AddProject("proj", "/home/user/app")
	const base = int64(1779012000000)
	seeder.AddSession("ses-a", "proj", "", "a", base, base)
	seeder.AddSession("ses-b", "proj", "", "b", base, base+1000)
	seeder.AddSession("ses-c", "proj", "", "c", base, base)

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
		Roots: []string{filepath.Dir(dbPath)}, Machine: "local",
	})
	require.True(t, ok)

	// Stored authority covers ses-a fully and ses-b only through an older
	// watermark; ses-c has no stored row and must be kept.
	stored := []StoredMemberFreshness{
		{Path: dbPath + "#ses-a", CoveredThroughNS: base * 1_000_000},
		{Path: dbPath + "#ses-b", CoveredThroughNS: base * 1_000_000},
	}
	pagerCalls := 0
	pager := func(
		_ context.Context, after string, limit int,
	) ([]StoredMemberFreshness, bool, error) {
		pagerCalls++
		require.Positive(t, limit, "pages must be bounded")
		for _, row := range stored {
			if row.Path > after {
				return []StoredMemberFreshness{row}, false, nil
			}
		}
		return nil, true, nil
	}

	sources, err := provider.SourcesForChangedPath(
		t.Context(), ChangedPathRequest{
			Path: dbPath, WatchRoot: filepath.Dir(dbPath),
			AllowWatermarkOnlySources: true,
			StoredMemberFreshnessPage: pager,
		},
	)
	require.NoError(t, err)
	var paths []string
	for _, source := range sources {
		paths = append(paths, source.DisplayPath)
	}
	assert.ElementsMatch(t,
		[]string{dbPath + "#ses-b", dbPath + "#ses-c"}, paths,
		"only the advanced member and the unknown member are emitted")
	assert.GreaterOrEqual(t, pagerCalls, 2,
		"the stored side is consumed page by page")
}

// TestOpenCodeChangedPathWatermarkMergeFailsOpenOnPagerError pins the
// fail-open contract: a stored-side failure keeps every remaining source so
// the caller's per-file gates decide, matching the pre-merge behavior of a
// failed freshness query.
func TestOpenCodeChangedPathWatermarkMergeFailsOpenOnPagerError(t *testing.T) {
	dbPath, seeder, _ := newTestDB(t)
	seeder.AddProject("proj", "/home/user/app")
	const base = int64(1779012000000)
	seeder.AddSession("ses-a", "proj", "", "a", base, base)
	seeder.AddSession("ses-b", "proj", "", "b", base, base)

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
		Roots: []string{filepath.Dir(dbPath)}, Machine: "local",
	})
	require.True(t, ok)

	pager := func(
		context.Context, string, int,
	) ([]StoredMemberFreshness, bool, error) {
		return nil, false, errors.New("stored freshness unavailable")
	}
	sources, err := provider.SourcesForChangedPath(
		t.Context(), ChangedPathRequest{
			Path: dbPath, WatchRoot: filepath.Dir(dbPath),
			AllowWatermarkOnlySources: true,
			StoredMemberFreshnessPage: pager,
		},
	)
	require.NoError(t, err)
	assert.Len(t, sources, 2,
		"a pager failure must keep every source for the per-file gates")
}

// TestOpenCodeChangedPathWatermarkMergeMaterializesOnlyChangedBatch is the
// cardinality-scaling regression for the watcher fast path: a container with
// many stored-covered sessions and one changed session must emit exactly the
// changed batch. Before the merge, the listing materialized every session as
// a SourceRef and the caller loaded every stored member into a map, so each
// database event allocated O(total sessions); the emitted-length assertion
// here fails against any regression to that shape, and the small/large
// comparison pins that per-event output does not scale with the archive.
func TestOpenCodeChangedPathWatermarkMergeMaterializesOnlyChangedBatch(
	t *testing.T,
) {
	emittedForContainerOf := func(sessions int) int {
		dbPath, seeder, _ := newTestDB(t)
		seeder.AddProject("proj", "/home/user/app")
		const base = int64(1779012000000)
		var stored []StoredMemberFreshness
		seeder.InTransaction(func(seeder *OpenCodeSeeder) {
			for i := range sessions {
				id := fmt.Sprintf("ses-%06d", i)
				seeder.AddSession(id, "proj", "", id, base, base)
				stored = append(stored, StoredMemberFreshness{
					Path: dbPath + "#" + id, CoveredThroughNS: base * 1_000_000,
				})
			}
		})
		// One session advances past its stored coverage.
		changed := fmt.Sprintf("ses-%06d", sessions/2)
		_, err := seeder.db.ExecContext(t.Context(),
			"UPDATE session SET time_updated = ? WHERE id = ?",
			base+1000, changed,
		)
		require.NoError(t, err, "advance changed session")

		provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
			Roots: []string{filepath.Dir(dbPath)}, Machine: "local",
		})
		require.True(t, ok)
		pager := func(
			_ context.Context, after string, limit int,
		) ([]StoredMemberFreshness, bool, error) {
			start := 0
			for start < len(stored) && stored[start].Path <= after {
				start++
			}
			end := min(start+limit, len(stored))
			return stored[start:end], end == len(stored), nil
		}
		sources, err := provider.SourcesForChangedPath(
			t.Context(), ChangedPathRequest{
				Path: dbPath, WatchRoot: filepath.Dir(dbPath),
				AllowWatermarkOnlySources: true,
				StoredMemberFreshnessPage: pager,
			},
		)
		require.NoError(t, err)
		return len(sources)
	}

	small := emittedForContainerOf(8)
	large := emittedForContainerOf(1200)
	assert.Equal(t, 1, small)
	assert.Equal(t, small, large,
		"the emitted batch must not scale with container size")
}
