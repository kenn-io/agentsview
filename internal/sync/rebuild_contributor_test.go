package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeSyncStatsIncludesAdditiveRebuildFields(t *testing.T) {
	dst := SyncStats{
		OrphanedCopied: 2,
		RebuildPhases:  []RebuildPhaseStats{{Contributor: "local", BatchedWrites: 1}},
	}
	src := SyncStats{
		OrphanedCopied: 3,
		RebuildPhases:  []RebuildPhaseStats{{Contributor: "remote", BatchedWrites: 2}},
	}

	mergeSyncStats(&dst, src)

	assert.Equal(t, 5, dst.OrphanedCopied)
	assert.Equal(t, []RebuildPhaseStats{
		{Contributor: "local", BatchedWrites: 1},
		{Contributor: "remote", BatchedWrites: 2},
	}, dst.RebuildPhases)
}

func TestResyncAllLegacyOmitsContributorDiagnostics(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	path := filepath.Join(root, "project", "legacy.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-01-01T00:00:00Z", "legacy phase fixture").String()), 0o644))

	stats := engine.ResyncAll(context.Background(), nil)

	require.False(t, stats.Aborted)
	assert.Nil(t, stats.RebuildPhases)
	assert.Nil(t, engine.LastSyncStats().RebuildPhases)
	payload, err := json.Marshal(stats)
	require.NoError(t, err)
	assert.NotContains(t, string(payload), "rebuild_phases")
}

func TestResyncContributorsRunInOrderWithCumulativeProgress(t *testing.T) {
	localRoot := t.TempDir()
	rootA := t.TempDir()
	rootB := t.TempDir()
	for _, fixture := range []struct {
		root    string
		project string
		id      string
		content string
	}{
		{localRoot, "local", "local", "local cumulative progress"},
		{rootA, "a", "contributor-a", "contributor A progress"},
		{rootB, "b", "contributor-b", "contributor B progress"},
	} {
		path := filepath.Join(fixture.root, fixture.project, fixture.id+".jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(testjsonl.NewSessionBuilder().
			AddClaudeUser("2026-01-01T00:00:00Z", fixture.content).String()), 0o644))
	}
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {localRoot}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)

	order := []string{}
	progressByContributor := map[string]Progress{}
	labelProgress := func(name string) func(Progress) Progress {
		return func(p Progress) Progress {
			p.Detail = name
			return p
		}
	}
	onProgress := func(p Progress) {
		if p.Detail == "A" || p.Detail == "B" {
			progressByContributor[p.Detail] = p
		}
	}
	ftsCalls := 0
	stats, err := engine.resyncAllWithOptionsAndOperations(
		context.Background(), onProgress, RebuildOptions{
			Contributors: []RebuildContributor{
				{
					Name: "A",
					Config: EngineConfig{
						AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {rootA}},
						Machine:   "A", IDPrefix: "A~", Ephemeral: true,
					},
					Progress: labelProgress("A"),
					AfterSync: func(*Engine, *db.DB) error {
						order = append(order, "A")
						return nil
					},
				},
				{
					Name: "B",
					Config: EngineConfig{
						AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {rootB}},
						Machine:   "B", IDPrefix: "B~", Ephemeral: true,
					},
					Progress: labelProgress("B"),
					AfterSync: func(*Engine, *db.DB) error {
						order = append(order, "B")
						return nil
					},
				},
			},
		}, rebuildOperations{
			rebuildFTS: func(database *db.DB) error {
				ftsCalls++
				return database.RebuildFTS()
			},
		},
	)
	require.NoError(t, err)
	require.False(t, stats.Aborted, "resync aborted: %+v", stats)
	assert.Equal(t, []string{"A", "B"}, order)
	assert.Equal(t, 3, stats.Synced)
	assert.Equal(t, []string{"local", "A", "B"}, []string{
		stats.RebuildPhases[0].Contributor,
		stats.RebuildPhases[1].Contributor,
		stats.RebuildPhases[2].Contributor,
	})
	assert.Equal(t, 1, ftsCalls, "FTS rebuilt once instead of per contributor")
	assert.Equal(t, Progress{
		Phase: PhaseDone, Detail: "A", Resync: true,
		SessionsTotal: 2, SessionsDone: 2, MessagesIndexed: 2,
	}, progressByContributor["A"])
	assert.Equal(t, Progress{
		Phase: PhaseDone, Detail: "B", Resync: true,
		SessionsTotal: 3, SessionsDone: 3, MessagesIndexed: 3,
	}, progressByContributor["B"])
}

