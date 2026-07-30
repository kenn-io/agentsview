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
		[]string{partialRoot, healthyRoot}, spec,
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

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 2)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.False(t, plan.Roots[0].Recursive)
	assert.Equal(t, "opencode-sqlite:"+root, plan.Roots[0].CoverageKey)
	assert.Equal(t, filepath.Join(root, "storage"), plan.Roots[1].Path)
	assert.True(t, plan.Roots[1].Recursive)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	source := discovered[0]
	assert.Equal(t, AgentOpenCode, source.Provider)
	assert.Equal(t, sessionPath, source.DisplayPath)
	assert.Equal(t, sessionPath, source.FingerprintKey)
	assert.Equal(t, "opencode_app", source.ProjectHint)

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

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, sessionPath, fingerprint.Key)
	assert.Positive(t, fingerprint.Size)
	assert.Positive(t, fingerprint.MTimeNS)
	// Storage-mode Fingerprint is stat-only: the content fingerprint is
	// computed by Parse, and hashing here would re-read every message and
	// part file on each fingerprint call.
	assert.Empty(t, fingerprint.Hash)

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
		"Parse must compute the storage content fingerprint itself")
	assert.Len(t, result.Result.Messages, 1)

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

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 2)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.False(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{
		"opencode.db", "opencode.db-wal",
	}, plan.Roots[0].IncludeGlobs)
	assert.Equal(t, "opencode-sqlite:"+root, plan.Roots[0].CoverageKey,
		"prospective coverage re-probes compatibility during dispatch")
	assert.Equal(t, filepath.Join(root, "storage"), plan.Roots[1].Path)
	assert.True(t, plan.Roots[1].Recursive)

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

func TestOpenCodeProviderWatchPlanCachesJournalCompatibility(t *testing.T) {
	dbPath, _ := newOpenCodeEventDB(t)
	root := filepath.Dir(dbPath)
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	for range 2 {
		plan, err := provider.WatchPlan(t.Context())
		require.NoError(t, err)
		require.Len(t, plan.Roots, 2)
		assert.Equal(t, "opencode-sqlite:"+root, plan.Roots[0].CoverageKey)
		assert.Equal(t, filepath.Join(root, "storage"), plan.Roots[1].Path)
		assert.True(t, plan.Roots[1].Recursive)
	}
}

func TestOpenCodeProviderProspectiveCoverageActivatesAfterDatabaseCreation(t *testing.T) {
	root := t.TempDir()
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	feed := provider.(BoundedCoverageProvider)
	task := CoverageTask{
		Agent: AgentOpenCode, Root: root,
		CoverageKey: "opencode-sqlite:" + root,
		Trigger:     CoverageTriggerDegraded,
	}
	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Equal(t, task.CoverageKey, plan.Roots[0].CoverageKey)
	missing, err := feed.PollCoverage(t.Context(), task)
	require.ErrorIs(t, err, ErrProviderCoverageUnavailable)
	assert.Nil(t, missing.Checkpoint)

	dbPath := filepath.Join(root, "opencode.db")
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Close() })
	_, err = writer.Exec(`
	CREATE TABLE project (id TEXT PRIMARY KEY, worktree TEXT NOT NULL);
	CREATE TABLE session (
		id TEXT PRIMARY KEY, project_id TEXT NOT NULL, parent_id TEXT,
		title TEXT, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL
	);
	CREATE TABLE event (
		id TEXT PRIMARY KEY,
		aggregate_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		type TEXT NOT NULL,
		data TEXT NOT NULL
	);
	INSERT INTO project(id, worktree) VALUES('prj_new', '/tmp/project');
	INSERT INTO session(id, project_id, title, time_created, time_updated)
	VALUES('ses_new', 'prj_new', 'New', 1, 2);
	`)
	require.NoError(t, err)
	task.Trigger = CoverageTriggerBaseline
	baseline, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	assert.False(t, baseline.AuditRequired)
	require.NoError(t, feed.CommitCoverage(t.Context(), task, baseline, false))
	task.Trigger = CoverageTriggerDegraded
	_, err = writer.Exec(
		"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES('evt_pending', 'ses_new', 1, 'message.part.updated.1', ?)",
		partUpdatedPayload("ses_new"),
	)
	require.NoError(t, err)
	changed, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	checkpoint := changed.Checkpoint.(OpenCodeCoverageState)
	assert.Equal(t, int64(1), checkpoint.LastRowID)
	assert.False(t, changed.AuditRequired)
}

