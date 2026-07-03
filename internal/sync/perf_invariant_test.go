package sync

import (
	"context"
	"fmt"
	"testing"

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
