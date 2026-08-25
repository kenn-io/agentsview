package sync_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// createCodebuffSingleSession creates exactly one Codebuff session in
// project-0/chats/<ts>/ and returns the archive root plus the canonical
// chat-messages.json path. New tests that target a single source prefer
// this helper over createCodebuffArchive(t, 1): the legacy helper
// distributes sessions across three projects and produces three
// sessions for any numSessions <= 3, which silently masks single-source
// regressions behind a triple-count expectation.
func createCodebuffSingleSession(t *testing.T) (root, chatPath string) {
	t.Helper()
	root = t.TempDir()
	project := "project-0"
	ts := "2026-07-15T10-00-00.000Z"
	dir := filepath.Join(root, project, "chats", ts)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeCodebuffTestFiles(t, dir, "Single source")
	chatPath = filepath.Join(dir, "chat-messages.json")
	return root, chatPath
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

func (p *codebuffFingerprintCountingProvider) ResolveReconciliationScopes(
	ctx context.Context, req parser.ReconciliationScopeRequest,
) (parser.ReconciliationScopePlan, error) {
	return p.inner.ResolveReconciliationScopes(ctx, req)
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

// ComputeMultiFileStatHash forwards to the inner provider through a
// MultiFileStatHasher type assertion because parser.Provider does not
// include it. Without this forwarding method the wrapper fails the
// interface assertion in buildProviderStatHashers, no hasher is
// registered for Codebuff, preParseStatHash stays nil, and the warm
// pass of the cardinality test satisfies its "zero Fingerprint calls"
// assertion through the legacy size/max-mtime composite arm — a path
// unreachable in the production configuration — instead of the
// per-component digest gate the test pins. Returning 0 on a failed
// assertion keeps miswiring loud: a 0 digest is never persisted to
// provider_freshness, so every warm pass would force fingerprints and
// the zero-call assertion would fail.
func (p *codebuffFingerprintCountingProvider) ComputeMultiFileStatHash(
	chatPath string,
) uint64 {
	hasher, ok := p.inner.(parser.MultiFileStatHasher)
	if !ok {
		return 0
	}
	return hasher.ComputeMultiFileStatHash(chatPath)
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

		// Warm pass: the counting wrapper forwards
		// ComputeMultiFileStatHash, so the engine registered a hasher
		// and the cold pass persisted a per-component digest for every
		// source. Each digest still matches the current stat snapshot,
		// so providerSourceFreshBeforeFingerprint short-circuits
		// through the production digest arm (stored == digest plus
		// providerFreshDigestSourceCurrentInDB) and provider.Fingerprint
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
	require.NotNil(t, sess.StartedAt,
		"meta-only session must have a StartedAt timestamp; nil means "+
			"the parser left it unset and analytics would fall back to "+
			"import-time created_at, incorrectly dating historical sessions")
	require.NotEmpty(t, *sess.StartedAt,
		"StartedAt must be non-empty for a meta-only session")
	require.NotNil(t, sess.EndedAt,
		"meta-only session must have an EndedAt timestamp; nil means "+
			"the parser left it unset")
	require.NotEmpty(t, *sess.EndedAt,
		"EndedAt must be non-empty for a meta-only session")
}

// TestSyncCodebuffMetaOnlyDriftReparsesSession pins the roborev LOW
// carryover at internal/parser/codebuff_provider.go:194 and the matching
// freshness gate in internal/sync/engine.go around line 12253: a future
// regression that drops chat-meta.json from the composite stat would
// silently orphan rows whose only on-disk change is a meta touch. The
// stat-only freshness gate must observe the meta-only mtime bump and
// reparse exactly one session while leaving the other five unchanged.
//
// chat-messages.json and run-state.json stay on disk untouched.
func TestSyncCodebuffMetaOnlyDriftReparsesSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	root := createCodebuffArchive(t, 6)
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(t, 6,
		engine.SyncAll(context.Background(), nil).Synced,
		"cold sync must parse every discovered session")

	targetDir := filepath.Join(
		root, "project-0", "chats", "2026-07-15T10-00-00.000Z",
	)
	metaPath := filepath.Join(targetDir, "chat-meta.json")
	require.NoError(t, os.WriteFile(metaPath, []byte(`{
		"messageCount": 1,
		"firstPrompt": "Meta-drift prompt",
		"messagesSize": 4096
	}`), 0o644))
	time.Sleep(10 * time.Millisecond)
	bump := time.Now()
	require.NoError(t, os.Chtimes(metaPath, bump, bump))

	assert.Equal(t, 1,
		engine.SyncAll(context.Background(), nil).Synced,
		"chat-meta.json-only drift must reparse exactly one "+
			"session; a zero here means the freshness composite "+
			"stat dropped chat-meta.json and the meta-only mtime "+
			"bump passed the gate as unchanged, and a value "+
			"above one means the composite double-counted the "+
			"shared session")
}

// TestSyncCodebuffRunStateOnlyDriftReparsesSession pins the run-state.json
// leg of the freshness composite trio at internal/parser/codebuff_provider.go:196
// and engine.go around line 12260: a future regression that drops
// run-state.json from the composite stat would silently orphan rows whose
// only on-disk change is a run-state touch (e.g. when the upstream agent
// accumulates credits without appending to chat-messages.json). The
// stat-only freshness gate must observe the run-state-only mtime bump
// and reparse exactly one session while leaving the other five unchanged.
//
// chat-messages.json and chat-meta.json stay on disk untouched.
func TestSyncCodebuffRunStateOnlyDriftReparsesSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	root := createCodebuffArchive(t, 6)
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(t, 6,
		engine.SyncAll(context.Background(), nil).Synced,
		"cold sync must parse every discovered session")

	targetDir := filepath.Join(
		root, "project-0", "chats", "2026-07-15T10-00-00.000Z",
	)
	runStatePath := filepath.Join(targetDir, "run-state.json")
	require.NoError(t, os.WriteFile(runStatePath, []byte(`{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek",
				"creditsUsed": 200
			},
			"fileContext": {"cwd": "/initial/cwd"}
		}
	}`), 0o644))
	time.Sleep(10 * time.Millisecond)
	bump := time.Now()
	require.NoError(t, os.Chtimes(runStatePath, bump, bump))

	assert.Equal(t, 1,
		engine.SyncAll(context.Background(), nil).Synced,
		"run-state.json-only drift must reparse exactly one "+
			"session; a zero here means the freshness composite "+
			"stat dropped run-state.json and the run-state-only "+
			"mtime bump passed the gate as unchanged, and a value "+
			"above one means the composite double-counted the "+
			"shared session")
}