func TestOpenCodeProviderDoesNotCacheTransientJournalProbe(t *testing.T) {
	dbPath, _ := newOpenCodeEventDB(t)
	root := filepath.Dir(dbPath)
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	feed := provider.(BoundedCoverageProvider)
	task := CoverageTask{
		Agent: AgentOpenCode, Root: root,
		CoverageKey: "opencode-sqlite:" + root,
		Trigger:     CoverageTriggerDegraded,
	}
	originalProbe := probeOpenCodeCoverageJournalFn
	t.Cleanup(func() { probeOpenCodeCoverageJournalFn = originalProbe })
	calls := 0
	probeOpenCodeCoverageJournalFn = func(
		context.Context, string,
	) openCodeCoverageJournalSupport {
		calls++
		if calls == 1 {
			return openCodeCoverageJournalUnknown
		}
		return openCodeCoverageJournalSupported
	}

	unknown, err := feed.PollCoverage(t.Context(), task)
	require.ErrorIs(t, err, ErrProviderCoverageUnavailable)
	assert.Nil(t, unknown.Checkpoint)
	task.Trigger = CoverageTriggerBaseline
	supported, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	assert.False(t, supported.AuditRequired)
	assert.Equal(t, 2, calls)
}

func TestOpenCodeProviderCoverageCheckpointSlicesAreIsolated(t *testing.T) {
	dbPath, writer := newOpenCodeEventDB(t)
	root := filepath.Dir(dbPath)
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	feed := provider.(BoundedCoverageProvider)
	task := CoverageTask{
		Agent: AgentOpenCode, Root: root,
		CoverageKey: "opencode-sqlite:" + root,
		Trigger:     CoverageTriggerBaseline,
	}
	baseline, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	require.NoError(t, feed.CommitCoverage(t.Context(), task, baseline, false))
	impl := provider.(*openCodeFormatProvider)
	impl.coverage[task.CoverageKey] = cloneOpenCodeCoverageState(
		baseline.Checkpoint.(OpenCodeCoverageState),
	)
	impl.coverage[task.CoverageKey] = func() OpenCodeCoverageState {
		state := impl.coverage[task.CoverageKey]
		state.PendingIDs = []string{"ses_changed"}
		return state
	}()
	_, err = writer.Exec(`
		CREATE TABLE project (id TEXT PRIMARY KEY, worktree TEXT NOT NULL);
		CREATE TABLE session (
			id TEXT PRIMARY KEY, project_id TEXT NOT NULL, parent_id TEXT,
			title TEXT, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL
		);
		INSERT INTO project(id, worktree) VALUES('prj_changed', '/tmp/project');
		INSERT INTO session(id, project_id, title, time_created, time_updated)
		VALUES('ses_changed', 'prj_changed', 'Changed', 1, 2);
	`)
	require.NoError(t, err)
	_, err = writer.Exec(
		"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES('evt_changed', 'ses_changed', 1, 'session.updated.1', ?)",
		sessionUpdatedPayload("ses_changed"),
	)
	require.NoError(t, err)
	task.Trigger = CoverageTriggerDegraded
	task.AuthoritativeFallback = true
	result, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	assert.Equal(t, []string{"ses_changed"}, impl.coverage[task.CoverageKey].PendingIDs)
	require.NoError(t, feed.CommitCoverage(t.Context(), task, result, false))
	checkpoint := result.Checkpoint.(OpenCodeCoverageState)
	checkpoint.ReadyIDs = append(checkpoint.ReadyIDs, "mutated")
	assert.NotContains(t, impl.coverage[task.CoverageKey].ReadyIDs, "mutated")
}

