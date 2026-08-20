package sync_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

type unavailableCursorFactory struct {
	base       parser.ProviderFactory
	state      parser.SourceCwdState
	resolution parser.SourceCwdResolution
}

func (f unavailableCursorFactory) Definition() parser.AgentDef {
	return f.base.Definition()
}

func (f unavailableCursorFactory) Capabilities() parser.Capabilities {
	return f.base.Capabilities()
}

func (f unavailableCursorFactory) NewProvider(
	cfg parser.ProviderConfig,
) parser.Provider {
	return &unavailableCursorProvider{
		Provider:   f.base.NewProvider(cfg),
		state:      f.state,
		resolution: f.resolution,
	}
}

type unavailableCursorProvider struct {
	parser.Provider
	state      parser.SourceCwdState
	resolution parser.SourceCwdResolution
}

func cursorSourceWithState(
	source parser.SourceRef, state parser.SourceCwdState,
) parser.SourceRef {
	source.CwdResolution = parser.SourceCwdResolution{
		State: state,
	}
	return source
}

func (p *unavailableCursorProvider) sourceWithCwd(
	source parser.SourceRef,
) parser.SourceRef {
	if p.resolution.State != parser.SourceCwdUnspecified {
		source.CwdResolution = p.resolution
		return source
	}
	return cursorSourceWithState(source, p.state)
}

func (p *unavailableCursorProvider) Discover(
	ctx context.Context,
) ([]parser.SourceRef, error) {
	sources, err := p.Provider.Discover(ctx)
	for i := range sources {
		sources[i] = p.sourceWithCwd(sources[i])
	}
	return sources, err
}

func (p *unavailableCursorProvider) DiscoverEach(
	ctx context.Context, yield func(parser.SourceRef) error,
) error {
	discoverer, ok := p.Provider.(parser.StreamingDiscoverer)
	if !ok {
		return fmt.Errorf("cursor test provider lacks streaming discovery")
	}
	return discoverer.DiscoverEach(ctx, func(source parser.SourceRef) error {
		return yield(p.sourceWithCwd(source))
	})
}

func (p *unavailableCursorProvider) FindSource(
	ctx context.Context, req parser.FindSourceRequest,
) (parser.SourceRef, bool, error) {
	source, found, err := p.Provider.FindSource(ctx, req)
	if found {
		source = p.sourceWithCwd(source)
	}
	return source, found, err
}

func cursorProviderFactoryForTest(
	t *testing.T, states ...parser.SourceCwdState,
) parser.ProviderFactory {
	t.Helper()
	state := parser.SourceCwdUnavailable
	if len(states) > 0 {
		state = states[0]
	}
	for _, factory := range parser.ProviderFactories() {
		if factory.Definition().Type == parser.AgentCursor {
			return unavailableCursorFactory{base: factory, state: state}
		}
	}
	t.Fatal("Cursor provider factory is not registered")
	return nil
}

func TestSyncEngineCursorUnavailableChangedTranscriptPreservesCwd(t *testing.T) {
	root := t.TempDir()
	workspace := t.TempDir()
	sessionID := "cccccccc-dddd-4eee-8fff-000000000000"
	path := filepath.Join(
		root, "Users-helix-Code-app", "agent-transcripts", sessionID+".jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"role":"user","message":{"content":"before"}}`+"\n",
	), 0o644))
	d := dbtest.OpenTestDB(t)
	e := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:   "local",
		ProviderFactories: []parser.ProviderFactory{
			cursorProviderFactoryForTest(t),
		},
	})
	t.Cleanup(func() { e.Close() })

	e.SyncAll(context.Background(), nil)
	fullID := "cursor:" + sessionID
	stored, err := d.GetSession(context.Background(), fullID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Empty(t, stored.Cwd)
	require.NoError(t, d.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE sessions SET cwd = ? WHERE id = ?", workspace, fullID)
		return err
	}))

	require.NoError(t, os.WriteFile(path, []byte(
		`{"role":"user","message":{"content":"after"}}`+"\n",
	), 0o644))
	changedAt := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(path, changedAt, changedAt))
	filtered := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs:          map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:            "local",
		IncludeCwdPrefixes: []string{workspace},
		ProviderFactories: []parser.ProviderFactory{
			cursorProviderFactoryForTest(t),
		},
	})
	t.Cleanup(func() { filtered.Close() })
	require.NoError(t, filtered.SyncSingleSessionContext(
		context.Background(), fullID,
	))
	stats := filtered.LastSyncStats()
	assert.Zero(t, stats.Failed)
	assert.False(t, stats.Aborted)

	stored, err = d.GetSession(context.Background(), fullID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, workspace, stored.Cwd)
}

