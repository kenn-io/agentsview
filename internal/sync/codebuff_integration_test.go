package sync_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// writeCodebuffTestFiles creates the three files that make up a Codebuff
// session directory: chat-messages.json, run-state.json, and chat-meta.json.
func writeCodebuffTestFiles(t *testing.T, dir, content string) {
	t.Helper()
	chatPath := filepath.Join(dir, "chat-messages.json")
	runStatePath := filepath.Join(dir, "run-state.json")
	chatMetaPath := filepath.Join(dir, "chat-meta.json")

	require.NoError(t, os.WriteFile(chatPath, []byte(`[
		{"id":"user-1","variant":"user","content":"`+content+`","timestamp":"03:04 PM"}
	]`), 0o644))
	require.NoError(t, os.WriteFile(runStatePath, []byte(`{
		"sessionState": {
			"mainAgentState": {"agentType": "base2-free-deepseek"}
		}
	}`), 0o644))
	require.NoError(t, os.WriteFile(chatMetaPath, []byte(`{
		"messageCount": 1,
		"firstPrompt": "`+content+`",
		"messagesSize": 50
	}`), 0o644))
}

// createCodebuffArchive creates a Codebuff archive with the given number of
// sessions distributed across projects. Returns the root directory.
func createCodebuffArchive(t *testing.T, numSessions int) string {
	t.Helper()
	root := t.TempDir()
	numProjects := 3
	sessionsPerProject := numSessions / numProjects
	if sessionsPerProject == 0 {
		sessionsPerProject = 1
	}

	for p := range numProjects {
		project := fmt.Sprintf("project-%d", p)
		for s := 0; s < sessionsPerProject; s++ {
			ts := fmt.Sprintf("2026-07-15T%02d-00-00.000Z", 10+s)
			dir := filepath.Join(root, project, "chats", ts)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			writeCodebuffTestFiles(t, dir, fmt.Sprintf("Session %d in %s", s, project))
		}
	}
	return root
}

// TestSyncAllCodebuffBoundedPerEventWork verifies that unchanged Codebuff
// sessions are skipped during reconciliation without reading transcript
// bytes. The stat-only freshness gate (providerSourceFreshBeforeFingerprint)
// should prevent the fingerprint from being called for unchanged sources.
func TestSyncAllCodebuffBoundedPerEventWork(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Create a small archive with 6 sessions.
	root := createCodebuffArchive(t, 6)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		Machine: "local",
	})

	// First sync: all sessions should be parsed.
	synced := engine.SyncAll(context.Background(), nil).Synced
	assert.Equal(t, 6, synced, "first sync should parse all 6 sessions")

	// Second sync with no changes: all sessions should be skipped.
	synced = engine.SyncAll(context.Background(), nil).Synced
	assert.Equal(t, 0, synced, "second sync with no changes should skip all sessions")

	// Modify one session's chat-messages.json.
	modifiedDir := filepath.Join(root, "project-0", "chats", "2026-07-15T10-00-00.000Z")
	modifiedChatPath := filepath.Join(modifiedDir, "chat-messages.json")
	require.NoError(t, os.WriteFile(modifiedChatPath, []byte(`[
		{"id":"user-1","variant":"user","content":"Modified message","timestamp":"03:04 PM"}
	]`), 0o644))

	// Touch the file to ensure mtime changes.
	time.Sleep(10 * time.Millisecond)
	now := time.Now()
	require.NoError(t, os.Chtimes(modifiedChatPath, now, now))

	// Third sync: only the modified session should be reparsed.
	synced = engine.SyncAll(context.Background(), nil).Synced
	assert.Equal(t, 1, synced, "third sync should only reparse the modified session")
}