func TestOpenCodeProviderFirstNativeChangeRequestsAudit(t *testing.T) {
	dbPath, _ := newOpenCodeEventDB(t)
	root := filepath.Dir(dbPath)
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	result, err := provider.(BoundedCoverageProvider).PollCoverage(
		t.Context(), CoverageTask{
			Agent: AgentOpenCode, Root: root,
			CoverageKey: "opencode-sqlite:" + root,
			Trigger:     CoverageTriggerNative, ChangedPaths: []string{dbPath},
		},
	)
	require.NoError(t, err)
	assert.True(t, result.AuditRequired)
}

func TestOpenCodeProviderNativeCoverageIgnoresNonDataSidecars(t *testing.T) {
	for _, tc := range []struct {
		name   string
		suffix string
		size   int
	}{
		{name: "missing WAL", suffix: "-wal"},
		{name: "header-only WAL", suffix: "-wal", size: int(sqliteWALHeaderSize)},
		{name: "SHM", suffix: "-shm", size: 32 * 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "opencode.db") + tc.suffix
			if tc.size > 0 {
				require.NoError(t, os.WriteFile(path, make([]byte, tc.size), 0o600))
			}
			provider, ok := NewProvider(
				AgentOpenCode, ProviderConfig{Roots: []string{root}},
			)
			require.True(t, ok)

			result, err := provider.(BoundedCoverageProvider).PollCoverage(
				t.Context(), CoverageTask{
					Agent: AgentOpenCode, Root: root,
					CoverageKey: "opencode-sqlite:" + root,
					Trigger:     CoverageTriggerNative, ChangedPaths: []string{path},
				},
			)
			require.NoError(t, err)
			assert.False(t, result.AuditRequired)
			assert.Empty(t, result.Sources)
			assert.Empty(t, result.Removed)
			checkpoint := result.Checkpoint.(OpenCodeCoverageState)
			assert.Equal(t, uint64(1), checkpoint.Generation)
		})
	}
}

func TestOpenCodeProviderNativeCoverageProcessesFramedWAL(t *testing.T) {
	dbPath, writer := newOpenCodeEventDB(t)
	root := filepath.Dir(dbPath)
	var journalMode string
	require.NoError(t, writer.QueryRow("PRAGMA journal_mode=WAL").Scan(&journalMode))
	require.Equal(t, "wal", journalMode)
	_, err := writer.Exec("PRAGMA wal_autocheckpoint=0")
	require.NoError(t, err)
	_, err = writer.Exec(
		"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES('evt_wal', 'ses_wal', 1, 'session.updated.1', ?)",
		sessionUpdatedPayload("ses_wal"),
	)
	require.NoError(t, err)
	walPath := dbPath + "-wal"
	walInfo, err := os.Stat(walPath)
	require.NoError(t, err)
	require.Greater(t, walInfo.Size(), sqliteWALHeaderSize)
	provider, ok := NewProvider(
		AgentOpenCode, ProviderConfig{Roots: []string{root}},
	)
	require.True(t, ok)

	result, err := provider.(BoundedCoverageProvider).PollCoverage(
		t.Context(), CoverageTask{
			Agent: AgentOpenCode, Root: root,
			CoverageKey: "opencode-sqlite:" + root,
			Trigger:     CoverageTriggerNative, ChangedPaths: []string{walPath},
		},
	)
	require.NoError(t, err)
	assert.True(t, result.AuditRequired)
}