// TestSyncCodebuffMetaAndRunStateCompositeDriftReparsesSession exercises
// freshness composite stat at the same time as each individual leg:
// mutating BOTH chat-meta.json AND run-state.json (chat-messages.json
// untouched) between two syncs must reparse exactly one session. The
// composite stat folds each leg's size and max mtime via sum/max, so
// the realistic failure modes for this test are:
//
//   - "missed both fresh mtime bumps": max(mtime) returned false
//     despite both legs bumping, surfacing as Synced == 0. (A
//     single missed leg would still advance the max — the
//     meta-only and run-state-only tests catch that case more
//     directly.)
//   - "double-count the shared session": the composite emits one
//     re-parse per bumped file, returning Synced >= 2.
//
// Both legs reference the SAME session directory, so they must
// de-duplicate to a single re-parse. The shared mtime bump between
// the two files removes a subtle flake risk if the host filesystem
// has a coarser-than-10ms mtime resolution.
func TestSyncCodebuffMetaAndRunStateCompositeDriftReparsesSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	root := createCodebuffArchive(t, 6)
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(t, 6,
		engine.SyncAll(context.Background(), nil).Synced,
		"cold sync must parse every discovered session")

	targetDir := filepath.Join(
		root, "project-0", "chats", "2026-07-15T10-00-00.000Z",
	)
	metaPath := filepath.Join(targetDir, "chat-meta.json")
	runStatePath := filepath.Join(targetDir, "run-state.json")
	require.NoError(t, os.WriteFile(metaPath, []byte(`{
		"messageCount": 1,
		"firstPrompt": "Composite-drift prompt",
		"messagesSize": 8192
	}`), 0o644))
	require.NoError(t, os.WriteFile(runStatePath, []byte(`{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek",
				"creditsUsed": 500
			},
			"fileContext": {"cwd": "/initial/cwd"}
		}
	}`), 0o644))
	time.Sleep(10 * time.Millisecond)
	bump := time.Now()
	require.NoError(t, os.Chtimes(metaPath, bump, bump))
	require.NoError(t, os.Chtimes(runStatePath, bump, bump))

	assert.Equal(t, 1,
		engine.SyncAll(context.Background(), nil).Synced,
		"composite (chat-meta + run-state) drift must reparse "+
			"exactly one session; a zero here means the freshness "+
			"composite missed BOTH bumped files (each leg is "+
			"folded via max mtime, so a single missing leg would "+
			"still let the other advance the max), and a value "+
			"above one means the composite double-counted the "+
			"shared session")
}

// TestSyncCodebuffCompanionFileDeletionReparsesSession verifies the
// directory-mtime cutoff signal for Codebuff sessions: when a companion
// file (run-state.json or chat-meta.json) is deleted from a session
// directory, the surviving files' mtimes may predate the cutoff, but the
// directory mtime changes on deletion. The stat-only freshness gate must
// observe the directory mtime bump and reparse exactly one session while
// leaving the other five unchanged.
//
// This covers the fix at internal/sync/engine.go discoveredFileEffective-
// Mtime: the Codebuff branch now considers the session directory mtime
// as a local cutoff signal, consistent with the Kilo Legacy branch, to
// detect companion-file deletions that would otherwise be invisible to
// the individual-file mtime composite.
func TestSyncCodebuffCompanionFileDeletionReparsesSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	root := createCodebuffArchive(t, 6)
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(t, 6,
		engine.SyncAll(context.Background(), nil).Synced,
		"cold sync must parse every discovered session")

	// Warm sync: all sessions unchanged, so all should be skipped.
	assert.Equal(t, 0,
		engine.SyncAll(context.Background(), nil).Synced,
		"warm sync must skip all unchanged sessions")

	// Delete run-state.json from one session directory. Deleting a
	// file changes the directory's mtime even though surviving files'
	// mtimes are unchanged. The freshness gate must observe this
	// directory mtime bump and reparse the session.
	targetDir := filepath.Join(
		root, "project-0", "chats", "2026-07-15T10-00-00.000Z",
	)
	runStatePath := filepath.Join(targetDir, "run-state.json")
	require.NoError(t, os.Remove(runStatePath),
		"deleting run-state.json must succeed")
	time.Sleep(10 * time.Millisecond)

	// Re-sync: exactly the session whose companion was deleted should
	// be reparsed. A value of zero means the directory mtime signal
	// was not picked up (the companion-file deletion was invisible to
	// the freshness gate), and a value above one means the composite
	// double-counted or other sessions were affected.
	assert.Equal(t, 1,
		engine.SyncAll(context.Background(), nil).Synced,
		"deleting run-state.json must trigger reparse of exactly "+
			"one session via the directory mtime cutoff signal; a "+
			"zero means the freshness gate missed the deletion, and "+
			"a value above one means it over-counted")
}

