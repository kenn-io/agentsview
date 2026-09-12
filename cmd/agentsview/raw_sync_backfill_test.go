package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/rawtest"
)

func TestRawSyncBackfillValidatesFlagsBeforeConfigSideEffects(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_RAW_SYNC_URL", "https://sync.example.test")
	t.Setenv("AGENTSVIEW_RAW_SYNC_DEVICE_ID", "device-a")
	t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", "credential-value")

	tests := [][]string{
		{"raw-sync", "backfill", "--provider", "claude"},
		{"raw-sync", "backfill", "--run-id", "run-a"},
		{"raw-sync", "backfill", "--run-id", "bad/value", "--provider", "claude"},
		{"raw-sync", "backfill", "--run-id", "run-a", "--provider", "claude", "--batch-size", "0"},
		{"raw-sync", "backfill", "--run-id", "run-a", "--provider", "claude", "--batch-size", "513"},
		{"raw-sync", "backfill", "--run-id", "run-a", "--provider", "claude", "--format", "yaml"},
	}
	for _, args := range tests {
		_, err := executeCommand(newRootCommand(), args...)
		require.Error(t, err, "args: %v", args)
	}

	entries, err := os.ReadDir(dataDir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestNormalizeRawSyncBackfillConfigCanonicalizesSelection(t *testing.T) {
	cfg, err := normalizeRawSyncBackfillConfig(rawSyncBackfillConfig{
		Server: "https://sync.example.test/", DeviceID: "device-a",
		RunID: "run_1", Providers: []string{"codex", " claude ", "CODEX"},
		BatchSize: 128, Format: "json",
	}, "credential-value")

	require.NoError(t, err)
	assert.Equal(t, "https://sync.example.test", cfg.Server)
	assert.Equal(t, []string{"claude", "codex"}, cfg.Providers)
}

func TestSelectRawSyncBackfillProvidersBindsConfiguredRoots(t *testing.T) {
	base := t.TempDir()
	claudeRoot := filepath.Join(base, "claude")
	codexHome := filepath.Join(base, "codex-home")
	codexRoot := filepath.Join(codexHome, "sessions")
	aliasHome := filepath.Join(base, "codex-alias")
	require.NoError(t, os.MkdirAll(claudeRoot, 0o700))
	require.NoError(t, os.MkdirAll(codexRoot, 0o700))
	require.NoError(t, os.MkdirAll(aliasHome, 0o700))
	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeRoot}, parser.AgentCodex: {codexRoot},
		},
		ProviderMetadata: map[parser.AgentType]map[string][]string{
			parser.AgentCodex: {codexRoot: {codexHome, aliasHome}},
		},
	}

	selected, err := selectRawSyncBackfillProviders(cfg, []string{"codex", "claude"})

	require.NoError(t, err)
	require.Len(t, selected, 2)
	assert.Equal(t, parser.AgentClaude, selected[0].Provider.Definition().Type)
	assert.Equal(t, []string{claudeRoot}, selected[0].ConfiguredRoots)
	assert.Equal(t, parser.AgentCodex, selected[1].Provider.Definition().Type)
	assert.Equal(t, []string{codexRoot}, selected[1].ConfiguredRoots)
	plan, err := selected[1].Provider.WatchPlan(t.Context())
	require.NoError(t, err)
	var planned []string
	for _, root := range plan.Roots {
		planned = append(planned, filepath.Clean(root.Path))
	}
	assert.Contains(t, planned, aliasHome,
		"Codex metadata sidecars are scheduling roots, not configured-root identity")
}

func TestSelectRawSyncBackfillProvidersRefusesInvalidSelections(t *testing.T) {
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentClaude:   {"s3://bucket/claude"},
		parser.AgentClaudeAI: {t.TempDir()},
	}}
	for _, name := range []string{"unknown-provider", "claude", "claude-ai"} {
		_, err := selectRawSyncBackfillProviders(cfg, []string{name})
		require.Error(t, err, "provider: %s", name)
		assert.NotContains(t, err.Error(), "s3://")
	}
}

