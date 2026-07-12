package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	gosync "sync"
	"testing"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Deterministic work-count gates for the "sync work scales with new
// data, not archive size" invariant. Unlike the wall-clock
// benchmarks in engine_bench_test.go, these run in the regular test
// suite and count work units, so they fail loudly in CI regardless
// of runner noise. Companion gates elsewhere:
//
//   - TestProviderAuthoritativeUnchangedSessionSkipsOnResync covers
//     the generic providerSourceUnchangedInDB skip for the
//     provider.Parse fallthrough group (Vibe as representative).
//   - TestWriteIncrementalDebouncesSignalRecompute pins the #954
//     debounce of the O(history) signal recompute.
//   - The count-based seam tests in internal/parser
//     (discovery_workspace_manifest_test.go, antigravity/gemini
//     provider tests) pin O(roots) discovery work (#912).

func TestSourceHashSkipMutationWorkIsArchiveCardinalityIndependent(t *testing.T) {
	type workCounts struct {
		insert int
		remove int
	}
	observed := make(map[int]workCounts)
	for _, cacheSize := range []int{8, 8000} {
		t.Run(fmt.Sprintf("%d_entries", cacheSize), func(t *testing.T) {
			database := openTestDB(t)
			engine := NewEngine(database, EngineConfig{})
			t.Cleanup(engine.Close)
			entries := make(map[string]int64, cacheSize)
			for i := range cacheSize {
				entries[fmt.Sprintf(
					"/archive/session-%05d.jsonl?source_hash=old", i,
				)] = 1
			}
			engine.InjectSkipCache(entries)

			const base = "/archive/session-00000.jsonl?source_hash="
			insertWork := engine.cacheSkip(base+"new", 2)
			removeWork := engine.clearSkip(base + "new")

			assert.Equal(t, cacheSize-1, len(engine.SnapshotSkipCache()))
			observed[cacheSize] = workCounts{
				insert: insertWork,
				remove: removeWork,
			}
		})
	}

	assert.Equal(t, observed[8], observed[8000],
		"one watcher mutation must do the same sibling work at every archive size")
}

// TestWarmFullSyncDoesNoBulkWriteWork verifies that a second full
// sync over an unchanged Claude archive skips every session before
// the parse and never enters the bulk-write pipeline. Claude has its
// own pre-parse freshness path (shouldSkipProviderSourceByDB),
// distinct from the generic check the Vibe test covers; both have
// regressed independently in the past.
func TestWarmFullSyncDoesNoBulkWriteWork(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	fx := newEngineFixture(t)
	const n = 5
	for i := range n {
		fx.writeClaudeSession(
			t, "proj", fmt.Sprintf("warm-%d.jsonl", i),
			fmt.Sprintf("hello %d", i),
		)
	}

	ctx := context.Background()
	first := fx.engine.SyncAll(ctx, nil)
	require.Equal(t, n, first.Synced,
		"first sync parses and stores every session")

	second := fx.engine.SyncAll(ctx, nil)
	assert.Equal(t, 0, second.Synced,
		"unchanged sessions must not be re-synced on a warm pass")
	assert.GreaterOrEqual(t, second.Skipped, n,
		"every unchanged session must be counted as skipped")

	// PhaseStats resets at the start of each pass, so after the
	// second pass it reflects only that pass: a warm no-op sync
	// must not have run a single bulk-write batch.
	stats := fx.engine.PhaseStats()
	assert.Zero(t, stats.Batches.Load(),
		"warm no-op sync must not run any bulk-write batch")
	assert.Zero(t, stats.BatchedWrites.Load(),
		"warm no-op sync must not rewrite any session")
}