// TestSyncCodebuffProviderStatHashSideTable pins Issue 1 (engine wire-up)
// at engine-side behavior: a successful cold sync against the default
// codebuff factory must populate provider_freshness for every persisted
// source. A regression that drops the SourceSetProvider forwarding
// method would leave provider_freshness empty here, since the engine's
// MultiFileStatHasher type assertion would not surface the inner
// codebuffSourceSet through the wrapper.
func TestSyncCodebuffProviderStatHashSideTable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	database := dbtest.OpenTestDB(t)
	root, chatPath := createCodebuffSingleSession(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(t, 1,
		engine.SyncAll(context.Background(), nil).Synced,
		"cold sync must parse the seeded codebuff session")
	hashed, hasHash, err := database.GetProviderStatHash(
		context.Background(), parser.AgentCodebuff, chatPath,
	)
	require.NoError(t, err)
	require.True(t, hasHash,
		"a successful cold sync must populate provider_freshness "+
			"for the registered codebuff source; a missing row "+
			"means the Engine's MultiFileStatHasher type "+
			"assertion failed to surface the SourceSetProvider "+
			"wrapping the codebuffSourceSet")
	require.NotZero(t, hashed,
		"the staged digest must be non-zero so a later warm pass "+
			"can short-circuit on a matching snapshot")

	// Warm sync with no changes leaves the side-table intact and
	// skips the source.
	require.Equal(t, 0,
		engine.SyncAll(context.Background(), nil).Synced,
		"warm sync over an unchanged codebuff source must skip "+
			"the source via the per-component digest short-circuit")
	hashedAgain, hasHashAgain, err := database.GetProviderStatHash(
		context.Background(), parser.AgentCodebuff, chatPath,
	)
	require.NoError(t, err)
	require.True(t, hasHashAgain)
	require.Equal(t, hashed, hashedAgain,
		"the side-table digest must be stable across an unchanged "+
			"warm sync; a divergence here means the digest "+
			"pre-check is hashing inconsistent inputs across runs")
}

// TestSyncCodebuffProviderStatHashSiblingDriftForcesReparse pins Issue 1
// at engine-side reparse behavior: a same-size sibling-file rewrite
// whose mtime stays strictly below the chat file's mtime must change the
// per-component digest enough to force provider.Fingerprint on the next
// sync. Without the per-component digest the legacy size/mtime composite
// would see size unchanged and max-mtime unchanged, falsely short-circuit
// the source and retain stale metadata, costs, or lifecycle state on the
// stored row. A regression that drops the hash lookup entirely (Issue 1)
// lets the source continue to skip; a regression that hashes the wrong
// inputs (the rewritten logical key instead of the physical file, Issue 3)
// sees the digest stay constant and the warm sync also continues to skip.
func TestSyncCodebuffProviderStatHashSiblingDriftForcesReparse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	database := dbtest.OpenTestDB(t)
	root, chatPath := createCodebuffSingleSession(t)
	runStatePath := filepath.Join(filepath.Dir(chatPath), "run-state.json")
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(t, 1,
		engine.SyncAll(context.Background(), nil).Synced,
		"cold sync must parse the seeded session")

	// Hold chat's mtime as the max; rewrite run-state.json with the
	// same byte length so size+max-mtime composite would stay
	// identical, but use a new mtime strictly below chatTime and
	// different content so the per-component digest must change.
	// Both bodies must be the same byte length so a size-only
	// reduction cannot detect the drift; the assertion specifically
	// pins the per-component digest path (Issue 1) -- if a future
	// composite change folds sibling mtimes into the max, the legacy
	// size/mtime composite would also catch this drift and the test
	// would pass via the wrong observation. The matching one-byte
	// swap of `"unusedC":"0"` -> `"unusedC":"9"` keeps the byte
	// length exactly equal while still changing the content SHA256.
	runStateBody := `{"sessionState":{"mainAgentState":{"agentType":"base2-free-deepseek","unusedA":"0","unusedB":"0","unusedC":"0"}}}`
	runStateBodyRewritten := strings.Replace(
		runStateBody, `"unusedC":"0"`, `"unusedC":"9"`, 1,
	)
	require.Equal(t, len(runStateBody), len(runStateBodyRewritten),
		"rewritten run-state body length must match exactly so "+
			"the sum-of-sizes and max-mtime composite stay "+
			"constant between the two syncs")
	chatTime := time.Date(2026, time.July, 4, 12, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(chatPath, chatTime, chatTime))

	// Sub-max mtime under the existing chat-messages.json max.
	time.Sleep(10 * time.Millisecond)
	subMaxTime := chatTime.Add(-1 * time.Minute)
	require.NoError(t, os.WriteFile(runStatePath, []byte(runStateBodyRewritten), 0o644))
	require.NoError(t, os.Chtimes(runStatePath, subMaxTime, subMaxTime))

	// After the rewrite the warm sync must reparse the session.
	// Without the per-component fix the engine's max-mtime
	// composite would stay at chatTime and skip the source,
	// leaving a stale row.
	require.Equal(t, 1,
		engine.SyncAll(context.Background(), nil).Synced,
		"a same-size sibling rewrite with a sub-max mtime must "+
			"force provider.Fingerprint on the next warm sync; "+
			"a zero here means Issue 1 or Issue 3 is broken and "+
			"the engine kept skipping with a stale digest")
}