func TestResyncAbortsWhenContributorLosesHistoricalSource(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	writeSession := func(root, project, name, content string) string {
		path := filepath.Join(root, project, name+".jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(testjsonl.NewSessionBuilder().
			AddClaudeUser("2026-01-01T00:00:00Z", content).String()), 0o644))
		return path
	}
	writeSession(localRoot, "local", "local", "local source")
	remotePath := writeSession(remoteRoot, "remote", "remote", "remote source")
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {localRoot}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	options := RebuildOptions{Contributors: []RebuildContributor{{
		Name: "remote",
		Config: EngineConfig{
			AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {remoteRoot}},
			Machine:   "remote", IDPrefix: "remote~", Ephemeral: true,
		},
	}}}

	initial, err := engine.ResyncAllWithOptions(context.Background(), nil, options)
	require.NoError(t, err)
	require.False(t, initial.Aborted)
	require.NoError(t, os.Remove(remotePath))

	stats, err := engine.ResyncAllWithOptions(context.Background(), nil, options)

	require.NoError(t, err)
	assert.True(t, stats.Aborted,
		"a healthy local pass must not mask an empty historical contributor")
	remote, err := database.GetSession(context.Background(), "remote~remote")
	require.NoError(t, err)
	assert.NotNil(t, remote, "aborted rebuild must preserve the active archive")
}

func TestResyncDoesNotTreatSameNamedContributorAsLocalHistory(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	remotePath := filepath.Join(remoteRoot, "project", "remote.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(remotePath), 0o755))
	require.NoError(t, os.WriteFile(remotePath, []byte(testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-01-01T00:00:00Z", "same-name remote source").String()), 0o644))
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {localRoot}},
		Machine:   "collector-host",
	})
	t.Cleanup(engine.Close)
	options := RebuildOptions{Contributors: []RebuildContributor{{
		Name: "collector-host",
		Config: EngineConfig{
			AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {remoteRoot}},
			Machine:   "collector-host", IDPrefix: "collector-host~", Ephemeral: true,
		},
	}}}

	initial, err := engine.ResyncAllWithOptions(context.Background(), nil, options)
	require.NoError(t, err)
	require.False(t, initial.Aborted)

	stats, err := engine.ResyncAllWithOptions(context.Background(), nil, options)

	require.NoError(t, err)
	assert.False(t, stats.Aborted,
		"remote-prefixed history must not make an empty local source look incomplete")
	remote, err := database.GetSession(context.Background(), "collector-host~remote")
	require.NoError(t, err)
	assert.NotNil(t, remote)
}

func TestResyncContributorPostSwapReopenFailureReturnsCoordinatorError(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	emitter := &fakeEmitter{}
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
		Emitter:   emitter,
	})
	t.Cleanup(engine.Close)
	path := filepath.Join(root, "project", "session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-01-01T00:00:00Z", "post swap reopen fixture").String()), 0o644))

	sentinel := errors.New("reopen sentinel")
	stats, err := engine.resyncAllWithOptionsAndOperations(
		context.Background(), nil, RebuildOptions{}, rebuildOperations{
			rebuildFTS: productionRebuildOperations.rebuildFTS,
			reopen:     func(*db.DB) error { return sentinel },
		},
	)
	require.ErrorIs(t, err, sentinel)
	assert.True(t, stats.Aborted)
	assert.Contains(t, stats.Warnings,
		"resync swap completed but reopening active database failed: reopen sentinel")
	assert.Empty(t, emitter.got(), "failed reopen published a successful sync")
	assert.True(t, engine.LastSyncStats().Aborted)

	// The rename already completed. Restore the handle and prove the rebuilt
	// file is now the active archive even though publication failed.
	require.NoError(t, database.Reopen())
	session, getErr := database.GetSession(context.Background(), "session")
	require.NoError(t, getErr)
	require.NotNil(t, session)
}

