package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHermesProviderTranscriptSourceMethods(t *testing.T) {

	root := t.TempDir()
	jsonlPath := filepath.Join(root, "child.jsonl")
	jsonPath := filepath.Join(root, "session_jsononly.json")
	writeSourceFile(t, jsonlPath, hermesProviderJSONLFixture("jsonl question"))
	writeSourceFile(t, jsonPath, hermesProviderJSONFixture("json question"))
	writeSourceFile(t, filepath.Join(root, "scratch.json"), "{}\n")

	provider, ok := NewProvider(AgentHermes, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"state.db", "state.db-wal", "*.jsonl", "session_*.json"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.ElementsMatch(t, []string{jsonlPath, jsonPath}, []string{
		discovered[0].DisplayPath,
		discovered[1].DisplayPath,
	})

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "remote~hermes:child",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, jsonlPath, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "jsononly",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, jsonPath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, jsonPath, fingerprint.Key)
	assert.Positive(t, fingerprint.Size)
	assert.Positive(t, fingerprint.MTimeNS)
	assert.NotEmpty(t, fingerprint.Hash)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: jsonlPath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, jsonlPath, changed[0].DisplayPath)

	require.NoError(t, os.Remove(jsonlPath))
	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: jsonlPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, jsonlPath, changed[0].DisplayPath)

	ignored, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "scratch.json"),
			EventKind: "write",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	assert.Empty(t, ignored)
}

func TestHermesProviderStateDBSourceMethods(t *testing.T) {

	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	createHermesStateDB(t, root)
	transcriptPath := filepath.Join(sessionsDir, "session_child.json")
	writeSourceFile(t, transcriptPath, hermesProviderJSONFixture("transcript question"))
	stateDB := filepath.Join(root, "state.db")

	provider, ok := NewProvider(AgentHermes, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 2)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.False(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"state.db", "state.db-wal"}, plan.Roots[0].IncludeGlobs)
	assert.Equal(t, sessionsDir, plan.Roots[1].Path)
	assert.True(t, plan.Roots[1].Recursive)
	assert.Equal(t, []string{"*.jsonl", "session_*.json"}, plan.Roots[1].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, stateDB, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "remote~hermes:child",
	})
	require.NoError(t, err)
	require.True(t, ok)
	memberPath := VirtualSourcePath(stateDB, "child")
	assert.Equal(t, memberPath, found.DisplayPath)

	stateInfo, err := os.Stat(stateDB)
	require.NoError(t, err)
	transcriptInfo, err := os.Stat(transcriptPath)
	require.NoError(t, err)
	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, memberPath, fingerprint.Key)
	assert.Equal(t, stateInfo.Size()+transcriptInfo.Size(), fingerprint.Size)
	assert.Equal(t,
		max(stateInfo.ModTime().UnixNano(), transcriptInfo.ModTime().UnixNano()),
		fingerprint.MTimeNS,
	)
	assert.NotEmpty(t, fingerprint.Hash)

	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "state db", path: stateDB},
		{name: "archive transcript", path: transcriptPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, err := provider.SourcesForChangedPath(
				context.Background(),
				ChangedPathRequest{Path: tc.path, EventKind: "write", WatchRoot: root},
			)
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, stateDB, changed[0].DisplayPath)
		})
	}

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: stateDB, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, stateDB, changed[0].DisplayPath)

	require.NoError(t, os.Remove(transcriptPath))
	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: transcriptPath, EventKind: "remove", WatchRoot: sessionsDir},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, stateDB, changed[0].DisplayPath)

	require.NoError(t, os.Remove(stateDB))
	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: stateDB, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, stateDB, changed[0].DisplayPath)
}