// codebuffFingerprintCountingProvider wraps the real Codebuff Provider so
// tests can observe how many session fingerprint calls the engine actually
// issues. Every other Provider method delegates to the real implementation
// so Discovery, class-changed-path, parse, and the freshness gate itself
// behave exactly as in production; only Fingerprint increments a counter
// before delegating. Per-event work that bypasses the freshness gate —
// such as a regression that re-fingerprints every unchanged session, or
// re-fingerprints sessions belonging to other archive entries — surfaces
// here as a non-zero count rather than as hidden wall-clock growth.
type codebuffFingerprintCountingProvider struct {
	inner parser.Provider
	calls atomic.Int64
}

func (p *codebuffFingerprintCountingProvider) Definition() parser.AgentDef {
	return p.inner.Definition()
}

func (p *codebuffFingerprintCountingProvider) Capabilities() parser.Capabilities {
	return p.inner.Capabilities()
}

func (p *codebuffFingerprintCountingProvider) Discover(
	ctx context.Context,
) ([]parser.SourceRef, error) {
	return p.inner.Discover(ctx)
}

func (p *codebuffFingerprintCountingProvider) WatchPlan(
	ctx context.Context,
) (parser.WatchPlan, error) {
	return p.inner.WatchPlan(ctx)
}

// WatchRoots delegates to the inner provider through a WatchRootPlanner
// type assertion because parser.Provider does not include WatchRoots.
// Without it, engine.providerChangedPathWatchRoots would type-assert the
// wrapper itself (the factory.h.NewProvider return path), fail the
// WatchRootPlanner assertion on a provider that advertises the WatchRoots
// capability, and surface an "unsupported provider feature watch roots"
// error from SyncPathsContext before classification ever runs.
func (p *codebuffFingerprintCountingProvider) WatchRoots(
	ctx context.Context,
) ([]parser.WatchRoot, error) {
	planner, ok := p.inner.(parser.WatchRootPlanner)
	if !ok {
		return nil, parser.UnsupportedProviderFeatureError{
			Provider: p.inner.Definition().Type,
			Feature:  parser.ProviderFeatureWatchRoots,
		}
	}
	return planner.WatchRoots(ctx)
}

func (p *codebuffFingerprintCountingProvider) SourcesForChangedPath(
	ctx context.Context, req parser.ChangedPathRequest,
) ([]parser.SourceRef, error) {
	return p.inner.SourcesForChangedPath(ctx, req)
}

func (p *codebuffFingerprintCountingProvider) FindSource(
	ctx context.Context, req parser.FindSourceRequest,
) (parser.SourceRef, bool, error) {
	return p.inner.FindSource(ctx, req)
}

func (p *codebuffFingerprintCountingProvider) Fingerprint(
	ctx context.Context, src parser.SourceRef,
) (parser.SourceFingerprint, error) {
	p.calls.Add(1)
	return p.inner.Fingerprint(ctx, src)
}

func (p *codebuffFingerprintCountingProvider) Parse(
	ctx context.Context, req parser.ParseRequest,
) (parser.ParseOutcome, error) {
	return p.inner.Parse(ctx, req)
}

func (p *codebuffFingerprintCountingProvider) ParseIncremental(
	ctx context.Context, req parser.IncrementalRequest,
) (parser.IncrementalOutcome, parser.IncrementalStatus, error) {
	return p.inner.ParseIncremental(ctx, req)
}

// codebuffCountingFactory hands out a single prebuilt
// codebuffFingerprintCountingProvider so every Engine.NewProvider call
// observes through the same counter.
type codebuffCountingFactory struct {
	provider parser.Provider
}

func (f codebuffCountingFactory) Definition() parser.AgentDef {
	return f.provider.Definition()
}

func (f codebuffCountingFactory) Capabilities() parser.Capabilities {
	return f.provider.Capabilities()
}

func (f codebuffCountingFactory) NewProvider(parser.ProviderConfig) parser.Provider {
	return f.provider
}

