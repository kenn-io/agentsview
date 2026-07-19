package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
)

// These tests cover the quack serve reopen loop's pure decision logic
// (file-identity polling, shutdown/backoff timing, probe-failure
// classification) without touching the Quack extension itself, which
// requires the duckdbtest build tag and a real network-installable
// extension. serveQuackOnce and runDuckDBQuackServeLoop are exercised
// end-to-end by the duckdbtest-gated quack serve tests instead.

func TestWaitForReplacementOrShutdownDetectsReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.duckdb")
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)
	// Prime while the path still points at the original, per the
	// PrimeFileIdentity contract. The replacement below deliberately has
	// the SAME size, and on Windows both writes can land in the same
	// ~15.6ms clock tick and share a ModTime, so this test exercises pure
	// primed-identity detection — without the prime it flakes whenever
	// the rename beats the first poll tick.
	duckdbsync.PrimeFileIdentity(info)

	ctx := context.Background()
	done := make(chan bool, 1)
	go func() {
		done <- waitForReplacementOrShutdown(ctx, path, info, 10*time.Millisecond)
	}()

	replacement := filepath.Join(dir, "next.duckdb")
	require.NoError(t, os.WriteFile(replacement, []byte("v2"), 0o644))
	require.NoError(t, os.Rename(replacement, path))

	select {
	case replaced := <-done:
		assert.True(t, replaced, "a file-identity change must report replaced=true")
	case <-time.After(30 * time.Second):
		t.Fatal("waitForReplacementOrShutdown did not observe the replacement")
	}
}

func TestWaitForReplacementOrShutdownReturnsFalseOnShutdown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.duckdb")
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- waitForReplacementOrShutdown(ctx, path, info, 10*time.Millisecond)
	}()
	cancel()

	select {
	case replaced := <-done:
		assert.False(t, replaced, "ctx cancellation must report replaced=false")
	case <-time.After(30 * time.Second):
		t.Fatal("waitForReplacementOrShutdown did not observe ctx cancellation")
	}
}

func TestWaitForReplacementOrShutdownTreatsMissingFileAsNoChangeYet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.duckdb")
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	replaced := waitForReplacementOrShutdown(ctx, path, info, 10*time.Millisecond)
	assert.False(t, replaced,
		"a briefly missing file must not be treated as a replacement")
}

func TestSleepOrShutdownReturnsFalseWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	ok := sleepOrShutdown(ctx, 5*time.Second)
	assert.False(t, ok)
	assert.Less(t, time.Since(start), 1*time.Second,
		"cancellation must interrupt the sleep, not wait out the full duration")
}

func TestSleepOrShutdownReturnsTrueAfterDuration(t *testing.T) {
	ok := sleepOrShutdown(context.Background(), 10*time.Millisecond)
	assert.True(t, ok)
}

func TestNextQuackServeBackoffDoublesAndCaps(t *testing.T) {
	backoff := quackServeReopenMinBackoff
	backoff = nextQuackServeBackoff(backoff)
	assert.Equal(t, 2*quackServeReopenMinBackoff, backoff)

	for range 10 {
		backoff = nextQuackServeBackoff(backoff)
	}
	assert.Equal(t, quackServeReopenMaxBackoff, backoff,
		"backoff must not grow past the configured maximum")
}

func TestDuckDBMirrorProbeFailureReason(t *testing.T) {
	tests := []struct {
		name  string
		probe duckdbsync.MirrorProbe
		want  string
	}{
		{
			name:  "missing file",
			probe: duckdbsync.MirrorProbe{},
			want:  "duckdb mirror file does not exist",
		},
		{
			name: "bad shape with issue detail",
			probe: duckdbsync.MirrorProbe{
				FileExists: true, ShapeOK: false, ShapeIssue: "missing table sessions",
			},
			want: "missing table sessions",
		},
		{
			name: "bad shape without issue detail",
			probe: duckdbsync.MirrorProbe{
				FileExists: true, ShapeOK: false,
			},
			want: "duckdb mirror shape incompatible",
		},
		{
			name: "version drift",
			probe: duckdbsync.MirrorProbe{
				FileExists: true, ShapeOK: true,
				SchemaVersion: duckdbsync.SchemaVersion + 1,
			},
			want: "duckdb mirror schema version",
		},
		{
			name: "lock conflict",
			probe: duckdbsync.MirrorProbe{
				FileExists: true, ShapeOK: false, LockConflict: true,
				ShapeIssue: "Conflicting lock is held in pid 123",
			},
			want: "Conflicting lock",
		},
		{
			name: "compatible",
			probe: duckdbsync.MirrorProbe{
				FileExists: true, ShapeOK: true, SchemaVersion: duckdbsync.SchemaVersion,
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := duckDBMirrorProbeFailureReason(tt.probe)
			if tt.want == "" {
				assert.Empty(t, got)
				return
			}
			assert.Contains(t, got, tt.want)
			t.Run("serve error remedy", func(t *testing.T) {
				err := duckDBMirrorServeProbeError(tt.probe)
				require.Error(t, err)
				if tt.probe.LockConflict {
					assert.Contains(t, err.Error(), "holds the mirror read-write")
					assert.NotContains(t, err.Error(), "push --full")
				} else {
					assert.Contains(t, err.Error(), "push --full")
				}
			})
		})
	}
}

func TestProbeDuckDBMirrorForServeMissingFileIsActionable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.duckdb")
	err := probeDuckDBMirrorForServe(context.Background(), path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
	assert.Contains(t, err.Error(), "agentsview duckdb push --full")
}

func TestProbeDuckDBMirrorForServeAcceptsCompatibleMirror(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	conn, err := duckdbsync.Open(path)
	require.NoError(t, err)
	require.NoError(t, duckdbsync.EnsureSchema(context.Background(), conn))
	require.NoError(t, conn.Close())

	assert.NoError(t, probeDuckDBMirrorForServe(context.Background(), path))
}
