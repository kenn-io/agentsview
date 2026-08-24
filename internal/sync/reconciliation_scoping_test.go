package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// writeClaudeProjectSession writes one minimal Claude session named
// <name>.jsonl under <root>/<project>; the session ID is the file base name.
func writeClaudeProjectSession(t *testing.T, root, project, name string) string {
	t.Helper()
	path := filepath.Join(root, project, name+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		testjsonl.NewSessionBuilder().
			AddClaudeUser("2024-01-01T00:00:00Z", "hi "+name).
			String(),
	), 0o644))
	return path
}

// TestReconcileProviderRootsDescendantDoesNotClaimSiblingScope is the
// reproduction row: a pass asked about one project directory must not open or
// tombstone sessions under a sibling directory it never requested.
func TestReconcileProviderRootsDescendantDoesNotClaimSiblingScope(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	database := openTestDB(t)
	claudeRoot := filepath.Join(t.TempDir(), "claude")
	projectA := filepath.Join(claudeRoot, "projA")
	projectB := filepath.Join(claudeRoot, "projB")
	writeClaudeProjectSession(t, claudeRoot, "projA", "a1")
	writeClaudeProjectSession(t, claudeRoot, "projA", "a2")
	siblingDeleted := writeClaudeProjectSession(t, claudeRoot, "projB", "b1")
	writeClaudeProjectSession(t, claudeRoot, "projB", "b2")

	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeRoot},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 4, engine.SyncAll(t.Context(), nil).Synced)

	// The sibling directory loses a source; only a pass with authority over
	// projB may treat that as deletion proof.
	require.NoError(t, os.Remove(siblingDeleted))
	rec := &lstatRecorder{}
	engine.lstat = rec.stat

	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), parser.AgentClaude, []string{projectA},
	))

	for _, id := range []string{"b1", "b2"} {
		active, err := database.GetSession(t.Context(), id)
		require.NoError(t, err)
		assert.NotNil(t, active,
			"a pass scoped to projA holds no tombstone authority over projB")
	}
	assert.Zero(t, rec.countUnder(projectB),
		"a projA-scoped pass must not stat sibling sources")
	assert.LessOrEqual(t,
		engine.LastReconciliationResult().Metrics.MaxRehydratedSources, 2,
		"rehydration must stay bounded by the requested scope")
}

// TestReconcileProviderRootsClaudeDescendantUsesConfiguredTraversal verifies
// the provider-owned gateway: a requested project directory traverses from
// the configured projects root while admission stays inside the descendant.
func TestReconcileProviderRootsClaudeDescendantUsesConfiguredTraversal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	database := openTestDB(t)
	claudeRoot := filepath.Join(t.TempDir(), "claude")
	projectA := filepath.Join(claudeRoot, "projA")
	writeClaudeProjectSession(t, claudeRoot, "projA", "a1")

	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeRoot},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	writeClaudeProjectSession(t, claudeRoot, "projA", "new-a")
	writeClaudeProjectSession(t, claudeRoot, "projB", "new-b")

	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), parser.AgentClaude, []string{projectA},
	))

	admitted, err := database.GetSession(t.Context(), "new-a")
	require.NoError(t, err)
	assert.NotNil(t, admitted,
		"traversal from the configured root must still discover the descendant")
	sibling, err := database.GetSession(t.Context(), "new-b")
	require.NoError(t, err)
	assert.Nil(t, sibling,
		"admission must stay bounded to the requested descendant")
}

