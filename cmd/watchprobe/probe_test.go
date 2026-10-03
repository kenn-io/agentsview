package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectiveCacheAcknowledgementAndRestart(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "cache.sqlite")
	c, err := openCache(ctx, path, 4<<20)
	require.NoError(t, err)
	records := []record{{Name: "a.jsonl", Signature: signature{Size: 4, Identity: [16]byte{0: 7, 15: 99}, Volume: 8, IdentityKnown: true}}, {Name: "b.jsonl", Signature: signature{Size: 8}}}
	admitted, err := c.write(ctx, 0, records)
	require.NoError(t, err)
	require.True(t, admitted)
	c.close()
	c, err = openCache(ctx, path, 4<<20)
	require.NoError(t, err)
	defer c.close()
	got, err := c.load(ctx, 0, []string{"a.jsonl", "b.jsonl"})
	require.NoError(t, err)
	assert.Equal(t, records[0].Signature, got["a.jsonl"])
	records[0].Signature.Size = 5
	admitted, err = c.write(ctx, 0, records[:1])
	require.NoError(t, err)
	assert.True(t, admitted)
	got, err = c.load(ctx, 0, []string{"a.jsonl", "b.jsonl"})
	require.NoError(t, err)
	assert.EqualValues(t, 5, got["a.jsonl"].Size)
	assert.EqualValues(t, 8, got["b.jsonl"].Size)
}

func TestScannerNoopRetryAndForcedVerification(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	path := filepath.Join(root, "a.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("0000"), 0o600))
	c, err := openCache(ctx, filepath.Join(t.TempDir(), "cache.sqlite"), 4<<20)
	require.NoError(t, err)
	defer c.close()
	p := probe{cache: c}
	first, err := p.scan(ctx, 0, root, false, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 1, first.Changed)
	warm, err := p.scan(ctx, 0, root, false, nil)
	require.NoError(t, err)
	assert.Zero(t, warm.Changed)
	assert.Zero(t, warm.CacheWrites)
	require.NoError(t, os.WriteFile(path, []byte("11111"), 0o600))
	p.failNextAck = true
	_, err = p.scan(ctx, 0, root, false, nil)
	require.ErrorIs(t, err, errInjectedAck)
	retry, err := p.scan(ctx, 0, root, false, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 1, retry.Changed)
	forced, err := p.scan(ctx, 0, root, true, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 1, forced.Verified)
	assert.EqualValues(t, 5, forced.ContentBytes)
	assert.Zero(t, forced.CacheWrites)
}

func TestScannerPagesAndCancellation(t *testing.T) {
	root := t.TempDir()
	for i := range 513 {
		require.NoError(t, os.WriteFile(filepath.Join(root, sourceName(i, "repetitive")), []byte("0"), 0o600))
	}
	p := probe{}
	pages := 0
	s, err := p.scan(t.Context(), 0, root, false, func() { pages++ })
	require.NoError(t, err)
	assert.EqualValues(t, 513, s.Stats)
	assert.Equal(t, 3, pages)
	assert.LessOrEqual(t, s.MaxPageRecords, 256)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = p.scan(ctx, 0, root, false, nil)
	require.ErrorIs(t, err, context.Canceled)
}

func TestOpenWriterSignatureSeesAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("0000"), 0o600))
	before, err := freshSignature(path)
	require.NoError(t, err)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	defer f.Close()
	_, err = f.WriteString("1")
	require.NoError(t, err)
	after, err := freshSignature(path)
	require.NoError(t, err)
	assert.EqualValues(t, 5, after.Size)
	assert.Equal(t, before.Identity, after.Identity)
	assert.True(t, after.IdentityKnown)
}

func TestCachePressureIsUnknownNotRemoval(t *testing.T) {
	c, err := openCache(t.Context(), filepath.Join(t.TempDir(), "cache.sqlite"), 128<<10)
	require.NoError(t, err)
	defer c.close()
	refused := false
	for unit := range 100 {
		rows := make([]record, 256)
		for i := range rows {
			rows[i] = record{Name: sourceName(i, "entropy"), Signature: signature{Size: 4}}
		}
		ok, err := c.write(t.Context(), unit, rows)
		require.NoError(t, err)
		if !ok {
			refused = true
			got, err := c.load(t.Context(), unit, []string{sourceName(0, "entropy")})
			require.NoError(t, err)
			assert.Empty(t, got)
			break
		}
	}
	assert.True(t, refused)
}