func TestHermesStreamingDiscoveryYieldsFallbackAndReportsUnreadableStateDB(
	t *testing.T,
) {
	tests := []struct {
		name    string
		setupDB func(*testing.T, string)
	}{
		{
			name: "malformed database",
			setupDB: func(t *testing.T, path string) {
				t.Helper()
				writeSourceFile(t, path, "not a sqlite database")
			},
		},
		{
			name: "incompatible schema",
			setupDB: func(t *testing.T, path string) {
				t.Helper()
				conn, err := sql.Open("sqlite3", path)
				require.NoError(t, err)
				_, err = conn.Exec("CREATE TABLE unrelated (id TEXT PRIMARY KEY)")
				require.NoError(t, err)
				require.NoError(t, conn.Close())
			},
		},
		{
			name: "first row scan failure",
			setupDB: func(t *testing.T, path string) {
				t.Helper()
				conn, err := sql.Open("sqlite3", path)
				require.NoError(t, err)
				_, err = conn.Exec(`
					CREATE TABLE sessions (id TEXT);
					INSERT INTO sessions (id) VALUES (NULL);
				`)
				require.NoError(t, err)
				require.NoError(t, conn.Close())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			sessionsDir := filepath.Join(root, "sessions")
			require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
			tt.setupDB(t, filepath.Join(root, "state.db"))
			jsonlPath := filepath.Join(sessionsDir, "orphan.jsonl")
			writeSourceFile(t, jsonlPath, hermesProviderJSONLFixture("question"))
			writeSourceFile(t, filepath.Join(sessionsDir, "session_orphan.json"),
				hermesProviderJSONFixture("duplicate"))
			jsonPath := filepath.Join(sessionsDir, "session_jsononly.json")
			writeSourceFile(t, jsonPath, hermesProviderJSONFixture("json question"))

			provider, ok := NewProvider(AgentHermes, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
				RawSessionID: "orphan",
			})
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, jsonlPath, found.DisplayPath,
				"FindSource establishes transcript fallback parity")

			discoverer, ok := provider.(StreamingDiscoverer)
			require.True(t, ok)
			var paths []string
			err = discoverer.DiscoverEach(t.Context(), func(source SourceRef) error {
				paths = append(paths, source.DisplayPath)
				return nil
			})

			require.Error(t, err,
				"transcript fallback must not make the state scope authoritative")
			assert.ElementsMatch(t, []string{jsonlPath, jsonPath}, paths)
			assert.Len(t, paths, 2,
				"JSONL and legacy JSON copies of one session must yield once")
		})
	}
}

func TestHermesStreamingDiscoveryPreservesStateYieldError(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	createHermesStateDB(t, root)
	writeSourceFile(t, filepath.Join(sessionsDir, "orphan.jsonl"),
		hermesProviderJSONLFixture("orphan question"))
	provider, ok := NewProvider(AgentHermes, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discoverer, ok := provider.(StreamingDiscoverer)
	require.True(t, ok)
	wantErr := errors.New("stop streaming")
	calls := 0

	err := discoverer.DiscoverEach(t.Context(), func(SourceRef) error {
		calls++
		return wantErr
	})

	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, 1, calls,
		"a state callback failure must not restart transcript discovery")
}

func TestHermesStreamingFallbackErrorPrecedence(t *testing.T) {
	newDiscoverer := func(t *testing.T) StreamingDiscoverer {
		t.Helper()
		root := t.TempDir()
		sessionsDir := filepath.Join(root, "sessions")
		require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
		writeSourceFile(t, filepath.Join(root, "state.db"), "not a sqlite database")
		writeSourceFile(t, filepath.Join(sessionsDir, "orphan.jsonl"),
			hermesProviderJSONLFixture("orphan question"))
		writeSourceFile(t, filepath.Join(sessionsDir, "session_orphan.json"),
			hermesProviderJSONFixture("duplicate legacy question"))
		provider, ok := NewProvider(AgentHermes, ProviderConfig{Roots: []string{root}})
		require.True(t, ok)
		discoverer, ok := provider.(StreamingDiscoverer)
		require.True(t, ok)
		return discoverer
	}

	t.Run("yield error", func(t *testing.T) {
		discoverer := newDiscoverer(t)
		wantErr := errors.New("stop transcript fallback")
		calls := 0

		err := discoverer.DiscoverEach(t.Context(), func(SourceRef) error {
			calls++
			return wantErr
		})

		assert.Same(t, wantErr, err,
			"callback failure must take precedence over state incompleteness")
		assert.Equal(t, 1, calls,
			"fallback deduplication must not invoke the callback for legacy JSON")
	})

	t.Run("context cancellation", func(t *testing.T) {
		discoverer := newDiscoverer(t)
		ctx, cancel := context.WithCancel(t.Context())
		ctx = withStreamingDirectoryReader(ctx, func(
			ctx context.Context, _ string, _ func(os.DirEntry) error,
		) error {
			cancel()
			return ctx.Err()
		})

		err := discoverer.DiscoverEach(ctx, func(SourceRef) error { return nil })

		assert.Equal(t, context.Canceled, err,
			"cancellation must take precedence over state incompleteness")
	})

	t.Run("transcript traversal error", func(t *testing.T) {
		discoverer := newDiscoverer(t)
		fallbackErr := errors.New("read transcript directory")
		ctx := withStreamingDirectoryReader(t.Context(), func(
			context.Context, string, func(os.DirEntry) error,
		) error {
			return fallbackErr
		})

		err := discoverer.DiscoverEach(ctx, func(SourceRef) error { return nil })

		require.ErrorIs(t, err, fallbackErr)
		assert.Contains(t, err.Error(), "query hermes sessions",
			"joined error must retain the original state failure")
	})
}