// TestReconcileProviderRootsProofBoundedTombstoneWithinDescendant verifies
// both halves of the tombstone guard in one pass: a missing source inside the
// descendant proof is tombstoned while an equally missing source outside the
// proof is neither paged nor touched.
func TestReconcileProviderRootsProofBoundedTombstoneWithinDescendant(t *testing.T) {
	database := openTestDB(t)
	const agent = parser.AgentType("scoped-descendant")
	root := t.TempDir()
	scoped := filepath.Join(root, "scoped")
	other := filepath.Join(root, "other")
	inProof := filepath.Join(scoped, "s-in.jsonl")
	outOfProof := filepath.Join(other, "s-out.jsonl")
	require.NoError(t, os.MkdirAll(scoped, 0o755))
	require.NoError(t, os.MkdirAll(other, 0o755))
	require.NoError(t, os.WriteFile(inProof, []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(outOfProof, []byte("{}\n"), 0o600))
	provider := newScopedStreamingProvider(agent)
	provider.sourcesByRoot[root] = []parser.SourceRef{
		scopedTestSource(agent, inProof),
		scopedTestSource(agent, outOfProof),
	}
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{agent: {root}},
		Machine:   "local",
		ProviderFactories: []parser.ProviderFactory{
			scopedStreamingFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			agent: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), agent, []string{root},
	))

	// Both sources vanish, but only the requested descendant grants proof.
	require.NoError(t, os.Remove(inProof))
	require.NoError(t, os.Remove(outOfProof))
	provider.sourcesByRoot[root] = nil
	rec := &lstatRecorder{}
	engine.lstat = rec.stat

	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), agent, []string{scoped},
	))

	deleted, err := database.GetSessionFull(t.Context(), "s-in")
	require.NoError(t, err)
	assertSourceMissingState(t, deleted)

	survivor, err := database.GetSession(t.Context(), "s-out")
	require.NoError(t, err)
	assert.NotNil(t, survivor,
		"a paged row outside the proof is retained even when its source is gone")
	assert.Zero(t, rec.countUnder(other),
		"ownership paging must stay inside the requested scope")
}

// TestReconcileProviderRootsSharedGatewayTraversesOnce pins the traversal
// grouping: a request naming several descendants under one configured
// gateway walks the gateway once, admitting each descendant against its own
// proof from that single stream.
func TestReconcileProviderRootsSharedGatewayTraversesOnce(t *testing.T) {
	database := openTestDB(t)
	const agent = parser.AgentType("scoped-shared-gateway")
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	firstSource := filepath.Join(first, "s-first.jsonl")
	secondSource := filepath.Join(second, "s-second.jsonl")
	require.NoError(t, os.MkdirAll(first, 0o755))
	require.NoError(t, os.MkdirAll(second, 0o755))
	require.NoError(t, os.WriteFile(firstSource, []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(secondSource, []byte("{}\n"), 0o600))
	provider := newScopedStreamingProvider(agent)
	provider.sourcesByRoot[root] = []parser.SourceRef{
		scopedTestSource(agent, firstSource),
		scopedTestSource(agent, secondSource),
	}
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{agent: {root}},
		Machine:   "local",
		ProviderFactories: []parser.ProviderFactory{
			scopedStreamingFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			agent: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)

	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), agent, []string{first, second},
	))

	assert.Equal(t, int32(1), provider.streamCalls.Load(),
		"descendants sharing a gateway must share one discovery walk")
	for _, id := range []string{"s-first", "s-second"} {
		admitted, err := database.GetSession(t.Context(), id)
		require.NoError(t, err)
		assert.NotNil(t, admitted,
			"each descendant is admitted against its own proof from the shared walk")
	}
}

// TestReconcileProviderRootsUnresolvedRootsAreBoundedNoOp covers the
// negative-space rows: blank, unrelated, remote, and empty requests complete
// before any spool allocation and never widen into other scopes.
func TestReconcileProviderRootsUnresolvedRootsAreBoundedNoOp(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	database := openTestDB(t)
	claudeRoot := filepath.Join(t.TempDir(), "claude")
	writeClaudeProjectSession(t, claudeRoot, "proj", "keep")
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeRoot},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	var spools atomic.Int32
	factory := engine.reconciliationSpoolFactory
	engine.reconciliationSpoolFactory = func(
		path string,
	) (reconciliationSpoolStore, error) {
		spools.Add(1)
		return factory(path)
	}

	for _, tc := range []struct {
		name       string
		roots      []string
		wantRemote int
	}{
		{name: "blank", roots: []string{""}},
		{name: "unrelated", roots: []string{t.TempDir()}},
		{name: "remote", roots: []string{"s3://bucket/prefix"}, wantRemote: 1},
		{name: "empty", roots: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, engine.ReconcileProviderRoots(
				t.Context(), parser.AgentClaude, tc.roots,
			))
			assert.Zero(t, spools.Load(),
				"an unresolved request must complete before spool allocation")
			result := engine.LastReconciliationResult()
			assert.True(t, result.Complete)
			assert.Equal(t, tc.wantRemote, result.Metrics.ExcludedRemoteRoots)
			active, err := database.GetSession(t.Context(), "keep")
			require.NoError(t, err)
			assert.NotNil(t, active,
				"an unresolved request holds no authority over stored sessions")
		})
	}
}

