package parser

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertDevinErrorRedacted(t *testing.T, err error, secretFragments ...string) {
	t.Helper()
	require.Error(t, err)
	for _, fragment := range secretFragments {
		assert.NotContains(t, err.Error(), fragment)
	}
}

func TestDevinProviderCapabilities(t *testing.T) {
	caps := devinProviderCapabilities()
	assert.Equal(t, CapabilitySupported, caps.Content.PerMessageTokenUsage)
	assert.Equal(t, CapabilityUnsupported, caps.Content.AggregateUsageEvents)
}

func TestDevinProviderDiscoverFindParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const sessionID = "session-123"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{
		ID:               sessionID,
		Title:            "DB title wins",
		WorkingDirectory: "/Users/alice/code/my-app",
		Model:            "db-model",
		CreatedAt:        new(int64(1704103199)),
		LastActivityAt:   new(int64(1704103265)),
	}, `{
		"agent":{"model_name":"devin-1"},
		"steps":[
			{"step_id":"step-1","source":"user","timestamp":"2024-01-01T10:00:01Z","message":"Fix the login bug"},
			{"step_id":"step-2","source":"agent","timestamp":"2024-01-01T10:00:05Z","message":[{"type":"text","text":"Inspecting files."}]}
		]
	}`)
	root := filepath.Dir(filepath.Dir(dbPath))
	virtualPath := VirtualSourcePath(dbPath, sessionID)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
	require.True(ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 2)
	assert.Equal(filepath.Join(root, "cli"), plan.Roots[0].Path)
	assert.False(plan.Roots[0].Recursive)
	assert.ElementsMatch([]string{devinDBFilename, devinDBFilename + "-*"},
		plan.Roots[0].IncludeGlobs,
	)
	assert.Equal(filepath.Join(root, "cli", "transcripts"), plan.Roots[1].Path)
	assert.False(plan.Roots[1].Recursive)
	assert.Equal([]string{"*.json"}, plan.Roots[1].IncludeGlobs)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(virtualPath, discovered[0].Key)
	assert.Equal(virtualPath, discovered[0].DisplayPath)
	assert.Equal(virtualPath, discovered[0].FingerprintKey)
	assert.Equal(int64(1704103265_000_000_000), discovered[0].DiscoveryMTimeNS)

	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path:      filepath.Join(root, "cli", "transcripts", sessionID+".json"),
		EventKind: "write",
		WatchRoot: filepath.Join(root, "cli", "transcripts"),
	})
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(virtualPath, changed[0].DisplayPath)

	fullIDSource, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~devin:" + sessionID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(virtualPath, fullIDSource.DisplayPath)

	storedSource, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: virtualPath,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(virtualPath, storedSource.DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), storedSource)
	require.NoError(err)
	assert.Equal(virtualPath, fingerprint.Key)
	assert.NotZero(fingerprint.MTimeNS)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: storedSource})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.True(outcome.ForceReplace)
	require.Len(outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(DataVersionCurrent, result.DataVersion)
	assert.Equal("devin:"+sessionID, result.Result.Session.ID)
	assert.Equal(virtualPath, result.Result.Session.File.Path)
	assert.Equal("devbox", result.Result.Session.Machine)
	assert.Len(result.Result.Messages, 2)
}

func TestDevinProviderDBEventsFanOutAndPreserveTombstones(t *testing.T) {
	require := require.New(t)

	const liveSessionID = "session-live"
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: liveSessionID, Title: "Live", WorkingDirectory: "/tmp/live", Model: "db-model", CreatedAt: new(int64(1704103199)), LastActivityAt: new(int64(1704103265))},
		devinSessionRow{ID: "session-deleted", Title: "Deleted", WorkingDirectory: "/tmp/deleted", Model: "db-model", CreatedAt: new(int64(1704103199)), LastActivityAt: new(int64(1704103264))},
	)
	fixture.writeTranscript(t, liveSessionID, `{"steps":[]}`)
	root := fixture.Root
	liveVirtualPath := fixture.sessionVirtualPath(liveSessionID)
	deletedVirtualPath := fixture.sessionVirtualPath("session-deleted")
	dbPath := fixture.DBPath
	execDevinTestSQL(t, dbPath, `DELETE FROM sessions WHERE id = 'session-deleted'`)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	for _, changedPath := range []string{dbPath, dbPath + "-wal"} {
		changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
			Path:              changedPath,
			EventKind:         "write",
			WatchRoot:         filepath.Join(root, "cli"),
			StoredSourcePaths: []string{deletedVirtualPath},
		})
		require.NoError(err)
		require.Len(changed, 2, changedPath)
		assert.ElementsMatch(t,
			[]string{liveVirtualPath, deletedVirtualPath},
			[]string{changed[0].DisplayPath, changed[1].DisplayPath},
		)
	}
}