func TestResyncCursorUnavailablePreservesArchiveCwd(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := cursorWorkspaceTempDir(t)
	workspace := filepath.Join(workspaceRoot, "Code", "app")
	projectDir := encodeCursorProjectDir(workspace)
	sessionID := "12121212-3434-4567-8899-aaaaaaaaaaaa"
	path := filepath.Join(root, projectDir, "agent-transcripts", sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"role":"user","message":{"content":"resync archive"}}`+"\n",
	), 0o644))

	d := dbtest.OpenTestDB(t)
	initial := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:   "local",
	})
	initial.SyncAll(context.Background(), nil)
	initial.Close()

	rebuild := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:   "local",
		ProviderFactories: []parser.ProviderFactory{
			cursorProviderFactoryForTest(t),
		},
	})
	t.Cleanup(func() { rebuild.Close() })
	stats := rebuild.ResyncAll(context.Background(), nil)
	assert.Zero(t, stats.Failed)
	assert.False(t, stats.Aborted)

	stored, err := d.GetSession(context.Background(), "cursor:"+sessionID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, workspace, stored.Cwd)
}

func TestResyncCursorFilteredCwdSurvivesOrphanCopy(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := cursorWorkspaceTempDir(t)
	oldWorkspace := filepath.Join(workspaceRoot, "Code", "old")
	newWorkspace := filepath.Join(workspaceRoot, "Code", "new")
	projectDir := encodeCursorProjectDir(oldWorkspace)
	sessionID := "23232323-4545-4676-8999-bbbbbbbbbbbb"
	path := filepath.Join(root, projectDir, "agent-transcripts", sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(oldWorkspace, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"role":"user","message":{"content":"resync filtered"}}`+"\n",
	), 0o644))

	d := dbtest.OpenTestDB(t)
	initial := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:   "local",
	})
	initial.SyncAll(context.Background(), nil)
	initial.Close()

	rebuild := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs:          map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:            "local",
		IncludeCwdPrefixes: []string{oldWorkspace},
		ProviderFactories: []parser.ProviderFactory{
			unavailableCursorFactory{
				base: cursorProviderFactoryForTest(t),
				resolution: parser.SourceCwdResolution{
					State: parser.SourceCwdResolved, Path: newWorkspace,
				},
			},
		},
	})
	t.Cleanup(func() { rebuild.Close() })
	stats := rebuild.ResyncAll(context.Background(), nil)
	assert.Zero(t, stats.Failed)
	assert.False(t, stats.Aborted)
	assert.NotZero(t, stats.CwdUpdated)

	stored, err := d.GetSession(context.Background(), "cursor:"+sessionID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, newWorkspace, stored.Cwd)
}