// TestReconcileProviderRootsCaseVariantRootAdmitsAsExact verifies platform
// path equality: on Windows a case-variant spelling of a configured root
// resolves to the exact configured scope instead of an empty pass.
func TestReconcileProviderRootsCaseVariantRootAdmitsAsExact(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("case-variant admission is a Windows filesystem property")
	}
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	database := openTestDB(t)
	claudeRoot := filepath.Join(t.TempDir(), "claude")
	removed := writeClaudeProjectSession(t, claudeRoot, "proj", "old-session")
	variant := strings.ToUpper(claudeRoot)
	require.NotEqual(t, claudeRoot, variant)

	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeRoot},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	require.NoError(t, os.Remove(removed))
	writeClaudeProjectSession(t, claudeRoot, "proj", "new-session")

	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), parser.AgentClaude, []string{variant},
	))

	admitted, err := database.GetSession(t.Context(), "new-session")
	require.NoError(t, err)
	assert.NotNil(t, admitted,
		"a case-variant of the configured root must not produce empty discovery")
	gone, err := database.GetSessionFull(t.Context(), "old-session")
	require.NoError(t, err)
	assertSourceMissingState(t, gone)
}

// scopedStreamingProvider is a multi-root fake whose discovery reflects the
// roots each construction binds, so a failure injected for one root fails
// only the scope the engine built for that root.
type scopedStreamingProvider struct {
	parser.ProviderBase
	sourcesByRoot map[string][]parser.SourceRef
	failRoots     map[string]bool
	// findable maps full session IDs to the source FindSource resolves for
	// them, standing in for a member that lives under another configured
	// root than the one a scoped pass streamed.
	findable map[string]parser.SourceRef
	// streamCalls is pointer-shared across the per-call copies NewProvider
	// returns, so tests observe every discovery walk on one counter.
	streamCalls *atomic.Int32
}

func (p *scopedStreamingProvider) FindSource(
	_ context.Context, req parser.FindSourceRequest,
) (parser.SourceRef, bool, error) {
	if source, ok := p.findable[req.FullSessionID]; ok {
		return source, true, nil
	}
	return parser.SourceRef{}, false, nil
}

