package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
	const sessionID = "session-123"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{
		ID:                 sessionID,
		Title:              "DB title wins",
		WorkingDirectory:   "/Users/alice/code/my-app",
		Model:              "db-model",
		CreatedAtMillis:    new(int64(1704103199000)),
		LastActivityMillis: new(int64(1704103265000)),
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
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 2)
	assert.Equal(t, filepath.Join(root, "cli"), plan.Roots[0].Path)
	assert.False(t, plan.Roots[0].Recursive)
	assert.ElementsMatch(t,
		[]string{devinDBFilename, devinDBFilename + "-*"},
		plan.Roots[0].IncludeGlobs,
	)
	assert.Equal(t, filepath.Join(root, "cli", "transcripts"), plan.Roots[1].Path)
	assert.False(t, plan.Roots[1].Recursive)
	assert.Equal(t, []string{"*.json"}, plan.Roots[1].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, virtualPath, discovered[0].Key)
	assert.Equal(t, virtualPath, discovered[0].DisplayPath)
	assert.Equal(t, virtualPath, discovered[0].FingerprintKey)
	assert.Equal(t, int64(1704103265000000000), discovered[0].DiscoveryMTimeNS)

	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path:      filepath.Join(root, "cli", "transcripts", sessionID+".json"),
		EventKind: "write",
		WatchRoot: filepath.Join(root, "cli", "transcripts"),
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, virtualPath, changed[0].DisplayPath)

	fullIDSource, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~devin:" + sessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtualPath, fullIDSource.DisplayPath)

	storedSource, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: virtualPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtualPath, storedSource.DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), storedSource)
	require.NoError(t, err)
	assert.Equal(t, virtualPath, fingerprint.Key)
	assert.NotZero(t, fingerprint.MTimeNS)

	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: storedSource})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.True(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "devin:"+sessionID, result.Result.Session.ID)
	assert.Equal(t, virtualPath, result.Result.Session.File.Path)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Len(t, result.Result.Messages, 2)
}

func TestDevinProviderDBEventsFanOutAndPreserveTombstones(t *testing.T) {
	const liveSessionID = "session-live"
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: liveSessionID, Title: "Live", WorkingDirectory: "/tmp/live", Model: "db-model", CreatedAtMillis: new(int64(1704103199000)), LastActivityMillis: new(int64(1704103265000))},
		devinSessionRow{ID: "session-deleted", Title: "Deleted", WorkingDirectory: "/tmp/deleted", Model: "db-model", CreatedAtMillis: new(int64(1704103199000)), LastActivityMillis: new(int64(1704103264000))},
	)
	fixture.writeTranscript(t, liveSessionID, `{"steps":[]}`)
	root := fixture.Root
	liveVirtualPath := fixture.sessionVirtualPath(liveSessionID)
	deletedVirtualPath := fixture.sessionVirtualPath("session-deleted")
	dbPath := fixture.DBPath
	execDevinTestSQL(t, dbPath, `DELETE FROM sessions WHERE id = 'session-deleted'`)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	for _, changedPath := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
			Path:              changedPath,
			EventKind:         "write",
			WatchRoot:         filepath.Join(root, "cli"),
			StoredSourcePaths: []string{deletedVirtualPath},
		})
		require.NoError(t, err)
		require.Len(t, changed, 2, changedPath)
		assert.ElementsMatch(t,
			[]string{liveVirtualPath, deletedVirtualPath},
			[]string{changed[0].DisplayPath, changed[1].DisplayPath},
		)
	}
}