func TestSelectRawSyncBackfillProvidersIgnoresUnselectedProviderFailures(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentClaude:   {root},
		parser.AgentClaudeAI: {filepath.Join(t.TempDir(), "unreadable-import")},
		parser.AgentCodex:    {"s3://bucket/codex"},
	}}

	selected, err := selectRawSyncBackfillProviders(cfg, []string{"claude"})

	require.NoError(t, err)
	require.Len(t, selected, 1)
	assert.Equal(t, parser.AgentClaude, selected[0].Provider.Definition().Type)
}

func TestWriteRawSyncBackfillProgressFormatsFinalResult(t *testing.T) {
	progress := rawcheckpoint.BackfillProgress{
		RunID: "run-a", Discovery: "sealed", Captured: 2,
		Acknowledged: 2, Watermark: 2, Failures: map[string]int64{}, Complete: true,
	}
	var jsonOut, humanOut testBuffer
	require.NoError(t, writeRawSyncBackfillProgress(&jsonOut, "json", progress))
	assert.JSONEq(t, `{"run_id":"run-a","discovery":"sealed","captured":2,"acknowledged":2,"pending":0,"watermark":2,"failures":{},"complete":true}`, jsonOut.String())
	require.NoError(t, writeRawSyncBackfillProgress(&humanOut, "human", progress))
	assert.Equal(t, "Backfill run-a complete: 2 captured, 2 acknowledged, 0 pending.\n", humanOut.String())

	progress.Complete = false
	humanOut.Reset()
	require.NoError(t, writeRawSyncBackfillProgress(&humanOut, "human", progress))
	assert.Equal(t, "Backfill run-a incomplete: 2 captured, 2 acknowledged, 0 pending.\n", humanOut.String())
}

func TestRecoverRawSyncBackfillCancellationEmitsDurableRun(t *testing.T) {
	root := t.TempDir()
	store, err := rawcheckpoint.Open(t.Context(), filepath.Join(t.TempDir(), "checkpoint.db"))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	configured, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, root)
	require.NoError(t, err)
	spec := rawcheckpoint.BackfillRunSpec{
		RunID: "run-cancelled", DeviceID: "device-a", Destination: "https://sync.example.test",
		Providers: []parser.AgentType{parser.AgentClaude},
		Roots:     []rawcheckpoint.BackfillSelection{{Provider: parser.AgentClaude, ConfiguredRootID: configured.ID}},
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	progress, err := recoverRawSyncBackfillProgress(ctx, store, spec, rawcheckpoint.BackfillProgress{}, context.Canceled)

	require.ErrorIs(t, err, rawcheckpoint.ErrBackfillIncomplete)
	assert.Equal(t, "run-cancelled", progress.RunID)
	assert.Equal(t, int64(0), progress.Captured)
	assert.Equal(t, int64(0), progress.Acknowledged)
	assert.Equal(t, int64(1), progress.Failures["cancelled"])
	assert.False(t, progress.Complete)

	conflict := spec
	conflict.Destination = "https://different.example.test"
	got, conflictErr := recoverRawSyncBackfillProgress(ctx, store, conflict, rawcheckpoint.BackfillProgress{}, context.Canceled)
	require.Error(t, conflictErr)
	assert.Empty(t, got.RunID, "a changed selection must not reuse stored proof")
}

type forbiddenRawSyncBackfillTransport struct{}

func (forbiddenRawSyncBackfillTransport) MissingObjects(context.Context, parser.AgentType, []rawsync.ObjectRef) ([]rawsync.ObjectRef, error) {
	panic("transport must not be called after cancellation")
}
func (forbiddenRawSyncBackfillTransport) UploadObject(context.Context, parser.AgentType, rawsync.ObjectRef, io.ReaderAt) error {
	panic("transport must not be called after cancellation")
}
func (forbiddenRawSyncBackfillTransport) CommitManifest(context.Context, rawsync.Manifest) (rawsync.CommitResult, error) {
	panic("transport must not be called after cancellation")
}

