package parser

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeAntigravitySummaryRow(t *testing.T, root, id, workspacePath string) {
	t.Helper()
	dbPath := filepath.Join(root, AntigravitySummariesDBName)
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.ExecContext(t.Context(), `
		CREATE TABLE IF NOT EXISTS conversation_summaries (
			conversation_id TEXT PRIMARY KEY,
			title TEXT NOT NULL DEFAULT '',
			workspace_uris TEXT NOT NULL DEFAULT '[]',
			parent_conversation_id TEXT NOT NULL DEFAULT '',
			app_data_dir TEXT NOT NULL DEFAULT ''
		)`)
	require.NoError(t, err)
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(workspacePath)}).String()
	_, err = conn.ExecContext(t.Context(),
		`INSERT OR REPLACE INTO conversation_summaries (conversation_id, title, workspace_uris, app_data_dir) VALUES (?, ?, ?, ?)`,
		id, "Sample Title", `["`+uri+`"]`, "antigravity-acp",
	)
	require.NoError(t, err)
}

func TestAntigravityCLIProviderExpandsMetaHarnessContainers(t *testing.T) {
	const (
		idA = "11111111-2222-3333-4444-555555555551"
		idB = "11111111-2222-3333-4444-555555555552"
	)

	tests := []struct {
		name           string
		setupContainer func(t *testing.T, base string) (configuredRoot, sessionRootA, sessionRootB string)
	}{
		{
			name: "t3 code providers container",
			setupContainer: func(t *testing.T, base string) (string, string, string) {
				t.Helper()
				container := filepath.Join(base, ".t3", "userdata", "providers", "antigravity")
				rootA := filepath.Join(container, "aaaaaaaaaaaaaaaa", "antigravity-acp")
				rootB := filepath.Join(container, "bbbbbbbbbbbbbbbb", "antigravity-acp")
				// Sibling config dir must not confuse expansion.
				require.NoError(t, os.MkdirAll(filepath.Join(container, "aaaaaaaaaaaaaaaa", "config"), 0o755))
				return container, rootA, rootB
			},
		},
		{
			name: "custom meta-harness nested instance container",
			setupContainer: func(t *testing.T, base string) (string, string, string) {
				t.Helper()
				container := filepath.Join(base, "my-harness", "agents")
				rootA := filepath.Join(container, "inst-alpha", "antigravity-acp")
				rootB := filepath.Join(container, "inst-beta", "antigravity-cli")
				return container, rootA, rootB
			},
		},
		{
			name: "custom meta-harness direct instance container",
			setupContainer: func(t *testing.T, base string) (string, string, string) {
				t.Helper()
				container := filepath.Join(base, "my-harness", "profiles")
				rootA := filepath.Join(container, "personal")
				rootB := filepath.Join(container, "work")
				return container, rootA, rootB
			},
		},
		{
			name: "single profile gemini-dir root",
			setupContainer: func(t *testing.T, base string) (string, string, string) {
				t.Helper()
				profile := filepath.Join(base, "isolated-profile")
				rootA := filepath.Join(profile, "antigravity-acp")
				rootB := filepath.Join(profile, "antigravity-cli")
				return profile, rootA, rootB
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			configuredRoot, rootA, rootB := tt.setupContainer(t, base)
			writeAntigravityCLIProviderFixture(t, rootA, idA)
			writeAntigravitySummaryRow(t, rootA, idA, "/work/alpha")
			writeAntigravityCLIProviderFixture(t, rootB, idB)
			writeAntigravitySummaryRow(t, rootB, idB, "/work/beta")

			dbPathA := filepath.Join(rootA, "conversations", idA+".db")
			dbPathB := filepath.Join(rootB, "conversations", idB+".db")
			implicitA := filepath.Join(rootA, "implicit", idA+".pb")
			implicitB := filepath.Join(rootB, "implicit", idB+".pb")

			provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
				Roots:   []string{configuredRoot},
				Machine: "devbox",
			})
			require.True(t, ok)

			// Batch discovery finds every instance's sessions.
			discovered, err := provider.Discover(t.Context())
			require.NoError(t, err)
			assertAntigravityCLISourcePaths(t, discovered, dbPathA, implicitA, dbPathB, implicitB)

			// Streaming discovery yields every conversation and implicit file across instances.
			var streamed []SourceRef
			require.NoError(t, provider.(StreamingDiscoverer).DiscoverEach(t.Context(), func(ref SourceRef) error {
				streamed = append(streamed, ref)
				return nil
			}))
			pbPathA := filepath.Join(rootA, "conversations", idA+".pb")
			pbPathB := filepath.Join(rootB, "conversations", idB+".pb")
			streamedPaths := make([]string, 0, len(streamed))
			for _, ref := range streamed {
				streamedPaths = append(streamedPaths, ref.DisplayPath)
			}
			assert.ElementsMatch(t, []string{dbPathA, pbPathA, implicitA, dbPathB, pbPathB, implicitB}, streamedPaths)

			// FindSource resolves by raw session ID and stored path across instances.
			foundA, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: idA})
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, dbPathA, foundA.DisplayPath)

			foundB, ok, err := provider.FindSource(t.Context(), FindSourceRequest{StoredFilePath: dbPathB})
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, dbPathB, foundB.DisplayPath)

			// Fingerprint and Parse succeed using the instance's own summary DB and files.
			fp, err := provider.Fingerprint(t.Context(), foundA)
			require.NoError(t, err)
			require.NotEmpty(t, fp.Hash)
			parsed, err := provider.Parse(t.Context(), ParseRequest{
				Source: foundA, Fingerprint: fp, Machine: "devbox",
			})
			require.NoError(t, err)
			require.Len(t, parsed.Results, 1)
			assert.Equal(t, "antigravity-cli:"+idA, parsed.Results[0].Result.Session.ID)

			// Changed-path routing maps .db-wal and brain transcript directly to the owning session.
			changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
				Path: dbPathA + "-wal", WatchRoot: configuredRoot, EventKind: "write",
			})
			require.NoError(t, err)
			assertAntigravityCLISourcePaths(t, changed, dbPathA)

			transcriptB := antigravityBrainTranscriptPath(rootB, idB)
			require.NoError(t, os.MkdirAll(filepath.Dir(transcriptB), 0o755))
			mustWrite(t, transcriptB, []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"hi"}`+"\n"))
			changed, err = provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
				Path: transcriptB, WatchRoot: configuredRoot, EventKind: "write",
			})
			require.NoError(t, err)
			assertAntigravityCLISourcePaths(t, changed, dbPathB, implicitB)
		})
	}
}

func TestAntigravityCLIProviderContainerWatchPlanAndRelevance(t *testing.T) {
	base := t.TempDir()
	// Known T3 container path must emit the container watch plan even before it exists.
	container := filepath.Join(base, ".t3", "userdata", "providers", "antigravity")
	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{container},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, container, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, 1, plan.Roots[0].MaxDepth)
	assert.Contains(t, plan.Roots[0].ExtraDirectories, "*/antigravity-acp")
	assert.Contains(t, plan.Roots[0].ExtraDirectories, "*/antigravity-acp/conversations")
	assert.Contains(t, plan.Roots[0].ExtraDirectories, "*/antigravity-acp/brain/*")
	assert.Contains(t, plan.Roots[0].ExtraDirectories, "*/antigravity-acp/brain/*/.system_generated/logs")

	const id = "22222222-3333-4444-5555-666666666666"
	instanceRoot := filepath.Join(container, "hash1", "antigravity-acp")
	relevance := provider.(ChangedPathRelevanceProvider)

	// Conversation and summary WAL index (-shm) writes stay non-data.
	for _, path := range []string{
		filepath.Join(instanceRoot, "conversations", id+".db-shm"),
		filepath.Join(instanceRoot, AntigravitySummariesDBName+"-shm"),
	} {
		got, relErr := relevance.ChangedPathRelevance(t.Context(), ChangedPathRequest{
			Path: path, WatchRoot: container, EventKind: "write",
		})
		require.NoError(t, relErr)
		assert.Equal(t, ChangedPathNonData, got, "path %s", path)
	}

	// Actual conversation database writes remain data-bearing.
	got, err := relevance.ChangedPathRelevance(t.Context(), ChangedPathRequest{
		Path: filepath.Join(instanceRoot, "conversations", id+".db-wal"), WatchRoot: container, EventKind: "write",
	})
	require.NoError(t, err)
	assert.Equal(t, ChangedPathUnclassified, got)
}

func TestAntigravityCLIProviderContainerChangedPathDoesNotScanSiblings(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("chmod 000 permission test requires non-root unix")
	}
	base := t.TempDir()
	container := filepath.Join(base, ".t3", "userdata", "providers", "antigravity")
	const id = "33333333-4444-5555-6666-777777777771"
	healthyRoot := filepath.Join(container, "healthy", "antigravity-acp")
	writeAntigravityCLIProviderFixture(t, healthyRoot, id)

	blockedSibling := filepath.Join(container, "blocked")
	require.NoError(t, os.MkdirAll(blockedSibling, 0o755))
	require.NoError(t, os.Chmod(blockedSibling, 0o000))
	t.Cleanup(func() { _ = os.Chmod(blockedSibling, 0o755) })

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{container},
		Machine: "devbox",
	})
	require.True(t, ok)

	dbPath := filepath.Join(healthyRoot, "conversations", id+".db")
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: dbPath + "-wal", WatchRoot: container, EventKind: "write",
	})
	require.NoError(t, err)
	assertAntigravityCLISourcePaths(t, changed, dbPath)
}

func TestAntigravityCLIProviderContainerSkipsSymlinksAndReportsUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("symlink and chmod 000 container test requires non-root unix")
	}
	base := t.TempDir()
	container := filepath.Join(base, ".t3", "userdata", "providers", "antigravity")
	directRoot := filepath.Join(base, ".gemini", "antigravity-cli")

	const (
		directID  = "44444444-5555-6666-7777-888888888881"
		linkID    = "44444444-5555-6666-7777-888888888882"
		healthyID = "44444444-5555-6666-7777-888888888883"
	)
	writeAntigravityCLIProviderFixture(t, directRoot, directID)

	externalProfile := filepath.Join(base, "external-profile")
	writeAntigravityCLIProviderFixture(t, filepath.Join(externalProfile, "antigravity-acp"), linkID)
	require.NoError(t, os.MkdirAll(container, 0o755))
	require.NoError(t, os.Symlink(externalProfile, filepath.Join(container, "symlinked-profile")))

	healthyInstance := filepath.Join(container, "healthy", "antigravity-acp")
	writeAntigravityCLIProviderFixture(t, healthyInstance, healthyID)

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{container, directRoot},
		Machine: "devbox",
	})
	require.True(t, ok)

	// Symlinked instance must be skipped while healthy instance and direct root are discovered.
	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	assertAntigravityCLISourcePaths(t, discovered,
		filepath.Join(directRoot, "conversations", directID+".db"),
		filepath.Join(directRoot, "implicit", directID+".pb"),
		filepath.Join(healthyInstance, "conversations", healthyID+".db"),
		filepath.Join(healthyInstance, "implicit", healthyID+".pb"),
	)

	// An unreadable container must report DiscoveryIncompleteError without starving the healthy direct root.
	require.NoError(t, os.Chmod(container, 0o000))
	t.Cleanup(func() { _ = os.Chmod(container, 0o755) })

	var incomplete DiscoveryIncompleteError
	discovered, err = provider.Discover(t.Context())
	require.ErrorAs(t, err, &incomplete)
	assertAntigravityCLISourcePaths(t, discovered,
		filepath.Join(directRoot, "conversations", directID+".db"),
		filepath.Join(directRoot, "implicit", directID+".pb"),
	)

	var streamed []SourceRef
	err = provider.(StreamingDiscoverer).DiscoverEach(t.Context(), func(ref SourceRef) error {
		streamed = append(streamed, ref)
		return nil
	})
	require.ErrorAs(t, err, &incomplete)
	streamedPaths := make([]string, 0, len(streamed))
	for _, ref := range streamed {
		streamedPaths = append(streamedPaths, ref.DisplayPath)
	}
	assert.ElementsMatch(t, []string{
		filepath.Join(directRoot, "conversations", directID+".db"),
		filepath.Join(directRoot, "conversations", directID+".pb"),
		filepath.Join(directRoot, "implicit", directID+".pb"),
	}, streamedPaths)

	_, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: healthyID})
	assert.False(t, found)
	assert.ErrorAs(t, err, &incomplete, "unreadable container must not report authoritative not-found")
}

func TestAntigravityCLIProviderContainerForeignMachineMarksRemoteCwd(t *testing.T) {
	base := t.TempDir()
	container := filepath.Join(base, ".t3", "userdata", "providers", "antigravity")
	const id = "55555555-6666-7777-8888-999999999991"
	instanceRoot := filepath.Join(container, "hash1", "antigravity-acp")
	writeAntigravityCLIProviderFixture(t, instanceRoot, id)
	writeAntigravitySummaryRow(t, instanceRoot, id, "/remote/repo")

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{container},
		Machine: "localbox",
		SourceMachines: map[string]string{
			container: "remotebox",
		},
	})
	require.True(t, ok)

	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: id})
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, SourceCwdRemote, source.CwdResolution.State)
	assert.Empty(t, source.CwdResolution.Path)
}

func TestAntigravityCLIProviderContainerInstanceNamedCacheOrBrain(t *testing.T) {
	tests := []struct {
		name           string
		instanceName   string
		nestedAppDir   string
		conversationID string
	}{
		{
			name:           "nested instance named cache",
			instanceName:   "cache",
			nestedAppDir:   "antigravity-acp",
			conversationID: "77777777-1111-4222-8333-444444444441",
		},
		{
			name:           "nested instance named brain",
			instanceName:   "brain",
			nestedAppDir:   "antigravity-acp",
			conversationID: "77777777-1111-4222-8333-444444444442",
		},
		{
			name:           "direct instance named cache",
			instanceName:   "cache",
			conversationID: "77777777-1111-4222-8333-444444444443",
		},
		{
			name:           "direct instance named brain",
			instanceName:   "brain",
			conversationID: "77777777-1111-4222-8333-444444444444",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			container := filepath.Join(t.TempDir(), "custom-harness")
			sessionRoot := filepath.Join(container, tt.instanceName)
			if tt.nestedAppDir != "" {
				sessionRoot = filepath.Join(sessionRoot, tt.nestedAppDir)
			}
			writeAntigravityCLIProviderFixture(t, sessionRoot, tt.conversationID)

			dbPath := filepath.Join(sessionRoot, "conversations", tt.conversationID+".db")
			implicitPath := filepath.Join(sessionRoot, "implicit", tt.conversationID+".pb")
			artifactPath := filepath.Join(sessionRoot, "brain", tt.conversationID, "plan.md")
			require.NoError(t, os.MkdirAll(filepath.Dir(artifactPath), 0o755))
			mustWrite(t, artifactPath, []byte("# Plan\n"))
			transcriptPath := antigravityBrainTranscriptPath(sessionRoot, tt.conversationID)
			require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o755))
			mustWrite(t, transcriptPath, []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"hi"}`+"\n"))

			provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
				Roots:   []string{container},
				Machine: "devbox",
			})
			require.True(t, ok)

			discovered, err := provider.Discover(t.Context())
			require.NoError(t, err)
			assertAntigravityCLISourcePaths(t, discovered, dbPath, implicitPath)

			for _, eventPath := range []string{dbPath + "-wal", artifactPath, transcriptPath} {
				changed, changeErr := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
					Path: eventPath, WatchRoot: container, EventKind: "write",
				})
				require.NoError(t, changeErr)
				if eventPath == dbPath+"-wal" {
					assertAntigravityCLISourcePaths(t, changed, dbPath)
				} else {
					assertAntigravityCLISourcePaths(t, changed, dbPath, implicitPath)
				}
			}

			relevance := provider.(ChangedPathRelevanceProvider)
			got, err := relevance.ChangedPathRelevance(t.Context(), ChangedPathRequest{
				Path: filepath.Join(sessionRoot, AntigravitySummariesDBName+"-shm"), WatchRoot: container, EventKind: "write",
			})
			require.NoError(t, err)
			assert.Equal(t, ChangedPathNonData, got)
		})
	}

	// A direct session root containing conversations/ must never be misrouted
	// as a container even if a brain directory contains unusual child names.
	directRoot := t.TempDir()
	const directID = "77777777-1111-4222-8333-444444444449"
	writeAntigravityCLIProviderFixture(t, directRoot, directID)
	_, isInstance := antigravityCLIContainerSpec.InstanceRootForPath(
		directRoot, filepath.Join(directRoot, "brain", "antigravity-acp", "plan.md"),
	)
	assert.False(t, isInstance)
}