func TestDevinProviderTranscriptEventsTargetLiveOrStoredSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const liveSessionID = "session-live"
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: liveSessionID, Title: "Live", WorkingDirectory: "/tmp/live", Model: "db-model", CreatedAt: new(int64(1704103199)), LastActivityAt: new(int64(1704103265))},
		devinSessionRow{ID: "session-deleted", Title: "Deleted", WorkingDirectory: "/tmp/deleted", Model: "db-model", CreatedAt: new(int64(1704103199)), LastActivityAt: new(int64(1704103264))},
	)
	fixture.writeTranscript(t, liveSessionID, `{"steps":[]}`)
	root := fixture.Root
	liveVirtualPath := fixture.sessionVirtualPath(liveSessionID)
	deletedVirtualPath := fixture.sessionVirtualPath("session-deleted")
	dbPath := fixture.DBPath
	execDevinTestSQL(t, dbPath, `DELETE FROM sessions WHERE id = 'session-deleted'`)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	liveChanged, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path:      filepath.Join(root, "cli", "transcripts", liveSessionID+".json"),
		EventKind: "write",
		WatchRoot: filepath.Join(root, "cli", "transcripts"),
	})
	require.NoError(err)
	require.Len(liveChanged, 1)
	assert.Equal(liveVirtualPath, liveChanged[0].DisplayPath)

	deletedChanged, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path:              filepath.Join(root, "cli", "transcripts", "session-deleted.json"),
		EventKind:         "write",
		WatchRoot:         filepath.Join(root, "cli", "transcripts"),
		StoredSourcePaths: []string{deletedVirtualPath},
	})
	require.NoError(err)
	require.Len(deletedChanged, 1)
	assert.Equal(deletedVirtualPath, deletedChanged[0].DisplayPath)
}

func TestDevinProviderRejectsUnrelatedChangedPaths(t *testing.T) {
	const sessionID = "session-123"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "Title", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAt: new(int64(1704103199)), LastActivityAt: new(int64(1704103265))}, `{"steps":[]}`)
	root := filepath.Dir(filepath.Dir(dbPath))

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	for _, req := range []ChangedPathRequest{
		{
			Path:      filepath.Join(root, "cli", "sessions.db-backup"),
			EventKind: "write",
			WatchRoot: filepath.Join(root, "cli"),
		},
		{
			// The provider's own read connection rewrites the -shm index.
			Path:      filepath.Join(root, "cli", devinDBFilename+"-shm"),
			EventKind: "write",
			WatchRoot: filepath.Join(root, "cli"),
		},
		{
			Path:      filepath.Join(root, "cli", "transcripts", sessionID+".txt"),
			EventKind: "write",
			WatchRoot: filepath.Join(root, "cli", "transcripts"),
		},
		{
			Path:      filepath.Join(root, "cli", "nested", devinDBFilename),
			EventKind: "write",
			WatchRoot: filepath.Join(root, "cli"),
		},
		{
			Path:      filepath.Join(root, "other", "sessions.db"),
			EventKind: "write",
			WatchRoot: filepath.Join(root, "other"),
		},
	} {
		changed, err := provider.SourcesForChangedPath(t.Context(), req)
		require.NoError(t, err)
		assert.Empty(t, changed, "%+v", req)
	}
}