// newCodebuffCountingEngine builds an Engine whose Codebuff provider is
// the prebuilt counting wrapper. The wrapper holds the real Codebuff
// Provider as inner, so behavior matches production; the counter is the
// only observability seam.
func newCodebuffCountingEngine(
	t *testing.T, root string,
) (*sync.Engine, *codebuffFingerprintCountingProvider) {
	t.Helper()
	database := dbtest.OpenTestDB(t)
	innerFactory, ok := parser.ProviderFactoryByType(parser.AgentCodebuff)
	require.True(t, ok, "codebuff factory must be registered")
	inner := innerFactory.NewProvider(parser.ProviderConfig{
		Roots:   []string{root},
		Machine: "local",
	})
	require.NotNil(t, inner)
	provider := &codebuffFingerprintCountingProvider{inner: inner}
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		Machine: "local",
		ProviderFactories: []parser.ProviderFactory{codebuffCountingFactory{
			provider: provider,
		}},
		// classifyProviderChangedPath only runs for agents whose
		// registered mode is ProviderMigrationProviderAuthoritative.
		// Without an explicit override here, the engine falls back to
		// the package-level default map, which classifies Codebuff
		// through the legacy non-authoritative path and skips the
		// changed-path classify loop entirely — disabling the
		// single-path SyncPaths phase of the bounded test.
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentCodebuff: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)
	return engine, provider
}

// TestSyncCodebuffPerEventWorkIsCardinalityIndependent verifies that the per-event
// work for unchanged Codebuff sessions does not scale with archive size.
// Asserting only Synced == 0 leaves an O(archive-size) regression in
// unchanged-session fingerprint reads invisible: re-fingerprinting every
// still-unchanged source costs the same archived counter, but burns
// transcript bytes per session. The freshness gate
// providerSourceFreshBeforeFingerprint is supposed to short-circuit
// every unchanged composite-stat so a warm SyncAll issues zero
// Fingerprint calls, and a single-path SyncPaths issues exactly one for
// the changed source. Counting provider.Fingerprint calls (the only path
// that reads transcript bytes) pins both invariants across a 5x archive
// growth and pins the constancy roborev flagged as missing. The
// single-path SyncPaths phase additionally exercises the watcher-shaped
// changed-path entry point instead of the bulk-discovery path, so a
// regression that scales per-event work with archive size surfaces as
// fingerprint count > 1 only on the larger archive.
func TestSyncCodebuffPerEventWorkIsCardinalityIndependent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	seedPath := filepath.Join(
		"project-0", "chats", "2026-07-15T10-00-00.000Z",
		"chat-messages.json",
	)

	probe := func(root string, numSessions int) {
		engine, codebuff := newCodebuffCountingEngine(t, root)

		// Cold pass: every session needs a fingerprint, so the counter
		// delta equals the archive size. This proves the counting
		// wrapper is wired correctly before the warm/SyncPaths checks
		// below.
		codebuff.calls.Store(0)
		require.Equal(t, numSessions,
			engine.SyncAll(context.Background(), nil).Synced,
			"first cold sync over %d-session archive must parse "+
				"every session", numSessions)
		assert.Equal(t, int64(numSessions), codebuff.calls.Load(),
			"cold sync must call provider.Fingerprint once per "+
				"discovered source")

		// Warm pass: composite-stats still match what was persisted on
		// the cold pass, so providerSourceFreshBeforeFingerprint
		// returns fresh=true for every source and provider.Fingerprint
		// is not called. A regression that lets the freshness gate
		// fall through on unchanged sessions surfaces here as a
		// non-zero call count for either archive — both must remain
		// at zero, and the equality itself is the cardinality-
		// independence check roborev flagged as missing.
		codebuff.calls.Store(0)
		assert.Equal(t, 0,
			engine.SyncAll(context.Background(), nil).Synced,
			"warm SyncAll over %d-session archive must skip "+
				"every unchanged session", numSessions)
		assert.Equal(t, int64(0), codebuff.calls.Load(),
			"warm SyncAll over %d-session archive must not call "+
				"provider.Fingerprint on any unchanged session "+
				"(a non-zero count is a per-archive-size "+
				"regression in the freshness gate or its "+
				"bypass)", numSessions)

		// Single changed-path sync: a watcher-shaped event for one
		// session. The path is unchanged on disk so the freshness
		// gate would short-circuit, but engine.providerChangedPath-
		// ForceParse forces provider.Fingerprint for any direct
		// chat-messages.json event so transcript bytes are
		// guaranteed to be re-read. The fingerprint call here is
		// therefore exactly one. A regression that scales per-event
		// work with archive size (e.g. re-fingerprinting every
		// stored source on every watcher event) would surface here
		// as fingerprint count >= 2 for the larger archive while
		// staying at 1 for the smaller one.
		codebuff.calls.Store(0)
		require.NoError(t, engine.SyncPathsContext(
			context.Background(),
			[]string{filepath.Join(root, seedPath)},
		), "single-path SyncPaths propagates errors that must not be "+
			"silently swallowed (a hidden failure could split the "+
			"small and large archive assertion paths)")
		assert.Equal(t, int64(1), codebuff.calls.Load(),
			"a single-path SyncPaths over %d-session archive "+
				"must call provider.Fingerprint exactly once "+
				"(any larger count means per-event work is "+
				"scaling with archive size)", numSessions)
	}

	probe(createCodebuffArchive(t, 6), 6)
	probe(createCodebuffArchive(t, 30), 30)
}