// TestSyncCodebuffProviderStatHashRemoteStoresUnderLogicalKey pins Issue 3
// at end-to-end behavior: a remote-import sync that wires the engine
// with a pathRewriter mapping the on-disk chat-messages.json to a
// canonical "host:/remote/path" key must persist the per-component
// freshness digest under the logical key, not the physical file. A
// regression that hashes the rewritten key (Issue 3) would compute a
// zero-value digest because the logical path is not stat-able on the
// local filesystem, then falsely report freshness forever.
//
// The test does not need a real remote end -- the engine reads files
// from the configured root, applies the rewriter at write time, and
// stores the side-table entry under whatever key the rewriter returns.
// We observe behavior through database.GetProviderStatHash directly,
// and additionally mutate the materialized file after the cold sync
// to prove the digest is computed from the physical on-disk file
// (not the logical path that would stat-empty).
func TestSyncCodebuffProviderStatHashRemoteStoresUnderLogicalKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	database := dbtest.OpenTestDB(t)
	root, _ := createCodebuffSingleSession(t)
	const rewritePrefix = "hosts~/remote/"
	logicalChat := rewritePrefix + "project-0/chats/2026-07-15T10-00-00.000Z/chat-messages.json"
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		// Production remote sync (importEngineConfig) always pairs
		// IDPrefix with PathRewriter: applyRemoteRewrites only rewrites
		// the stored session file_path when a host prefix is configured,
		// so the sessions table row lands under the same logical key the
		// provider_freshness side-table uses. Without the prefix the row
		// keeps the physical temp path and the digest's logical key has
		// no matching session row.
		IDPrefix: "remote-host~",
		Machine:  "local",
		PathRewriter: func(p string) string {
			if !strings.HasPrefix(p, root) {
				return p
			}
			trimmed := strings.TrimPrefix(p, root+string(filepath.Separator))
			return rewritePrefix + filepath.ToSlash(trimmed)
		},
	})
	t.Cleanup(engine.Close)

	require.Equal(t, 1,
		engine.SyncAll(context.Background(), nil).Synced,
		"remote-import engine must sync with the path rewriter")

	// The side-table row must be keyed by the logical (rewritten)
	// path, not the physical chat-messages.json file. A non-nil
	// digest under the physical key would mean the engine hashed
	// the wrong path; a missing digest under the logical key would
	// mean the engine did not honor the rewriter at all.
	logicalHash, hasLogical, err := database.GetProviderStatHash(
		context.Background(), parser.AgentCodebuff, logicalChat,
	)
	require.NoError(t, err)
	require.True(t, hasLogical,
		"a successful remote-import sync must persist the "+
			"per-component digest under the rewritten logical "+
			"key; a missing row means Issue 3 is broken and the "+
			"engine either hashed the wrong path or dropped the "+
			"rewriter at the write site")
	require.NotZero(t, logicalHash,
		"logical-key digest must be non-zero; a zero means the "+
			"engine hashed the logical path itself rather than the "+
			"physical materialized file (which is not stat-able "+
			"locally)")

	// The actual physical-vs-logical hash invariant: mutate the
	// materialized chat-messages.json after the cold sync and confirm
	// the warm sync's digest differs from the cold sync's digest. If
	// Issue 3 ever regressed to hashing the logical key, this
	// warm-pass digest would either stay constant (because every
	// logical-path stat fails) or match a missing-file pattern that
	// SHA-comparing the logical path never reaches.
	require.Equal(t, 0,
		engine.SyncAll(context.Background(), nil).Synced,
		"warm sync over an unchanged materialized file must skip "+
			"via the per-component digest short-circuit")
	rewriteMaterializedChat := filepath.Join(root, "project-0", "chats",
		"2026-07-15T10-00-00.000Z", "chat-messages.json")
	require.NoError(t, os.WriteFile(rewriteMaterializedChat, []byte(
		`[{"id":"u1","variant":"user","content":"hi","timestamp":"03:04 PM"},
        {"id":"u2","variant":"user","content":"there","timestamp":"03:05 PM"}]`,
	), 0o644))
	time.Sleep(10 * time.Millisecond)
	bump := time.Now()
	require.NoError(t, os.Chtimes(rewriteMaterializedChat, bump, bump))
	require.Equal(t, 1,
		engine.SyncAll(context.Background(), nil).Synced,
		"mutating the materialized chat-messages.json must "+
			"trigger a reparse via the per-component digest; a "+
			"zero means Issue 3 is regressing to logical-path "+
			"hashing where stat would always miss the file")
	logicalHashAfter, hasLogicalAfter, err := database.GetProviderStatHash(
		context.Background(), parser.AgentCodebuff, logicalChat,
	)
	require.NoError(t, err)
	require.True(t, hasLogicalAfter)
	require.NotEqual(t, logicalHash, logicalHashAfter,
		"post-mutation digest must differ from the pre-mutation "+
			"digest; equality means the engine is hashing something "+
			"other than the materialized file (the original Issue 3 "+
			"regression)")
}