func TestHermesStreamingFallbackContinuesAcrossRoots(t *testing.T) {
	malformedRoot := t.TempDir()
	malformedSessions := filepath.Join(malformedRoot, "sessions")
	require.NoError(t, os.MkdirAll(malformedSessions, 0o755))
	writeSourceFile(t, filepath.Join(malformedRoot, "state.db"), "not a sqlite database")
	malformedTranscript := filepath.Join(malformedSessions, "first.jsonl")
	writeSourceFile(t, malformedTranscript, hermesProviderJSONLFixture("first fallback"))

	incompatibleRoot := t.TempDir()
	incompatibleSessions := filepath.Join(incompatibleRoot, "sessions")
	require.NoError(t, os.MkdirAll(incompatibleSessions, 0o755))
	conn, err := sql.Open("sqlite3", filepath.Join(incompatibleRoot, "state.db"))
	require.NoError(t, err)
	_, err = conn.Exec("CREATE TABLE unrelated (id TEXT PRIMARY KEY)")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	incompatibleTranscript := filepath.Join(incompatibleSessions, "second.jsonl")
	writeSourceFile(t, incompatibleTranscript, hermesProviderJSONLFixture("second fallback"))

	provider, ok := NewProvider(AgentHermes, ProviderConfig{
		Roots: []string{malformedRoot, incompatibleRoot},
	})
	require.True(t, ok)
	discoverer, ok := provider.(StreamingDiscoverer)
	require.True(t, ok)
	var paths []string

	err = discoverer.DiscoverEach(t.Context(), func(source SourceRef) error {
		paths = append(paths, source.DisplayPath)
		return nil
	})

	require.Error(t, err)
	assert.ElementsMatch(t, []string{malformedTranscript, incompatibleTranscript}, paths,
		"a failed root must not prevent safe fallback discovery for later roots")
	assert.Contains(t, err.Error(), "file is not a database")
	assert.Contains(t, err.Error(), "no such table: sessions")
}