func TestRunRawSyncBackfillAttemptCancellationWritesDurableJSON(t *testing.T) {
	root := t.TempDir()
	store, err := rawcheckpoint.Open(t.Context(), filepath.Join(t.TempDir(), "checkpoint.db"))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	configured, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, root)
	require.NoError(t, err)
	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	spec := rawcheckpoint.BackfillRunSpec{
		RunID: "run-json-cancel", DeviceID: "device-a", Destination: "https://sync.example.test",
		Providers: []parser.AgentType{parser.AgentClaude},
		Roots:     []rawcheckpoint.BackfillSelection{{Provider: parser.AgentClaude, ConfiguredRootID: configured.ID}},
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var output testBuffer

	err = runRawSyncBackfillAttempt(ctx, &output, rawSyncBackfillConfig{
		RunID: spec.RunID, DeviceID: spec.DeviceID, Format: "json", BatchSize: 1,
	}, store, spec, []parser.Provider{provider}, forbiddenRawSyncBackfillTransport{})

	require.Error(t, err)
	assert.True(t, isSilentExitError(err))
	assert.JSONEq(t, `{"run_id":"run-json-cancel","discovery":"open","captured":0,"acknowledged":0,"pending":0,"watermark":0,"failures":{"cancelled":1},"complete":false}`, output.String())
}

func TestRawSyncBackfillSpecReusesCompletedSelectionAfterRootRemoval(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	require.NoError(t, os.Mkdir(root, 0o700))
	store, err := rawcheckpoint.Open(t.Context(), filepath.Join(t.TempDir(), "checkpoint.db"))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	configured, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, root)
	require.NoError(t, err)
	original := rawcheckpoint.BackfillRunSpec{
		RunID: "run-complete", DeviceID: "device-a", Destination: "https://sync.example.test",
		Providers: []parser.AgentType{parser.AgentClaude},
		Roots:     []rawcheckpoint.BackfillSelection{{Provider: parser.AgentClaude, ConfiguredRootID: configured.ID}},
	}
	_, err = store.BeginBackfill(t.Context(), original)
	require.NoError(t, err)
	require.NoError(t, store.FinishBackfillProvider(t.Context(), original.RunID, parser.AgentClaude, rawcheckpoint.BackfillPassResult{Complete: true}))
	_, err = store.SealBackfill(t.Context(), original.RunID)
	require.NoError(t, err)
	_, err = store.CompleteBackfill(t.Context(), original.RunID)
	require.NoError(t, err)
	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	require.NoError(t, os.Remove(root))

	got, err := rawSyncBackfillSpec(t.Context(), store, rawSyncBackfillConfig{
		RunID: original.RunID, DeviceID: original.DeviceID, Server: original.Destination,
	}, []rawSyncBackfillProvider{{Provider: provider, ConfiguredRoots: []string{root}}})

	require.NoError(t, err)
	assert.Equal(t, original, got)
}

func TestRawSyncBackfillSpecReusesCompletedDanglingSymlinkSelection(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "sessions")
	link := filepath.Join(base, "configured")
	require.NoError(t, os.Mkdir(target, 0o700))
	requireSymlinkOrSkip(t, target, link)
	store, original, provider := completedRawSyncBackfillSelection(t, link)
	require.NoError(t, os.Remove(target))

	got, err := rawSyncBackfillSpec(t.Context(), store, rawSyncBackfillConfig{
		RunID: original.RunID, DeviceID: original.DeviceID, Server: original.Destination,
	}, []rawSyncBackfillProvider{{Provider: provider, ConfiguredRoots: []string{link}}})

	require.NoError(t, err)
	assert.Equal(t, original, got)
}

func TestRawSyncBackfillSpecRejectsRetargetedDanglingSymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "sessions")
	otherTarget := filepath.Join(base, "other-sessions")
	link := filepath.Join(base, "configured")
	require.NoError(t, os.Mkdir(target, 0o700))
	requireSymlinkOrSkip(t, target, link)
	store, original, provider := completedRawSyncBackfillSelection(t, link)
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Mkdir(otherTarget, 0o700))
	requireSymlinkOrSkip(t, otherTarget, link)
	require.NoError(t, os.Remove(otherTarget))

	_, err := rawSyncBackfillSpec(t.Context(), store, rawSyncBackfillConfig{
		RunID: original.RunID, DeviceID: original.DeviceID, Server: original.Destination,
	}, []rawSyncBackfillProvider{{Provider: provider, ConfiguredRoots: []string{link}}})

	require.ErrorIs(t, err, rawcheckpoint.ErrBackfillConflict)
}