func TestOpenCodeProviderUnprimedDegradedPollRequestsAudit(t *testing.T) {
	dbPath, _ := newOpenCodeEventDB(t)
	root := filepath.Dir(dbPath)
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	result, err := provider.(BoundedCoverageProvider).PollCoverage(
		t.Context(), CoverageTask{
			Agent: AgentOpenCode, Root: root,
			CoverageKey: "opencode-sqlite:" + root,
			Trigger:     CoverageTriggerDegraded,
		},
	)
	require.NoError(t, err)
	assert.True(t, result.AuditRequired)
}

func TestOpenCodeProviderRevalidatesJournalAfterContainerReplacement(t *testing.T) {
	dbPath, writer := newOpenCodeEventDB(t)
	root := filepath.Dir(dbPath)
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Equal(t, "opencode-sqlite:"+root, plan.Roots[0].CoverageKey)
	require.NoError(t, writer.Close())

	replacement := dbPath + ".replacement"
	replacementDB, err := sql.Open("sqlite3", replacement)
	require.NoError(t, err)
	_, err = replacementDB.Exec(
		"CREATE TABLE event(id TEXT PRIMARY KEY); CREATE TABLE padding(value BLOB); INSERT INTO padding VALUES(zeroblob(8192))",
	)
	require.NoError(t, err)
	require.NoError(t, replacementDB.Close())
	require.NoError(t, os.Remove(dbPath))
	require.NoError(t, os.Rename(replacement, dbPath))

	plan, err = provider.WatchPlan(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "opencode-sqlite:"+root, plan.Roots[0].CoverageKey)
	_, err = provider.(BoundedCoverageProvider).PollCoverage(
		t.Context(), CoverageTask{
			Agent: AgentOpenCode, Root: root,
			CoverageKey: "opencode-sqlite:" + root,
			Trigger:     CoverageTriggerDegraded,
		},
	)
	require.ErrorIs(t, err, ErrUnsupportedProviderFeature)
}

func TestOpenCodeProviderCoverageResolvesOneSettledSession(t *testing.T) {
	dbPath, seeder, writer := newTestDB(t)
	defer writer.Close()
	seeder.AddProject("prj_coverage", t.TempDir())
	seeder.AddSession(
		"ses_coverage", "prj_coverage", "", "Coverage",
		1700000000000, 1700000010000,
	)
	_, err := writer.Exec(`CREATE TABLE event (
		id TEXT PRIMARY KEY,
		aggregate_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		type TEXT NOT NULL,
		data TEXT NOT NULL
	)`)
	require.NoError(t, err)
	root := filepath.Dir(dbPath)
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	feed := provider.(BoundedCoverageProvider)
	task := CoverageTask{
		Agent: AgentOpenCode, Root: root,
		CoverageKey: "opencode-sqlite:" + root,
		Trigger:     CoverageTriggerBaseline,
	}
	baseline, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	assert.Empty(t, baseline.Sources)
	assert.False(t, baseline.AuditRequired)
	require.NoError(t, feed.CommitCoverage(t.Context(), task, baseline, false))
	task.Trigger = CoverageTriggerDegraded
	task.AuthoritativeFallback = true
	_, err = writer.Exec(
		"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES('evt_0001', 'ses_coverage', 1, 'session.updated.1', ?)",
		`{"sessionID":"ses_coverage","info":{"id":"ses_coverage","projectID":"prj_coverage"}}`,
	)
	require.NoError(t, err)

	result, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	require.Len(t, result.Sources, 1)
	assert.Equal(t, OpenCodeSQLiteVirtualPath(dbPath, "ses_coverage"),
		result.Sources[0].DisplayPath)
	require.NoError(t, feed.CommitCoverage(t.Context(), task, result, false))

	storagePath := writeOpenCodeProviderStorageSession(
		t, root, "session", "ses_coverage", "prj_coverage", "Coverage",
	)
	unexpectedProjectDir := filepath.Join(
		root, "storage", "session", "unexpected-project",
	)
	require.NoError(t, os.MkdirAll(unexpectedProjectDir, 0o755))
	unexpectedStoragePath := filepath.Join(
		unexpectedProjectDir, "ses_coverage.json",
	)
	require.NoError(t, os.Rename(storagePath, unexpectedStoragePath))
	storagePath = unexpectedStoragePath
	_, err = provider.Discover(t.Context())
	require.NoError(t, err)
	_, err = writer.Exec(
		"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES('evt_0002', 'ses_coverage', 2, 'session.updated.1', ?)",
		`{"sessionID":"ses_coverage","info":{"id":"ses_coverage","projectID":"prj_coverage"}}`,
	)
	require.NoError(t, err)
	transitioned, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	require.Len(t, transitioned.Sources, 1)
	assert.Equal(t, storagePath, transitioned.Sources[0].DisplayPath)
	require.NoError(t, feed.CommitCoverage(t.Context(), task, transitioned, false))

	_, err = writer.Exec(
		"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES('evt_0003', 'ses_coverage', 3, 'future.event.1', '{}')",
	)
	require.NoError(t, err)
	audit, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	assert.True(t, audit.AuditRequired)
	retriedAudit, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	assert.True(t, retriedAudit.AuditRequired)
	_, err = writer.Exec(
		"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES('evt_0004', 'ses_coverage', 4, 'session.updated.1', ?)",
		`{"sessionID":"ses_coverage","info":{"id":"ses_coverage","projectID":"prj_coverage"}}`,
	)
	require.NoError(t, err)
	require.NoError(t, feed.CommitCoverage(t.Context(), task, audit, true))
	recovered, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	require.Len(t, recovered.Sources, 1)
}