func TestHermesStreamingTranscriptFailureContinuesLaterRoots(t *testing.T) {
	failedRoot := t.TempDir()
	healthyRoot := t.TempDir()
	healthyPath := filepath.Join(healthyRoot, "healthy.jsonl")
	writeSourceFile(t, healthyPath, hermesProviderJSONLFixture("healthy root"))
	discoveryErr := errors.New("read failed transcript root")
	ctx := withStreamingDirectoryReader(t.Context(), func(
		ctx context.Context, dir string, yield func(os.DirEntry) error,
	) error {
		if samePath(dir, failedRoot) {
			return discoveryErr
		}
		return streamDirectoryEntriesDirect(ctx, dir, yield)
	})
	provider, ok := NewProvider(AgentHermes, ProviderConfig{
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
	assert.Equal(t, AgentHermes, incomplete.Provider)
	assert.Equal(t, []string{healthyPath}, paths,
		"a root-local transcript failure must not starve later roots")
}

// TestHermesProfilesContainerEnumerationFailureIsIncompleteDiscovery guards
// the reconciliation tombstoning path: hermesProfileArchiveRoots used to
// convert every os.ReadDir failure into an empty profile list, so a
// transient permission or I/O failure on the profiles container made
// discovery look authoritatively empty while ReconciliationOwnershipScopes
// still claimed the whole container — and the engine tombstoned every
// stored hermes session under it as source_missing. Enumeration failures
// must surface as DiscoveryIncompleteError so the engine retains
// reconciliation markers and retries instead.
func TestHermesProfilesContainerEnumerationFailureIsIncompleteDiscovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory-permission read failures are not portable to Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	profilesRoot := filepath.Join(t.TempDir(), ".hermes", "profiles")
	profileRoot := filepath.Join(profilesRoot, "research")
	require.NoError(t, os.MkdirAll(profileRoot, 0o755))
	createHermesStateDB(t, profileRoot)

	healthyRoot := t.TempDir()
	healthyPath := filepath.Join(healthyRoot, "healthy.jsonl")
	writeSourceFile(t, healthyPath, hermesProviderJSONLFixture("healthy root"))

	provider, ok := NewProvider(AgentHermes, ProviderConfig{
		Roots: []string{profilesRoot, healthyRoot},
	})
	require.True(t, ok)

	require.NoError(t, os.Chmod(profilesRoot, 0o000))
	t.Cleanup(func() {
		require.NoError(t, os.Chmod(profilesRoot, 0o755))
	})

	var paths []string
	err := provider.(StreamingDiscoverer).DiscoverEach(
		t.Context(), func(source SourceRef) error {
			paths = append(paths, source.DisplayPath)
			return nil
		},
	)
	var incomplete DiscoveryIncompleteError
	require.ErrorAs(t, err, &incomplete,
		"an unreadable profiles container must make streamed discovery incomplete, not empty")
	assert.Equal(t, AgentHermes, incomplete.Provider)
	assert.Equal(t, []string{healthyPath}, paths,
		"a failed profiles container must not starve other configured roots")

	_, err = provider.Discover(t.Context())
	require.ErrorAs(t, err, &incomplete,
		"an unreadable profiles container must make batch discovery incomplete, not empty")

	resolver, ok := provider.(ReconciliationSourceResolver)
	require.True(t, ok)
	_, found, err := resolver.SourceForReconciliation(
		t.Context(), filepath.Join(profileRoot, "state.db"), "",
	)
	assert.False(t, found)
	require.ErrorAs(t, err, &incomplete,
		"not-found under a failed container expansion must not be authoritative")
}

func TestHermesStreamingArchiveTranscriptFailureContinuesLaterRoots(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state func(*testing.T, string)
	}{
		{
			name: "after state discovery",
			state: func(t *testing.T, root string) {
				createHermesStateDB(t, root)
			},
		},
		{
			name: "during state fallback",
			state: func(t *testing.T, root string) {
				writeSourceFile(t, filepath.Join(root, "state.db"), "not sqlite")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failedRoot := t.TempDir()
			failedSessions := filepath.Join(failedRoot, "sessions")
			require.NoError(t, os.MkdirAll(failedSessions, 0o755))
			tc.state(t, failedRoot)
			healthyRoot := t.TempDir()
			healthyPath := filepath.Join(healthyRoot, "healthy.jsonl")
			writeSourceFile(t, healthyPath, hermesProviderJSONLFixture("healthy root"))
			discoveryErr := errors.New("read failed archive transcripts")
			ctx := withStreamingDirectoryReader(t.Context(), func(
				ctx context.Context, dir string, yield func(os.DirEntry) error,
			) error {
				if samePath(dir, failedSessions) {
					return discoveryErr
				}
				return streamDirectoryEntriesDirect(ctx, dir, yield)
			})
			provider, ok := NewProvider(AgentHermes, ProviderConfig{
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
			assert.Contains(t, paths, healthyPath,
				"an archive transcript failure must not starve later roots")
		})
	}
}

func TestHermesSourceForReconciliationPreservesOrdinaryTranscript(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	createHermesStateDB(t, root)
	transcriptPath := filepath.Join(sessionsDir, "orphan.jsonl")
	writeSourceFile(t, transcriptPath, hermesProviderJSONLFixture("orphan question"))

	provider, ok := NewProvider(AgentHermes, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	resolver, ok := provider.(ReconciliationSourceResolver)
	require.True(t, ok)
	source, found, err := resolver.SourceForReconciliation(
		t.Context(), transcriptPath, "project",
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, transcriptPath, source.DisplayPath)
	assert.Equal(t, transcriptPath, source.FingerprintKey)
	assert.Equal(t, "project", source.ProjectHint)
}

func TestHermesStateMemberFingerprintIncludesSelectedTranscriptMetadata(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	createHermesStateDB(t, root)
	transcriptPath := filepath.Join(sessionsDir, "session_child.json")
	writeSourceFile(t, transcriptPath, hermesProviderJSONFixture("transcript question"))
	transcriptTime := time.Now().Add(2 * time.Second).Truncate(time.Second)
	require.NoError(t, os.Chtimes(transcriptPath, transcriptTime, transcriptTime))
	stateDB := filepath.Join(root, "state.db")

	provider, ok := NewProvider(AgentHermes, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "child",
	})
	require.NoError(t, err)
	require.True(t, found)

	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	stateInfo, err := os.Stat(stateDB)
	require.NoError(t, err)
	transcriptInfo, err := os.Stat(transcriptPath)
	require.NoError(t, err)
	assert.Equal(t, stateInfo.Size()+transcriptInfo.Size(), fingerprint.Size)
	assert.Equal(t, transcriptInfo.ModTime().UnixNano(), fingerprint.MTimeNS)
}

// TestHermesArchiveFingerprintIgnoresEmptyWAL pins fingerprint determinism
// across a parse: opening state.db read-only creates a zero-length -wal as a
// side effect, and if its mtime entered the fingerprint, the identity stored
// before a parse would never match the one computed after it, so every sync
// would re-parse an unchanged archive. A WAL with committed frames must still
// change the fingerprint.
func TestHermesArchiveFingerprintIgnoresEmptyWAL(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sessions"), 0o755))
	createHermesStateDB(t, root)
	stateDB := filepath.Join(root, "state.db")

	provider, ok := NewProvider(AgentHermes, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	require.Equal(t, stateDB, discovered[0].DisplayPath)

	before, err := provider.Fingerprint(context.Background(), discovered[0])
	require.NoError(t, err)

	walPath := stateDB + "-wal"
	require.NoError(t, os.WriteFile(walPath, nil, 0o644))
	walTime := time.Now().Add(2 * time.Second).Truncate(time.Second)
	require.NoError(t, os.Chtimes(walPath, walTime, walTime))

	after, err := provider.Fingerprint(context.Background(), discovered[0])
	require.NoError(t, err)
	assert.Equal(t, before.Size, after.Size,
		"a zero-length WAL must not change the archive size")
	assert.Equal(t, before.MTimeNS, after.MTimeNS,
		"a zero-length WAL's mtime must not change the archive freshness")
	assert.Equal(t, before.Hash, after.Hash,
		"a zero-length WAL must not change the archive hash")

	require.NoError(t, os.WriteFile(walPath, []byte("wal frames"), 0o644))
	committedTime := walTime.Add(2 * time.Second)
	require.NoError(t, os.Chtimes(walPath, committedTime, committedTime))

	committed, err := provider.Fingerprint(context.Background(), discovered[0])
	require.NoError(t, err)
	assert.Equal(t, before.Size+int64(len("wal frames")), committed.Size,
		"a WAL with frames must add its size to the archive fingerprint")
	assert.Equal(t, committedTime.UnixNano(), committed.MTimeNS,
		"a WAL with frames must advance the archive freshness")
	assert.NotEqual(t, before.Hash, committed.Hash,
		"a WAL with frames must change the archive hash")
}

func TestHermesStateMemberFingerprintIncludesStateMetadataWhenTranscriptWins(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	createHermesStateDB(t, root)
	transcriptPath := filepath.Join(sessionsDir, "session_child.json")
	writeSourceFile(t, transcriptPath, hermesProviderJSONFixture("transcript question"))
	stateDB := filepath.Join(root, "state.db")

	provider, ok := NewProvider(AgentHermes, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "child",
	})
	require.NoError(t, err)
	require.True(t, found)
	before, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	stateInfo, err := os.Stat(stateDB)
	require.NoError(t, err)

	conn, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	_, err = conn.Exec("UPDATE sessions SET title = ? WHERE id = ?", "Other Session", "child")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.NoError(t, os.Chtimes(stateDB, stateInfo.ModTime(), stateInfo.ModTime()))
	after, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	assert.NotEqual(t, before.Hash, after.Hash,
		"state metadata used by parsing must participate even when transcript messages win")
}

func TestHermesProviderArchiveWatchRoots(t *testing.T) {

	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	createHermesStateDB(t, root)
	stateDB := filepath.Join(root, "state.db")

	for _, tc := range []struct {
		name       string
		configRoot string
	}{
		{name: "archive parent", configRoot: root},
		{name: "sessions directory", configRoot: sessionsDir},
		{name: "state db file", configRoot: stateDB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, ok := NewProvider(AgentHermes, ProviderConfig{
				Roots:   []string{tc.configRoot},
				Machine: "devbox",
			})
			require.True(t, ok)

			plan, err := provider.WatchPlan(context.Background())
			require.NoError(t, err)
			require.Len(t, plan.Roots, 2)
			assert.Equal(t, root, plan.Roots[0].Path)
			assert.False(t, plan.Roots[0].Recursive)
			assert.Equal(t, []string{"state.db", "state.db-wal"}, plan.Roots[0].IncludeGlobs)
			assert.Equal(t, sessionsDir, plan.Roots[1].Path)
			assert.True(t, plan.Roots[1].Recursive)
			assert.Equal(t, []string{"*.jsonl", "session_*.json"}, plan.Roots[1].IncludeGlobs)

			changed, err := provider.SourcesForChangedPath(
				context.Background(),
				ChangedPathRequest{Path: stateDB, EventKind: "write", WatchRoot: root},
			)
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, stateDB, changed[0].DisplayPath)
		})
	}
}