func TestResyncContributorFTSFailureAbortsAndCleansTempDB(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	oldPath := filepath.Join(root, "old", "old.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(oldPath), 0o755))
	require.NoError(t, os.WriteFile(oldPath, []byte(testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-01-01T00:00:00Z", "search survives failure").String()), 0o644))
	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)
	require.NoError(t, os.Remove(oldPath))
	newPath := filepath.Join(root, "new", "new.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(newPath), 0o755))
	require.NoError(t, os.WriteFile(newPath, []byte(testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-01-01T00:00:01Z", "partial new data").String()), 0o644))

	sentinel := errors.New("fts sentinel")
	engine.syncMu.Lock()
	stats, err := engine.resyncAllWithOptionsLocked(
		context.Background(), nil, RebuildOptions{}, rebuildOperations{
			rebuildFTS: func(*db.DB) error { return sentinel },
		},
	)
	engine.syncMu.Unlock()
	require.ErrorIs(t, err, sentinel)
	assert.True(t, stats.Aborted)
	page, searchErr := database.Search(context.Background(), db.SearchFilter{
		Query: "search survives failure", Limit: 5,
	})
	require.NoError(t, searchErr)
	require.Len(t, page.Results, 1)
	assert.NoFileExists(t, database.Path()+resyncTempSuffix)
	assert.NoFileExists(t, database.Path()+resyncTempSuffix+"-wal")
	assert.NoFileExists(t, database.Path()+resyncTempSuffix+"-shm")
}

type blockingRebuildProvider struct {
	parser.ProviderBase
	started chan struct{}
}

func (p *blockingRebuildProvider) Discover(context.Context) ([]parser.SourceRef, error) {
	return []parser.SourceRef{{
		Provider: parser.AgentCowork, Key: "blocked-source",
		DisplayPath: "blocked-source", FingerprintKey: "blocked-source",
	}}, nil
}

func (p *blockingRebuildProvider) Fingerprint(
	context.Context, parser.SourceRef,
) (parser.SourceFingerprint, error) {
	return parser.SourceFingerprint{Key: "blocked-source", Size: 1, MTimeNS: 1}, nil
}

func (p *blockingRebuildProvider) Parse(
	ctx context.Context, _ parser.ParseRequest,
) (parser.ParseOutcome, error) {
	close(p.started)
	<-ctx.Done()
	return parser.ParseOutcome{}, ctx.Err()
}

type blockingRebuildFactory struct{ provider *blockingRebuildProvider }

func (f blockingRebuildFactory) Definition() parser.AgentDef {
	return f.provider.Definition()
}

func (f blockingRebuildFactory) Capabilities() parser.Capabilities {
	return f.provider.Capabilities()
}

func (f blockingRebuildFactory) NewProvider(parser.ProviderConfig) parser.Provider {
	return f.provider
}

type trackingRebuildProvider struct {
	parser.ProviderBase
	discoverCalls int
}

func (p *trackingRebuildProvider) Discover(context.Context) ([]parser.SourceRef, error) {
	p.discoverCalls++
	return nil, nil
}

func (p *trackingRebuildProvider) Parse(
	context.Context, parser.ParseRequest,
) (parser.ParseOutcome, error) {
	return parser.ParseOutcome{}, nil
}

type trackingRebuildFactory struct{ provider *trackingRebuildProvider }

func (f trackingRebuildFactory) Definition() parser.AgentDef {
	return f.provider.Definition()
}

func (f trackingRebuildFactory) Capabilities() parser.Capabilities {
	return f.provider.Capabilities()
}

func (f trackingRebuildFactory) NewProvider(parser.ProviderConfig) parser.Provider {
	return f.provider
}

func TestResyncContributorCancellationPreservesArchiveAndCleansTempDB(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	oldPath := filepath.Join(root, "old", "old.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(oldPath), 0o755))
	require.NoError(t, os.WriteFile(oldPath, []byte(testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-01-01T00:00:00Z", "archive before cancel").String()), 0o644))
	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)
	require.NoError(t, os.Remove(oldPath))

	provider := &blockingRebuildProvider{
		ProviderBase: parser.ProviderBase{
			Def: parser.AgentDef{Type: parser.AgentCowork},
			Caps: parser.Capabilities{Source: parser.SourceCapabilities{
				DiscoverSources: parser.CapabilitySupported,
			}},
		},
		started: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	firstHookCalls := 0
	secondHookCalls := 0
	secondProvider := &trackingRebuildProvider{ProviderBase: parser.ProviderBase{
		Def: parser.AgentDef{Type: parser.AgentCowork},
		Caps: parser.Capabilities{Source: parser.SourceCapabilities{
			DiscoverSources: parser.CapabilitySupported,
		}},
	}}
	result := make(chan struct {
		stats SyncStats
		err   error
	}, 1)
	go func() {
		stats, runErr := engine.ResyncAllWithOptions(ctx, nil, RebuildOptions{
			Contributors: []RebuildContributor{
				{
					Name: "blocking",
					Config: EngineConfig{
						AgentDirs: map[parser.AgentType][]string{parser.AgentCowork: {root}},
						Machine:   "blocking", Ephemeral: true,
						ProviderFactories: []parser.ProviderFactory{
							blockingRebuildFactory{provider: provider},
						},
						ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
							parser.AgentCowork: parser.ProviderMigrationProviderAuthoritative,
						},
					},
					AfterSync: func(*Engine, *db.DB) error {
						firstHookCalls++
						return nil
					},
				},
				{
					Name: "later",
					Config: EngineConfig{
						AgentDirs: map[parser.AgentType][]string{parser.AgentCowork: {root}},
						Machine:   "later", Ephemeral: true,
						ProviderFactories: []parser.ProviderFactory{
							trackingRebuildFactory{provider: secondProvider},
						},
						ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
							parser.AgentCowork: parser.ProviderMigrationProviderAuthoritative,
						},
					},
					AfterSync: func(*Engine, *db.DB) error {
						secondHookCalls++
						return nil
					},
				},
			},
		})
		result <- struct {
			stats SyncStats
			err   error
		}{stats: stats, err: runErr}
	}()

	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("contributor parse did not reach controlled boundary")
	}
	cancel()
	select {
	case got := <-result:
		require.ErrorIs(t, got.err, context.Canceled)
		assert.True(t, got.stats.Aborted)
		assert.Zero(t, firstHookCalls, "AfterSync ran on incomplete contributor data")
		assert.Zero(t, secondProvider.discoverCalls, "later contributor started after cancellation")
		assert.Zero(t, secondHookCalls, "later contributor hook ran after cancellation")
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled contributor rebuild did not return")
	}

	page, searchErr := database.Search(context.Background(), db.SearchFilter{
		Query: "archive before cancel", Limit: 5,
	})
	require.NoError(t, searchErr)
	require.Len(t, page.Results, 1)
	assert.NoFileExists(t, database.Path()+resyncTempSuffix)
	assert.NoFileExists(t, database.Path()+resyncTempSuffix+"-wal")
	assert.NoFileExists(t, database.Path()+resyncTempSuffix+"-shm")
}

func TestResyncLocalCancellationPreventsContributors(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	oldPath := filepath.Join(root, "old", "old.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(oldPath), 0o755))
	require.NoError(t, os.WriteFile(oldPath, []byte(testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-01-01T00:00:00Z", "archive before local cancel").String()), 0o644))
	seedEngine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
	})
	require.Equal(t, 1, seedEngine.SyncAll(context.Background(), nil).Synced)
	seedEngine.Close()
	require.NoError(t, os.Remove(oldPath))

	blocking := &blockingRebuildProvider{
		ProviderBase: parser.ProviderBase{
			Def: parser.AgentDef{Type: parser.AgentCowork},
			Caps: parser.Capabilities{Source: parser.SourceCapabilities{
				DiscoverSources: parser.CapabilitySupported,
			}},
		},
		started: make(chan struct{}),
	}
	later := &trackingRebuildProvider{ProviderBase: parser.ProviderBase{
		Def: parser.AgentDef{Type: parser.AgentCowork},
		Caps: parser.Capabilities{Source: parser.SourceCapabilities{
			DiscoverSources: parser.CapabilitySupported,
		}},
	}}
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCowork: {root}},
		Machine:   "local",
		ProviderFactories: []parser.ProviderFactory{
			blockingRebuildFactory{provider: blocking},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentCowork: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan struct {
		stats SyncStats
		err   error
	}, 1)
	go func() {
		stats, runErr := engine.ResyncAllWithOptions(ctx, nil, RebuildOptions{
			Contributors: []RebuildContributor{{
				Name: "later",
				Config: EngineConfig{
					AgentDirs: map[parser.AgentType][]string{parser.AgentCowork: {root}},
					Machine:   "later", Ephemeral: true,
					ProviderFactories: []parser.ProviderFactory{
						trackingRebuildFactory{provider: later},
					},
					ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
						parser.AgentCowork: parser.ProviderMigrationProviderAuthoritative,
					},
				},
			}},
		})
		result <- struct {
			stats SyncStats
			err   error
		}{stats: stats, err: runErr}
	}()
	select {
	case <-blocking.started:
	case <-time.After(5 * time.Second):
		t.Fatal("local parse did not reach controlled boundary")
	}
	cancel()
	select {
	case got := <-result:
		require.ErrorIs(t, got.err, context.Canceled)
		assert.True(t, got.stats.Aborted)
		assert.Zero(t, later.discoverCalls)
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled local rebuild did not return")
	}
	page, searchErr := database.Search(context.Background(), db.SearchFilter{
		Query: "archive before local cancel", Limit: 5,
	})
	require.NoError(t, searchErr)
	require.Len(t, page.Results, 1)
	assert.NoFileExists(t, database.Path()+resyncTempSuffix)
	assert.NoFileExists(t, database.Path()+resyncTempSuffix+"-wal")
	assert.NoFileExists(t, database.Path()+resyncTempSuffix+"-shm")
}

type staticUsageRebuildProvider struct {
	parser.ProviderBase
	source parser.SourceRef
	result parser.ParseResult
}

func (p *staticUsageRebuildProvider) Discover(context.Context) ([]parser.SourceRef, error) {
	return []parser.SourceRef{p.source}, nil
}

func (p *staticUsageRebuildProvider) Fingerprint(
	context.Context, parser.SourceRef,
) (parser.SourceFingerprint, error) {
	return parser.SourceFingerprint{Key: p.source.FingerprintKey, Size: 10, MTimeNS: 10}, nil
}

func (p *staticUsageRebuildProvider) Parse(
	context.Context, parser.ParseRequest,
) (parser.ParseOutcome, error) {
	return parser.ParseOutcome{
		Results:           []parser.ParseResultOutcome{{Result: p.result}},
		ResultSetComplete: true,
	}, nil
}

type staticUsageRebuildFactory struct{ provider *staticUsageRebuildProvider }

func (f staticUsageRebuildFactory) Definition() parser.AgentDef {
	return f.provider.Definition()
}

func (f staticUsageRebuildFactory) Capabilities() parser.Capabilities {
	return f.provider.Capabilities()
}

func (f staticUsageRebuildFactory) NewProvider(parser.ProviderConfig) parser.Provider {
	return f.provider
}

func newStaticUsageRebuildProvider(id, path string, input, output int) *staticUsageRebuildProvider {
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &staticUsageRebuildProvider{
		ProviderBase: parser.ProviderBase{
			Def: parser.AgentDef{Type: parser.AgentCowork, FileBased: true},
			Caps: parser.Capabilities{Source: parser.SourceCapabilities{
				DiscoverSources:      parser.CapabilitySupported,
				CompositeFingerprint: parser.CapabilitySupported,
			}},
		},
		source: parser.SourceRef{
			Provider: parser.AgentCowork, Key: path, DisplayPath: path, FingerprintKey: path,
		},
		result: parser.ParseResult{
			Session: parser.ParsedSession{
				ID: id, Project: "usage", Machine: "fixture", Agent: parser.AgentCowork,
				StartedAt: started, EndedAt: started, FirstMessage: "usage fixture",
				MessageCount: 1, UserMessageCount: 1,
				File: parser.FileInfo{Path: path, Size: 10, Mtime: 10},
			},
			Messages: []parser.ParsedMessage{{
				Ordinal: 0, Role: parser.RoleUser, Content: "usage fixture", Timestamp: started,
			}},
			UsageEvents: []parser.ParsedUsageEvent{{
				SessionID: id, Source: "fixture", Model: "fixture-model",
				InputTokens: input, OutputTokens: output,
				OccurredAt: "2026-01-01T00:00:00Z", DedupKey: id + "-usage",
			}},
		},
	}
}

func TestResyncContributorBatchFailureAbortsAndCleansTempDB(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	oldPath := filepath.Join(root, "old", "old.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(oldPath), 0o755))
	require.NoError(t, os.WriteFile(oldPath, []byte(testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-01-01T00:00:00Z", "archive before batch failure").String()), 0o644))
	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)
	require.NoError(t, os.Remove(oldPath))

	bad := newStaticUsageRebuildProvider("batch-rejected", "bad-source", 1, 1)
	bad.result.UsageEvents = nil
	bad.result.Messages = append(bad.result.Messages, parser.ParsedMessage{
		Ordinal: 0, Role: parser.RoleAssistant, Content: "duplicate ordinal",
		Timestamp: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC),
	})
	stats, err := engine.ResyncAllWithOptions(context.Background(), nil, RebuildOptions{
		Contributors: []RebuildContributor{{
			Name: "bad-batch",
			Config: EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCowork: {"bad-root"}},
				Machine:   "bad-batch", Ephemeral: true,
				ProviderFactories: []parser.ProviderFactory{
					staticUsageRebuildFactory{provider: bad},
				},
				ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
					parser.AgentCowork: parser.ProviderMigrationProviderAuthoritative,
				},
			},
		}},
	})
	require.NoError(t, err)
	assert.True(t, stats.Aborted)
	assert.Equal(t, 1, stats.Failed)
	page, searchErr := database.Search(context.Background(), db.SearchFilter{
		Query: "archive before batch failure", Limit: 5,
	})
	require.NoError(t, searchErr)
	require.Len(t, page.Results, 1)
	assert.NoFileExists(t, database.Path()+resyncTempSuffix)
	assert.NoFileExists(t, database.Path()+resyncTempSuffix+"-wal")
	assert.NoFileExists(t, database.Path()+resyncTempSuffix+"-shm")
}

type malformedRebuildProvider struct{ parser.ProviderBase }

func (p *malformedRebuildProvider) Discover(context.Context) ([]parser.SourceRef, error) {
	out := make([]parser.SourceRef, 3)
	for i := range out {
		key := fmt.Sprintf("malformed-%d.jsonl", i)
		out[i] = parser.SourceRef{
			Provider: parser.AgentCowork, Key: key, DisplayPath: key, FingerprintKey: key,
		}
	}
	return out, nil
}

func (p *malformedRebuildProvider) Fingerprint(
	_ context.Context, source parser.SourceRef,
) (parser.SourceFingerprint, error) {
	return parser.SourceFingerprint{Key: source.FingerprintKey, Size: 8, MTimeNS: 10}, nil
}

func (p *malformedRebuildProvider) Parse(
	_ context.Context, request parser.ParseRequest,
) (parser.ParseOutcome, error) {
	return parser.ParseOutcome{}, fmt.Errorf("malformed fixture %s", request.Source.Key)
}

type malformedRebuildFactory struct{ provider *malformedRebuildProvider }

func (f malformedRebuildFactory) Definition() parser.AgentDef {
	return f.provider.Definition()
}

func (f malformedRebuildFactory) Capabilities() parser.Capabilities {
	return f.provider.Capabilities()
}

func (f malformedRebuildFactory) NewProvider(parser.ProviderConfig) parser.Provider {
	return f.provider
}

func TestResyncContributorParserFailuresAbortAndCleanTempDB(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	oldPath := filepath.Join(root, "old", "old.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(oldPath), 0o755))
	require.NoError(t, os.WriteFile(oldPath, []byte(testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-01-01T00:00:00Z", "archive before parser failures").String()), 0o644))
	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)
	require.NoError(t, os.Remove(oldPath))
	provider := &malformedRebuildProvider{ProviderBase: parser.ProviderBase{
		Def: parser.AgentDef{Type: parser.AgentCowork, FileBased: true},
		Caps: parser.Capabilities{Source: parser.SourceCapabilities{
			DiscoverSources:      parser.CapabilitySupported,
			CompositeFingerprint: parser.CapabilitySupported,
		}},
	}}

	stats, err := engine.ResyncAllWithOptions(context.Background(), nil, RebuildOptions{
		Contributors: []RebuildContributor{{
			Name: "malformed",
			Config: EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCowork: {"malformed-root"}},
				Machine:   "malformed", Ephemeral: true,
				ProviderFactories: []parser.ProviderFactory{
					malformedRebuildFactory{provider: provider},
				},
				ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
					parser.AgentCowork: parser.ProviderMigrationProviderAuthoritative,
				},
			},
		}},
	})
	require.NoError(t, err)
	assert.True(t, stats.Aborted)
	assert.Equal(t, 3, stats.Failed)
	assert.Equal(t, 3, stats.TotalSessions)
	page, searchErr := database.Search(context.Background(), db.SearchFilter{
		Query: "archive before parser failures", Limit: 5,
	})
	require.NoError(t, searchErr)
	require.Len(t, page.Results, 1)
	assert.NoFileExists(t, database.Path()+resyncTempSuffix)
	assert.NoFileExists(t, database.Path()+resyncTempSuffix+"-wal")
	assert.NoFileExists(t, database.Path()+resyncTempSuffix+"-shm")
}