func completedRawSyncBackfillSelection(
	t *testing.T,
	configuredPath string,
) (*rawcheckpoint.Store, rawcheckpoint.BackfillRunSpec, parser.Provider) {
	t.Helper()
	store, err := rawcheckpoint.Open(t.Context(), filepath.Join(t.TempDir(), "checkpoint.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	configured, err := store.ResolveConfiguredRoot(
		t.Context(), parser.AgentClaude, configuredPath,
	)
	require.NoError(t, err)
	spec := rawcheckpoint.BackfillRunSpec{
		RunID: "run-complete-symlink", DeviceID: "device-a",
		Destination: "https://sync.example.test",
		Providers:   []parser.AgentType{parser.AgentClaude},
		Roots: []rawcheckpoint.BackfillSelection{{
			Provider: parser.AgentClaude, ConfiguredRootID: configured.ID,
		}},
	}
	_, err = store.BeginBackfill(t.Context(), spec)
	require.NoError(t, err)
	require.NoError(t, store.FinishBackfillProvider(
		t.Context(), spec.RunID, parser.AgentClaude,
		rawcheckpoint.BackfillPassResult{Complete: true},
	))
	_, err = store.SealBackfill(t.Context(), spec.RunID)
	require.NoError(t, err)
	_, err = store.CompleteBackfill(t.Context(), spec.RunID)
	require.NoError(t, err)
	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{
		Roots: []string{configuredPath},
	})
	require.True(t, ok)
	return store, spec, provider
}

type testBuffer struct{ data []byte }

func (b *testBuffer) Write(p []byte) (int, error) { b.data = append(b.data, p...); return len(p), nil }
func (b *testBuffer) String() string              { return string(b.data) }
func (b *testBuffer) Reset()                      { b.data = nil }

func TestRawSyncBackfillIncompleteErrorIsSilentAfterProgress(t *testing.T) {
	err := rawSyncBackfillResultError(errors.New("transport exposed private-value"), true)
	require.Error(t, err)
	assert.True(t, isSilentExitError(err))
	assert.Equal(t, "raw-sync backfill incomplete", err.Error())
	assert.NotContains(t, err.Error(), "private-value")
}

func TestRawSyncBackfillCommandRegistersPublicContract(t *testing.T) {
	help, err := executeCommand(newRootCommand(), "raw-sync", "backfill", "--help")
	require.NoError(t, err)
	for _, want := range []string{
		"--run-id", "--provider", "--batch-size", "--format", "--json",
		"--server", "--device-id", "--allow-insecure-http",
		"AGENTSVIEW_RAW_SYNC_CREDENTIAL",
	} {
		assert.Contains(t, help, want)
	}
	assert.NotContains(t, help, "--credential")
}

func TestRawSyncBackfillCommandReturnsIncompleteWithoutWaitingOffline(t *testing.T) {
	dataDir := t.TempDir()
	root := filepath.Join(t.TempDir(), "claude")
	rawtest.Claude(t, root)
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"),
		[]byte(fmt.Sprintf("[agents.claude]\ndirs = [%q]\n", root)), 0o600,
	))
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_RAW_SYNC_URL", "http://127.0.0.1:1")
	t.Setenv("AGENTSVIEW_RAW_SYNC_DEVICE_ID", "device-a")
	t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", "credential-value")
	args := []string{
		"raw-sync", "backfill", "--run-id", "offline-a", "--provider", "claude",
		"--batch-size", "1", "--json", "--allow-insecure-http",
	}

	started := time.Now()
	output, err := executeCommand(newRootCommand(), args...)
	require.Error(t, err)
	assert.True(t, isSilentExitError(err))
	assert.Less(t, time.Since(started), 15*time.Second,
		"a finite invocation must not wait for the one-minute upload retry")
	assert.JSONEq(t, `{"run_id":"offline-a","discovery":"open","captured":1,"acknowledged":0,"pending":1,"watermark":0,"failures":{"upload":1},"complete":false}`, output)
	assert.NotContains(t, output, root)
	assert.NotContains(t, err.Error(), root)
	assert.NotContains(t, err.Error(), "credential-value")

	started = time.Now()
	output, err = executeCommand(newRootCommand(), args...)
	require.Error(t, err)
	assert.Less(t, time.Since(started), 15*time.Second,
		"deferred work must return without waiting for its retry deadline")
	assert.JSONEq(t, `{"run_id":"offline-a","discovery":"open","captured":1,"acknowledged":0,"pending":1,"watermark":0,"failures":{"deferred":1},"complete":false}`, output)
}

