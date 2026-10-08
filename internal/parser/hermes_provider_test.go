package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHermesProviderTranscriptSourceMethods(t *testing.T) {
	root := t.TempDir()
	jsonlPath := filepath.Join(root, "child.jsonl")
	const dottedID = "cron_job.1_20261007_120000"
	jsonPath := filepath.Join(root, "session_"+dottedID+".json")
	writeSourceFile(t, jsonlPath, hermesProviderJSONLFixture("jsonl question"))
	writeSourceFile(t, jsonPath, hermesProviderJSONFixture("json question"))
	writeSourceFile(t, filepath.Join(root, "scratch.json"), "{}\n")

	provider, ok := NewProvider(AgentHermes, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"state.db", "state.db-wal", "*.jsonl", "session_*.json"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.ElementsMatch(t, []string{jsonlPath, jsonPath}, []string{
		discovered[0].DisplayPath,
		discovered[1].DisplayPath,
	})

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "remote~hermes:child",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, jsonlPath, found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: dottedID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, jsonPath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), found)
	require.NoError(t, err)
	assert.Equal(t, jsonPath, fingerprint.Key)
	assert.Positive(t, fingerprint.Size)
	assert.Positive(t, fingerprint.MTimeNS)
	assert.NotEmpty(t, fingerprint.Hash)
	for _, path := range []string{jsonPath, jsonlPath} {
		for _, body := range []string{"", "!", `{"source":"cron","parent_session_id":"cron_job-a_20261007_120000"`} {
			writeSourceFile(t, path, body)
			fingerprint, err := provider.Fingerprint(t.Context(), SourceRef{Opaque: hermesSource{Path: path}})
			require.NoError(t, err)
			plainHash, err := hashJSONLSourceFile(path)
			require.NoError(t, err)
			assert.Equal(t, plainHash, fingerprint.Hash)
		}
	}

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: jsonlPath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, jsonlPath, changed[0].DisplayPath)

	require.NoError(t, os.Remove(jsonlPath))
	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: jsonlPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, jsonlPath, changed[0].DisplayPath)

	ignored, err := provider.SourcesForChangedPath(
		t.Context(),
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

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 2)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.False(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"state.db", "state.db-wal"}, plan.Roots[0].IncludeGlobs)
	assert.Equal(t, sessionsDir, plan.Roots[1].Path)
	assert.True(t, plan.Roots[1].Recursive)
	assert.Equal(t, []string{"*.jsonl", "session_*.json"}, plan.Roots[1].IncludeGlobs)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, stateDB, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
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
	fingerprint, err := provider.Fingerprint(t.Context(), found)
	require.NoError(t, err)
	assert.Equal(t, memberPath, fingerprint.Key)
	assert.Equal(t, stateInfo.Size()+transcriptInfo.Size(), fingerprint.Size)
	assert.Equal(t, max(stateInfo.ModTime().UnixNano(), transcriptInfo.ModTime().UnixNano()),
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
				t.Context(),
				ChangedPathRequest{Path: tc.path, EventKind: "write", WatchRoot: root},
			)
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, stateDB, changed[0].DisplayPath)
		})
	}

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: stateDB, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, stateDB, changed[0].DisplayPath)

	require.NoError(t, os.Remove(transcriptPath))
	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: transcriptPath, EventKind: "remove", WatchRoot: sessionsDir},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, stateDB, changed[0].DisplayPath)

	require.NoError(t, os.Remove(stateDB))
	changed, err = provider.SourcesForChangedPath(
		t.Context(),
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
				_, err = conn.ExecContext(t.Context(), "CREATE TABLE unrelated (id TEXT PRIMARY KEY)")
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
				_, err = conn.ExecContext(t.Context(), `
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
	_, err = conn.ExecContext(t.Context(), "CREATE TABLE unrelated (id TEXT PRIMARY KEY)")
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
// discovery look authoritatively empty while the resolved reconciliation
// scope still proved the whole container — and the engine tombstoned every
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
				t.Helper()

				createHermesStateDB(t, root)
			},
		},
		{
			name: "during state fallback",
			state: func(t *testing.T, root string) {
				t.Helper()

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
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "child",
	})
	require.NoError(t, err)
	require.True(t, found)

	fingerprint, err := provider.Fingerprint(t.Context(), source)
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
	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	require.Equal(t, stateDB, discovered[0].DisplayPath)

	before, err := provider.Fingerprint(t.Context(), discovered[0])
	require.NoError(t, err)

	walPath := stateDB + "-wal"
	require.NoError(t, os.WriteFile(walPath, nil, 0o644))
	walTime := time.Now().Add(2 * time.Second).Truncate(time.Second)
	require.NoError(t, os.Chtimes(walPath, walTime, walTime))

	after, err := provider.Fingerprint(t.Context(), discovered[0])
	require.NoError(t, err)
	assert.Equal(t, before.Size, after.Size,
		"a zero-length WAL must not change the archive size")
	assert.Equal(t, before.MTimeNS, after.MTimeNS,
		"a zero-length WAL's mtime must not change the archive freshness")
	assert.Equal(t, before.Hash, after.Hash,
		"a zero-length WAL must not change the archive hash")

	require.NoError(t, os.WriteFile(walPath, []byte(walWithFramesFixture), 0o644))
	committedTime := walTime.Add(2 * time.Second)
	require.NoError(t, os.Chtimes(walPath, committedTime, committedTime))

	committed, err := provider.Fingerprint(t.Context(), discovered[0])
	require.NoError(t, err)
	assert.Equal(t, before.Size+int64(len(walWithFramesFixture)), committed.Size,
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
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "child",
	})
	require.NoError(t, err)
	require.True(t, found)
	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	stateInfo, err := os.Stat(stateDB)
	require.NoError(t, err)

	conn, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "UPDATE sessions SET title = ? WHERE id = ?", "Other Session", "child")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.NoError(t, os.Chtimes(stateDB, stateInfo.ModTime(), stateInfo.ModTime()))
	after, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)

	assert.NotEqual(t, before.Hash, after.Hash,
		"state metadata used by parsing must participate even when transcript messages win")
}

func TestHermesCronParentLookupUsesIDIndex(t *testing.T) {
	var allocations []float64
	for _, count := range []int{74, 7400} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			root := t.TempDir()
			createHermesStateDB(t, root)
			conn, err := sql.Open("sqlite3", filepath.Join(root, "state.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, conn.Close()) })
			_, err = conn.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < ?)
				INSERT INTO sessions(id, source, started_at) SELECT printf('unrelated-%06d', i), 'cron', 1 FROM n`, count)
			require.NoError(t, err)
			for _, analyzed := range []bool{false, true} {
				if analyzed {
					_, err = conn.ExecContext(t.Context(), "ANALYZE")
					require.NoError(t, err)
				}
				var id, parent, unused int
				var detail string
				require.NoError(t, conn.QueryRowContext(t.Context(), "EXPLAIN QUERY PLAN "+hermesCronParentQuery, "child").Scan(&id, &parent, &unused, &detail))
				assert.Contains(t, detail, "id=?", "count=%d analyzed=%t", count, analyzed)
			}
			dir := filepath.Join(root, "sessions")
			require.NoError(t, os.MkdirAll(dir, 0o755))
			for i := range count {
				require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("session_unrelated-%d.json", i)), []byte(`{}`), 0o600))
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "session_middle.json"), []byte(`{"parent_session_id":"cron_job-a_20261007_120000"}`), 0o600))
			allocations = append(allocations, testing.AllocsPerRun(20, func() {
				ss := hermesStateSession{id: "tip", source: "cron", parentSessionID: "middle"}
				require.NoError(t, resolveHermesCronJob(dir, &ss, nil))
				require.Equal(t, "job-a", ss.cronJob)
			}))
			header := "{\"role\":\"session_meta\",\"platform\":\"cron\",\"parent_session_id\":\"middle\"}\n"
			lr := newLineReader(strings.NewReader(header+strings.Repeat("x", count*1024)), maxLineSize)
			metadata, err := readHermesJSONLMetadata(lr)
			require.NoError(t, err)
			assert.Equal(t, "middle", metadata.parentSessionID)
			assert.LessOrEqual(t, lr.cr.n, int64(initialScanBufSize), "ancestor metadata reads only the header buffer")
			releaseLineReader(lr)
			if count == 74 {
				_, err = conn.ExecContext(t.Context(), "DROP TABLE sessions")
				require.NoError(t, err)
				for _, parent := range []string{"", "cron_job-a_20261007_120000"} {
					ss := hermesStateSession{id: "tip", source: "cron", parentSessionID: parent}
					require.NoError(t, resolveHermesCronMember(t.Context(), conn, filepath.Join(root, "state.db"), &ss))
					assert.Equal(t, HermesCronJobID(parent, nil), ss.cronJob)
				}
				ss := hermesStateSession{id: "tip", source: "cron", parentSessionID: "middle"}
				require.Error(t, resolveHermesCronMember(t.Context(), conn, filepath.Join(root, "state.db"), &ss))
			}
		})
	}
	require.Len(t, allocations, 2)
	assert.LessOrEqual(t, allocations[1], allocations[0]+5, "transcript ancestry lookup must stay bounded by the selected lineage")
}