func TestOpenCodeProviderCoverageDefersMissingStorageShadowScope(t *testing.T) {
	root := t.TempDir()
	_, seeder, writer := newTestDBAt(
		t, filepath.Join(root, "opencode.db"),
	)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	seeder.AddProject("proj", t.TempDir())
	seeder.AddSession(
		"ses_deferred", "proj", "", "Deferred",
		1700000000000, 1700000010000,
	)
	_, err := writer.Exec(`CREATE TABLE event (
		id TEXT PRIMARY KEY,
		aggregate_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		type TEXT NOT NULL,
		data TEXT NOT NULL
	)`)
	require.NoError(t, err)
	writeOpenCodeProviderStorageSession(
		t, root, "session", "ses_deferred", "storage", "Deferred",
	)
	provider, ok := NewProvider(
		AgentOpenCode, ProviderConfig{Roots: []string{root}},
	)
	require.True(t, ok)
	feed := provider.(BoundedCoverageProvider)
	task := CoverageTask{
		Agent: AgentOpenCode, Root: root,
		CoverageKey: "opencode-sqlite:" + root,
		Trigger:     CoverageTriggerBaseline,
	}
	baseline, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	require.NoError(t, feed.CommitCoverage(t.Context(), task, baseline, false))
	require.NoError(t, os.RemoveAll(filepath.Join(root, "storage")))
	_, err = writer.Exec(
		"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES('evt_deferred', 'ses_deferred', 1, 'session.updated.1', ?)",
		sessionUpdatedPayload("ses_deferred"),
	)
	require.NoError(t, err)
	task.Trigger = CoverageTriggerDegraded
	task.AuthoritativeFallback = false

	_, err = feed.PollCoverage(t.Context(), task)
	require.ErrorIs(t, err, ErrProviderCoverageUnavailable)
	storagePath := writeOpenCodeProviderStorageSession(
		t, root, "session", "ses_deferred", "storage", "Deferred",
	)
	task.AuthoritativeFallback = true
	retried, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	require.Len(t, retried.Sources, 1)
	assert.Equal(t, storagePath, retried.Sources[0].DisplayPath)
	assert.Empty(t, retried.Removed)
}