func TestHermesProviderDiscoversProfileCreatedAfterInitialization(t *testing.T) {
	profilesRoot := filepath.Join(t.TempDir(), ".hermes", "profiles")
	require.NoError(t, os.MkdirAll(profilesRoot, 0o755))

	provider, ok := NewProvider(AgentHermes, ProviderConfig{
		Roots:   []string{profilesRoot},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, profilesRoot, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)

	before, err := provider.Discover(context.Background())
	require.NoError(t, err)
	assert.Empty(t, before)

	profileRoot := filepath.Join(profilesRoot, "research")
	require.NoError(t, os.MkdirAll(profileRoot, 0o755))
	createHermesStateDB(t, profileRoot)
	stateDB := filepath.Join(profileRoot, "state.db")

	after, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.Equal(t, stateDB, after[0].DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      stateDB,
			EventKind: "create",
			WatchRoot: profilesRoot,
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, stateDB, changed[0].DisplayPath)

	require.NoError(t, os.Remove(stateDB))
	removed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      stateDB,
			EventKind: "remove",
			WatchRoot: profilesRoot,
		},
	)
	require.NoError(t, err)
	require.Len(t, removed, 1)
	assert.Equal(t, stateDB, removed[0].DisplayPath)
}

func TestHermesProfileChangedPathAllocationsStayBounded(t *testing.T) {
	measure := func(t *testing.T, profileCount int) float64 {
		t.Helper()
		profilesRoot := filepath.Join(t.TempDir(), ".hermes", "profiles")
		for i := range profileCount {
			require.NoError(t, os.MkdirAll(filepath.Join(
				profilesRoot, fmt.Sprintf("archive-%04d", i),
			), 0o755))
		}
		targetRoot := filepath.Join(profilesRoot, "current")
		targetPath := filepath.Join(targetRoot, "sessions", "child.jsonl")
		writeSourceFile(t, targetPath, hermesProviderJSONLFixture("bounded"))
		provider, ok := NewProvider(AgentHermes, ProviderConfig{
			Roots: []string{profilesRoot},
		})
		require.True(t, ok)

		request := ChangedPathRequest{
			Path: targetPath, EventKind: "write", WatchRoot: profilesRoot,
		}
		warm, err := provider.SourcesForChangedPath(context.Background(), request)
		require.NoError(t, err)
		require.Len(t, warm, 1)
		assert.Equal(t, targetPath, warm[0].DisplayPath)

		return testing.AllocsPerRun(20, func() {
			sources, sourceErr := provider.SourcesForChangedPath(
				context.Background(), request,
			)
			if sourceErr != nil || len(sources) != 1 {
				panic("Hermes changed-path classification failed")
			}
		})
	}

	smallAllocs := measure(t, 10)
	largeAllocs := measure(t, 1000)
	assert.LessOrEqual(t, largeAllocs, smallAllocs*2,
		"Hermes profile events must not scan unrelated profiles")
}