// codebuffMetaOnlySessionFiles creates a codebuff session
// directory whose chat-messages.json is "[]" while chat-meta.json
// reports a non-zero messageCount and firstPrompt. The parser
// must set CountsAuthoritative=true for this fallback path so the
// engine's per-message reconciliation cannot overwrite the meta
// totals with zero derived from the empty parsed-message slice.
func codebuffMetaOnlySessionFiles(
	t *testing.T, dir string, metaCount int, firstPrompt string,
) {
	t.Helper()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "chat-messages.json"),
		[]byte("[]"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "run-state.json"),
		[]byte(`{
			"sessionState": {
				"mainAgentState": {"agentType": "base2-deepseek"},
				"fileContext": {"cwd": "/initial/cwd"}
			}
		}`),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "chat-meta.json"),
		fmt.Appendf(nil, `{
			"messageCount": %d,
			"firstPrompt": %q,
			"messagesSize": 1024
		}`, metaCount, firstPrompt),
		0o644,
	))
}

// TestSyncCodebuffMetaOnlySessionKeepsCounts pins the regression
// the roborev review identified at internal/parser/codebuff.go:131:
// when a codebuff session's chat-messages.json is empty but
// chat-meta.json reports a non-zero messageCount, the sync engine
// must preserve the meta-derived counts on the row. Without
// CountsAuthoritative=true the engine's per-message
// reconciliation recomputes counts from the empty parsed-message
// slice and overwrites MessageCount with zero, hiding the session
// from any UI that filters on nonzero counts.
//
// Exercise the full sync path (Parse -> db.Session write) so a
// regression that touches any stage — the parser flag, the engine
// reconciliation pass, or the db.Session write — surfaces as
// MessageCount == 0 here.
func TestSyncCodebuffMetaOnlySessionKeepsCounts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	root := t.TempDir()
	project := "codebuff-meta"
	ts := "2026-07-16T00-09-00.236Z"
	sessionDir := filepath.Join(root, project, "chats", ts)
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	codebuffMetaOnlySessionFiles(t, sessionDir, 7, "Alpha prompt")

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(t, 1,
		engine.SyncAll(context.Background(), nil).Synced,
		"meta-only session with non-zero chat-meta.json must sync")

	canonicalID := "codebuff:codebuff-meta:" + ts
	sess, err := database.GetSession(
		context.Background(), canonicalID,
	)
	require.NoError(t, err)
	require.NotNil(t, sess,
		"synced session must persist to the database")
	require.Equal(t, 7, sess.MessageCount,
		"meta-derived counts must survive sync; a 0 here means "+
			"the engine recomputed from the empty parsed-message "+
			"slice and overwrote chat-meta.json's count")
}