func (p *scopedStreamingProvider) DiscoverEach(
	ctx context.Context, yield func(parser.SourceRef) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.streamCalls.Add(1)
	for _, root := range p.Config.Roots {
		if p.failRoots[root] {
			return errors.New("scoped discovery failure")
		}
		for _, source := range p.sourcesByRoot[root] {
			if err := yield(source); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *scopedStreamingProvider) WatchPlan(
	context.Context,
) (parser.WatchPlan, error) {
	return parser.WatchPlan{}, nil
}

func (p *scopedStreamingProvider) SourcesForChangedPath(
	_ context.Context, req parser.ChangedPathRequest,
) ([]parser.SourceRef, error) {
	for _, sources := range p.sourcesByRoot {
		for _, source := range sources {
			if source.DisplayPath == req.Path {
				return []parser.SourceRef{source}, nil
			}
		}
	}
	return nil, nil
}

func (p *scopedStreamingProvider) Fingerprint(
	_ context.Context, source parser.SourceRef,
) (parser.SourceFingerprint, error) {
	return parser.SourceFingerprint{Key: source.FingerprintKey}, nil
}

func (p *scopedStreamingProvider) Parse(
	_ context.Context, req parser.ParseRequest,
) (parser.ParseOutcome, error) {
	path := req.Source.DisplayPath
	started := time.Unix(1704067200, 0)
	return parser.ParseOutcome{
		Results: []parser.ParseResultOutcome{{
			Result: parser.ParseResult{Session: parser.ParsedSession{
				ID:      strings.TrimSuffix(filepath.Base(path), ".jsonl"),
				Agent:   p.Def.Type,
				Project: "project", Machine: "local",
				StartedAt: started, EndedAt: started,
				File: parser.FileInfo{Path: path},
			}},
			DataVersion: parser.DataVersionCurrent,
		}},
		ResultSetComplete: true,
	}, nil
}

type scopedStreamingFactory struct{ provider *scopedStreamingProvider }

func (f scopedStreamingFactory) Definition() parser.AgentDef {
	return f.provider.Definition()
}

func (f scopedStreamingFactory) Capabilities() parser.Capabilities {
	return f.provider.Capabilities()
}

func (f scopedStreamingFactory) NewProvider(
	cfg parser.ProviderConfig,
) parser.Provider {
	// Per-call copy: DiscoverEach walks this construction's roots, and the
	// shared instance must not be written — sync workers construct providers
	// concurrently. The maps and the stream counter stay shared so tests
	// mutate and observe one place.
	provider := *f.provider
	provider.Config = cfg.Clone()
	return &provider
}

func newScopedStreamingProvider(
	agent parser.AgentType,
) *scopedStreamingProvider {
	return &scopedStreamingProvider{
		Def: parser.AgentDef{Type: agent, FileBased: true},
		Caps: parser.Capabilities{Source: parser.SourceCapabilities{
			DiscoverSources:    parser.CapabilitySupported,
			StreamingDiscovery: parser.CapabilitySupported,
			WatchSources:       parser.CapabilitySupported,
			FindSource:         parser.CapabilitySupported,
		}},
		sourcesByRoot: make(map[string][]parser.SourceRef),
		failRoots:     make(map[string]bool),
		findable:      make(map[string]parser.SourceRef),
		streamCalls:   &atomic.Int32{},
	}
}

// TestReconcileProviderRootsScopedVirtualMemberChecksRelocationWhenContainerGone
// pins the relocation guard for a virtual member whose home container is
// itself deleted: the stale spelling resolves nowhere, so no branch-level
// guard fires, and only the shared pre-tombstone check can distinguish a
// member that moved to another configured root from one that is gone. The
// moved member is preserved; a sibling the provider resolves nowhere is
// reclaimed in the same pass.
func TestReconcileProviderRootsScopedVirtualMemberChecksRelocationWhenContainerGone(
	t *testing.T,
) {
	database := openTestDB(t)
	const agent = parser.AgentType("scoped-virtual-relocation")
	rootOne := t.TempDir()
	rootTwo := t.TempDir()
	// The container under rootOne is already gone; its two stored members
	// differ only in whether the provider still resolves them elsewhere.
	container := filepath.Join(rootOne, "traces", "chat.db")
	movedPath := container + "#moved"
	gonePath := container + "#gone"
	for id, path := range map[string]string{
		"moved": movedPath, "gone": gonePath,
	} {
		p := path
		require.NoError(t, database.UpsertSession(db.Session{
			ID: id, Agent: string(agent), Project: "proj",
			Machine: "local", FilePath: &p,
		}))
	}
	require.NoError(t, database.BaselineActiveSessionSourcePaths(
		t.Context(), "local", []db.SessionSourcePath{
			{Agent: string(agent), FilePath: movedPath},
			{Agent: string(agent), FilePath: gonePath},
		},
	))

	provider := newScopedStreamingProvider(agent)
	provider.findable["moved"] = scopedTestSource(
		agent, filepath.Join(rootTwo, "traces", "chat.db")+"#moved",
	)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{agent: {rootOne, rootTwo}},
		Machine:   "local",
		ProviderFactories: []parser.ProviderFactory{
			scopedStreamingFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			agent: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)

	// Only rootOne is requested, so the pass holds no full-root coverage
	// and never streams rootTwo.
	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), agent, []string{rootOne},
	))

	survivor, err := database.GetSession(t.Context(), "moved")
	require.NoError(t, err)
	assert.NotNil(t, survivor,
		"a member the provider resolves under another root is a move")
	gone, err := database.GetSessionFull(t.Context(), "gone")
	require.NoError(t, err)
	assertSourceMissingState(t, gone)
}