func TestOpenCodeProviderCoverageContinuationRequiresStorageBeforeCheckpoint(t *testing.T) {
	root := t.TempDir()
	_, seeder, writer := newTestDBAt(t, filepath.Join(root, "opencode.db"))
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	seeder.AddProject("proj", t.TempDir())
	seeder.AddSession("ses_cont", "proj", "", "Continuation", 1700000000000, 1700000010000)
	_, err := writer.Exec(`CREATE TABLE event (id TEXT PRIMARY KEY, aggregate_id TEXT NOT NULL, seq INTEGER NOT NULL, type TEXT NOT NULL, data TEXT NOT NULL)`)
	require.NoError(t, err)
	writeOpenCodeProviderStorageSession(t, root, "session", "ses_cont", "storage", "Continuation")
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	feed := provider.(BoundedCoverageProvider)
	task := CoverageTask{Agent: AgentOpenCode, Root: root, CoverageKey: "opencode-sqlite:" + root, Trigger: CoverageTriggerBaseline}
	baseline, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	require.NoError(t, feed.CommitCoverage(t.Context(), task, baseline, false))
	for i := 0; i < OpenCodeCoverageMaxRows+1; i++ {
		_, err = writer.Exec("INSERT INTO event(id, aggregate_id, seq, type, data) VALUES(?, 'ses_cont', ?, 'session.updated.1', ?)", fmt.Sprintf("evt_cont_%03d", i), i+1, sessionUpdatedPayload("ses_cont"))
		require.NoError(t, err)
	}
	require.NoError(t, os.RemoveAll(filepath.Join(root, "storage")))
	task.Trigger = CoverageTriggerDegraded
	_, err = feed.PollCoverage(t.Context(), task)
	require.ErrorIs(t, err, ErrProviderCoverageUnavailable)
	writeOpenCodeProviderStorageSession(t, root, "session", "ses_cont", "storage", "Continuation")
	retried, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	assert.True(t, retried.More || len(retried.Sources) > 0)
}

func TestOpenCodeProviderCoverageDefersStorageDisappearanceDuringIndex(t *testing.T) {
	root := t.TempDir()
	_, seeder, writer := newTestDBAt(t, filepath.Join(root, "opencode.db"))
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	seeder.AddProject("proj", t.TempDir())
	seeder.AddSession("ses_race", "proj", "", "Race", 1700000000000, 1700000010000)
	_, err := writer.Exec(`CREATE TABLE event (id TEXT PRIMARY KEY, aggregate_id TEXT NOT NULL, seq INTEGER NOT NULL, type TEXT NOT NULL, data TEXT NOT NULL)`)
	require.NoError(t, err)
	writeOpenCodeProviderStorageSession(t, root, "session", "ses_race", "storage", "Race")
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	feed := provider.(BoundedCoverageProvider)
	task := CoverageTask{Agent: AgentOpenCode, Root: root, CoverageKey: "opencode-sqlite:" + root, Trigger: CoverageTriggerBaseline}
	baseline, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	require.NoError(t, feed.CommitCoverage(t.Context(), task, baseline, false))
	_, err = writer.Exec("INSERT INTO event(id, aggregate_id, seq, type, data) VALUES('evt_race', 'ses_race', 1, 'session.updated.1', ?)", sessionUpdatedPayload("ses_race"))
	require.NoError(t, err)
	task.Trigger = CoverageTriggerDegraded
	storageRoot := filepath.Join(root, "storage")
	sessionRoot := filepath.Join(storageRoot, "session")
	ctx := withStreamingDirectoryReader(t.Context(), func(ctx context.Context, dir string, yield func(os.DirEntry) error) error {
		if samePath(dir, sessionRoot) {
			require.NoError(t, os.RemoveAll(storageRoot))
		}
		return streamDirectoryEntriesDirect(ctx, dir, yield)
	})

	_, err = feed.PollCoverage(ctx, task)
	require.ErrorIs(t, err, ErrProviderCoverageUnavailable)
	storagePath := writeOpenCodeProviderStorageSession(t, root, "session", "ses_race", "storage", "Race")
	retried, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	require.Len(t, retried.Sources, 1)
	assert.Equal(t, storagePath, retried.Sources[0].DisplayPath)
}