// TestSyncCodebuffCwdFilteredSourceDoesNotPersistStatHash pins Issue 2
// at end-to-end behavior: a Codebuff source that the engine parses
// and discovers but then CWD-filters before write must NOT land its
// per-component digest in provider_freshness. The staged digest is
// dropped explicitly in flushPending whenever outcome.written[i] is
// false, so a CWD-filtered source row (or any row whose session
// upsert failed) cannot escape the gate and mark an absent session
// as fresh on the next warm pass. A regression that re-flattens the
// per-row gate (removes the !outcome.written[i] check, or persists
// before the session write commits) surfaces here as a non-nil
// digest for the CWD-filtered source.
//
// The asserted invariants are:
//   - StagedProviderStatHashes() == 1 -- the per-component digest
//     staging block in applyProviderFilePathPolicies actually ran
//     for the discovered source. This distinguishes a regression
//     that drops just the staging block (which would otherwise
//     silently pass any hasStored==false check via
//     flushPending's no-op nil skip) from a regression that drops
//     the whole gate.
//   - SyncAll returns zero synced -- the source row was
//     CWD-filtered at the write seam.
//   - provider_freshness carries no row for the chat path -- the
//     per-row gate suppressed digest persist despite a non-nil
//     staged hash.
func TestSyncCodebuffCwdFilteredSourceDoesNotPersistStatHash(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	database := dbtest.OpenTestDB(t)
	root, chatPath := createCodebuffSingleSession(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		IncludeCwdPrefixes: []string{
			"/this/prefix/cannot/match/the/seeded/archive",
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	engine.ResetStagedProviderStatHashes()
	require.Equal(t, 0,
		engine.SyncAll(context.Background(), nil).Synced,
		"CWD-prefix mismatch must CWD-filter every discovered "+
			"source; a non-zero synced count means the test "+
			"prefix accidentally matches the seeded archive")
	require.Equal(t, int64(1), engine.StagedProviderStatHashes(),
		"the per-component digest staging block must have "+
			"run once for the discovered source even though "+
			"the CWD filter rejects its session write; "+
			"a zero here means applyProviderFilePathPolicies "+
			"dropped its staging call and the hasStored "+
			"assertion below pins nothing about the gate")
	_, hasStored, err := database.GetProviderStatHash(
		context.Background(), parser.AgentCodebuff, chatPath,
	)
	require.NoError(t, err)
	require.False(t, hasStored,
		"provider_freshness must NOT be populated when the "+
			"source row was CWD-filtered despite a non-nil "+
			"staged digest; a non-nil row here means the "+
			"per-row gate that suppresses digest persist "+
			"(Issue 2) regressed")
}

// TestSyncCodebuffMissingSourceClearsProviderStatHash verifies that removing a
// Codebuff source marks the archived session source-missing and invalidates its
// provider freshness. The archived transcript remains readable, while a later
// byte-identical restoration is forced through parsing so it can clear the
// source-missing state.
//
// ReconcileProviderRoots is the production-realistic trigger for
// tombstoneMissingWatchSourcesForAgentLocked: it iterates the
// archived ownership records for the agent's roots, checks each
// against the filesystem, and routes any provably-missing source
// through tombstoneSessionSourceOwnership. SyncPathsContext has
// the same end-of-pass tombstone path but with single-file
// granularity and the remove-event filter that drops missing
// non-persistent sources — which means a SyncPathsContext test
// would never reach the tombstone, only mark the source via the
// in-memory skip cache.
func TestSyncCodebuffMissingSourceClearsProviderStatHash(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	database := dbtest.OpenTestDB(t)
	root, chatPath := createCodebuffSingleSession(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(t, 1,
		engine.SyncAll(context.Background(), nil).Synced,
		"cold sync must parse the seeded Codebuff session")
	_, hasBefore, err := database.GetProviderStatHash(
		context.Background(), parser.AgentCodebuff, chatPath,
	)
	require.NoError(t, err)
	require.True(t, hasBefore,
		"a successful cold sync must populate provider_freshness "+
			"as the precondition for the tombstone clear")

	// Remove the entire session directory so reconcile's lstat check
	// sees a fully missing source. Reconcile iterates archived
	// ownership, calls lstat on each, and tombstones any source that
	// is gone without container fallback.
	require.NoError(t, os.RemoveAll(filepath.Dir(chatPath)),
		"deleting the session directory must succeed; partial "+
			"deletions leave companion files for the engine to "+
			"still consider the source present")

	_, _, err = engine.ReconcileWatchRootsWithStats(
		context.Background(), []string{root}, true, nil,
	)
	require.NoError(t, err,
		"reconcile must complete; an error here would mask whether "+
			"the tombstone path was actually reached")

	// The shared fixture declares a free-tier agent in run-state.json, so the
	// stored session uses the Freebuff identity even though the owning provider
	// and provider_freshness key are Codebuff.
	const sessionID = "freebuff:project-0:2026-07-15T10-00-00.000Z"
	full, err := database.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err)
	assertSourceMissingState(t, full)
	sess, err := database.GetSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess, "a missing source must not hide the archived session")
	assertSessionMessageCount(t, database, sessionID, 1)

	_, hasAfter, err := database.GetProviderStatHash(
		context.Background(), parser.AgentCodebuff, chatPath,
	)
	require.NoError(t, err)
	require.False(t, hasAfter,
		"tombstoning must clear provider_freshness; a non-nil row "+
			"after this means Issue 3 regressed and a future "+
			"byte-identical restore would falsely mark the source as "+
			"fresh")
}

// TestSyncCodebuffColdStartForcesFingerprintUntilStamped pins Issue 4a:
// when provider_freshness carries no row for a discovered Codebuff
// source (cold-start before the column is populated, or the
// post-tombstone state), the freshness gate must force a real
// fingerprint instead of falling through to the legacy size/max-mtime
// composite. The composite would short-circuit a coincidental
// size/mtime match and leave provider_freshness permanently empty,
// so the per-component gate never engages and a subsequent real
// content change could go undetected. After a forced fingerprint
// the staging block stamps the digest, so the warm sync must
// repopulate the row.
func TestSyncCodebuffColdStartForcesFingerprintUntilStamped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	database := dbtest.OpenTestDB(t)
	root, chatPath := createCodebuffSingleSession(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(t, 1,
		engine.SyncAll(context.Background(), nil).Synced,
		"cold sync must parse the seeded session")
	_, hasBefore, err := database.GetProviderStatHash(
		context.Background(), parser.AgentCodebuff, chatPath,
	)
	require.NoError(t, err)
	require.True(t, hasBefore,
		"first sync must populate provider_freshness as the "+
			"precondition to clearing it post-cold-warm")

	// Manual delete simulates the post-tombstone / cold-warm
	// transition. A correct implementation forces provider.Fingerprint
	// on the next SyncAll because the freshness gate sees !hasStored;
	// the fingerprint is then verified against the existing session row
	// and the DB-confirmed unchanged skip (providerSourceUnchangedInDB)
	// re-stamps the digest via stampProviderStatHashForConfirmedSource.
	// No digest may be persisted before fingerprinting, parsing, or
	// writing succeeds, so the re-stamp must ride on that confirmed
	// skip rather than a pre-parse cold-stamp.
	require.NoError(t, database.DeleteProviderStatHash(
		context.Background(), parser.AgentCodebuff, chatPath,
	))
	_, hasCleared, err := database.GetProviderStatHash(
		context.Background(), parser.AgentCodebuff, chatPath,
	)
	require.NoError(t, err)
	require.False(t, hasCleared,
		"DeleteProviderStatHash must take effect so the cold-warm "+
			"path is exercised next")

	engine.SyncAll(context.Background(), nil)

	_, hasAfter, err := database.GetProviderStatHash(
		context.Background(), parser.AgentCodebuff, chatPath,
	)
	require.NoError(t, err)
	require.True(t, hasAfter,
		"a warm SyncAll with provider_freshness cleared must "+
			"re-stamp the digest; a missing row here means the "+
			"per-component gate fell through to the legacy "+
			"size/mtime composite and the cold-start branch was "+
			"never forced through provider.Fingerprint")
}

// TestSyncCodebuffSameSizeSameMtimeRewriteIsDetected pins the
// roborev-medium finding: the provider_freshness digest folds in ctime, so a
// same-size companion rewrite that preserves (or coarse-grains) mtime still
// changes the digest and forces provider.Fingerprint, whose SHA-256 content
// hash then detects the rewrite. The old (size, mtime)-only digest would
// match and short-circuit the source, leaving classification, costs, or
// metadata stale despite FingerprintHashRequiredForFreshness.
func TestSyncCodebuffSameSizeSameMtimeRewriteIsDetected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	database := dbtest.OpenTestDB(t)
	root, chatPath := createCodebuffSingleSession(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(t, 1,
		engine.SyncAll(context.Background(), nil).Synced,
		"cold sync must parse the seeded Codebuff session")

	// Digest-level pin. Rewrite run-state.json with byte-identical length
	// but different content, then restore its mtime so only ctime (and
	// content) change. A digest over (size, mtime) alone would be unchanged;
	// the ctime term must move it.
	dir := filepath.Dir(chatPath)
	runStatePath := filepath.Join(dir, "run-state.json")
	original, err := os.ReadFile(runStatePath)
	require.NoError(t, err)
	info, err := os.Stat(runStatePath)
	require.NoError(t, err)
	originalMtime := info.ModTime()

	hasher := engine.ProviderStatHasher(parser.AgentCodebuff)
	require.NotNil(t, hasher,
		"Codebuff must register a MultiFileStatHasher")
	digestBefore := hasher.ComputeMultiFileStatHash(chatPath)
	require.NotZero(t, digestBefore)

	replacement := bytes.Replace(
		original,
		[]byte("base2-free-deepseek"),
		[]byte("base3-free-deepseek"),
		1,
	)
	require.Equal(t, len(original), len(replacement),
		"the rewrite must preserve byte length so only ctime "+
			"distinguishes the new content")
	require.NotEqual(t, original, replacement,
		"the rewrite must change content so the fingerprint hash "+
			"can detect it")

	time.Sleep(10 * time.Millisecond) // distinct ctime tick across platforms
	require.NoError(t, os.WriteFile(runStatePath, replacement, 0o644))
	require.NoError(t, os.Chtimes(runStatePath, originalMtime, originalMtime))

	digestAfter := hasher.ComputeMultiFileStatHash(chatPath)
	assert.NotEqual(t, digestBefore, digestAfter,
		"the ctime term must fold into the digest so a same-size, "+
			"mtime-preserved rewrite is not invisible to the freshness gate")

	// End-to-end pin: the digest mismatch forces provider.Fingerprint,
	// whose content hash detects the rewrite, so the session must be
	// reparsed and rewritten rather than skipped. This is the load-bearing
	// assertion — the third-sync check below is only meaningful after the
	// rewrite was actually synced, so a regression here surfaces as a single
	// clean failure. Note the ctime term comes from codexIndexChangeTime,
	// which returns non-zero only on darwin/linux/windows (the project's CI
	// matrix); on other platforms the digest cannot move and this test would
	// not be representative.
	second := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, second.Synced,
		"a same-size, mtime-preserved companion rewrite must be "+
			"detected and re-synced; a skip means the freshness gate "+
			"never re-verified the content")

	// The re-stamped digest must now short-circuit the unchanged source.
	third := engine.SyncAll(context.Background(), nil)
	assert.Zero(t, third.Synced,
		"after the rewrite is synced the digest must match again and "+
			"the source must short-circuit")
}