func TestTesterReportAndScopedNativeObservation(t *testing.T) {
	report, err := runProbe(t.Context(), options{Files: 513, Passes: 2, MaxWatches: 1, Queue: 32, CacheBytes: 4 << 20, Output: filepath.Join(t.TempDir(), "artifacts"), Pattern: "repetitive"})
	require.NoError(t, err)
	assert.Equal(t, 513, report.Options.Files)
	assert.Equal(t, 3, report.Directories)
	assert.True(t, report.Checks["open_writer_append"])
	assert.True(t, report.Checks["native_delivery"])
	assert.True(t, report.Checks["retry_preserves_baseline"])
	assert.True(t, report.Checks["restart_preserves_baseline"])
	assert.True(t, report.Checks["explicit_dirty_same_stat_verified"])
	assert.Positive(t, report.Native.Observed)
	assert.LessOrEqual(t, report.MaxPageRecords, 256)
	for _, p := range report.Phases {
		if p.Name == "warm_scan" {
			assert.Zero(t, p.Scan.CacheWrites)
			assert.Zero(t, p.Scan.ContentBytes)
		}
	}
}

func TestCoverageOnlyAndCacheDisabled(t *testing.T) {
	r, err := runProbe(t.Context(), options{Files: 2, Passes: 1, Queue: 1, Output: filepath.Join(t.TempDir(), "probe"), Pattern: "entropy"})
	require.NoError(t, err)
	assert.Zero(t, r.Native.Allocated)
	assert.Equal(t, 1, r.Native.Unavailable)
	assert.True(t, r.Checks["open_writer_append"])
	assert.True(t, r.Checks["explicit_dirty_same_stat_verified"])
	assert.True(t, r.Checks["retry_preserves_baseline"])
	assert.True(t, r.Checks["loss_verifies_owned_content"])
	assert.NotContains(t, r.Checks, "native_delivery")
	assert.NotContains(t, r.Checks, "restart_preserves_baseline")
}

func TestSustainedTelemetryAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	out := filepath.Join(t.TempDir(), "artifacts")
	done := make(chan error, 1)
	go func() {
		_, err := runProbe(ctx, options{Files: 2, Passes: 1, Queue: 8, Output: out, Pattern: "repetitive", Duration: time.Hour})
		done <- err
	}()
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(out, "metrics.jsonl")); return err == nil }, time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "owned probe work did not cancel")
	}
	_, err := os.Stat(filepath.Join(out, "sources"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestLifecycleChecksReplacementRenameAndRemoval(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	target := filepath.Join(root, "a.jsonl")
	require.NoError(t, os.WriteFile(target, []byte("original"), 0o600))
	c, err := openCache(ctx, filepath.Join(t.TempDir(), "cache.sqlite"), 4<<20)
	require.NoError(t, err)
	defer c.close()
	p := probe{cache: c}
	_, err = p.scan(ctx, 0, root, false, nil)
	require.NoError(t, err)
	checks := map[string]bool{}
	_, err = p.lifecycle(ctx, 0, target, checks)
	require.NoError(t, err)
	assert.True(t, checks["replacement_identity"])
	assert.True(t, checks["rename_routes_new_name"])
	assert.True(t, checks["remove_drops_cache_entry"])
}

func TestLatencyReportsUnresolvedClockSamples(t *testing.T) {
	h := histogram{}
	h.observe(0)
	h.observe(10 * time.Nanosecond)
	s := h.summary()
	assert.Zero(t, s.MinNS)
	assert.EqualValues(t, 5, s.MeanNS)
	assert.Zero(t, s.P50UpperNS)
}

func TestNativePressurePreservesEarliestLossAndOperationCounts(t *testing.T) {
	n := nativeSource{queue: make(chan notice, 1), loss: make([]atomic.Int64, 1), started: time.Now()}
	n.offer(notice{Unit: 0, Operation: fsnotify.Write})
	n.offer(notice{Unit: 0, Operation: fsnotify.Chmod})
	first := n.loss[0].Load()
	n.offer(notice{Unit: 0, Operation: fsnotify.Chmod | fsnotify.Write})
	s := n.snapshot()
	assert.Equal(t, first, n.loss[0].Load())
	assert.EqualValues(t, 2, s.Dropped)
	assert.EqualValues(t, 2, s.Operations.Write)
	assert.EqualValues(t, 2, s.Operations.Chmod)
	assert.Equal(t, 1, s.PendingLossUnits)
	assert.GreaterOrEqual(t, s.OldestPendingLossSeconds, 0.0)
	assert.Len(t, n.queue, 1)
}