func TestParseDiffCursorCwdDoesNotWriteOnParseError(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := cursorWorkspaceTempDir(t)
	workspace := filepath.Join(workspaceRoot, "Code", "app")
	projectDir := encodeCursorProjectDir(workspace)
	sessionID := "56565656-7878-4901-8222-bbbbbbbbbbbb"
	path := filepath.Join(root, projectDir, "agent-transcripts", sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"role":"user","message":{"content":"parse diff"}}`+"\n",
	), 0o644))

	d := dbtest.OpenTestDB(t)
	initial := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:   "local",
	})
	initial.SyncAll(context.Background(), nil)
	initial.Close()
	require.NoError(t, d.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"UPDATE sessions SET local_modified_at = 'before' WHERE id = ?",
			"cursor:"+sessionID,
		)
		return err
	}))
	before, err := d.GetSessionFull(context.Background(), "cursor:"+sessionID)
	require.NoError(t, err)
	require.NotNil(t, before)
	require.NotNil(t, before.LocalModifiedAt)

	require.NoError(t, os.Truncate(path, 10<<20+1))
	diff := sync.NewDiffEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:   "local",
		ProviderFactories: []parser.ProviderFactory{
			unavailableCursorFactory{
				base: cursorProviderFactoryForTest(t),
				resolution: parser.SourceCwdResolution{
					State: parser.SourceCwdResolved, Path: filepath.Join(workspaceRoot, "Code", "new"),
				},
			},
		},
	})
	t.Cleanup(func() { diff.Close() })
	_, err = diff.ParseDiff(context.Background(), sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentCursor},
	})
	require.NoError(t, err)

	after, err := d.GetSessionFull(context.Background(), "cursor:"+sessionID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, workspace, after.Cwd)
	assert.Equal(t, *before.LocalModifiedAt, *after.LocalModifiedAt)
}

func TestSyncEngineCursorResolvedFilteredCwdIsReconciled(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := cursorWorkspaceTempDir(t)
	oldWorkspace := filepath.Join(workspaceRoot, "Code", "old")
	workspace := filepath.Join(workspaceRoot, "Code", "new")
	projectDir := encodeCursorProjectDir(workspace)
	sessionID := "abababab-cdcd-4efe-8111-121212121212"
	path := filepath.Join(root, projectDir, "agent-transcripts", sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"role":"user","message":{"content":"filtered resolved"}}`+"\n",
	), 0o644))

	d := dbtest.OpenTestDB(t)
	initial := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:   "local",
	})
	initial.SyncAll(context.Background(), nil)
	initial.Close()
	fullID := "cursor:" + sessionID
	require.NoError(t, d.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE sessions SET cwd = ? WHERE id = ?", oldWorkspace, fullID)
		return err
	}))
	require.NoError(t, os.MkdirAll(workspace, 0o755))

	filtered := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs:          map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:            "local",
		IncludeCwdPrefixes: []string{oldWorkspace},
	})
	t.Cleanup(func() { filtered.Close() })
	stats := filtered.SyncAll(context.Background(), nil)
	assert.Zero(t, stats.Failed)
	assert.False(t, stats.Aborted)
	assert.NotZero(t, stats.CwdUpdated)

	stored, err := d.GetSession(context.Background(), fullID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, workspace, stored.Cwd)
}

func TestSyncEngineCursorOversizedTranscriptReconcilesWorkspace(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := cursorWorkspaceTempDir(t)
	workspace := filepath.Join(workspaceRoot, "Code", "app")
	projectDir := encodeCursorProjectDir(workspace)
	sessionID := "eeeeeeee-ffff-4000-8111-222222222222"
	path := filepath.Join(root, projectDir, "agent-transcripts", sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"role":"user","message":{"content":"oversized"}}`+"\n",
	), 0o644))

	d := dbtest.OpenTestDB(t)
	e := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:   "local",
	})
	t.Cleanup(func() { e.Close() })

	e.SyncAll(context.Background(), nil)
	fullID := "cursor:" + sessionID
	first, err := d.GetSession(context.Background(), fullID)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Empty(t, first.Cwd)

	// Seed the hash-empty state before the final workspace-only transition.
	require.NoError(t, os.Truncate(path, 10<<20+1))
	require.NoError(t, d.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"UPDATE sessions SET file_hash = '' WHERE id = ?", fullID,
		)
		return err
	}))
	e.SyncAll(context.Background(), nil)
	hash, ok := d.GetSessionFileHash(fullID)
	assert.True(t, ok)
	assert.Empty(t, hash)

	require.NoError(t, os.MkdirAll(workspace, 0o755))
	e.SyncAll(context.Background(), nil)

	second, err := d.GetSession(context.Background(), fullID)
	require.NoError(t, err)
	require.NotNil(t, second)
	hash, ok = d.GetSessionFileHash(fullID)
	assert.True(t, ok)
	assert.Empty(t, hash)
	assert.Equal(t, workspace, second.Cwd)
	assert.Less(t, d.GetSessionDataVersion(fullID), db.CurrentDataVersion())
}

func TestSyncEngineCursorNoneSingleSessionClearsFilteredCwd(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := cursorWorkspaceTempDir(t)
	workspace := filepath.Join(workspaceRoot, "Code", "app")
	projectDir := encodeCursorProjectDir(workspace)
	sessionID := "ffffffff-0000-4111-8222-333333333333"
	path := filepath.Join(root, projectDir, "agent-transcripts", sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"role":"user","message":{"content":"single session"}}`+"\n",
	), 0o644))

	d := dbtest.OpenTestDB(t)
	e := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:   "local",
	})
	e.SyncAll(context.Background(), nil)
	e.Close()
	fullID := "cursor:" + sessionID
	require.NoError(t, d.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE sessions SET cwd = ? WHERE id = ?", workspace, fullID)
		return err
	}))

	filtered := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs:          map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:            "local",
		IncludeCwdPrefixes: []string{workspaceRoot},
	})
	t.Cleanup(func() { filtered.Close() })
	require.NoError(t, filtered.SyncSingleSessionContext(
		context.Background(), fullID,
	))
	stats := filtered.LastSyncStats()
	assert.Zero(t, stats.Failed)
	assert.False(t, stats.Aborted)

	stored, err := d.GetSession(context.Background(), fullID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Empty(t, stored.Cwd)
}