func TestAntigravityCLIProviderContainerDiscoverEachContinuesPastUnreadableInstance(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("chmod 000 permission test requires non-root unix")
	}
	base := t.TempDir()
	container := filepath.Join(base, ".t3", "userdata", "providers", "antigravity")

	const (
		blockedID = "88888888-1111-4222-8333-444444444441"
		healthyID = "88888888-1111-4222-8333-444444444442"
	)

	// Place the unreadable instance lexicographically before the healthy
	// sibling. Because the parent profile directory stays readable, ExpandRoot
	// returns both roots and DiscoverEach encounters EACCES on the first root.
	blockedACP := filepath.Join(container, "aaa-blocked", "antigravity-acp")
	healthyACP := filepath.Join(container, "zzz-healthy", "antigravity-acp")
	writeAntigravityCLIProviderFixture(t, blockedACP, blockedID)
	writeAntigravityCLIProviderFixture(t, healthyACP, healthyID)

	require.NoError(t, os.Chmod(blockedACP, 0o000))
	t.Cleanup(func() { _ = os.Chmod(blockedACP, 0o755) })

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{container},
		Machine: "devbox",
	})
	require.True(t, ok)
	streamer := provider.(StreamingDiscoverer)

	var streamed []string
	err := streamer.DiscoverEach(t.Context(), func(ref SourceRef) error {
		streamed = append(streamed, ref.DisplayPath)
		return nil
	})
	var incomplete DiscoveryIncompleteError
	require.ErrorAs(t, err, &incomplete, "unreadable instance must surface DiscoveryIncompleteError")
	assert.ElementsMatch(t, []string{
		filepath.Join(healthyACP, "conversations", healthyID+".db"),
		filepath.Join(healthyACP, "conversations", healthyID+".pb"),
		filepath.Join(healthyACP, "implicit", healthyID+".pb"),
	}, streamed, "healthy later sibling must still be yielded")

	// Callback errors stop immediately and propagate verbatim (not wrapped as
	// DiscoveryIncompleteError).
	stopErr := errors.New("consumer stopped")
	var beforeStop int
	err = streamer.DiscoverEach(t.Context(), func(SourceRef) error {
		beforeStop++
		return stopErr
	})
	require.ErrorIs(t, err, stopErr)
	_, isIncomplete := errors.AsType[DiscoveryIncompleteError](err)
	assert.False(t, isIncomplete, "callback error must not be disguised as DiscoveryIncompleteError")
	assert.Equal(t, 1, beforeStop)

	// Context cancellation stops immediately.
	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	err = streamer.DiscoverEach(canceledCtx, func(SourceRef) error {
		require.Fail(t, "yield should not run on canceled context")
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
}