func TestHermesMemberCoreSeedRetainedIDBytesStayBounded(t *testing.T) {
	measure := func(t *testing.T, sessionCount int) int64 {
		t.Helper()
		root := t.TempDir()
		createHermesStateDB(t, root)
		stateDB := filepath.Join(root, "state.db")
		conn, err := sql.Open("sqlite3", stateDB)
		require.NoError(t, err)
		_, err = conn.Exec("DELETE FROM messages; DELETE FROM sessions")
		require.NoError(t, err)
		for i := range sessionCount {
			id := fmt.Sprintf("member-%06d", i)
			_, err = conn.Exec(`INSERT INTO sessions
				(id, source, started_at, estimated_cost_usd, actual_cost_usd)
				VALUES (?, 'cli', ?, 0, 0)`, id, i)
			require.NoError(t, err)
			_, err = conn.Exec(`INSERT INTO messages
				(session_id, role, content, timestamp)
				VALUES (?, 'user', 'hello', ?)`, id, i)
			require.NoError(t, err)
		}
		require.NoError(t, conn.Close())

		var retained, peak int64
		ctx := WithStreamingRetainedBytesObserver(t.Context(), func(delta int64) {
			retained += delta
			peak = max(peak, retained)
		})
		ctx, cleanup, err := WithReconciliationCache(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, cleanup()) })

		require.NoError(t, seedHermesMemberCoresLocked(ctx, stateDB))
		assert.Zero(t, retained, "seeded IDs must be released before the pass returns")
		return peak
	}

	small := measure(t, 3)
	large := measure(t, 300)
	assert.Positive(t, small, "the retained-ID allocation boundary must be observed")
	assert.LessOrEqual(t, large, small*2,
		"peak retained ID bytes must not scale with Hermes archive cardinality")
}