func TestRawSyncBackfillCommandReportsSpoolCapacity(t *testing.T) {
	dataDir := t.TempDir()
	root := filepath.Join(t.TempDir(), "claude")
	source := rawtest.Claude(t, root)
	require.NoError(t, os.Truncate(source, (1<<30)+1),
		"a sparse source exercises the production one-GiB spool limit without allocating it")
	configureRawSyncBackfillCommand(t, dataDir, root)

	output, err := executeCommand(
		newRootCommand(), "raw-sync", "backfill", "--run-id", "capacity-a",
		"--provider", "claude", "--format", "json", "--allow-insecure-http",
	)

	require.Error(t, err)
	assert.True(t, isSilentExitError(err))
	progress := decodeRawSyncBackfillProgress(t, output)
	assert.Equal(t, "capacity-a", progress.RunID)
	assert.Zero(t, progress.Acknowledged)
	assert.Zero(t, progress.Pending)
	assert.Equal(t, int64(1), progress.Failures["capacity"])
	assert.False(t, progress.Complete)
}

func TestRawSyncBackfillCommandReportsUnreadableSelectedRoot(t *testing.T) {
	dataDir := t.TempDir()
	root := filepath.Join(t.TempDir(), "claude")
	rawtest.Claude(t, root)
	configureRawSyncBackfillCommand(t, dataDir, root)
	require.NoError(t, os.Chmod(root, 0))
	t.Cleanup(func() { require.NoError(t, os.Chmod(root, 0o700)) })

	output, err := executeCommand(
		newRootCommand(), "raw-sync", "backfill", "--run-id", "unreadable-a",
		"--provider", "claude", "--format", "json", "--allow-insecure-http",
	)

	require.Error(t, err)
	assert.True(t, isSilentExitError(err))
	progress := decodeRawSyncBackfillProgress(t, output)
	assert.Equal(t, "unreadable-a", progress.RunID)
	assert.Zero(t, progress.Acknowledged)
	assert.False(t, progress.Complete)
	assert.NotEmpty(t, progress.Failures)
	assert.NotContains(t, output, root)
	assert.NotContains(t, err.Error(), root)
}

func TestRawSyncBackfillCommandRefusesMissingSelectedRoot(t *testing.T) {
	dataDir := t.TempDir()
	root := filepath.Join(t.TempDir(), "missing-claude")
	configureRawSyncBackfillCommand(t, dataDir, root)

	output, err := executeCommand(
		newRootCommand(), "raw-sync", "backfill", "--run-id", "missing-a",
		"--provider", "claude", "--format", "json", "--allow-insecure-http",
	)

	require.Error(t, err)
	assert.Empty(t, output)
	assert.Equal(t, rawSyncBackfillExitCode, exitCodeFromError(err))
	assert.NotContains(t, err.Error(), root)
}

func configureRawSyncBackfillCommand(t *testing.T, dataDir, root string) {
	t.Helper()
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"),
		[]byte(fmt.Sprintf("[agents.claude]\ndirs = [%q]\n", root)), 0o600,
	))
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_RAW_SYNC_URL", "http://127.0.0.1:1")
	t.Setenv("AGENTSVIEW_RAW_SYNC_DEVICE_ID", "device-a")
	t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", "credential-value")
}

func decodeRawSyncBackfillProgress(t *testing.T, output string) rawcheckpoint.BackfillProgress {
	t.Helper()
	var progress rawcheckpoint.BackfillProgress
	require.NoError(t, json.Unmarshal([]byte(output), &progress))
	return progress
}