// TestSyncCodebuffIncrementalCutoffDetectsCtimeDrift pins the
// roborev-medium finding: discoveredFileEffectiveMtime for Codebuff
// must include ctime in the cutoff signal. A same-size companion
// rewrite with restored mtime changes ctime but not mtime, so an
// mtime-only cutoff would drop the source before the full freshness
// check (providerSourceFreshBeforeFingerprint) can detect the rewrite.
// The ctime-inclusive cutoff must advance past the SyncAllSince
// parameter so the source reaches the per-component digest gate, which
// then forces a full fingerprint and detects the content drift.
func TestSyncCodebuffIncrementalCutoffDetectsCtimeDrift(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	database := dbtest.OpenTestDB(t)
	root, chatPath := createCodebuffSingleSession(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(t, 1,
		engine.SyncAll(context.Background(), nil).Synced,
		"cold sync must parse the seeded Codebuff session")

	// Confirm the warm pass skips the unchanged source.
	require.Equal(t, 0,
		engine.SyncAll(context.Background(), nil).Synced,
		"warm SyncAll must skip the unchanged source")

	// Rewrite run-state.json with byte-identical length but different
	// content, then restore its mtime so only ctime distinguishes the
	// new content. An mtime-only cutoff (the old behavior) would drop
	// this source because max(mtime) is unchanged.
	dir := filepath.Dir(chatPath)
	runStatePath := filepath.Join(dir, "run-state.json")
	original, err := os.ReadFile(runStatePath)
	require.NoError(t, err)
	info, err := os.Stat(runStatePath)
	require.NoError(t, err)
	originalMtime := info.ModTime()

	replacement := bytes.Replace(
		original,
		[]byte("base2-free-deepseek"),
		[]byte("base3-free-deepseek"),
		1,
	)
	require.Equal(t, len(original), len(replacement),
		"rewrite must preserve byte length so mtime is the only "+
			"pre-ctime signal")

	time.Sleep(10 * time.Millisecond)
	// Use a cutoff anchored between the cold-sync mtimes and the
	// rewrite ctime: the ctime-inclusive cutoff must see the source
	// as fresh even though mtime alone would not.
	cutoff := time.Now()
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(runStatePath, replacement, 0o644))
	require.NoError(t, os.Chtimes(runStatePath, originalMtime, originalMtime))

	stats := engine.SyncAllSince(context.Background(), cutoff, nil)
	require.Equal(t, 1, stats.Synced,
		"SyncAllSince must pick up a same-size, mtime-preserved "+
			"companion rewrite via the ctime-inclusive cutoff; "+
			"a zero here means the cutoff saw only max(mtime) and "+
			"dropped the source before the fingerprint could detect it")

	// The re-stamped digest must then short-circuit the unchanged source.
	warm := engine.SyncAll(context.Background(), nil)
	assert.Zero(t, warm.Synced,
		"after the rewrite is synced the digest must match again")
}