func TestHermesProviderArchiveWatchRootsBeforeArchiveComplete(t *testing.T) {

	t.Run("state db exists before sessions directory", func(t *testing.T) {

		root := t.TempDir()
		createHermesStateDB(t, root)
		stateDB := filepath.Join(root, "state.db")
		sessionsDir := filepath.Join(root, "sessions")

		provider, ok := NewProvider(AgentHermes, ProviderConfig{
			Roots:   []string{root},
			Machine: "devbox",
		})
		require.True(t, ok)

		plan, err := provider.WatchPlan(context.Background())
		require.NoError(t, err)
		require.Len(t, plan.Roots, 2)
		assert.Equal(t, root, plan.Roots[0].Path)
		assert.False(t, plan.Roots[0].Recursive)
		assert.Equal(t, []string{"state.db", "state.db-wal"}, plan.Roots[0].IncludeGlobs)
		assert.Equal(t, sessionsDir, plan.Roots[1].Path)
		assert.True(t, plan.Roots[1].Recursive)
		assert.Equal(t, []string{"*.jsonl", "session_*.json"}, plan.Roots[1].IncludeGlobs)

		changed, err := provider.SourcesForChangedPath(
			context.Background(),
			ChangedPathRequest{Path: stateDB, EventKind: "write", WatchRoot: root},
		)
		require.NoError(t, err)
		require.Len(t, changed, 1)
		assert.Equal(t, stateDB, changed[0].DisplayPath)
	})

	t.Run("direct state db root before file exists", func(t *testing.T) {

		root := t.TempDir()
		stateDB := filepath.Join(root, "state.db")
		sessionsDir := filepath.Join(root, "sessions")

		provider, ok := NewProvider(AgentHermes, ProviderConfig{
			Roots:   []string{stateDB},
			Machine: "devbox",
		})
		require.True(t, ok)

		plan, err := provider.WatchPlan(context.Background())
		require.NoError(t, err)
		require.Len(t, plan.Roots, 2)
		assert.Equal(t, root, plan.Roots[0].Path)
		assert.False(t, plan.Roots[0].Recursive)
		assert.Equal(t, []string{"state.db", "state.db-wal"}, plan.Roots[0].IncludeGlobs)
		assert.Equal(t, sessionsDir, plan.Roots[1].Path)
		assert.True(t, plan.Roots[1].Recursive)
		assert.Equal(t, []string{"*.jsonl", "session_*.json"}, plan.Roots[1].IncludeGlobs)

		createHermesStateDB(t, root)
		changed, err := provider.SourcesForChangedPath(
			context.Background(),
			ChangedPathRequest{Path: stateDB, EventKind: "write", WatchRoot: root},
		)
		require.NoError(t, err)
		require.Len(t, changed, 1)
		assert.Equal(t, stateDB, changed[0].DisplayPath)
	})

	t.Run("sessions directory root before state db exists", func(t *testing.T) {

		root := t.TempDir()
		stateDB := filepath.Join(root, "state.db")
		sessionsDir := filepath.Join(root, "sessions")
		require.NoError(t, os.MkdirAll(sessionsDir, 0o755))

		provider, ok := NewProvider(AgentHermes, ProviderConfig{
			Roots:   []string{sessionsDir},
			Machine: "devbox",
		})
		require.True(t, ok)

		plan, err := provider.WatchPlan(context.Background())
		require.NoError(t, err)
		require.Len(t, plan.Roots, 2)
		assert.Equal(t, root, plan.Roots[0].Path)
		assert.False(t, plan.Roots[0].Recursive)
		assert.Equal(t, []string{"state.db", "state.db-wal"}, plan.Roots[0].IncludeGlobs)
		assert.Equal(t, sessionsDir, plan.Roots[1].Path)
		assert.True(t, plan.Roots[1].Recursive)
		assert.Equal(t, []string{"*.jsonl", "session_*.json"}, plan.Roots[1].IncludeGlobs)

		createHermesStateDB(t, root)
		changed, err := provider.SourcesForChangedPath(
			context.Background(),
			ChangedPathRequest{Path: stateDB, EventKind: "write", WatchRoot: root},
		)
		require.NoError(t, err)
		require.Len(t, changed, 1)
		assert.Equal(t, stateDB, changed[0].DisplayPath)
	})
}