func TestHermesCronRunGroupingAndFreshness(t *testing.T) {
	root := t.TempDir()
	createHermesStateDB(t, root)
	stateDB := filepath.Join(root, "state.db")
	conn, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	const runID = "cron_job.1_20261007_120000"
	_, err = conn.ExecContext(t.Context(), `INSERT INTO sessions(id, source, model, parent_session_id, started_at) VALUES
		('cron_job.1_20261007_120000', 'cron', 'gpt-5.4', NULL, 1),
		('middle', 'cron', 'gpt-5.4', 'cron_job.1_20261007_120000', 2),
		('tip', 'cron', 'gpt-5.4', 'middle', 3);
		INSERT INTO messages(session_id, role, content, timestamp)
		SELECT id, 'user', 'run message', started_at FROM sessions WHERE source = 'cron';`)
	require.NoError(t, err)
	provider := newHermesTestProvider(t, root)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: runID})
	require.NoError(t, err)
	require.True(t, found)
	unrelated, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "child"})
	require.NoError(t, err)
	require.True(t, found)
	fingerprint := func(source SourceRef) SourceFingerprint {
		fp, err := provider.Fingerprint(t.Context(), source)
		require.NoError(t, err)
		ctx, cleanup, err := WithReconciliationCache(t.Context())
		require.NoError(t, err)
		cached, err := provider.Fingerprint(ctx, source)
		require.NoError(t, err)
		require.NoError(t, cleanup())
		assert.Equal(t, fp.Hash, cached.Hash)
		return fp
	}
	before, unrelatedBefore := fingerprint(source), fingerprint(unrelated)
	_, err = conn.ExecContext(t.Context(), "UPDATE sessions SET title = 'Daily digest · Oct 07 12:00' WHERE id = 'tip'")
	require.NoError(t, err)
	want := "hermes-cron"
	archive, err := provider.parseArchive(t.Context(), stateDB, "", "local")
	require.NoError(t, err)
	for _, res := range archive {
		if res.Session.SourceSessionID == "child" {
			continue
		}
		assert.Equal(t, want, res.Session.Project)
		assert.Equal(t, "job.1", res.Session.GroupKey)
		member, err := provider.parseStateMember(t.Context(), hermesSource{StateDB: stateDB, SessionID: res.Session.SourceSessionID}, "", "local", SourceFingerprint{})
		require.NoError(t, err)
		require.Len(t, member.Results, 1)
		assert.Equal(t, want, member.Results[0].Result.Session.Project)
		assert.Equal(t, "job.1", member.Results[0].Result.Session.GroupKey)
		assert.Equal(t, res.Session.SessionName, member.Results[0].Result.Session.SessionName)
	}
	after := fingerprint(source)
	assert.Equal(t, before.Hash, after.Hash)
	assert.Equal(t, unrelatedBefore.Hash, fingerprint(unrelated).Hash)
	tip, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "tip"})
	require.NoError(t, err)
	require.True(t, found)
	_, err = conn.ExecContext(t.Context(), "UPDATE sessions SET parent_session_id = 'tip' WHERE id = 'middle'")
	require.NoError(t, err)
	missingParent := fingerprint(tip)
	_, err = conn.ExecContext(t.Context(), "UPDATE sessions SET parent_session_id = ? WHERE id = 'middle'", runID)
	require.NoError(t, err)
	assert.NotEqual(t, missingParent.Hash, fingerprint(tip).Hash)
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	path := filepath.Join(sessionsDir, "session_transcript-only.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"source":"cron","parent_session_id":"middle","messages":[{"role":"user","content":"retained run"}]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(sessionsDir, "session_middle.json"), []byte(`{"source":"cron","parent_session_id":"cron_job-2_20261007_120000"}`), 0o600))
	_, err = conn.ExecContext(t.Context(), "UPDATE sessions SET parent_session_id = 'cron_job-2_20261007_120000' WHERE id = 'middle'")
	require.NoError(t, err)
	archive, err = provider.parseArchive(t.Context(), stateDB, "", "local")
	require.NoError(t, err)
	for _, result := range archive {
		if result.Session.ID == "hermes:transcript-only" {
			assert.Equal(t, "hermes-cron", result.Session.Project)
			assert.Equal(t, "job-2", result.Session.GroupKey)
		}
	}
	require.NoError(t, os.WriteFile(filepath.Join(sessionsDir, "session_cli-only.json"), []byte(`{"platform":"cli","source":"cron","parent_session_id":"middle","messages":[{"role":"user","content":"ordinary run"}]}`), 0o600))
	hinted, err := provider.parseArchive(t.Context(), stateDB, "hermes-cron", "local")
	require.NoError(t, err)
	for _, result := range hinted {
		switch result.Session.ID {
		case "hermes:transcript-only":
			assert.Equal(t, "hermes-cron", result.Session.Project)
			assert.Equal(t, "job-2", result.Session.GroupKey)
			assert.True(t, result.Session.projectSynthesizedByHermes)
		case "hermes:cli-only":
			assert.Equal(t, "hermes-cli", result.Session.Project)
			assert.Empty(t, result.Session.ParentSessionID)
			assert.Empty(t, result.Session.RelationshipType)
		default:
			assert.Equal(t, "hermes-cron", result.Session.Project)
			assert.False(t, result.Session.projectSynthesizedByHermes)
		}
	}
	t.Run("transcript ancestry", func(t *testing.T) {
		for _, format := range []string{"json", "jsonl"} {
			t.Run(format, func(t *testing.T) {
				dir := t.TempDir()
				name, body := "session_tip.json", `{"source":"cron","parent_session_id":"middle","messages":[{"role":"user","content":"run"}]}`
				if format == "jsonl" {
					name, body = "tip.jsonl", "{\"role\":\"session_meta\",\"platform\":\"cron\",\"parent_session_id\":\"middle\"}\n{\"role\":\"user\",\"content\":\"run\"}\n"
				}
				path := filepath.Join(dir, name)
				require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
				provider := newHermesTestProvider(t, dir)
				source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "tip"})
				require.NoError(t, err)
				require.True(t, found)
				before, err := provider.Fingerprint(t.Context(), source)
				require.NoError(t, err)
				middle := filepath.Join(dir, "session_middle.json")
				for _, parent := range []string{runID, "cron_job-2_20261007_120000", "tip", "missing"} {
					require.NoError(t, os.WriteFile(middle, []byte(fmt.Sprintf(`{"source":"cron","parent_session_id":%q}`, parent)), 0o600))
					after, err := provider.Fingerprint(t.Context(), source)
					require.NoError(t, err)
					assert.Equal(t, before.MTimeNS, after.MTimeNS)
					assert.Equal(t, before.Size, after.Size)
					if parent == runID || parent == "cron_job-2_20261007_120000" {
						assert.NotEqual(t, before.Hash, after.Hash)
					} else {
						assert.Equal(t, before.Hash, after.Hash)
					}
				}
				require.NoError(t, os.Remove(middle))
				after, err := provider.Fingerprint(t.Context(), source)
				require.NoError(t, err)
				assert.Equal(t, before, after)
			})
		}
	})
	_, err = conn.ExecContext(t.Context(), `INSERT INTO sessions (id, source, model, started_at, input_tokens)
		VALUES ('unusable', 'cron', 'gpt-5.4', 1, 1)`)
	require.NoError(t, err)
	for _, tc := range []struct{ body, parent, format, want, group string }{
		{"", "unusable", "", "hermes-cron", ""},
		{"!", "unusable", "", "hermes-cron", ""},
		{`{"source":"cron","parent_session_id":"cron_job-b_20261007_120000"`, "unusable", "", "hermes-cron", ""},
		{"", runID, "json", "hermes-cron", "job.1"},
		{"!", runID, "jsonl", "hermes-cron", "job.1"},
	} {
		_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET parent_session_id = ?, input_tokens = 10,
			actual_cost_usd = 3, cost_status = 'actual', cost_source = 'hermes' WHERE id = 'tip'`, tc.parent)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(sessionsDir, "session_unusable.json"), []byte(tc.body), 0o600))
		selectedPath := filepath.Join(sessionsDir, "session_tip.json")
		if tc.format != "" {
			body := `{"source":"cron","parent_session_id":"unusable","messages":[{"role":"user","content":"run message"},{"role":"assistant","content":"richer reply"}]}`
			if tc.format == "jsonl" {
				selectedPath = filepath.Join(sessionsDir, "tip.jsonl")
				body = "{\"role\":\"session_meta\",\"platform\":\"cron\",\"parent_session_id\":\"unusable\"}\n{\"role\":\"user\",\"content\":\"run message\"}\n{\"role\":\"assistant\",\"content\":\"richer reply\"}\n"
			}
			require.NoError(t, os.WriteFile(selectedPath, []byte(body), 0o600))
		}
		fingerprint(tip)
		for _, hint := range []string{"", "hermes-cron"} {
			member, err := provider.parseStateMember(t.Context(), hermesSource{StateDB: stateDB, SessionID: "tip"}, hint, "local", SourceFingerprint{})
			require.NoError(t, err)
			require.Len(t, member.Results, 1)
			results := []ParseResult{member.Results[0].Result}
			bulk, err := provider.parseArchive(t.Context(), stateDB, hint, "local")
			require.NoError(t, err)
			for _, res := range bulk {
				if res.Session.ID == "hermes:tip" {
					results = append(results, res)
				}
			}
			require.Len(t, results, 2)
			for _, res := range results {
				assert.Equal(t, firstNonEmptyJSONLString(hint, tc.want), res.Session.Project)
				assert.Equal(t, tc.group, res.Session.GroupKey)
				assert.Equal(t, hint == "", res.Session.projectSynthesizedByHermes)
				require.Len(t, res.UsageEvents, 1)
				require.NotNil(t, res.UsageEvents[0].Cost)
				assert.EqualValues(t, 3_000_000, res.UsageEvents[0].Cost.Microdollars)
				if tc.format == "" {
					require.Len(t, res.Messages, 1)
				} else {
					require.Len(t, res.Messages, 2)
					assert.Equal(t, "richer reply", res.Messages[1].Content)
				}
			}
		}
		if tc.format != "" {
			require.NoError(t, os.Remove(selectedPath))
		}
	}
	_, err = conn.ExecContext(t.Context(), "ALTER TABLE sessions RENAME TO unavailable_sessions")
	require.NoError(t, err)
	_, err = provider.parseStateMember(t.Context(), hermesSource{StateDB: stateDB, SessionID: "tip"}, "", "local", SourceFingerprint{})
	require.Error(t, err)
	_, err = provider.Fingerprint(t.Context(), tip)
	require.Error(t, err)
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

			plan, err := provider.WatchPlan(t.Context())
			require.NoError(t, err)
			require.Len(t, plan.Roots, 2)
			assert.Equal(t, root, plan.Roots[0].Path)
			assert.False(t, plan.Roots[0].Recursive)
			assert.Equal(t, []string{"state.db", "state.db-wal"}, plan.Roots[0].IncludeGlobs)
			assert.Equal(t, sessionsDir, plan.Roots[1].Path)
			assert.True(t, plan.Roots[1].Recursive)
			assert.Equal(t, []string{"*.jsonl", "session_*.json"}, plan.Roots[1].IncludeGlobs)

			changed, err := provider.SourcesForChangedPath(
				t.Context(),
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

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, profilesRoot, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)

	before, err := provider.Discover(t.Context())
	require.NoError(t, err)
	assert.Empty(t, before)

	profileRoot := filepath.Join(profilesRoot, "research")
	require.NoError(t, os.MkdirAll(profileRoot, 0o755))
	createHermesStateDB(t, profileRoot)
	stateDB := filepath.Join(profileRoot, "state.db")

	after, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.Equal(t, stateDB, after[0].DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
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
		t.Context(),
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
		warm, err := provider.SourcesForChangedPath(t.Context(), request)
		require.NoError(t, err)
		require.Len(t, warm, 1)
		assert.Equal(t, targetPath, warm[0].DisplayPath)

		return testing.AllocsPerRun(20, func() {
			sources, sourceErr := provider.SourcesForChangedPath(
				t.Context(), request,
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
	measure := func(t *testing.T, sessionCount int, source string) int64 {
		t.Helper()

		root := t.TempDir()
		createHermesStateDB(t, root)
		stateDB := filepath.Join(root, "state.db")
		conn, err := sql.Open("sqlite3", stateDB)
		require.NoError(t, err)
		tx, err := conn.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(t.Context(), "DELETE FROM messages; DELETE FROM sessions")
		require.NoError(t, err)
		for i := range sessionCount {
			id := fmt.Sprintf("cron_job.%06d_20261007_120000", i)
			_, err = tx.ExecContext(t.Context(), `INSERT INTO sessions
				(id, source, started_at, estimated_cost_usd, actual_cost_usd)
				VALUES (?, ?, ?, 0, 0)`, id, source, i)
			require.NoError(t, err)
			_, err = tx.ExecContext(t.Context(), `INSERT INTO messages
				(session_id, role, content, timestamp)
				VALUES (?, 'user', 'hello', ?)`, id, i)
			require.NoError(t, err)
		}
		require.NoError(t, tx.Commit())
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

	for _, source := range []string{"cli", "cron"} {
		t.Run(source, func(t *testing.T) {
			small := measure(t, 3, source)
			large := measure(t, 300, source)
			assert.Positive(t, small, "the retained-ID allocation boundary must be observed")
			assert.LessOrEqual(t, large, small*2,
				"peak retained ID bytes must not scale with Hermes archive cardinality")
		})
	}
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

		plan, err := provider.WatchPlan(t.Context())
		require.NoError(t, err)
		require.Len(t, plan.Roots, 2)
		assert.Equal(t, root, plan.Roots[0].Path)
		assert.False(t, plan.Roots[0].Recursive)
		assert.Equal(t, []string{"state.db", "state.db-wal"}, plan.Roots[0].IncludeGlobs)
		assert.Equal(t, sessionsDir, plan.Roots[1].Path)
		assert.True(t, plan.Roots[1].Recursive)
		assert.Equal(t, []string{"*.jsonl", "session_*.json"}, plan.Roots[1].IncludeGlobs)

		changed, err := provider.SourcesForChangedPath(
			t.Context(),
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

		plan, err := provider.WatchPlan(t.Context())
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
			t.Context(),
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

		plan, err := provider.WatchPlan(t.Context())
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
			t.Context(),
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
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
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
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
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
	assert.Equal(t,
		stateInfo.Size()+transcriptInfo.Size(),
		result.Result.Session.File.Size,
	)
	assert.Equal(t,
		max(stateInfo.ModTime().UnixNano(), transcriptInfo.ModTime().UnixNano()),
		result.Result.Session.File.Mtime,
	)
}

func TestHermesDefaultRootsDiscoverWindowsStateDB(t *testing.T) {
	newProvider := func(t *testing.T) (Provider, string, string) {
		t.Helper()
		home := t.TempDir()
		windowsRoot := filepath.Join(home, "AppData", "Local", "hermes", "sessions")
		hermesHome := filepath.Dir(windowsRoot)
		require.NoError(t, os.MkdirAll(hermesHome, 0o755))
		createHermesStateDB(t, hermesHome)

		var def AgentDef
		for _, candidate := range Registry {
			if candidate.Type == AgentHermes {
				def = candidate
				break
			}
		}
		require.NotEmpty(t, def.DefaultDirs)
		roots := make([]string, 0, len(def.DefaultDirs))
		for _, relative := range def.DefaultDirs {
			roots = append(roots, filepath.Join(home, filepath.FromSlash(relative)))
		}

		provider, ok := NewProvider(AgentHermes, ProviderConfig{
			Roots:   roots,
			Machine: "test-machine",
		})
		require.True(t, ok)
		return provider, windowsRoot, filepath.Join(hermesHome, "state.db")
	}

	t.Run("state_db_under_windows_hermes_home", func(t *testing.T) {
		provider, windowsRoot, stateDB := newProvider(t)

		sources, err := provider.Discover(t.Context())
		require.NoError(t, err)
		require.Len(t, sources, 1)
		assert.Equal(t, windowsRoot, sources[0].ConfiguredRoot)
		assert.Equal(t, stateDB, sources[0].DisplayPath)

		outcome, err := provider.Parse(t.Context(), ParseRequest{
			Source:      sources[0],
			Fingerprint: SourceFingerprint{Key: stateDB, Hash: "aggregate-hash"},
		})
		require.NoError(t, err)
		require.True(t, outcome.ResultSetComplete)
		require.True(t, outcome.ForceReplace)
		require.Len(t, outcome.Results, 1)
		result := outcome.Results[0]
		assert.Equal(t, DataVersionCurrent, result.DataVersion)
		assert.Equal(t, "hermes:child", result.Result.Session.ID)
		assert.Equal(t, "child", result.Result.Session.SourceSessionID)
		assert.Equal(t, "hermes:parent", result.Result.Session.ParentSessionID)
		assert.Equal(t, RelContinuation, result.Result.Session.RelationshipType)
		assert.Equal(t, "hermes-state-db", result.Result.Session.SourceVersion)
		assert.Equal(t, stateDB, result.Result.Session.File.Path)
	})

	t.Run("streamed_state_member", func(t *testing.T) {
		provider, windowsRoot, stateDB := newProvider(t)
		discoverer, ok := provider.(StreamingDiscoverer)
		require.True(t, ok)

		var sources []SourceRef
		err := discoverer.DiscoverEach(t.Context(), func(source SourceRef) error {
			sources = append(sources, source)
			return nil
		})
		require.NoError(t, err)
		require.Len(t, sources, 1)

		memberPath := VirtualSourcePath(stateDB, "child")
		assert.Equal(t, windowsRoot, sources[0].ConfiguredRoot)
		assert.Equal(t, memberPath, sources[0].DisplayPath)

		outcome, err := provider.Parse(t.Context(), ParseRequest{
			Source:      sources[0],
			Fingerprint: SourceFingerprint{Key: memberPath, Hash: "streamed-hash"},
		})
		require.NoError(t, err)
		require.True(t, outcome.ResultSetComplete)
		require.True(t, outcome.ForceReplace)
		require.Len(t, outcome.Results, 1)
		result := outcome.Results[0]
		assert.Equal(t, DataVersionCurrent, result.DataVersion)
		assert.Equal(t, "hermes:child", result.Result.Session.ID)
		assert.Equal(t, "child", result.Result.Session.SourceSessionID)
		assert.Equal(t, "hermes:parent", result.Result.Session.ParentSessionID)
		assert.Equal(t, RelContinuation, result.Result.Session.RelationshipType)
		assert.Equal(t, "hermes-state-db", result.Result.Session.SourceVersion)
		assert.Equal(t, memberPath, result.Result.Session.File.Path)
		require.Len(t, result.Result.Messages, 1)
		assert.Equal(t, "state db only has one message", result.Result.Messages[0].Content)
	})

	t.Run("watch_plan_before_hermes_home_exists", func(t *testing.T) {
		home := t.TempDir()
		windowsRoot := filepath.Join(home, "AppData", "Local", "hermes", "sessions")
		var def AgentDef
		for _, candidate := range Registry {
			if candidate.Type == AgentHermes {
				def = candidate
				break
			}
		}
		roots := make([]string, 0, len(def.DefaultDirs))
		for _, relative := range def.DefaultDirs {
			roots = append(roots, filepath.Join(home, filepath.FromSlash(relative)))
		}
		provider, ok := NewProvider(AgentHermes, ProviderConfig{Roots: roots})
		require.True(t, ok)

		plan, err := provider.WatchPlan(t.Context())
		require.NoError(t, err)
		hermesHome := filepath.Dir(windowsRoot)
		var homeWatch, sessionsWatch *WatchRoot
		for i := range plan.Roots {
			root := &plan.Roots[i]
			if samePath(root.Path, hermesHome) {
				homeWatch = root
			}
			if samePath(root.Path, windowsRoot) {
				sessionsWatch = root
			}
			assert.False(t, samePath(root.Path, hermesHome) && root.Recursive)
		}
		require.NotNil(t, homeWatch)
		assert.False(t, homeWatch.Recursive)
		assert.Equal(t, []string{"state.db", "state.db-wal"}, homeWatch.IncludeGlobs)
		require.NotNil(t, sessionsWatch)
		assert.True(t, sessionsWatch.Recursive)
		assert.Equal(t, []string{"*.jsonl", "session_*.json"}, sessionsWatch.IncludeGlobs)
	})
}

func TestHermesStateMembershipHonorsCallerCancellation(t *testing.T) {
	root := t.TempDir()
	createHermesStateDB(t, root)
	membership, err := openHermesStateMembership(t.Context(), filepath.Join(root, "state.db"))
	require.NoError(t, err)
	t.Cleanup(membership.Close)

	found, err := membership.Has(t.Context(), "child")
	require.NoError(t, err)
	require.True(t, found)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = membership.Has(ctx, "child")
	require.ErrorIs(t, err, context.Canceled)
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

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
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

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
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