func TestSyncEngineCursorRemoteClearsStoredCwd(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := cursorWorkspaceTempDir(t)
	workspace := filepath.Join(workspaceRoot, "Code", "app")
	projectDir := encodeCursorProjectDir(workspace)
	sessionID := "11111111-2222-4333-8444-555555555555"
	path := filepath.Join(root, projectDir, "agent-transcripts", sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"role":"user","message":{"content":"remote"}}`+"\n",
	), 0o644))

	d := dbtest.OpenTestDB(t)
	local := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:   "local",
	})
	local.SyncAll(context.Background(), nil)
	local.Close()
	fullID := "cursor:" + sessionID
	stored, err := d.GetSession(context.Background(), fullID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, workspace, stored.Cwd)

	remote := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:   "local",
		ProviderFactories: []parser.ProviderFactory{
			cursorProviderFactoryForTest(t, parser.SourceCwdRemote),
		},
	})
	t.Cleanup(func() { remote.Close() })
	stats := remote.SyncAll(context.Background(), nil)
	assert.Zero(t, stats.Failed)
	assert.False(t, stats.Aborted)

	stored, err = d.GetSession(context.Background(), fullID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Empty(t, stored.Cwd)
}

func TestSyncEngineCursorSourceMissingRevivalPreservesCwd(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := cursorWorkspaceTempDir(t)
	workspace := filepath.Join(workspaceRoot, "Code", "app")
	projectDir := encodeCursorProjectDir(workspace)
	sessionID := "22222222-3333-4444-8555-666666666666"
	path := filepath.Join(root, projectDir, "agent-transcripts", sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"role":"user","message":{"content":"before revive"}}`+"\n",
	), 0o644))

	d := dbtest.OpenTestDB(t)
	e := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:   "local",
	})
	t.Cleanup(func() { e.Close() })
	e.SyncAll(context.Background(), nil)
	fullID := "cursor:" + sessionID
	stored, err := d.GetSession(context.Background(), fullID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, workspace, stored.Cwd)
	require.NoError(t, d.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"UPDATE sessions SET deleted_at = 'now', deletion_cause = ? WHERE id = ?",
			"source_missing", fullID,
		)
		return err
	}))

	require.NoError(t, os.WriteFile(path, []byte(
		`{"role":"user","message":{"content":"after revive"}}`+"\n",
	), 0o644))
	changedAt := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(path, changedAt, changedAt))
	stats := e.SyncAll(context.Background(), nil)
	assert.Zero(t, stats.Failed)
	assert.False(t, stats.Aborted)

	stored, err = d.GetSession(context.Background(), fullID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Nil(t, stored.DeletedAt)
	assert.Equal(t, workspace, stored.Cwd)
}