func TestOpenCodeProviderCoverageRejectsDatabasePathComponents(t *testing.T) {
	root := t.TempDir()
	dbPath, seeder, writer := newTestDBAt(
		t, filepath.Join(root, "opencode.db"),
	)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	seeder.AddProject("..", t.TempDir())
	seeder.AddSession(
		"ses_safe", "..", "", "Coverage",
		1700000000000, 1700000010000,
	)
	_, err := writer.Exec(`CREATE TABLE event (
		id TEXT PRIMARY KEY,
		aggregate_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		type TEXT NOT NULL,
		data TEXT NOT NULL
	)`)
	require.NoError(t, err)
	escapedPath := filepath.Join(root, "storage", "ses_safe.json")
	writeOpenCodeStorageFile(t, escapedPath, map[string]any{
		"id": "ses_safe", "directory": "/home/user/code/escaped",
	})
	provider, ok := NewProvider(
		AgentOpenCode, ProviderConfig{Roots: []string{root}},
	)
	require.True(t, ok)
	_, err = provider.Discover(t.Context())
	require.NoError(t, err)
	feed := provider.(BoundedCoverageProvider)
	task := CoverageTask{
		Agent: AgentOpenCode, Root: root,
		CoverageKey: "opencode-sqlite:" + root,
		Trigger:     CoverageTriggerBaseline,
	}
	baseline, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	require.NoError(t, feed.CommitCoverage(t.Context(), task, baseline, false))
	_, err = writer.Exec(
		"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES('evt_safe', 'ses_safe', 1, 'session.updated.1', ?)",
		`{"sessionID":"ses_safe","info":{"id":"ses_safe","projectID":".."}}`,
	)
	require.NoError(t, err)
	task.Trigger = CoverageTriggerDegraded
	task.AuthoritativeFallback = true

	result, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	require.Len(t, result.Sources, 1)
	assert.Equal(t,
		OpenCodeSQLiteVirtualPath(dbPath, "ses_safe"),
		result.Sources[0].DisplayPath,
	)
}