func scopedTestSource(agent parser.AgentType, path string) parser.SourceRef {
	return parser.SourceRef{
		Provider: agent, Key: path, DisplayPath: path, FingerprintKey: path,
	}
}

// TestReconcileProviderRootsScopedFailureCommitsHealthySiblingScope pins
// scope-level partial success within one provider: the healthy scope commits
// its proof-bounded tombstone while the failed scope returns only its own
// retry roots.
func TestReconcileProviderRootsScopedFailureCommitsHealthySiblingScope(t *testing.T) {
	database := openTestDB(t)
	const agent = parser.AgentType("scoped-stream")
	rootOne := t.TempDir()
	rootTwo := t.TempDir()
	pathOne := filepath.Join(rootOne, "s1.jsonl")
	pathTwo := filepath.Join(rootTwo, "s2.jsonl")
	require.NoError(t, os.WriteFile(pathOne, []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(pathTwo, []byte("{}\n"), 0o600))
	provider := newScopedStreamingProvider(agent)
	provider.sourcesByRoot[rootOne] = []parser.SourceRef{
		scopedTestSource(agent, pathOne),
	}
	provider.sourcesByRoot[rootTwo] = []parser.SourceRef{
		scopedTestSource(agent, pathTwo),
	}
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{agent: {rootOne, rootTwo}},
		Machine:   "local",
		ProviderFactories: []parser.ProviderFactory{
			scopedStreamingFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			agent: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), agent, []string{rootOne, rootTwo},
	))

	require.NoError(t, os.Remove(pathOne))
	provider.sourcesByRoot[rootOne] = nil
	provider.failRoots[rootTwo] = true

	err := engine.ReconcileProviderRoots(
		t.Context(), agent, []string{rootOne, rootTwo},
	)

	require.Error(t, err)
	var retryErr reconciliationRetryRootError
	require.ErrorAs(t, err, &retryErr)
	assert.ElementsMatch(t, []string{rootTwo},
		retryErr.ReconciliationRetryRoots(),
		"the failed scope retries at the caller's own width")
	gone, getErr := database.GetSessionFull(t.Context(), "s1")
	require.NoError(t, getErr)
	assertSourceMissingState(t, gone)
	survivor, getErr := database.GetSession(t.Context(), "s2")
	require.NoError(t, getErr)
	assert.NotNil(t, survivor, "the failed scope preserves its sessions")
}

// TestReconcileProviderRootsContractViolationFailsScopeClosed verifies the
// fail-closed path: a provider emitting a source outside both traversal and
// proof fails that scope, preserves its sessions, and returns retry roots.
func TestReconcileProviderRootsContractViolationFailsScopeClosed(t *testing.T) {
	database := openTestDB(t)
	const agent = parser.AgentType("scoped-violation")
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	live := filepath.Join(root, "live.jsonl")
	require.NoError(t, os.WriteFile(live, []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(outside, []byte("{}\n"), 0o600))
	provider := newScopedStreamingProvider(agent)
	provider.sourcesByRoot[root] = []parser.SourceRef{
		scopedTestSource(agent, live),
	}
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{agent: {root}},
		Machine:   "local",
		ProviderFactories: []parser.ProviderFactory{
			scopedStreamingFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			agent: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), agent, []string{root},
	))

	require.NoError(t, os.Remove(live))
	provider.sourcesByRoot[root] = []parser.SourceRef{
		scopedTestSource(agent, outside),
	}

	err := engine.ReconcileProviderRoots(t.Context(), agent, []string{root})

	require.Error(t, err)
	var retryErr reconciliationRetryRootError
	require.ErrorAs(t, err, &retryErr)
	assert.ElementsMatch(t, []string{root},
		retryErr.ReconciliationRetryRoots())
	assert.Equal(t, 1, engine.LastReconciliationResult().ProviderFailures)
	preserved, getErr := database.GetSession(t.Context(), "live")
	require.NoError(t, getErr)
	assert.NotNil(t, preserved,
		"a contract violation withholds deletion authority for the scope")
	spooled, getErr := database.GetSession(t.Context(), "outside")
	require.NoError(t, getErr)
	assert.Nil(t, spooled,
		"a source outside traversal and proof must never be spooled")
}