// TestSyncCodebuffSingleSessionWritesProviderStatHash pins Issue 4b:
// a single-session sync (SyncSingleSession) that writes through
// writeSessionFull must also persist the staged
// res.providerStatHash, mirroring the per-row persist gate in
// flushPending. Without this, single-session syncs leave
// provider_freshness empty for Codebuff/Freebuff and the next
// warm pass short-circuits on a coincidental size/mtime match
// before any digest is stamped. A regression that drops the
// per-row gate for the single-session path surfaces here as a
// missing digest after SyncSingleSession.
func TestSyncCodebuffSingleSessionWritesProviderStatHash(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	database := dbtest.OpenTestDB(t)
	root, chatPath := createCodebuffSingleSession(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.NoError(t,
		engine.SyncSingleSession(
			"codebuff:project-0:2026-07-15T10-00-00.000Z",
		),
		"a single-session sync on a live source must commit "+
			"without errors; ErrOrNil semantics here ensure the "+
			"test only exercises the digest-persist gate")

	_, has, err := database.GetProviderStatHash(
		context.Background(), parser.AgentCodebuff, chatPath,
	)
	require.NoError(t, err)
	require.True(t, has,
		"single-session SyncAll must persist provider_freshness; "+
			"a missing row here means the per-row persist gate "+
			"in writeSessionFull is missing (Issue 4) and the "+
			"per-component digest gate never engages on the next "+
			"warm pass")
}

// TestSyncEngineProviderStatHashersRegistrationIsCapabilityGated pins the
// side-effect of adding Source.MultiFileStatHash capability gating to
// buildProviderStatHashers: every SourceSet-wrapped agent unconditionally
// satisfies parser.MultiFileStatHasher via the SourceSetProvider
// forwarding method, so the engine must NOT register a hasher unless the
// provider explicitly declares the multi-file capability. Without the
// gate every Claude/Codex/RooCode/KiloLegacy/etc. provider would land in
// providerStatHashers as a 0-returning stub, store a 0 digest on first
// cold sync, then short-circuit every warm pass on a 0==0 match --
// every non-Codebuff SourceSet agent's parse left stale.
//
// The test does not depend on any Codebuff fixture; constructing an
// engine with the default registry must produce a non-nil entry only
// for the agents that opt in via the capability: Codebuff, plus the
// content-hashing single-file providers (Claude, Codex, TraeX) whose
// persisted stat digest spares a full-content fingerprint on every
// fresh-process sweep.
func TestSyncEngineProviderStatHashersRegistrationIsCapabilityGated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		// No AgentDirs: the engine is registered with the default
		// factory map but discovers nothing; that is sufficient to
		// populate providerStatHashers at construction time.
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	for _, agent := range []parser.AgentType{
		parser.AgentCodebuff,
		parser.AgentClaude,
		parser.AgentCodex,
		parser.AgentTraeX,
	} {
		require.NotNil(t, engine.ProviderStatHasher(agent),
			"%s declares Source.MultiFileStatHash=Supported and "+
				"must land in providerStatHashers; a nil here means the "+
				"capability gate dropped it or its provider capabilities "+
				"lost the override",
			agent)
	}
	for _, agent := range []parser.AgentType{
		parser.AgentRooCode,
		parser.AgentKiloLegacy,
		parser.AgentGemini,
		parser.AgentCopilot,
	} {
		assert.Nil(t, engine.ProviderStatHasher(agent),
			"%s does not declare Source.MultiFileStatHash and must "+
				"NOT be registered in providerStatHashers; a non-nil "+
				"entry means the SourceSetProvider forwarding method "+
				"is registering 0-returning stubs that would falsely "+
				"short-circuit warm passes on 0==0 digest matches",
			agent)
	}
}