func TestOpenCodeProviderCoverageRejectsEscapedStorageSymlink(t *testing.T) {
	root := t.TempDir()
	dbPath, seeder, writer := newTestDBAt(
		t, filepath.Join(root, "opencode.db"),
	)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	seeder.AddProject("linked", t.TempDir())
	seeder.AddSession(
		"ses_safe", "linked", "", "Coverage",
		1700000000000, 1700000010000,
	)
	_, err := writer.Exec(`CREATE TABLE event (
		id TEXT PRIMARY KEY,
		aggregate_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		type TEXT NOT NULL,
		data TEXT NOT NULL
	)`)
	require.NoError(t, err)
	outside := t.TempDir()
	writeOpenCodeStorageFile(t,
		filepath.Join(outside, "ses_safe.json"),
		map[string]any{"id": "ses_safe", "directory": "/outside"},
	)
	sessionRoot := filepath.Join(root, "storage", "session")
	require.NoError(t, os.MkdirAll(sessionRoot, 0o755))
	if err := os.Symlink(outside, filepath.Join(sessionRoot, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	provider, ok := NewProvider(
		AgentOpenCode, ProviderConfig{Roots: []string{root}},
	)
	require.True(t, ok)
	_, err = provider.Discover(t.Context())
	require.NoError(t, err)
	feed := provider.(BoundedCoverageProvider)
	task := CoverageTask{
		Agent: AgentOpenCode, Root: root,
		CoverageKey: "opencode-sqlite:" + root,
		Trigger:     CoverageTriggerBaseline,
	}
	baseline, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	require.NoError(t, feed.CommitCoverage(t.Context(), task, baseline, false))
	_, err = writer.Exec(
		"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES('evt_safe', 'ses_safe', 1, 'session.updated.1', ?)",
		`{"sessionID":"ses_safe","info":{"id":"ses_safe","projectID":"linked"}}`,
	)
	require.NoError(t, err)
	task.Trigger = CoverageTriggerDegraded

	result, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	require.Len(t, result.Sources, 1)
	assert.Equal(t,
		OpenCodeSQLiteVirtualPath(dbPath, "ses_safe"),
		result.Sources[0].DisplayPath,
	)
}

func TestOpenCodeCoverageStorageIndexHasFixedProbeBound(t *testing.T) {
	root := t.TempDir()
	sessionRoot := filepath.Join(root, "storage", "session")
	require.NoError(t, os.MkdirAll(sessionRoot, 0o755))
	for i := 0; i <= openCodeCoverageMaxStorageProbes/2; i++ {
		require.NoError(t, os.Mkdir(
			filepath.Join(sessionRoot, fmt.Sprintf("project-%04d", i)),
			0o755,
		))
	}
	sources := newOpenCodeFormatSourceSet(
		[]string{root}, openCodeProviderSpecForAgent(AgentOpenCode),
	)

	index, complete, err := sources.coverageStorageIndex(
		t.Context(), root, []string{"missing-session"},
	)
	require.NoError(t, err)
	assert.False(t, complete)
	assert.Empty(t, index)
}

func TestOpenCodeProviderCoverageAcknowledgesReplacementAudit(t *testing.T) {
	dbPath, writer := newOpenCodeEventDB(t)
	insertOpenCodeEvents(t, writer, 1, 1)
	root := filepath.Dir(dbPath)
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	feed := provider.(BoundedCoverageProvider)
	task := CoverageTask{
		Agent: AgentOpenCode, Root: root,
		CoverageKey: "opencode-sqlite:" + root,
		Trigger:     CoverageTriggerBaseline,
	}
	baseline, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	require.False(t, baseline.AuditRequired)
	require.NoError(t, feed.CommitCoverage(t.Context(), task, baseline, false))
	task.Trigger = CoverageTriggerDegraded
	require.NoError(t, writer.Close())
	replaceOpenCodeEventDBPreservingEndpoint(t, dbPath, "ses_replaced")

	replacement, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	require.True(t, replacement.AuditRequired)
	require.NoError(t, feed.CommitCoverage(t.Context(), task, replacement, true))

	idle, err := feed.PollCoverage(t.Context(), task)
	require.NoError(t, err)
	assert.False(t, idle.AuditRequired)
	assert.Empty(t, idle.Sources)
}

func TestOpenCodeProviderCoverageRejectsStorageScope(t *testing.T) {
	root := t.TempDir()
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	feed := provider.(BoundedCoverageProvider)

	_, err := feed.PollCoverage(t.Context(), CoverageTask{
		Agent: AgentOpenCode, Root: root, Trigger: CoverageTriggerDegraded,
	})
	require.ErrorIs(t, err, ErrUnsupportedProviderFeature)
}

func TestOpenCodeProviderCoverageNormalizesTaskRoot(t *testing.T) {
	dbPath, _ := newOpenCodeEventDB(t)
	root := filepath.Dir(dbPath)
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	feed := provider.(BoundedCoverageProvider)

	result, err := feed.PollCoverage(t.Context(), CoverageTask{
		Agent: AgentOpenCode, Root: root + string(filepath.Separator),
		CoverageKey: "opencode-sqlite:" + root,
		Trigger:     CoverageTriggerBaseline,
	})
	require.NoError(t, err)
	assert.False(t, result.AuditRequired)
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
		"fingerprint mtime must be the discovered time_updated in ns")
	assert.Equal(t, int64(len(garbage)), fp.Size,
		"fingerprint size stays the shared container file size")
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
	return dbPath, &OpenCodeSeeder{db: db, t: t}, db
}