// TestReconcilePartialRequestCoveringAllRootsKeepsFullAuthority is the
// full-coverage preservation row: a partial request naming every configured
// root keeps exactly the authority a full pass has.
func TestReconcilePartialRequestCoveringAllRootsKeepsFullAuthority(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	database := openTestDB(t)
	rootOne := filepath.Join(t.TempDir(), "claude-one")
	rootTwo := filepath.Join(t.TempDir(), "claude-two")
	writeClaudeProjectSession(t, rootOne, "proj", "keep-one")
	removedOne := writeClaudeProjectSession(t, rootOne, "proj", "gone-one")
	writeClaudeProjectSession(t, rootTwo, "proj", "keep-two")
	removedTwo := writeClaudeProjectSession(t, rootTwo, "proj", "gone-two")

	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {rootOne, rootTwo},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 4, engine.SyncAll(t.Context(), nil).Synced)

	require.NoError(t, os.Remove(removedOne))
	require.NoError(t, os.Remove(removedTwo))

	require.NoError(t, engine.ReconcileWatchRoots(
		t.Context(), []string{rootOne, rootTwo}, false,
	))

	for _, id := range []string{"gone-one", "gone-two"} {
		gone, err := database.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		assertSourceMissingState(t, gone)
	}
	for _, id := range []string{"keep-one", "keep-two"} {
		active, err := database.GetSession(t.Context(), id)
		require.NoError(t, err)
		assert.NotNil(t, active)
	}
}

// TestSyncPathsMissingSourceResolvesReplacementAcrossRoots preserves the
// deliberate replacement-index bypass: proving a replacement must span the
// provider's full configured scope even when the pass itself is narrow.
func TestSyncPathsMissingSourceResolvesReplacementAcrossRoots(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	database := openTestDB(t)
	rootOne := filepath.Join(t.TempDir(), "claude-one")
	rootTwo := filepath.Join(t.TempDir(), "claude-two")
	moved := writeClaudeProjectSession(t, rootOne, "proj", "moved-session")
	require.NoError(t, os.MkdirAll(filepath.Join(rootTwo, "proj"), 0o755))

	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {rootOne, rootTwo},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	require.NoError(t, os.Rename(
		moved, filepath.Join(rootTwo, "proj", "moved-session.jsonl"),
	))

	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{moved}))

	active, err := database.GetSession(t.Context(), "moved-session")
	require.NoError(t, err)
	assert.NotNil(t, active,
		"a surviving same-identity copy under another configured root is a replacement")
}

// TestStoredSourceDBHintScopesPreservesFields is the adapter-fidelity row:
// parser scopes convert to database scopes without losing either field.
func TestStoredSourceDBHintScopesPreservesFields(t *testing.T) {
	converted := storedSourceDBHintScopes([]parser.StoredSourceHintScope{
		{Path: "/archive/state.db", IncludeVirtualMembers: true},
		{Path: "/archive/sessions"},
	})
	assert.Equal(t, []db.StoredSourcePathHintScope{
		{Path: "/archive/state.db", IncludeVirtualMembers: true},
		{Path: "/archive/sessions"},
	}, converted)
}