func TestDevinProviderMissingTranscriptUsesMessageNodeFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const sessionID = "session-db-only"
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: sessionID, Title: "DB only session", WorkingDirectory: "/tmp/db-only-project", Model: "db-only-model", CreatedAt: new(int64(1704103200)), LastActivityAt: new(int64(1704103209))},
	)
	root := fixture.Root
	fixture.insertMessageNodes(t,
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 1, ChatMessage: `{"role":"user","content":"fallback user"}`, CreatedAt: 1704103201},
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 2, ChatMessage: `{"role":"assistant","content":"fallback assistant"}`, CreatedAt: 1704103205},
	)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
	require.True(ok)

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(err)
	require.True(ok)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(err)
	assert.True(outcome.ResultSetComplete)
	assert.True(outcome.ForceReplace)
	require.Len(outcome.Results, 1)
	assert.Empty(outcome.SourceErrors)
	assert.Equal("devin:"+sessionID, outcome.Results[0].Result.Session.ID)
	assert.Len(outcome.Results[0].Result.Messages, 2)
	assert.Equal("fallback user", outcome.Results[0].Result.Messages[0].Content)
}

func TestDevinProviderMissingTranscriptWithoutDBMessagesReturnsProviderError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const sessionID = "session-db-only-empty"
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: sessionID, Title: "DB only session", WorkingDirectory: "/tmp/db-only-project", Model: "db-only-model", CreatedAt: new(int64(1704103200)), LastActivityAt: new(int64(1704103209))},
	)
	root := fixture.Root

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
	require.True(ok)

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(err)
	require.True(ok)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.Error(err)
	assert.Empty(outcome.Results)
	assert.Empty(outcome.SourceErrors)
	assert.ErrorContains(err, "missing devin transcript")
	assert.NotContains(err.Error(), source.DisplayPath)
	assert.NotContains(err.Error(), sessionID)
}

func TestDevinProviderCompositeFingerprintStableAndRedacted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const sessionID = "session-fingerprint"
	dbPath, transcriptPath := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "Stable title", WorkingDirectory: "/Users/alice/.config/devin/project", Model: "db-model", CreatedAt: new(int64(1704103200)), LastActivityAt: new(int64(1704103209)), MetadataJSON: `{"token_hint":"redacted"}`}, `{"token":"secret-token-123","steps":[{"step_id":"step-1","source":"user","message":"hello"}]}`)
	root := filepath.Dir(filepath.Dir(dbPath))
	virtualPath := VirtualSourcePath(dbPath, sessionID)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(err)
	require.True(ok)

	first, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	second, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	assert.Equal(first, second)
	assert.Equal(virtualPath, first.Key)
	assert.NotZero(first.MTimeNS)
	assert.NotEmpty(first.Hash)
	assert.NotContains(first.Hash, "secret-token-123")
	assert.NotContains(first.Hash, "/Users/alice/.config/devin/project")
	assert.NotContains(first.Key, transcriptPath)
}

func TestDevinProviderFingerprintChangesWhenTranscriptChangesWithoutDBMetadataChange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const sessionID = "session-transcript-change"
	dbPath, transcriptPath := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "Transcript change", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAt: new(int64(1704103200)), LastActivityAt: new(int64(1704103209))}, `{"steps":[{"step_id":"step-1","source":"user","message":"alpha"}]}`)
	root := filepath.Dir(filepath.Dir(dbPath))

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(err)
	require.True(ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	transcriptInfo, err := os.Stat(transcriptPath)
	require.NoError(err)

	require.NoError(os.WriteFile(transcriptPath, []byte(`{"steps":[{"step_id":"step-1","source":"user","message":"omega"}]}`), 0o644))
	require.NoError(os.Chtimes(transcriptPath, transcriptInfo.ModTime(), transcriptInfo.ModTime()))

	after, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	assert.Equal(before.Key, after.Key)
	assert.Equal(before.MTimeNS, after.MTimeNS)
	assert.NotEqual(before.Hash, after.Hash)
}

