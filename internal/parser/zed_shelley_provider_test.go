package parser

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestZedProviderCapabilities(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	factory, ok := ProviderFactoryByType(AgentZed)
	require.True(ok)
	require.NotNil(factory)
	caps := factory.Capabilities()
	assert.Equal(CapabilityUnsupported, caps.Content.Relationships)
	assert.Equal(CapabilitySupported, caps.Content.AggregateUsageEvents)

	provider, ok := NewProvider(AgentZed, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(ok)
	require.NotNil(provider)
}

func TestZedProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := zedProviderReadFixture(t)
	root := fixture.Root
	dbPath := fixture.DBPath
	threadID := fixture.FirstThreadID
	virtualPath := ZedSQLiteVirtualPath(dbPath, threadID)

	provider, ok := NewProvider(AgentZed, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 1)
	assert.Equal(filepath.Join(root, "threads"), plan.Roots[0].Path)
	assert.False(plan.Roots[0].Recursive)
	assert.Equal([]string{"threads.db", "threads.db-*"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(AgentZed, discovered[0].Provider)
	assert.Equal(dbPath, discovered[0].DisplayPath)
	assert.Equal(dbPath, discovered[0].FingerprintKey)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~zed:" + threadID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(virtualPath, found.DisplayPath)
	assert.Equal(virtualPath, found.FingerprintKey)

	fingerprint, err := provider.Fingerprint(t.Context(), found)
	require.NoError(err)
	assert.Equal(virtualPath, fingerprint.Key)
	assert.Positive(fingerprint.Size)
	assert.NotZero(fingerprint.MTimeNS)
	assert.NotEmpty(fingerprint.Hash)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: dbPath + "-wal", EventKind: "write", WatchRoot: filepath.Dir(dbPath)},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(dbPath, changed[0].DisplayPath)
}

func TestZedProviderParsePhysicalAndVirtualSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := zedProviderReadFixture(t)
	root := fixture.Root
	threadOne := fixture.FirstThreadID
	threadTwo := fixture.SecondThreadID

	provider, ok := NewProvider(AgentZed, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	allOutcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: sources[0],
	})
	require.NoError(err)
	require.True(allOutcome.ResultSetComplete)
	require.True(allOutcome.ForceReplace)
	require.Len(allOutcome.Results, 2)
	assert.Equal("zed:"+threadOne, allOutcome.Results[0].Result.Session.ID)
	assert.Equal("zed:"+threadTwo, allOutcome.Results[1].Result.Session.ID)

	virtualSource, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: threadTwo,
	})
	require.NoError(err)
	require.True(ok)
	oneOutcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: virtualSource,
	})
	require.NoError(err)
	require.True(oneOutcome.ResultSetComplete)
	require.True(oneOutcome.ForceReplace)
	require.Len(oneOutcome.Results, 1)
	assert.Equal("zed:"+threadTwo, oneOutcome.Results[0].Result.Session.ID)
	assert.Equal("devbox", oneOutcome.Results[0].Result.Session.Machine)
	assert.Len(oneOutcome.Results[0].Result.Messages, 1)
}