func TestDevinProviderTranscriptEventsTargetLiveOrStoredSession(t *testing.T) {
	const liveSessionID = "session-live"
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: liveSessionID, Title: "Live", WorkingDirectory: "/tmp/live", Model: "db-model", CreatedAtMillis: new(int64(1704103199000)), LastActivityMillis: new(int64(1704103265000))},
		devinSessionRow{ID: "session-deleted", Title: "Deleted", WorkingDirectory: "/tmp/deleted", Model: "db-model", CreatedAtMillis: new(int64(1704103199000)), LastActivityMillis: new(int64(1704103264000))},
	)
	fixture.writeTranscript(t, liveSessionID, `{"steps":[]}`)
	root := fixture.Root
	liveVirtualPath := fixture.sessionVirtualPath(liveSessionID)
	deletedVirtualPath := fixture.sessionVirtualPath("session-deleted")
	dbPath := fixture.DBPath
	execDevinTestSQL(t, dbPath, `DELETE FROM sessions WHERE id = 'session-deleted'`)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	liveChanged, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path:      filepath.Join(root, "cli", "transcripts", liveSessionID+".json"),
		EventKind: "write",
		WatchRoot: filepath.Join(root, "cli", "transcripts"),
	})
	require.NoError(t, err)
	require.Len(t, liveChanged, 1)
	assert.Equal(t, liveVirtualPath, liveChanged[0].DisplayPath)

	deletedChanged, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path:              filepath.Join(root, "cli", "transcripts", "session-deleted.json"),
		EventKind:         "write",
		WatchRoot:         filepath.Join(root, "cli", "transcripts"),
		StoredSourcePaths: []string{deletedVirtualPath},
	})
	require.NoError(t, err)
	require.Len(t, deletedChanged, 1)
	assert.Equal(t, deletedVirtualPath, deletedChanged[0].DisplayPath)
}

func TestDevinProviderRejectsUnrelatedChangedPaths(t *testing.T) {
	const sessionID = "session-123"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "Title", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAtMillis: new(int64(1704103199000)), LastActivityMillis: new(int64(1704103265000))}, `{"steps":[]}`)
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
		changed, err := provider.SourcesForChangedPath(context.Background(), req)
		require.NoError(t, err)
		assert.Empty(t, changed, "%+v", req)
	}
}

func TestDevinProviderMissingTranscriptUsesMessageNodeFallback(t *testing.T) {
	const sessionID = "session-db-only"
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: sessionID, Title: "DB only session", WorkingDirectory: "/tmp/db-only-project", Model: "db-only-model", CreatedAtMillis: new(int64(1704103200000)), LastActivityMillis: new(int64(1704103209000))},
	)
	root := fixture.Root
	fixture.insertMessageNodes(t,
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 1, ChatMessage: `{"role":"user","content":"fallback user"}`, CreatedAtMillis: 1704103201000},
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 2, ChatMessage: `{"role":"assistant","content":"fallback assistant"}`, CreatedAtMillis: 1704103205000},
	)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(t, err)
	require.True(t, ok)
	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: source})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	assert.True(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	assert.Empty(t, outcome.SourceErrors)
	assert.Equal(t, "devin:"+sessionID, outcome.Results[0].Result.Session.ID)
	assert.Len(t, outcome.Results[0].Result.Messages, 2)
	assert.Equal(t, "fallback user", outcome.Results[0].Result.Messages[0].Content)
}

func TestDevinProviderMissingTranscriptWithoutDBMessagesReturnsProviderError(t *testing.T) {
	const sessionID = "session-db-only-empty"
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: sessionID, Title: "DB only session", WorkingDirectory: "/tmp/db-only-project", Model: "db-only-model", CreatedAtMillis: new(int64(1704103200000)), LastActivityMillis: new(int64(1704103209000))},
	)
	root := fixture.Root

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(t, err)
	require.True(t, ok)
	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: source})
	require.Error(t, err)
	assert.Empty(t, outcome.Results)
	assert.Empty(t, outcome.SourceErrors)
	assert.ErrorContains(t, err, "missing devin transcript")
	assert.NotContains(t, err.Error(), source.DisplayPath)
	assert.NotContains(t, err.Error(), sessionID)
}