func TestHermesProviderParse(t *testing.T) {

	root := t.TempDir()
	sourcePath := filepath.Join(root, "child.jsonl")
	writeSourceFile(t, sourcePath, hermesProviderJSONLFixture("parse question"))

	provider, ok := NewProvider(AgentHermes, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: SourceFingerprint{Key: sourcePath, Hash: "abc123"},
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.False(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "hermes:child", result.Result.Session.ID)
	assert.Equal(t, AgentHermes, result.Result.Session.Agent)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, sourcePath, result.Result.Session.File.Path)
	assert.Equal(t, "abc123", result.Result.Session.File.Hash)
	assert.Equal(t, "parse question", result.Result.Session.FirstMessage)
	assert.Len(t, result.Result.Messages, 2)
}

func TestHermesProviderParseStateDB(t *testing.T) {

	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	createHermesStateDB(t, root)
	transcriptPath := filepath.Join(sessionsDir, "session_child.json")
	writeSourceFile(
		t,
		transcriptPath,
		hermesProviderJSONFixture("archive transcript"),
	)
	stateDB := filepath.Join(root, "state.db")

	provider, ok := NewProvider(AgentHermes, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: SourceFingerprint{Key: stateDB, Hash: "archive-hash"},
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.True(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "hermes:child", result.Result.Session.ID)
	assert.Equal(t, "hermes:parent", result.Result.Session.ParentSessionID)
	assert.Equal(t, RelContinuation, result.Result.Session.RelationshipType)
	assert.Equal(t, "Child Session", result.Result.Session.SessionName)
	assert.Equal(t, "hermes-state-db", result.Result.Session.SourceVersion)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	require.Len(t, result.Result.UsageEvents, 1)
	assert.Len(t, result.Result.Messages, 2)

	// The provider reproduces the legacy engine's stampHermesArchiveResults:
	// every archive session's stored file identity is the state.db path with
	// the aggregate (state.db plus transcripts) size and mtime, so a
	// transcript-only change still refreshes the archive's freshness.
	stateInfo, err := os.Stat(stateDB)
	require.NoError(t, err)
	transcriptInfo, err := os.Stat(transcriptPath)
	require.NoError(t, err)
	assert.Equal(t, stateDB, result.Result.Session.File.Path)
	assert.Equal(
		t,
		stateInfo.Size()+transcriptInfo.Size(),
		result.Result.Session.File.Size,
	)
	assert.Equal(
		t,
		max(stateInfo.ModTime().UnixNano(), transcriptInfo.ModTime().UnixNano()),
		result.Result.Session.File.Mtime,
	)
}

func TestHermesProviderFindSourceDoesNotReturnStateDBForMissingRawID(t *testing.T) {

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sessions"), 0o755))
	createHermesStateDB(t, root)

	provider, ok := NewProvider(AgentHermes, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "missing-valid-id",
	})

	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, source)
}

func TestHermesProviderFindSourceFallsBackToTranscriptWhenStateDBUnreadable(t *testing.T) {

	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))

	// A present-but-unreadable state.db: hermesStateDBHasSession opens it
	// lazily, then errors on the first query because the bytes are not a
	// SQLite database. parseArchive logs and falls back to transcripts in this
	// case, so FindSource must do the same rather than aborting the lookup.
	stateDB := filepath.Join(root, "state.db")
	writeSourceFile(t, stateDB, "not a sqlite database")

	transcriptPath := filepath.Join(sessionsDir, "freshchild.jsonl")
	writeSourceFile(t, transcriptPath, hermesProviderJSONLFixture("transcript question"))

	provider, ok := NewProvider(AgentHermes, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "freshchild",
	})

	require.NoError(t, err, "unreadable state.db must not abort transcript lookup")
	require.True(t, ok, "valid transcript next to a bad state.db must be found")
	assert.Equal(t, transcriptPath, source.DisplayPath)
}

func hermesProviderJSONLFixture(firstMessage string) string {
	return `{"role":"session_meta","platform":"cli","timestamp":"2026-05-14T10:00:00.000000"}` + "\n" +
		`{"role":"user","content":"` + firstMessage + `","timestamp":"2026-05-14T10:01:00.000000"}` + "\n" +
		`{"role":"assistant","content":"Done.","timestamp":"2026-05-14T10:02:00.000000"}` + "\n"
}

func hermesProviderJSONFixture(firstMessage string) string {
	return `{
		"platform":"cli",
		"session_start":"2026-05-14T10:00:00Z",
		"last_updated":"2026-05-14T10:02:00Z",
		"messages":[
			{"role":"user","content":"` + firstMessage + `","timestamp":"2026-05-14T10:01:00Z"},
			{"role":"assistant","content":"Done.","timestamp":"2026-05-14T10:02:00Z"}
		]
	}`
}