func TestDevinProviderFingerprintChangesWhenLastActivityChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const sessionID = "session-last-activity"
	dbPath, transcriptPath := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "DB change", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAt: new(int64(1704103200)), LastActivityAt: new(int64(1704103209))}, `{"steps":[]}`)
	root := filepath.Dir(filepath.Dir(dbPath))
	// Keep filesystem timestamps older than the session activity so the
	// fingerprint change measures metadata, independent of filesystem timing.
	fileTime := time.Unix(1704103200, 0)
	require.NoError(os.Chtimes(dbPath, fileTime, fileTime))
	require.NoError(os.Chtimes(transcriptPath, fileTime, fileTime))

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(err)
	require.True(ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	execDevinTestSQL(t, dbPath, `UPDATE sessions SET last_activity_at = 1704103215 WHERE id = 'session-last-activity'`)
	require.NoError(os.Chtimes(dbPath, fileTime, fileTime))

	after, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	assert.Equal(before.Key, after.Key)
	assert.Equal(int64(1704103209000000000), before.MTimeNS)
	assert.Equal(int64(1704103215000000000), after.MTimeNS)
	assert.NotEqual(before.Hash, after.Hash)
}

func TestDevinProviderFingerprintChangesWhenWorkingDirectoryChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const sessionID = "session-cwd-change"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "CWD change", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAt: new(int64(1704103200)), LastActivityAt: new(int64(1704103209))}, `{"steps":[]}`)
	root := filepath.Dir(filepath.Dir(dbPath))

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(err)
	require.True(ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	execDevinTestSQL(t, dbPath, `UPDATE sessions SET working_directory = '/tmp/renamed-app' WHERE id = 'session-cwd-change'`)

	after, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	assert.Equal(before.Key, after.Key)
	assert.NotEqual(before.Hash, after.Hash)
}

func TestDevinProviderFingerprintWithoutTranscriptUsesDBFreshnessOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const sessionID = "session-missing-transcript"
	dbPath, transcriptPath := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "DB only session", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAt: new(int64(1704103200)), LastActivityAt: new(int64(1704103209))}, `{"steps":[]}`)
	root := filepath.Dir(filepath.Dir(dbPath))
	virtualPath := VirtualSourcePath(dbPath, sessionID)
	require.NoError(os.Remove(transcriptPath))

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(err)
	require.True(ok)

	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	assert.Equal(virtualPath, fingerprint.Key)
	assert.NotZero(fingerprint.MTimeNS)
	assert.Zero(fingerprint.Size)
	assert.NotEmpty(fingerprint.Hash)
	assert.NotContains(fingerprint.Hash, transcriptPath)
}

func TestDevinProviderFingerprintWithoutTranscriptChangesWhenMessageNodesChange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const sessionID = "session-message-node-change"
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: sessionID, Title: "DB messages", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAt: new(int64(1704103200)), LastActivityAt: new(int64(1704103209))},
	)
	fixture.insertMessageNodes(t,
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 1, ChatMessage: `{"role":"user","content":"alpha"}`, CreatedAt: 1704103201},
	)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{fixture.Root}})
	require.True(ok)

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(err)
	require.True(ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	execDevinTestSQL(t, fixture.DBPath, `UPDATE message_nodes SET chat_message = '{"role":"user","content":"omega"}' WHERE session_id = 'session-message-node-change'`)

	after, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	assert.Equal(before.Key, after.Key)
	assert.NotEqual(before.Hash, after.Hash)
}