func TestDevinProviderCompositeFingerprintStableAndRedacted(t *testing.T) {
	const sessionID = "session-fingerprint"
	dbPath, transcriptPath := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "Stable title", WorkingDirectory: "/Users/alice/.config/devin/project", Model: "db-model", CreatedAtMillis: new(int64(1704103200000)), LastActivityMillis: new(int64(1704103209000)), MetadataJSON: `{"token_hint":"redacted"}`}, `{"token":"secret-token-123","steps":[{"step_id":"step-1","source":"user","message":"hello"}]}`)
	root := filepath.Dir(filepath.Dir(dbPath))
	virtualPath := VirtualSourcePath(dbPath, sessionID)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(t, err)
	require.True(t, ok)

	first, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	second, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.Equal(t, virtualPath, first.Key)
	assert.NotZero(t, first.MTimeNS)
	assert.NotEmpty(t, first.Hash)
	assert.NotContains(t, first.Hash, "secret-token-123")
	assert.NotContains(t, first.Hash, "/Users/alice/.config/devin/project")
	assert.NotContains(t, first.Key, transcriptPath)
}

func TestDevinProviderFingerprintChangesWhenTranscriptChangesWithoutDBMetadataChange(t *testing.T) {
	const sessionID = "session-transcript-change"
	dbPath, transcriptPath := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "Transcript change", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAtMillis: new(int64(1704103200000)), LastActivityMillis: new(int64(1704103209000))}, `{"steps":[{"step_id":"step-1","source":"user","message":"alpha"}]}`)
	root := filepath.Dir(filepath.Dir(dbPath))

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(t, err)
	require.True(t, ok)

	before, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	transcriptInfo, err := os.Stat(transcriptPath)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"steps":[{"step_id":"step-1","source":"user","message":"omega"}]}`), 0o644))
	require.NoError(t, os.Chtimes(transcriptPath, transcriptInfo.ModTime(), transcriptInfo.ModTime()))

	after, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	assert.Equal(t, before.Key, after.Key)
	assert.Equal(t, before.MTimeNS, after.MTimeNS)
	assert.NotEqual(t, before.Hash, after.Hash)
}

func TestDevinProviderFingerprintChangesWhenLastActivityChanges(t *testing.T) {
	const sessionID = "session-last-activity"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "DB change", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAtMillis: new(int64(1704103200000)), LastActivityMillis: new(int64(1704103209000))}, `{"steps":[]}`)
	root := filepath.Dir(filepath.Dir(dbPath))

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(t, err)
	require.True(t, ok)

	before, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	execDevinTestSQL(t, dbPath, `UPDATE sessions SET last_activity_at = 1704103215000 WHERE id = 'session-last-activity'`)

	after, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	assert.Equal(t, before.Key, after.Key)
	assert.Greater(t, after.MTimeNS, before.MTimeNS)
	assert.NotEqual(t, before.Hash, after.Hash)
}

func TestDevinProviderFingerprintChangesWhenWorkingDirectoryChanges(t *testing.T) {
	const sessionID = "session-cwd-change"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "CWD change", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAtMillis: new(int64(1704103200000)), LastActivityMillis: new(int64(1704103209000))}, `{"steps":[]}`)
	root := filepath.Dir(filepath.Dir(dbPath))

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(t, err)
	require.True(t, ok)

	before, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	execDevinTestSQL(t, dbPath, `UPDATE sessions SET working_directory = '/tmp/renamed-app' WHERE id = 'session-cwd-change'`)

	after, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	assert.Equal(t, before.Key, after.Key)
	assert.NotEqual(t, before.Hash, after.Hash)
}

func TestDevinProviderFingerprintWithoutTranscriptUsesDBFreshnessOnly(t *testing.T) {
	const sessionID = "session-missing-transcript"
	dbPath, transcriptPath := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "DB only session", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAtMillis: new(int64(1704103200000)), LastActivityMillis: new(int64(1704103209000))}, `{"steps":[]}`)
	root := filepath.Dir(filepath.Dir(dbPath))
	virtualPath := VirtualSourcePath(dbPath, sessionID)
	require.NoError(t, os.Remove(transcriptPath))

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(t, err)
	require.True(t, ok)

	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	assert.Equal(t, virtualPath, fingerprint.Key)
	assert.NotZero(t, fingerprint.MTimeNS)
	assert.Zero(t, fingerprint.Size)
	assert.NotEmpty(t, fingerprint.Hash)
	assert.NotContains(t, fingerprint.Hash, transcriptPath)
}

func TestDevinProviderFingerprintWithoutTranscriptChangesWhenMessageNodesChange(t *testing.T) {
	const sessionID = "session-message-node-change"
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: sessionID, Title: "DB messages", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAtMillis: new(int64(1704103200000)), LastActivityMillis: new(int64(1704103209000))},
	)
	fixture.insertMessageNodes(t,
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 1, ChatMessage: `{"role":"user","content":"alpha"}`, CreatedAtMillis: 1704103201000},
	)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{fixture.Root}})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(t, err)
	require.True(t, ok)

	before, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	execDevinTestSQL(t, fixture.DBPath, `UPDATE message_nodes SET chat_message = '{"role":"user","content":"omega"}' WHERE session_id = 'session-message-node-change'`)

	after, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	assert.Equal(t, before.Key, after.Key)
	assert.NotEqual(t, before.Hash, after.Hash)
}

func TestDevinProviderRejectsInvalidStoredVirtualPaths(t *testing.T) {
	const sessionID = "session-123"
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: sessionID, Title: "Title", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAtMillis: new(int64(1704103199000)), LastActivityMillis: new(int64(1704103265000))},
		devinSessionRow{ID: "session-999", Title: "Other", WorkingDirectory: "/tmp/other", Model: "db-model", CreatedAtMillis: new(int64(1704103199000)), LastActivityMillis: new(int64(1704103265000))},
	)
	fixture.writeTranscript(t, sessionID, `{"steps":[]}`)
	root := fixture.Root
	virtualPath := fixture.sessionVirtualPath(sessionID)
	otherPath := fixture.sessionVirtualPath("session-999")
	dbPath := fixture.DBPath

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	for _, path := range []string{
		dbPath + "#",
		filepath.Join(root, "cli", "other.db") + "#" + sessionID,
		filepath.Join(root, devinDBFilename) + "#" + sessionID,
		filepath.Join(root, "cli", "nested", devinDBFilename) + "#" + sessionID,
	} {
		_, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
			StoredFilePath:     path,
			RequireFreshSource: true,
		})
		require.NoError(t, err)
		assert.False(t, ok, "stored path %q", path)
	}

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID:       sessionID,
		StoredFilePath:     otherPath,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtualPath, source.DisplayPath)
}

func TestDevinProviderDedupesDuplicateRoots(t *testing.T) {
	const sessionID = "session-123"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "Title", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAtMillis: new(int64(1704103199000)), LastActivityMillis: new(int64(1704103265000))}, `{"steps":[]}`)
	root := filepath.Dir(filepath.Dir(dbPath))

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root, root, filepath.Join(root, ".")}})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
}

func TestDevinProviderDeletedRowFingerprintsTombstoneAndSkips(t *testing.T) {
	const sessionID = "session-123"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "Title", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAtMillis: new(int64(1704103199000)), LastActivityMillis: new(int64(1704103265000))}, `{"steps":[]}`)
	root := filepath.Dir(filepath.Dir(dbPath))
	virtualPath := VirtualSourcePath(dbPath, sessionID)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtualPath, source.DisplayPath)
	execDevinTestSQL(t, dbPath, `DELETE FROM sessions WHERE id = 'session-123'`)

	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path:              dbPath,
		EventKind:         "write",
		WatchRoot:         filepath.Join(root, "cli"),
		StoredSourcePaths: []string{virtualPath},
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)

	fingerprint, err := provider.Fingerprint(context.Background(), changed[0])
	require.NoError(t, err)
	assert.Equal(t, SourceFingerprint{Key: virtualPath}, fingerprint)

	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: changed[0]})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	assert.True(t, outcome.ForceReplace)
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
	assert.Empty(t, outcome.Results)
}

func TestDevinProviderHiddenRowFingerprintsTombstoneAndSkips(t *testing.T) {
	const sessionID = "session-hidden"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "Title", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAtMillis: new(int64(1704103199000)), LastActivityMillis: new(int64(1704103265000)), Hidden: false}, `{"steps":[]}`)
	root := filepath.Dir(filepath.Dir(dbPath))
	virtualPath := VirtualSourcePath(dbPath, sessionID)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtualPath, source.DisplayPath)
	execDevinTestSQL(t, dbPath, `UPDATE sessions SET hidden = 1 WHERE id = 'session-hidden'`)

	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path:              dbPath,
		EventKind:         "write",
		WatchRoot:         filepath.Join(root, "cli"),
		StoredSourcePaths: []string{virtualPath},
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)

	fingerprint, err := provider.Fingerprint(context.Background(), changed[0])
	require.NoError(t, err)
	assert.Equal(t, SourceFingerprint{Key: virtualPath}, fingerprint)

	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: changed[0]})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	assert.True(t, outcome.ForceReplace)
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
	assert.Empty(t, outcome.Results)
}

func TestDevinProviderCorruptTranscriptReturnsProviderError(t *testing.T) {
	const sessionID = "session-corrupt"
	dbPath, transcriptPath := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "Corrupt transcript", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAtMillis: new(int64(1704103199000)), LastActivityMillis: new(int64(1704103265000))}, `{"steps":[]}`)
	root := filepath.Dir(filepath.Dir(dbPath))
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"secret":"token-123","steps":[`), 0o644))

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(t, err)
	require.True(t, ok)

	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: source})
	assert.Empty(t, outcome.Results)
	assert.Empty(t, outcome.SourceErrors)
	require.Error(t, err)
	assert.ErrorContains(t, err, "invalid devin transcript")
	assert.NotContains(t, err.Error(), transcriptPath)
	assert.NotContains(t, err.Error(), sessionID)
	assert.NotContains(t, err.Error(), "token-123")
}