// TestRebuildLocalAndRemoteContributorsBulkWriteDiscoveredCount protects the
// shared full-rebuild ingest path: both source shapes must send every
// discovered session through batched writes. A remote contributor accidentally
// restored to the active-archive write mode would report zero batched writes.
func TestRebuildLocalAndRemoteContributorsBulkWriteDiscoveredCount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	type workCounts struct {
		localBatches  int64
		remoteBatches int64
		localWrites   int64
		remoteWrites  int64
	}
	observed := make(map[int]workCounts)
	for _, sessionsPerSource := range []int{5, 500} {
		t.Run(fmt.Sprintf("%d_sessions", sessionsPerSource), func(t *testing.T) {
			localRoot := t.TempDir()
			remoteRoot := t.TempDir()
			writeSessions := func(root, prefix string) {
				t.Helper()
				for i := range sessionsPerSource {
					path := filepath.Join(root, "project",
						fmt.Sprintf("%s-%03d.jsonl", prefix, i))
					require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
					require.NoError(t, os.WriteFile(path, []byte(
						testjsonl.NewSessionBuilder().
							AddClaudeUser("2024-01-01T00:00:00Z",
								fmt.Sprintf("%s %d", prefix, i)).
							String(),
					), 0o644))
				}
			}
			writeSessions(localRoot, "local")
			writeSessions(remoteRoot, "remote")

			database := openTestDB(t)
			engine := NewEngine(database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {localRoot}},
				Machine:   "local",
			})
			t.Cleanup(engine.Close)

			var progressMu gosync.Mutex
			remoteStarted := false
			localDiscovered := 0
			remoteDiscovered := 0
			stats, err := engine.ResyncAllWithOptions(context.Background(), func(p Progress) {
				progressMu.Lock()
				defer progressMu.Unlock()
				if !remoteStarted && p.SessionsTotal > localDiscovered {
					localDiscovered = p.SessionsTotal
				}
			}, RebuildOptions{
				Contributors: []RebuildContributor{{
					Name: "remote",
					Config: EngineConfig{
						AgentDirs: map[parser.AgentType][]string{
							parser.AgentClaude: {remoteRoot},
						},
						Machine:   "remote",
						IDPrefix:  "remote~",
						Ephemeral: true,
					},
					Progress: func(p Progress) Progress {
						progressMu.Lock()
						defer progressMu.Unlock()
						remoteStarted = true
						if p.SessionsTotal > remoteDiscovered {
							remoteDiscovered = p.SessionsTotal
						}
						return p
					},
				}},
			})
			require.NoError(t, err)
			require.False(t, stats.Aborted)
			require.Len(t, stats.RebuildPhases, 2)

			progressMu.Lock()
			assert.Equal(t, sessionsPerSource, localDiscovered,
				"local discovery must stay bounded by the local corpus")
			assert.Equal(t, sessionsPerSource, remoteDiscovered,
				"remote discovery must stay bounded by the remote corpus")
			progressMu.Unlock()

			wantBatches := int64((sessionsPerSource + batchSize - 1) / batchSize)
			for i, wantName := range []string{"local", "remote"} {
				phase := stats.RebuildPhases[i]
				assert.Equal(t, wantName, phase.Contributor,
					"contributors must retain deterministic local-then-remote order")
				assert.EqualValues(t, sessionsPerSource, phase.BatchedWrites,
					"%s must bulk-write every discovered session", wantName)
				assert.EqualValues(t, sessionsPerSource, phase.WriteBatchSize,
					"%s batch sizes must sum to its discovered count", wantName)
				assert.Equal(t, wantBatches, phase.Batches,
					"%s must use ceil(%d/%d) batches", wantName,
					sessionsPerSource, batchSize)
			}
			assert.Equal(t, sessionsPerSource*2, stats.TotalSessions,
				"combined discovery count")
			assert.Equal(t, sessionsPerSource*2, stats.Synced,
				"combined written session count")
			assert.Equal(t, stats.RebuildPhases[0].Batches,
				stats.RebuildPhases[1].Batches,
				"equivalent local and remote corpora must have equal batch work")
			assert.Equal(t, stats.RebuildPhases[0].BatchedWrites,
				stats.RebuildPhases[1].BatchedWrites,
				"equivalent local and remote corpora must have equal write work")

			observed[sessionsPerSource] = workCounts{
				localBatches:  stats.RebuildPhases[0].Batches,
				remoteBatches: stats.RebuildPhases[1].Batches,
				localWrites:   stats.RebuildPhases[0].BatchedWrites,
				remoteWrites:  stats.RebuildPhases[1].BatchedWrites,
			}
		})
	}

	small := observed[5]
	large := observed[500]
	assert.Equal(t, small.localBatches*5, large.localBatches,
		"local batch count must grow from 1 to 5 at the 100-session boundary")
	assert.Equal(t, small.remoteBatches*5, large.remoteBatches,
		"remote batch count must grow from 1 to 5 at the 100-session boundary")
	assert.Equal(t, small.localWrites*100, large.localWrites,
		"local writes must grow exactly with corpus cardinality")
	assert.Equal(t, small.remoteWrites*100, large.remoteWrites,
		"remote writes must grow exactly with corpus cardinality")
	assert.Equal(t, large.localWrites, large.remoteWrites,
		"large equivalent contributors must retain equal work counts")
}