func TestSyncEngineCursorUnchangedTranscriptFollowsWorkspaceLifecycle(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := cursorWorkspaceTempDir(t)
	workspace := filepath.Join(workspaceRoot, "Code", "app")
	projectDir := encodeCursorProjectDir(workspace)
	sessionID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	path := filepath.Join(root, projectDir, "agent-transcripts", sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := []byte(`{"role":"user","message":{"content":"workspace lifecycle"}}` + "\n")
	require.NoError(t, os.WriteFile(path, content, 0o644))
	before, err := os.Stat(path)
	require.NoError(t, err)

	d := dbtest.OpenTestDB(t)
	e := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:   "local",
	})
	e.SyncAll(context.Background(), nil)
	first, err := d.GetSession(context.Background(), "cursor:"+sessionID)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Empty(t, first.Cwd)

	require.NoError(t, os.MkdirAll(workspace, 0o755))
	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, before.Size(), after.Size())
	assert.Equal(t, before.ModTime(), after.ModTime())

	e.SyncAllSince(context.Background(), before.ModTime().Add(time.Second), nil)
	second, err := d.GetSession(context.Background(), "cursor:"+sessionID)
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.Equal(t, workspace, second.Cwd)

	e.Close()
	cold := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:   "local",
	})
	t.Cleanup(func() { cold.Close() })
	cold.SyncAll(context.Background(), nil)
	third, err := d.GetSession(context.Background(), "cursor:"+sessionID)
	require.NoError(t, err)
	require.NotNil(t, third)
	assert.Equal(t, workspace, third.Cwd)

	require.NoError(t, os.RemoveAll(workspace))
	otherWorkspace := filepath.Join(workspaceRoot, "Code-app")
	require.NoError(t, os.MkdirAll(otherWorkspace, 0o755))
	fourthStats := cold.SyncAll(context.Background(), nil)
	assert.Zero(t, fourthStats.Failed)
	assert.False(t, fourthStats.Aborted)
	fourth, err := d.GetSession(context.Background(), "cursor:"+sessionID)
	require.NoError(t, err)
	require.NotNil(t, fourth)
	assert.Equal(t, otherWorkspace, fourth.Cwd)
}

func TestSyncEngineCursorCompleteNoneAndAmbiguousClearStoredCwd(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := cursorWorkspaceTempDir(t)
	workspace := filepath.Join(workspaceRoot, "Code", "app")
	projectDir := encodeCursorProjectDir(workspace)
	sessionID := "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
	path := filepath.Join(root, projectDir, "agent-transcripts", sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"role":"user","message":{"content":"clear cwd"}}`+"\n",
	), 0o644))
	d := dbtest.OpenTestDB(t)
	e := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs:          map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:            "local",
		IncludeCwdPrefixes: []string{workspaceRoot},
	})
	t.Cleanup(func() { e.Close() })

	require.NoError(t, os.MkdirAll(workspace, 0o755))
	stats := e.SyncAll(context.Background(), nil)
	assert.Zero(t, stats.Failed)
	assert.False(t, stats.Aborted)
	fullID := "cursor:" + sessionID
	session, err := d.GetSession(context.Background(), fullID)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, workspace, session.Cwd)

	require.NoError(t, os.RemoveAll(workspace))
	stats = e.SyncAll(context.Background(), nil)
	assert.Zero(t, stats.Failed)
	assert.False(t, stats.Aborted)
	session, err = d.GetSession(context.Background(), fullID)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Empty(t, session.Cwd)

	require.NoError(t, os.MkdirAll(workspace, 0o755))
	stats = e.SyncAll(context.Background(), nil)
	assert.Zero(t, stats.Failed)
	assert.False(t, stats.Aborted)
	session, err = d.GetSession(context.Background(), fullID)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, workspace, session.Cwd)

	require.NoError(t, os.MkdirAll(filepath.Join(workspaceRoot, "Code-app"), 0o755))
	stats = e.SyncAll(context.Background(), nil)
	assert.Zero(t, stats.Failed)
	assert.False(t, stats.Aborted)
	session, err = d.GetSession(context.Background(), fullID)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Empty(t, session.Cwd)
}