func TestDevinProviderFingerprintWithoutTranscriptChangesWhenMainChainChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const sessionID = "session-main-chain-change"
	fixture := newDevinTestFixture(t,
		devinSessionRow{
			ID:               sessionID,
			Title:            "DB messages",
			WorkingDirectory: "/tmp/app",
			Model:            "db-model",
			CreatedAt:        new(int64(1704103200)),
			LastActivityAt:   new(int64(1704103209)),
			MainChainID:      new(int64(2)),
		},
	)
	fixture.insertMessageNodes(t,
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 1, ChatMessage: `{"role":"user","content":"question"}`, CreatedAt: 1704103201},
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 2, ParentNodeID: new(int64(1)), ChatMessage: `{"role":"assistant","content":"first branch"}`, CreatedAt: 1704103205},
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 3, ParentNodeID: new(int64(1)), ChatMessage: `{"role":"assistant","content":"second branch"}`, CreatedAt: 1704103205},
	)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{fixture.Root}})
	require.True(ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(err)
	require.True(ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	execDevinTestSQL(t, fixture.DBPath,
		`UPDATE sessions SET main_chain_id = 3 WHERE id = 'session-main-chain-change'`)
	after, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	assert.Equal(before.Key, after.Key)
	assert.NotEqual(before.Hash, after.Hash)
}

func TestDevinProviderRejectsInvalidStoredVirtualPaths(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const sessionID = "session-123"
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: sessionID, Title: "Title", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAt: new(int64(1704103199)), LastActivityAt: new(int64(1704103265))},
		devinSessionRow{ID: "session-999", Title: "Other", WorkingDirectory: "/tmp/other", Model: "db-model", CreatedAt: new(int64(1704103199)), LastActivityAt: new(int64(1704103265))},
	)
	fixture.writeTranscript(t, sessionID, `{"steps":[]}`)
	root := fixture.Root
	virtualPath := fixture.sessionVirtualPath(sessionID)
	otherPath := fixture.sessionVirtualPath("session-999")
	dbPath := fixture.DBPath

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	for _, path := range []string{
		dbPath + "#",
		filepath.Join(root, "cli", "other.db") + "#" + sessionID,
		filepath.Join(root, devinDBFilename) + "#" + sessionID,
		filepath.Join(root, "cli", "nested", devinDBFilename) + "#" + sessionID,
	} {
		_, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
			StoredFilePath:     path,
			RequireFreshSource: true,
		})
		require.NoError(err)
		assert.False(ok, "stored path %q", path)
	}

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID:       sessionID,
		StoredFilePath:     otherPath,
		RequireFreshSource: true,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(virtualPath, source.DisplayPath)
}

func TestDevinProviderDedupesDuplicateRoots(t *testing.T) {
	const sessionID = "session-123"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "Title", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAt: new(int64(1704103199)), LastActivityAt: new(int64(1704103265))}, `{"steps":[]}`)
	root := filepath.Dir(filepath.Dir(dbPath))

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root, root, filepath.Join(root, ".")}})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
}

func TestDevinProviderDeletedRowFingerprintsTombstoneAndSkips(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const sessionID = "session-123"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "Title", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAt: new(int64(1704103199)), LastActivityAt: new(int64(1704103265))}, `{"steps":[]}`)
	root := filepath.Dir(filepath.Dir(dbPath))
	virtualPath := VirtualSourcePath(dbPath, sessionID)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(err)
	require.True(ok)
	assert.Equal(virtualPath, source.DisplayPath)
	execDevinTestSQL(t, dbPath, `DELETE FROM sessions WHERE id = 'session-123'`)

	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path:              dbPath,
		EventKind:         "write",
		WatchRoot:         filepath.Join(root, "cli"),
		StoredSourcePaths: []string{virtualPath},
	})
	require.NoError(err)
	require.Len(changed, 1)

	fingerprint, err := provider.Fingerprint(t.Context(), changed[0])
	require.NoError(err)
	assert.Equal(SourceFingerprint{Key: virtualPath}, fingerprint)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: changed[0]})
	require.NoError(err)
	assert.True(outcome.ResultSetComplete)
	assert.True(outcome.ForceReplace)
	assert.Equal(SkipNoSession, outcome.SkipReason)
	assert.Empty(outcome.Results)
}

func TestDevinProviderHiddenRowFingerprintsTombstoneAndSkips(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const sessionID = "session-hidden"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "Title", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAt: new(int64(1704103199)), LastActivityAt: new(int64(1704103265)), Hidden: false}, `{"steps":[]}`)
	root := filepath.Dir(filepath.Dir(dbPath))
	virtualPath := VirtualSourcePath(dbPath, sessionID)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(err)
	require.True(ok)
	assert.Equal(virtualPath, source.DisplayPath)
	execDevinTestSQL(t, dbPath, `UPDATE sessions SET hidden = 1 WHERE id = 'session-hidden'`)

	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path:              dbPath,
		EventKind:         "write",
		WatchRoot:         filepath.Join(root, "cli"),
		StoredSourcePaths: []string{virtualPath},
	})
	require.NoError(err)
	require.Len(changed, 1)

	fingerprint, err := provider.Fingerprint(t.Context(), changed[0])
	require.NoError(err)
	assert.Equal(SourceFingerprint{Key: virtualPath}, fingerprint)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: changed[0]})
	require.NoError(err)
	assert.True(outcome.ResultSetComplete)
	assert.True(outcome.ForceReplace)
	assert.Equal(SkipNoSession, outcome.SkipReason)
	assert.Empty(outcome.Results)
}