// TestSourceMtimeCodebuffUsesPerFileHash pins the roborev-medium
// regression on ab050f8: SourceMtime for Codebuff sessions must
// detect (a) a same-size companion-file rewrite whose mtime stays
// below the existing max, (b) offsetting size changes that keep the
// total size unchanged, and (c) a missing companion file's later
// appearance. The freshness gate reduces chat-messages.json +
// run-state.json + chat-meta.json to a single int64 via an FNV-1a
// hash of each (size, mtime) pair so any per-file change in size or
// mtime is detected. A max-mtime-only reduction would miss (a) and
// (b); a sum-of-sizes reduction would miss (a). The watcher polls
// SourceMtime continuously, so the lookup must stay stat-only.
func TestSourceMtimeCodebuffUsesPerFileHash(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		Machine: "local",
	})

	// Codebuff session IDs are codebuff:<project>:<ts> and the
	// corresponding on-disk layout is root/<project>/chats/<ts>/,
	// mirroring createCodebuffArchive. Using a single-component
	// rawID would miss FindSourceFile's expected layout.
	project := "test-project"
	ts := "2026-07-15T10-00-00.000Z"
	rawID := project + ":" + ts
	dir := filepath.Join(root, project, "chats", ts)
	require.NoError(t, os.MkdirAll(dir, 0o755))

	chatPath := filepath.Join(dir, "chat-messages.json")
	runStatePath := filepath.Join(dir, "run-state.json")
	chatMetaPath := filepath.Join(dir, "chat-meta.json")

	// Fixed contents so we can rewrite a sibling with the *same byte
	// length* but a new mtime. The FNV-1a hash includes size, so a
	// pure same-size, same-mtime rewrite is the only input that would
	// legitimately leave SourceMtime unchanged.
	const chatBody = `[{"id":"u1","variant":"user","content":"hello","timestamp":"03:04 PM"}]`
	const runStateBody = `{"sessionState":{"mainAgentState":{"agentType":"base2-free-deepseek"}}}`
	const chatMetaBody = `{"messageCount":1,"firstPrompt":"hello","messagesSize":50}`
	require.NoError(t, os.WriteFile(chatPath, []byte(chatBody), 0o644))
	require.NoError(t, os.WriteFile(runStatePath, []byte(runStateBody), 0o644))
	require.NoError(t, os.WriteFile(chatMetaPath, []byte(chatMetaBody), 0o644))

	// Stagger mtimes so chat-messages.json holds the max; the other
	// two files are below it. Rewriting run-state.json with a new
	// mtime that is still below the max would be invisible to the
	// old max-mtime reduction.
	chatTime := time.Date(2026, time.July, 4, 12, 0, 0, 0, time.UTC)
	runStateTime := chatTime.Add(-5 * time.Minute)
	chatMetaTime := chatTime.Add(-10 * time.Minute)
	require.NoError(t, os.Chtimes(chatPath, chatTime, chatTime))
	require.NoError(t, os.Chtimes(runStatePath, runStateTime, runStateTime))
	require.NoError(t, os.Chtimes(chatMetaPath, chatMetaTime, chatMetaTime))

	baseline := engine.SourceMtime("codebuff:" + rawID)
	require.NotZero(t, baseline,
		"baseline SourceMtime must be non-zero for a live Codebuff session")

	// (a) Same-size companion-file rewrite with a *new* mtime that
	// stays below the existing max. The old max-mtime reduction
	// would return the same value; the per-file hash must change.
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(runStatePath, []byte(runStateBody), 0o644))
	subMaxTime := chatTime.Add(-1 * time.Minute) // still below chatTime
	require.NoError(t, os.Chtimes(runStatePath, subMaxTime, subMaxTime))
	afterSubMaxRewrite := engine.SourceMtime("codebuff:" + rawID)
	assert.NotEqual(t, baseline, afterSubMaxRewrite,
		"a same-size run-state.json rewrite with a sub-max mtime "+
			"must change SourceMtime; equal values pin the regression "+
			"where SourceMtime reduced to max mtime across the three files")

	// (b) Offsetting size changes: grow chat-messages.json by 10
	// bytes and shrink chat-meta.json by 10 bytes. The total size
	// delta is zero, but per-file stats must still trigger a hash
	// change. The old code never inspected sizes, so a per-file
	// size shift would have been invisible.
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(chatPath, []byte(chatBody+"          "), 0o644))
	require.NoError(t, os.WriteFile(chatMetaPath, []byte(chatMetaBody[:len(chatMetaBody)-10]), 0o644))
	require.NoError(t, os.Chtimes(chatPath, chatTime, chatTime))
	require.NoError(t, os.Chtimes(chatMetaPath, chatMetaTime, chatMetaTime))
	afterOffsettingSize := engine.SourceMtime("codebuff:" + rawID)
	assert.NotEqual(t, afterSubMaxRewrite, afterOffsettingSize,
		"offsetting per-file size changes that keep the sum unchanged "+
			"must still change SourceMtime; equal values pin the "+
			"regression where freshness ignored per-file size")

	// (c) A missing companion file's later appearance. Delete
	// chat-meta.json, capture SourceMtime, recreate it with new
	// content, and capture again. The new appearance must change
	// the hash because the missing-file block is a fixed zero
	// sequence distinct from any real (size, mtime) pair.
	require.NoError(t, os.Remove(chatMetaPath))
	afterMissingMeta := engine.SourceMtime("codebuff:" + rawID)
	assert.NotEqual(t, afterOffsettingSize, afterMissingMeta,
		"a deleted chat-meta.json must change SourceMtime; equal "+
			"values mean the missing-companion branch is hashed the "+
			"same as a present one")
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(chatMetaPath, []byte(chatMetaBody), 0o644))
	recreatedTime := chatTime.Add(2 * time.Minute)
	require.NoError(t, os.Chtimes(chatMetaPath, recreatedTime, recreatedTime))
	afterRecreatedMeta := engine.SourceMtime("codebuff:" + rawID)
	assert.NotEqual(t, afterMissingMeta, afterRecreatedMeta,
		"a recreated chat-meta.json with a new mtime must change "+
			"SourceMtime back to a value distinct from the missing-file state")
}