func TestDevinProviderIgnoresCredentialPathsAndRedactsSecretBearingErrors(t *testing.T) {
	const (
		sessionID      = "session-privacy"
		secretSentinel = "oauth-token-SYNTHETIC-SECRET-SENTINEL"
	)
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: sessionID, Title: "Privacy", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAtMillis: new(int64(1704103199000)), LastActivityMillis: new(int64(1704103265000))},
	)
	transcriptPath := fixture.writeTranscript(t, sessionID, `{"api_key":"oauth-token-SYNTHETIC-SECRET-SENTINEL","steps":[`)

	secretRoot := filepath.Join(t.TempDir(), secretSentinel, "config", "mcp", "oauth", "devin-root")
	require.NoError(t, os.MkdirAll(filepath.Dir(secretRoot), 0o755))
	require.NoError(t, os.Rename(fixture.Root, secretRoot))

	dbPath := filepath.Join(secretRoot, "cli", devinDBFilename)
	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{secretRoot}})
	require.True(t, ok)

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
		changed, err := provider.SourcesForChangedPath(context.Background(), req)
		require.NoError(t, err)
		assert.Empty(t, changed, "%+v", req)
	}

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: sessionID})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, VirtualSourcePath(dbPath, sessionID), source.DisplayPath)

	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: source})
	assert.Empty(t, outcome.Results)
	assert.Empty(t, outcome.SourceErrors)
	require.Error(t, err)
	assert.ErrorContains(t, err, "invalid devin transcript")
	assertDevinErrorRedacted(t, err,
		secretSentinel,
		"mcp/oauth",
		"config",
		"api_key",
		transcriptPath,
	)
}

func TestDevinProviderMissingDBSkipsAndPreservesSessions(t *testing.T) {
	const sessionID = "session-123"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{ID: sessionID, Title: "Title", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAtMillis: new(int64(1704103199000)), LastActivityMillis: new(int64(1704103265000))}, `{"steps":[]}`)
	root := filepath.Dir(filepath.Dir(dbPath))
	virtualPath := VirtualSourcePath(dbPath, sessionID)

	provider, ok := NewProvider(AgentDevin, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{StoredFilePath: virtualPath})
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, os.Remove(dbPath))
	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path:              dbPath,
		EventKind:         "remove",
		WatchRoot:         filepath.Join(root, "cli"),
		StoredSourcePaths: []string{virtualPath},
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)

	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, SourceFingerprint{Key: virtualPath}, fingerprint)

	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: changed[0]})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	assert.False(t, outcome.ForceReplace)
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
	assert.Empty(t, outcome.Results)
}