func TestDevinProviderCorruptTranscriptReturnsProviderError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const sessionID = "session-corrupt"
	dbPath, transcriptPath := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "Corrupt transcript", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAt: new(int64(1704103199)), LastActivityAt: new(int64(1704103265))}, `{"steps":[]}`)
	root := filepath.Dir(filepath.Dir(dbPath))
	require.NoError(os.WriteFile(transcriptPath, []byte(`{"secret":"token-123","steps":[`), 0o644))

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(err)
	require.True(ok)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	assert.Empty(outcome.Results)
	assert.Empty(outcome.SourceErrors)
	require.Error(err)
	assert.ErrorContains(err, "invalid devin transcript")
	assert.NotContains(err.Error(), transcriptPath)
	assert.NotContains(err.Error(), sessionID)
	assert.NotContains(err.Error(), "token-123")
}

func TestDevinProviderIgnoresCredentialPathsAndRedactsSecretBearingErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const (
		sessionID      = "session-privacy"
		secretSentinel = "oauth-token-SYNTHETIC-SECRET-SENTINEL"
	)
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: sessionID, Title: "Privacy", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAt: new(int64(1704103199)), LastActivityAt: new(int64(1704103265))},
	)
	transcriptPath := fixture.writeTranscript(t, sessionID, `{"api_key":"oauth-token-SYNTHETIC-SECRET-SENTINEL","steps":[`)

	secretRoot := filepath.Join(t.TempDir(), secretSentinel, "config", "mcp", "oauth", "devin-root")
	require.NoError(os.MkdirAll(filepath.Dir(secretRoot), 0o755))
	require.NoError(os.Rename(fixture.Root, secretRoot))

	dbPath := filepath.Join(secretRoot, "cli", devinDBFilename)
	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{secretRoot}})
	require.True(ok)

	for _, req := range []ChangedPathRequest{
		{
			Path:      filepath.Join(secretRoot, "cli", "config.json"),
			EventKind: "write",
			WatchRoot: filepath.Join(secretRoot, "cli"),
		},
		{
			Path:      filepath.Join(secretRoot, "cli", "mcp", "oauth", "credentials.db"),
			EventKind: "write",
			WatchRoot: filepath.Join(secretRoot, "cli"),
		},
		{
			Path:      filepath.Join(secretRoot, "cli", "mcp", "oauth", "token-cache.json"),
			EventKind: "write",
			WatchRoot: filepath.Join(secretRoot, "cli"),
		},
	} {
		changed, err := provider.SourcesForChangedPath(t.Context(), req)
		require.NoError(err)
		assert.Empty(changed, "%+v", req)
	}

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(err)
	require.True(ok)
	assert.Equal(VirtualSourcePath(dbPath, sessionID), source.DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	assert.Empty(outcome.Results)
	assert.Empty(outcome.SourceErrors)
	require.Error(err)
	assert.ErrorContains(err, "invalid devin transcript")
	assertDevinErrorRedacted(t, err,
		secretSentinel,
		"mcp/oauth",
		"config",
		"api_key",
		transcriptPath,
	)
}

func TestDevinProviderMissingDBSkipsAndPreservesSessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const sessionID = "session-123"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "Title", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAt: new(int64(1704103199)), LastActivityAt: new(int64(1704103265))}, `{"steps":[]}`)
	root := filepath.Dir(filepath.Dir(dbPath))
	virtualPath := VirtualSourcePath(dbPath, sessionID)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{StoredFilePath: virtualPath})
	require.NoError(err)
	require.True(ok)

	require.NoError(os.Remove(dbPath))
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path:              dbPath,
		EventKind:         "remove",
		WatchRoot:         filepath.Join(root, "cli"),
		StoredSourcePaths: []string{virtualPath},
	})
	require.NoError(err)
	require.Len(changed, 1)

	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.Equal(SourceFingerprint{Key: virtualPath}, fingerprint)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: changed[0]})
	require.NoError(err)
	assert.True(outcome.ResultSetComplete)
	assert.False(outcome.ForceReplace)
	assert.Equal(SkipNoSession, outcome.SkipReason)
	assert.Empty(outcome.Results)
}