func TestZedProviderLegacySchemaPhysicalAndVirtual(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	dbPath := filepath.Join(root, zedThreadsDBRelPath)
	require.NoError(os.MkdirAll(filepath.Dir(dbPath), 0o755))
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	artifact, err := os.ReadFile("testdata/zed-legacy-threads.sql")
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), string(artifact))
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO threads (id, summary, updated_at, data_type, data) VALUES (?, ?, ?, ?, ?)`,
		"legacy", "Legacy", "2026-06-08T09:14:10Z", "json", []byte(`{"messages":[{"User":{"content":[{"Text":"hello"}]}}]}`))
	require.NoError(err)
	require.NoError(db.Close())

	provider, ok := NewProvider(AgentZed, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	physical, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(err)
	require.Len(physical.Results, 1)
	assert.Equal("zed:legacy", physical.Results[0].Result.Session.ID)

	virtual, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "legacy"})
	require.NoError(err)
	require.True(ok)
	fingerprint, err := provider.Fingerprint(t.Context(), virtual)
	require.NoError(err)
	parsed, err := provider.Parse(t.Context(), ParseRequest{Source: virtual, Fingerprint: fingerprint})
	require.NoError(err)
	require.Len(parsed.Results, 1)
	assert.Equal("zed:legacy", parsed.Results[0].Result.Session.ID)
}

func TestZedProviderMalformedSchemaReturnsError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	dbPath := filepath.Join(root, zedThreadsDBRelPath)
	require.NoError(os.MkdirAll(filepath.Dir(dbPath), 0o755))
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `CREATE TABLE threads (
		id TEXT PRIMARY KEY,
		summary TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		data_type TEXT NOT NULL
	)`)
	require.NoError(err)
	require.NoError(db.Close())

	provider, ok := NewProvider(AgentZed, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	_, err = zedFingerprintSource(t.Context(), multiSessionSource{
		Root: root, Path: ZedSQLiteVirtualPath(dbPath, "malformed"),
		Container: dbPath, MemberID: "malformed",
	})
	require.Error(err)
	assert.Contains(err.Error(), "missing required Zed threads column data")
	_, err = provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.Error(err)
	assert.Contains(err.Error(), "missing required Zed threads column data")
}

func TestZedProviderPropagatesParseAndFingerprintContext(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	dbPath := filepath.Join(root, zedThreadsDBRelPath)
	threadID := "10431c84-c47b-4e6c-b2df-f9f3b9ad025b"
	require.NoError(os.MkdirAll(filepath.Dir(dbPath), 0o755))
	createZedThreadsDBAt(t, dbPath, []zedTestThread{{
		id: threadID, summary: "Context", updatedAt: "2026-06-08T09:14:10Z",
		dataType: "json", data: []byte(`{"messages":[]}`),
	}})
	src := multiSessionSource{
		Root: root, Path: ZedSQLiteVirtualPath(dbPath, threadID),
		Container: dbPath, MemberID: threadID,
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := zedParseContainer(ctx, multiSessionSource{
		Root: root, Path: dbPath, Container: dbPath,
	}, ParseRequest{})
	require.Error(err)
	assert.ErrorIs(err, context.Canceled)

	_, err = zedParseMember(ctx, src, ParseRequest{})
	require.Error(err)
	assert.ErrorIs(err, context.Canceled)

	_, err = zedFingerprintSource(ctx, src)
	require.Error(err)
	assert.True(errors.Is(err, context.Canceled), "fingerprint error = %v", err)
}

func TestZedProviderFingerprintIncludesWALSiblings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	dbPath := filepath.Join(root, zedThreadsDBRelPath)
	require.NoError(os.MkdirAll(filepath.Dir(dbPath), 0o755))
	createZedThreadsDBAt(t, dbPath, []zedTestThread{{
		id:        "10431c84-c47b-4e6c-b2df-f9f3b9ad025b",
		summary:   "Provider thread",
		updatedAt: "2026-06-08T09:14:10Z",
		dataType:  "json",
		data:      []byte(`{"messages":[{"User":{"content":[{"Text":"Hello Zed"}]}}]}`),
	}})

	provider, ok := NewProvider(AgentZed, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	before, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)

	walPath := dbPath + "-wal"
	writeSourceFile(t, walPath, "wal")
	walTime := time.Unix(0, before.MTimeNS+int64(time.Second))
	require.NoError(os.Chtimes(walPath, walTime, walTime))
	after, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)

	assert.Equal(before.Size, after.Size)
	assert.Greater(after.MTimeNS, before.MTimeNS)
}

func TestZedProviderClassifiesDeletedPhysicalDB(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	dbPath := filepath.Join(root, zedThreadsDBRelPath)
	require.NoError(os.MkdirAll(filepath.Dir(dbPath), 0o755))
	createZedThreadsDBAt(t, dbPath, []zedTestThread{{
		id:        "10431c84-c47b-4e6c-b2df-f9f3b9ad025b",
		summary:   "Provider thread",
		updatedAt: "2026-06-08T09:14:10Z",
		dataType:  "json",
		data:      []byte(`{"messages":[{"User":{"content":[{"Text":"Hello Zed"}]}}]}`),
	}})
	require.NoError(os.Remove(dbPath))

	provider, ok := NewProvider(AgentZed, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: dbPath, EventKind: "remove", WatchRoot: filepath.Dir(dbPath)},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(dbPath, changed[0].DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: changed[0]})
	require.NoError(err)
	assert.True(outcome.ResultSetComplete)
	// The backing DB file is gone, so the outcome must not force-replace: the
	// persistent archive preserves sessions whose source file no longer exists.
	assert.False(outcome.ForceReplace)
	assert.Equal(SkipNoSession, outcome.SkipReason)
	assert.Empty(outcome.Results)
}

func TestZedProviderStoredVirtualPathFreshness(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	dbPath := filepath.Join(root, zedThreadsDBRelPath)
	threadID := "10431c84-c47b-4e6c-b2df-f9f3b9ad025b"
	require.NoError(os.MkdirAll(filepath.Dir(dbPath), 0o755))
	createZedThreadsDBAt(t, dbPath, []zedTestThread{{
		id:        threadID,
		summary:   "Provider thread",
		updatedAt: "2026-06-08T09:14:10Z",
		dataType:  "json",
		data:      []byte(`{"messages":[{"User":{"content":[{"Text":"Hello Zed"}]}}]}`),
	}})
	virtualPath := ZedSQLiteVirtualPath(dbPath, threadID)

	provider, ok := NewProvider(AgentZed, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath:     virtualPath,
		RequireFreshSource: true,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(virtualPath, found.DisplayPath)

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `DELETE FROM threads WHERE id = ?`, threadID)
	require.NoError(err)
	require.NoError(db.Close())

	_, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath:     virtualPath,
		RequireFreshSource: true,
	})
	require.NoError(err)
	assert.False(ok, "fresh lookup must reject a deleted virtual row")

	staleSource, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: virtualPath,
	})
	require.NoError(err)
	require.True(ok, "non-fresh lookup keeps tombstone source identity")
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: staleSource,
	})
	require.NoError(err)
	assert.True(outcome.ResultSetComplete)
	assert.True(outcome.ForceReplace)
	assert.Equal(SkipNoSession, outcome.SkipReason)
	assert.Empty(outcome.Results)
}

// TestZedProviderChangedPathTombstonesDeletedThread verifies the changed-path
// classifier emits a tombstone for a stored Zed thread deleted from a
// still-present database, so the engine force-replaces it out of the archive.
// The surviving thread is left to the whole-DB fan-out; a vanished database
// emits no tombstone (stored sessions preserved per the persistent-archive
// rule).
func TestZedProviderChangedPathTombstonesDeletedThread(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	dbPath := filepath.Join(root, zedThreadsDBRelPath)
	threadsDir := filepath.Join(root, "threads")
	survivingID := "10431c84-c47b-4e6c-b2df-f9f3b9ad025b"
	deletedID := "20431c84-c47b-4e6c-b2df-f9f3b9ad025b"
	require.NoError(os.MkdirAll(filepath.Dir(dbPath), 0o755))
	createZedThreadsDBAt(t, dbPath, []zedTestThread{
		{
			id: survivingID, summary: "Surviving thread",
			updatedAt: "2026-06-08T09:14:10Z", dataType: "json",
			data: []byte(`{"messages":[{"User":{"content":[{"Text":"Hello"}]}}]}`),
		},
		{
			id: deletedID, summary: "Doomed thread",
			updatedAt: "2026-06-08T09:15:10Z", dataType: "json",
			data: []byte(`{"messages":[{"User":{"content":[{"Text":"Bye"}]}}]}`),
		},
	})

	provider, ok := NewProvider(AgentZed, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	survivingPath := ZedSQLiteVirtualPath(dbPath, survivingID)
	deletedPath := ZedSQLiteVirtualPath(dbPath, deletedID)

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `DELETE FROM threads WHERE id = ?`, deletedID)
	require.NoError(err)
	require.NoError(db.Close())

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:              dbPath,
			EventKind:         "write",
			WatchRoot:         threadsDir,
			StoredSourcePaths: []string{survivingPath, deletedPath},
		},
	)
	require.NoError(err)
	gotPaths := make([]string, len(changed))
	for i, src := range changed {
		gotPaths[i] = src.DisplayPath
	}
	assert.ElementsMatch([]string{dbPath, deletedPath}, gotPaths,
		"whole-DB source plus a tombstone for the deleted thread only")

	var tombstone SourceRef
	for _, src := range changed {
		if src.DisplayPath == deletedPath {
			tombstone = src
		}
	}
	require.NotEmpty(tombstone.DisplayPath, "deleted-thread tombstone source")

	// The fingerprint of a deleted-but-present-DB thread must be keyed-empty:
	// it must not error (or the engine aborts before Parse) and must not carry
	// the physical DB size/mtime/hash. A DB-level fingerprint here lets the
	// engine's pre-parse freshness check skip Parse whenever stored metadata
	// happens to match, stranding the deleted thread. This mirrors Shelley and
	// Kiro tombstone fingerprinting.
	fingerprint, err := provider.Fingerprint(t.Context(), tombstone)
	require.NoError(err, "missing-thread fingerprint must not error")
	assert.Equal(tombstone.FingerprintKey, fingerprint.Key)
	assert.Zero(fingerprint.Size,
		"deleted-thread fingerprint must not carry the DB size")
	assert.Zero(fingerprint.MTimeNS,
		"deleted-thread fingerprint must not carry the DB mtime")
	assert.Empty(fingerprint.Hash,
		"deleted-thread fingerprint must not carry the DB hash")

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      tombstone,
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	assert.True(outcome.ResultSetComplete)
	assert.True(outcome.ForceReplace,
		"a thread deleted from a present DB is force-replaced out of the archive")
	assert.Equal(SkipNoSession, outcome.SkipReason)
	assert.Empty(outcome.Results)

	require.NoError(os.Remove(dbPath))
	gone, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:              dbPath,
			EventKind:         "remove",
			WatchRoot:         threadsDir,
			StoredSourcePaths: []string{survivingPath, deletedPath},
		},
	)
	require.NoError(err)
	for _, src := range gone {
		assert.NotEqual(deletedPath, src.DisplayPath,
			"a vanished database must not tombstone stored sessions")
		assert.NotEqual(survivingPath, src.DisplayPath)
	}
}

func TestZedProviderRejectsInvalidStoredVirtualPaths(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	dbPath := filepath.Join(root, zedThreadsDBRelPath)
	threadID := "10431c84-c47b-4e6c-b2df-f9f3b9ad025b"
	require.NoError(os.MkdirAll(filepath.Dir(dbPath), 0o755))
	createZedThreadsDBAt(t, dbPath, []zedTestThread{{
		id:        threadID,
		summary:   "Provider thread",
		updatedAt: "2026-06-08T09:14:10Z",
		dataType:  "json",
		data:      []byte(`{"messages":[{"User":{"content":[{"Text":"Hello Zed"}]}}]}`),
	}})

	provider, ok := NewProvider(AgentZed, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	for _, path := range []string{
		dbPath + "#",
		filepath.Join(root, "threads", "threads-copy.db") + "#" + threadID,
		filepath.Join(root, "debug", "threads.db") + "#" + threadID,
	} {
		_, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
			StoredFilePath:     path,
			RequireFreshSource: true,
		})
		require.NoError(err)
		assert.False(t, ok, "stored path %q", path)
	}
}

func TestZedProviderIgnoresUnrelatedSidecarBasename(t *testing.T) {
	root := t.TempDir()
	provider, ok := NewProvider(AgentZed, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "other", "threads.db-wal"),
			EventKind: "remove",
			WatchRoot: filepath.Join(root, "other"),
		},
	)
	require.NoError(t, err)
	assert.Empty(t, changed)
}

func TestShelleyProviderCapabilities(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	factory, ok := ProviderFactoryByType(AgentShelley)
	require.True(ok)
	require.NotNil(factory)
	caps := factory.Capabilities()
	assert.Equal(CapabilitySupported, caps.Content.Relationships)
	assert.Equal(CapabilityUnsupported, caps.Content.AggregateUsageEvents)

	provider, ok := NewProvider(AgentShelley, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(ok)
	require.NotNil(provider)
}

func TestShelleyProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := shelleyProviderReadFixture(t)
	root := fixture.Root
	dbPath := fixture.DBPath
	virtualPath := ShelleyVirtualPath(dbPath, "cMAIN1")

	provider, ok := NewProvider(AgentShelley, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 1)
	assert.Equal(root, plan.Roots[0].Path)
	assert.False(plan.Roots[0].Recursive)
	assert.Equal([]string{shelleyDBName, shelleyDBName + "-*"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(AgentShelley, discovered[0].Provider)
	assert.Equal(dbPath, discovered[0].DisplayPath)
	assert.Equal(dbPath, discovered[0].FingerprintKey)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~shelley:cMAIN1",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(virtualPath, found.DisplayPath)
	assert.Equal(virtualPath, found.FingerprintKey)

	fingerprint, err := provider.Fingerprint(t.Context(), found)
	require.NoError(err)
	assert.Equal(virtualPath, fingerprint.Key)
	assert.Positive(fingerprint.Size)
	assert.NotZero(fingerprint.MTimeNS)
	assert.NotEmpty(fingerprint.Hash)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: dbPath + "-wal", EventKind: "write", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(dbPath, changed[0].DisplayPath)
}

func TestShelleyProviderParsePhysicalAndVirtualSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := shelleyProviderReadFixture(t)
	root := fixture.Root
	dbPath := fixture.DBPath

	provider, ok := NewProvider(AgentShelley, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	allOutcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: sources[0],
	})
	require.NoError(err)
	require.True(allOutcome.ResultSetComplete)
	require.True(allOutcome.ForceReplace)
	require.Len(allOutcome.Results, 2)
	assert.Equal("shelley:cAUX1", allOutcome.Results[0].Result.Session.ID)
	assert.Equal("shelley:cMAIN1", allOutcome.Results[1].Result.Session.ID)

	virtualSource, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: ShelleyVirtualPath(dbPath, "cMAIN1"),
	})
	require.NoError(err)
	require.True(ok)
	oneOutcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: virtualSource,
	})
	require.NoError(err)
	require.True(oneOutcome.ResultSetComplete)
	require.True(oneOutcome.ForceReplace)
	require.Len(oneOutcome.Results, 1)
	assert.Equal("shelley:cMAIN1", oneOutcome.Results[0].Result.Session.ID)
	assert.Equal("devbox", oneOutcome.Results[0].Result.Session.Machine)
	assert.Len(oneOutcome.Results[0].Result.Messages, 5)
}

func TestShelleyProviderFingerprintChangesForSameSecondRewrite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root, _, db := newShelleyTestDB(t)
	seedShelleyMainConversation(t, db)

	provider, ok := NewProvider(AgentShelley, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "cMAIN1",
	})
	require.NoError(err)
	require.True(ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	_, err = db.ExecContext(t.Context(),
		`UPDATE messages
		    SET llm_data = ?
		  WHERE conversation_id = ? AND sequence_id = ?`,
		`{"Role":1,"Content":[{"Type":2,"Text":"Changed content."}]}`,
		"cMAIN1",
		4,
	)
	require.NoError(err)
	after, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	assert.Equal(before.MTimeNS, after.MTimeNS)
	assert.NotEqual(before.Hash, after.Hash)
}

func TestShelleyProviderFingerprintIncludesWALSiblings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root, dbPath, db := newShelleyTestDB(t)
	seedShelleyMainConversation(t, db)

	provider, ok := NewProvider(AgentShelley, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	before, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)

	walPath := dbPath + "-wal"
	writeSourceFile(t, walPath, "wal")
	walTime := time.Unix(0, before.MTimeNS+int64(time.Second))
	require.NoError(os.Chtimes(walPath, walTime, walTime))
	after, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)

	assert.Equal(before.Size, after.Size)
	assert.Greater(after.MTimeNS, before.MTimeNS)
}

func TestShelleyProviderClassifiesDeletedVirtualPath(t *testing.T) {
	require := require.New(t)

	root, dbPath, db := newShelleyTestDB(t)
	seedShelleyMainConversation(t, db)
	virtualPath := ShelleyVirtualPath(dbPath, "cMAIN1")
	// Close the setup handle before deleting; Windows will not unlink a file
	// this process still holds open.
	require.NoError(db.Close())
	require.NoError(os.Remove(dbPath))

	provider, ok := NewProvider(AgentShelley, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: virtualPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(t, virtualPath, changed[0].DisplayPath)
}

func TestShelleyProviderClassifiesDeletedPhysicalDB(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root, dbPath, db := newShelleyTestDB(t)
	seedShelleyMainConversation(t, db)
	// Close the setup handle before deleting; Windows will not unlink a file
	// this process still holds open.
	require.NoError(db.Close())
	require.NoError(os.Remove(dbPath))

	provider, ok := NewProvider(AgentShelley, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: dbPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(dbPath, changed[0].DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: changed[0]})
	require.NoError(err)
	assert.True(outcome.ResultSetComplete)
	// The backing DB file is gone, so the outcome must not force-replace: the
	// persistent archive preserves sessions whose source file no longer exists.
	assert.False(outcome.ForceReplace)
	assert.Equal(SkipNoSession, outcome.SkipReason)
	assert.Empty(outcome.Results)
}

// TestShelleyProviderChangedPathTombstonesDeletedConversation verifies the
// changed-path classifier emits a tombstone for a stored Shelley conversation
// deleted from a still-present database, and that the tombstone's fingerprint
// no longer errors (it previously aborted before Parse and stranded the stale
// session). The surviving conversation is left to the whole-DB fan-out; a
// vanished database emits no tombstone.
func TestShelleyProviderChangedPathTombstonesDeletedConversation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root, dbPath, db := newShelleyTestDB(t)
	seedShelleyMainConversation(t, db) // cMAIN1 survives
	seedShelleyConversation(
		t, db, "cDEL1", "Doomed conversation",
		"/home/user/dev/myapp", "claude-sonnet-4-6", "", true,
		"2026-06-15T11:00:00Z", "2026-06-15T11:05:00Z",
	)
	seedShelleyMessage(t, db, "cDEL1", 1, 1, "user",
		`{"Role":0,"Content":[{"Type":2,"Text":"hi"}]}`, "", "",
		"2026-06-15T11:00:00Z")

	provider, ok := NewProvider(AgentShelley, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	survivingPath := ShelleyVirtualPath(dbPath, "cMAIN1")
	deletedPath := ShelleyVirtualPath(dbPath, "cDEL1")

	_, err := db.ExecContext(t.Context(), `DELETE FROM messages WHERE conversation_id = ?`, "cDEL1")
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `DELETE FROM conversations WHERE conversation_id = ?`, "cDEL1")
	require.NoError(err)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:              dbPath,
			EventKind:         "write",
			WatchRoot:         root,
			StoredSourcePaths: []string{survivingPath, deletedPath},
		},
	)
	require.NoError(err)
	gotPaths := make([]string, len(changed))
	for i, src := range changed {
		gotPaths[i] = src.DisplayPath
	}
	assert.ElementsMatch([]string{dbPath, deletedPath}, gotPaths,
		"whole-DB source plus a tombstone for the deleted conversation only")

	var tombstone SourceRef
	for _, src := range changed {
		if src.DisplayPath == deletedPath {
			tombstone = src
		}
	}
	require.NotEmpty(tombstone.DisplayPath, "deleted-conversation tombstone source")

	// The fingerprint of a deleted-but-present-DB member must not error, or the
	// engine aborts before Parse and the stale session is never dropped.
	fingerprint, err := provider.Fingerprint(t.Context(), tombstone)
	require.NoError(err, "missing-member fingerprint must not error")
	assert.Equal(tombstone.FingerprintKey, fingerprint.Key)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      tombstone,
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	assert.True(outcome.ResultSetComplete)
	assert.True(outcome.ForceReplace,
		"a conversation deleted from a present DB is force-replaced out of the archive")
	assert.Equal(SkipNoSession, outcome.SkipReason)
	assert.Empty(outcome.Results)

	// A vanished database file emits no tombstone (stored sessions preserved).
	require.NoError(db.Close())
	require.NoError(os.Remove(dbPath))
	gone, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:              dbPath,
			EventKind:         "remove",
			WatchRoot:         root,
			StoredSourcePaths: []string{survivingPath, deletedPath},
		},
	)
	require.NoError(err)
	for _, src := range gone {
		assert.NotEqual(deletedPath, src.DisplayPath,
			"a vanished database must not tombstone stored sessions")
		assert.NotEqual(survivingPath, src.DisplayPath)
	}
}

func TestShelleyProviderStoredVirtualPathFreshness(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root, dbPath, db := newShelleyTestDB(t)
	seedShelleyMainConversation(t, db)
	virtualPath := ShelleyVirtualPath(dbPath, "cMAIN1")

	provider, ok := NewProvider(AgentShelley, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath:     virtualPath,
		RequireFreshSource: true,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(virtualPath, found.DisplayPath)

	_, err = db.ExecContext(t.Context(), `DELETE FROM messages WHERE conversation_id = ?`, "cMAIN1")
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `DELETE FROM conversations WHERE conversation_id = ?`, "cMAIN1")
	require.NoError(err)

	_, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath:     virtualPath,
		RequireFreshSource: true,
	})
	require.NoError(err)
	assert.False(ok, "fresh lookup must reject a deleted virtual row")

	staleSource, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: virtualPath,
	})
	require.NoError(err)
	require.True(ok, "non-fresh lookup keeps tombstone source identity")
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: staleSource,
	})
	require.NoError(err)
	assert.True(outcome.ResultSetComplete)
	assert.True(outcome.ForceReplace)
	assert.Equal(SkipNoSession, outcome.SkipReason)
	assert.Empty(outcome.Results)
}

func TestShelleyProviderRejectsInvalidStoredVirtualPaths(t *testing.T) {
	root, dbPath, db := newShelleyTestDB(t)
	seedShelleyMainConversation(t, db)

	provider, ok := NewProvider(AgentShelley, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	for _, path := range []string{
		dbPath + "#",
		filepath.Join(root, "shelley-debug.db") + "#cMAIN1",
		filepath.Join(root, "nested", shelleyDBName) + "#cMAIN1",
	} {
		_, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
			StoredFilePath:     path,
			RequireFreshSource: true,
		})
		require.NoError(t, err)
		assert.False(t, ok, "stored path %q", path)
	}
}

func TestShelleyProviderIgnoresUnrelatedSidecarBasename(t *testing.T) {
	root := t.TempDir()
	provider, ok := NewProvider(AgentShelley, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "nested", shelleyDBName+"-wal"),
			EventKind: "remove",
			WatchRoot: filepath.Join(root, "nested"),
		},
	)
	require.NoError(t, err)
	assert.Empty(t, changed)
}

func TestZedProviderIgnoresBareShmSiblingEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	// The provider's own read connection rewrites the -shm index, so a bare
	// -shm event must not resolve to the container or every scan would
	// schedule the next one.
	root := t.TempDir()
	dbPath := filepath.Join(root, zedThreadsDBRelPath)
	require.NoError(os.MkdirAll(filepath.Dir(dbPath), 0o755))
	createZedThreadsDBAt(t, dbPath, []zedTestThread{{
		id:        "10431c84-c47b-4e6c-b2df-f9f3b9ad025b",
		summary:   "Provider thread",
		updatedAt: "2026-06-08T09:14:10Z",
		dataType:  "json",
		data:      []byte(`{"messages":[{"User":{"content":[{"Text":"Hello Zed"}]}}]}`),
	}})

	provider, ok := NewProvider(AgentZed, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: dbPath + "-shm", EventKind: "write", WatchRoot: root},
	)
	require.NoError(err)
	assert.Empty(changed)

	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: dbPath + "-wal", EventKind: "write", WatchRoot: root},
	)
	require.NoError(err)
	assert.NotEmpty(changed, "-wal writes still resolve to the container")
}

func TestZedProviderFingerprintIgnoresShmSibling(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	dbPath := filepath.Join(root, zedThreadsDBRelPath)
	require.NoError(os.MkdirAll(filepath.Dir(dbPath), 0o755))
	createZedThreadsDBAt(t, dbPath, []zedTestThread{{
		id:        "10431c84-c47b-4e6c-b2df-f9f3b9ad025b",
		summary:   "Provider thread",
		updatedAt: "2026-06-08T09:14:10Z",
		dataType:  "json",
		data:      []byte(`{"messages":[{"User":{"content":[{"Text":"Hello Zed"}]}}]}`),
	}})

	provider, ok := NewProvider(AgentZed, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	before, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)

	shmPath := dbPath + "-shm"
	writeSourceFile(t, shmPath, "shm")
	shmTime := time.Unix(0, before.MTimeNS+int64(time.Hour))
	require.NoError(os.Chtimes(shmPath, shmTime, shmTime))
	after, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)

	assert.Equal(t, before.MTimeNS, after.MTimeNS)
}

func TestShelleyProviderIgnoresBareShmSiblingEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root, dbPath, db := newShelleyTestDB(t)
	seedShelleyMainConversation(t, db)

	provider, ok := NewProvider(AgentShelley, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: dbPath + "-shm", EventKind: "write", WatchRoot: root},
	)
	require.NoError(err)
	assert.Empty(changed)

	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: dbPath + "-wal", EventKind: "write", WatchRoot: root},
	)
	require.NoError(err)
	assert.NotEmpty(changed, "-wal writes still resolve to the container")
}

func TestShelleyProviderFingerprintIgnoresShmSibling(t *testing.T) {
	require := require.New(t)

	root, dbPath, db := newShelleyTestDB(t)
	seedShelleyMainConversation(t, db)

	provider, ok := NewProvider(AgentShelley, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	before, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)

	shmPath := dbPath + "-shm"
	writeSourceFile(t, shmPath, "shm")
	shmTime := time.Unix(0, before.MTimeNS+int64(time.Hour))
	require.NoError(os.Chtimes(shmPath, shmTime, shmTime))
	after, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)

	assert.Equal(t, before.MTimeNS, after.MTimeNS)
}
