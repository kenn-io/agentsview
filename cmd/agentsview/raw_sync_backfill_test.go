package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/rawtest"
	"go.kenn.io/agentsview/internal/rawwatch"
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
	for _, basePath := range []string{"", "/av", "/tenant%2Fname"} {
		t.Run(basePath, func(t *testing.T) {
			cfg, err := normalizeRawSyncBackfillConfig(rawSyncBackfillConfig{
				Server: "https://sync.example.test" + basePath + "/", DeviceID: "device-a",
				RunID: "run_1", Providers: []string{"codex", " claude ", "CODEX"},
				BatchSize: 128, Format: "json",
			}, "credential-value")

			require.NoError(t, err)
			assert.Equal(t, "https://sync.example.test"+basePath, cfg.Server)
			assert.Equal(t, []string{"claude", "codex"}, cfg.Providers)
		})
	}
}

func TestRawSyncBackfillRejectsUnreadableCrushRegistry(t *testing.T) {
	for _, registryState := range []string{"malformed", "unreadable"} {
		t.Run(registryState, func(t *testing.T) {
			dataDir, registry, projectData := t.TempDir(), t.TempDir(), t.TempDir()
			conn, err := sql.Open("sqlite3", filepath.Join(projectData, parser.CrushDBName))
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), `CREATE TABLE sessions (id TEXT PRIMARY KEY)`)
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			registryPath := filepath.Join(registry, parser.CrushProjectsFileName)
			if registryState == "malformed" {
				body, err := json.Marshal(map[string]any{"projects": []map[string]string{{"path": t.TempDir(), "data_dir": projectData}}})
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(registryPath, body[:len(body)-1], 0o600))
			} else {
				require.NoError(t, os.Mkdir(registryPath, 0o700))
			}
			require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(fmt.Sprintf("[agents.crush]\ndirs = [%q]\n", registry)), 0o600))
			t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
			t.Setenv("AGENTSVIEW_RAW_SYNC_URL", "https://sync.example.test")
			t.Setenv("AGENTSVIEW_RAW_SYNC_DEVICE_ID", "device-a")
			t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", "credential-value")
			output, err := executeCommand(newRootCommand(), "raw-sync", "backfill", "--run-id", "bad-registry", "--provider", "crush", "--format", "json")
			require.Error(t, err)
			assert.Empty(t, output)
			assert.Contains(t, err.Error(), "project registry")
			assert.NotContains(t, err.Error(), registry)
			store, err := rawcheckpoint.Open(t.Context(), rawSyncCheckpointPath(dataDir))
			require.NoError(t, err)
			defer store.Close()
			_, err = store.BackfillProgress(t.Context(), "bad-registry")
			require.ErrorIs(t, err, rawcheckpoint.ErrBackfillConflict, "an invalid registry must not save a run")
		})
	}
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

	for _, failure := range []string{"rejected", "capture_lost"} {
		progress.Failures = map[string]int64{failure: 1}
		humanOut.Reset()
		require.NoError(t, writeRawSyncBackfillProgress(&humanOut, "human", progress))
		assert.Contains(t, humanOut.String(), "start a new run ID", "failure: %s", failure)
	}
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
		Entries:   []rawcheckpoint.BackfillEntry{{Provider: parser.AgentClaude, Path: root}},
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
	}, []rawSyncBackfillProvider{{Provider: provider, ConfigEntries: []string{root}, ConfiguredRoots: []string{root}}})

	require.NoError(t, err)
	assert.Equal(t, original, got)

	changed, err := rawSyncBackfillSpec(t.Context(), store, rawSyncBackfillConfig{
		RunID: original.RunID, DeviceID: original.DeviceID, Server: original.Destination,
	}, []rawSyncBackfillProvider{{Provider: provider, ConfigEntries: []string{root + "-moved"}, ConfiguredRoots: []string{root + "-moved"}}})
	require.NoError(t, err)
	_, err = store.BeginBackfill(t.Context(), changed)
	require.ErrorIs(t, err, rawcheckpoint.ErrBackfillConflict)
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

func TestRawSyncBackfillCommandRefusesToStartWithUnavailableRoot(t *testing.T) {
	dataDir := t.TempDir()
	present := filepath.Join(t.TempDir(), "claude")
	rawtest.Claude(t, present)
	missing := filepath.Join(t.TempDir(), "unmounted", "claude")
	configureRawSyncBackfillCommandRoots(t, dataDir, present, missing)
	args := []string{
		"raw-sync", "backfill", "--run-id", "missing-root", "--provider", "claude",
		"--json", "--allow-insecure-http",
	}

	output, err := executeCommand(newRootCommand(), args...)

	require.Error(t, err)
	assert.Empty(t, output)
	assert.Equal(t, 1, exitCodeFromError(err), "rerunning cannot start this run")
	assert.Contains(t, err.Error(), "configured claude root is unavailable")
	assert.NotContains(t, err.Error(), missing)

	configureRawSyncBackfillCommandRoots(t, dataDir, present)
	output, err = executeCommand(newRootCommand(), args...)

	require.Error(t, err)
	assert.Equal(t, rawSyncBackfillExitCode, exitCodeFromError(err))
	progress := decodeRawSyncBackfillProgress(t, output)
	assert.Equal(t, int64(1), progress.Captured,
		"the refused attempt must not save a run bound to the unavailable root")
}

func configureRawSyncBackfillCommandRoots(t *testing.T, dataDir string, roots ...string) {
	t.Helper()
	quoted := make([]string, 0, len(roots))
	for _, root := range roots {
		quoted = append(quoted, fmt.Sprintf("%q", root))
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"),
		[]byte("[agents.claude]\ndirs = ["+strings.Join(quoted, ", ")+"]\n"), 0o600,
	))
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_RAW_SYNC_URL", "http://127.0.0.1:1")
	t.Setenv("AGENTSVIEW_RAW_SYNC_DEVICE_ID", "device-a")
	t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", "credential-value")
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

type acceptingRawSyncBackfillTransport struct{ commits int }

func (*acceptingRawSyncBackfillTransport) MissingObjects(context.Context, parser.AgentType, []rawsync.ObjectRef) ([]rawsync.ObjectRef, error) {
	return nil, nil
}

func (*acceptingRawSyncBackfillTransport) UploadObject(context.Context, parser.AgentType, rawsync.ObjectRef, io.ReaderAt) error {
	return nil
}

func (t *acceptingRawSyncBackfillTransport) CommitManifest(context.Context, rawsync.Manifest) (rawsync.CommitResult, error) {
	t.commits++
	return rawsync.CommitResult{
		ManifestID: fmt.Sprintf("%064x", t.commits), Receipt: fmt.Sprintf("%064x", t.commits+100),
		Generation: 1, Created: true,
	}, nil
}

type recordingRawSyncBackfillTransport struct {
	acceptingRawSyncBackfillTransport
	paths []string
}

func (t *recordingRawSyncBackfillTransport) CommitManifest(ctx context.Context, manifest rawsync.Manifest) (rawsync.CommitResult, error) {
	for _, entry := range manifest.Entries {
		t.paths = append(t.paths, entry.Path)
	}
	return t.acceptingRawSyncBackfillTransport.CommitManifest(ctx, manifest)
}

func TestRawSyncBackfillResumeKeepsCrushRegistryProjectPath(t *testing.T) {
	for _, registryChange := range []string{"unchanged", "added project", "removed registry", "malformed registry", "overlapping registry root"} {
		t.Run(registryChange, func(t *testing.T) {
			registry := t.TempDir()
			project := filepath.Join(t.TempDir(), "project-a")
			dataDir := filepath.Join(t.TempDir(), "elsewhere", "crush-data")
			require.NoError(t, os.MkdirAll(project, 0o700))
			require.NoError(t, os.MkdirAll(dataDir, 0o700))
			conn, err := sql.Open("sqlite3", filepath.Join(dataDir, parser.CrushDBName))
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), `CREATE TABLE sessions (id TEXT PRIMARY KEY)`)
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			registryDataDir := dataDir
			configuredRoots := []string{registry}
			if registryChange == "overlapping registry root" {
				registryDataDir = filepath.Join(t.TempDir(), "data-alias")
				require.NoError(t, os.Symlink(dataDir, registryDataDir))
				configuredRoots = append(configuredRoots, dataDir)
			}
			registryBody, err := json.Marshal(map[string]any{
				"projects": []map[string]string{{"path": project, "data_dir": registryDataDir}},
			})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(registry, parser.CrushProjectsFileName), registryBody, 0o600))
			cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentCrush: configuredRoots}}
			store, err := rawcheckpoint.Open(t.Context(), filepath.Join(t.TempDir(), "checkpoint.db"))
			require.NoError(t, err)
			defer store.Close()
			require.NoError(t, store.SetDevice(t.Context(), "device-a"))
			backfill := rawSyncBackfillConfig{
				RunID: "run-crush", DeviceID: "device-a", Server: "https://sync.example.test",
				Providers: []string{"crush"}, Format: "json", BatchSize: 8,
			}
			selected, err := selectRawSyncBackfillProviders(cfg, backfill.Providers)
			require.NoError(t, err)
			spec, err := rawSyncBackfillSpec(t.Context(), store, backfill, selected)
			require.NoError(t, err)
			_, err = store.BeginBackfill(t.Context(), spec)
			require.NoError(t, err, "an interrupted first attempt saves the run before capturing")

			switch registryChange {
			case "added project":
				another := t.TempDir()
				otherDB, err := sql.Open("sqlite3", filepath.Join(another, parser.CrushDBName))
				require.NoError(t, err)
				_, err = otherDB.ExecContext(t.Context(), `CREATE TABLE sessions (id TEXT PRIMARY KEY)`)
				require.NoError(t, err)
				require.NoError(t, otherDB.Close())
				registryBody, err = json.Marshal(map[string]any{
					"projects": []map[string]string{
						{"path": project, "data_dir": dataDir},
						{"path": filepath.Join(t.TempDir(), "project-b"), "data_dir": another},
					},
				})
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(registry, parser.CrushProjectsFileName), registryBody, 0o600))
			case "removed registry":
				require.NoError(t, os.Remove(filepath.Join(registry, parser.CrushProjectsFileName)))
			case "malformed registry":
				require.NoError(t, os.WriteFile(filepath.Join(registry, parser.CrushProjectsFileName), []byte("{"), 0o600))
			}

			selected, err = selectRawSyncBackfillProviders(cfg, backfill.Providers)
			require.NoError(t, err)
			spec, err = rawSyncBackfillSpec(t.Context(), store, backfill, selected)
			require.NoError(t, err)
			transport := &recordingRawSyncBackfillTransport{}
			var output testBuffer
			err = runRawSyncBackfillAttempt(t.Context(), &output, backfill, store, spec,
				[]parser.Provider{selected[0].Provider}, transport)

			require.NoError(t, err, output.String())
			encoded := base64.RawURLEncoding.EncodeToString([]byte(project))
			assert.Equal(t, []string{"projects/" + encoded + "/" + parser.CrushDBName}, transport.paths,
				"a resumed run must attribute the database to the project in projects.json")
		})
	}
}

func TestRawSyncBackfillCompletesGooseRootsTheProviderNormalizes(t *testing.T) {
	for name, entry := range map[string]func(home string) string{
		"goose home":  func(home string) string { return home },
		"sessions.db": func(home string) string { return filepath.Join(home, "data", "sessions", parser.GooseDBName) },
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			sessions := filepath.Join(home, "data", "sessions")
			require.NoError(t, os.MkdirAll(sessions, 0o700))
			conn, err := sql.Open("sqlite3", filepath.Join(sessions, parser.GooseDBName))
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), `CREATE TABLE sessions (id TEXT PRIMARY KEY)`)
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentGoose: {entry(home)}}}
			selected, err := selectRawSyncBackfillProviders(cfg, []string{"goose"})
			require.NoError(t, err)
			store, err := rawcheckpoint.Open(t.Context(), filepath.Join(t.TempDir(), "checkpoint.db"))
			require.NoError(t, err)
			defer store.Close()
			require.NoError(t, store.SetDevice(t.Context(), "device-a"))
			backfill := rawSyncBackfillConfig{
				RunID: "run-goose", DeviceID: "device-a", Server: "https://sync.example.test",
				Providers: []string{"goose"}, Format: "json", BatchSize: 8,
			}
			spec, err := rawSyncBackfillSpec(t.Context(), store, backfill, selected)
			require.NoError(t, err)
			var output testBuffer

			err = runRawSyncBackfillAttempt(t.Context(), &output, backfill, store, spec,
				[]parser.Provider{selected[0].Provider}, &acceptingRawSyncBackfillTransport{})

			require.NoError(t, err, output.String())
			var progress rawcheckpoint.BackfillProgress
			require.NoError(t, json.Unmarshal(output.data, &progress))
			assert.True(t, progress.Complete)
			assert.Equal(t, int64(1), progress.Captured)
		})
	}
}

type flakyRawSyncBackfillTransport struct {
	acceptingRawSyncBackfillTransport
	fail bool
}

func (t *flakyRawSyncBackfillTransport) CommitManifest(ctx context.Context, manifest rawsync.Manifest) (rawsync.CommitResult, error) {
	if t.fail {
		return rawsync.CommitResult{}, errors.New("server unavailable")
	}
	return t.acceptingRawSyncBackfillTransport.CommitManifest(ctx, manifest)
}

func TestRawSyncBackfillSealedRunUploadsAfterSourceRootUnmounted(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	rawtest.Claude(t, root)
	now := time.Now()
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(t.Context(), filepath.Join(base, "checkpoint.db"), rawcheckpoint.Options{
		SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20, Now: func() time.Time { return now },
	})
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}}
	backfill := rawSyncBackfillConfig{
		RunID: "run-sealed", DeviceID: "device-a", Server: "https://sync.example.test",
		Providers: []string{"claude"}, Format: "json", BatchSize: 8,
	}
	attempt := func(transport *flakyRawSyncBackfillTransport) (rawcheckpoint.BackfillProgress, error) {
		selected, err := selectRawSyncBackfillProviders(cfg, backfill.Providers)
		require.NoError(t, err)
		spec, err := rawSyncBackfillSpec(t.Context(), store, backfill, selected)
		require.NoError(t, err)
		var output testBuffer
		runErr := runRawSyncBackfillAttempt(t.Context(), &output, backfill, store, spec,
			[]parser.Provider{selected[0].Provider}, transport)
		var progress rawcheckpoint.BackfillProgress
		require.NoError(t, json.Unmarshal(output.data, &progress))
		return progress, runErr
	}

	progress, err := attempt(&flakyRawSyncBackfillTransport{fail: true})
	require.Error(t, err)
	require.Equal(t, "sealed", progress.Discovery)
	require.Positive(t, progress.Pending)

	require.NoError(t, os.RemoveAll(root))
	now = now.Add(2 * time.Hour)
	transport := &flakyRawSyncBackfillTransport{}
	progress, err = attempt(transport)

	require.NoError(t, err)
	assert.True(t, progress.Complete)
	assert.Positive(t, progress.Acknowledged)
	assert.Equal(t, progress.Captured, progress.Acknowledged)
	assert.Positive(t, transport.commits)
}

func TestRawSyncBackfillResumeDiscoversSavedRootsWhenResolutionChanges(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "first")
	second := filepath.Join(base, "second")
	link := filepath.Join(base, "configured")
	require.NoError(t, os.Mkdir(first, 0o700))
	require.NoError(t, os.Mkdir(second, 0o700))
	requireSymlinkOrSkip(t, first, link)
	store, err := rawcheckpoint.Open(t.Context(), filepath.Join(t.TempDir(), "checkpoint.db"))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {link}}}
	backfill := rawSyncBackfillConfig{RunID: "run-retarget", DeviceID: "device-a", Server: "https://sync.example.test"}
	selected, err := selectRawSyncBackfillProviders(cfg, []string{"claude"})
	require.NoError(t, err)
	spec, err := rawSyncBackfillSpec(t.Context(), store, backfill, selected)
	require.NoError(t, err)
	_, err = store.BeginBackfill(t.Context(), spec)
	require.NoError(t, err)
	saved, err := store.BackfillRoots(t.Context(), spec.RunID, parser.AgentClaude)
	require.NoError(t, err)
	require.Len(t, saved, 1)
	require.NoError(t, os.Remove(link))
	requireSymlinkOrSkip(t, second, link)

	selected, err = selectRawSyncBackfillProviders(cfg, []string{"claude"})
	require.NoError(t, err)
	resumed, err := rawSyncBackfillSpec(t.Context(), store, backfill, selected)

	require.NoError(t, err)
	assert.Equal(t, spec, resumed)
	roots := selected[0].Provider.(interface{ ConfiguredRoots() []string }).ConfiguredRoots()
	assert.Equal(t, []string{saved[0].LocalPath}, roots)
}

func TestRawSyncBackfillOwnsOverlappingRootsLikeWatch(t *testing.T) {
	// Claude accepts this subagent transcript under both roots, and watch gives
	// it to whichever root its config lists first.
	parent := t.TempDir()
	nested := filepath.Join(parent, "nested")
	transcript := filepath.Join(nested, "project", "subagents", "subagents", "agent-child.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcript), 0o700))
	require.NoError(t, os.WriteFile(transcript, []byte(`{"type":"user","uuid":"u1","isSidechain":true,"timestamp":"2026-07-06T12:00:00Z","message":{"content":"Inspect."}}`+"\n"), 0o600))
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {nested, parent}}}
	store, err := rawcheckpoint.Open(t.Context(), filepath.Join(t.TempDir(), "checkpoint.db"))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	backfill := rawSyncBackfillConfig{
		RunID: "run-overlap", DeviceID: "device-a", Server: "https://sync.example.test",
		Providers: []string{"claude"}, Format: "json", BatchSize: 64,
	}
	selected, err := selectRawSyncBackfillProviders(cfg, backfill.Providers)
	require.NoError(t, err)
	spec, err := rawSyncBackfillSpec(t.Context(), store, backfill, selected)
	require.NoError(t, err)
	var output testBuffer
	require.NoError(t, runRawSyncBackfillAttempt(t.Context(), &output, backfill, store, spec,
		[]parser.Provider{selected[0].Provider}, &acceptingRawSyncBackfillTransport{}), output.String())

	providers, _, err := rawSyncProvidersAndRoots(t.Context(), cfg)
	require.NoError(t, err)
	var watched parser.Provider
	for _, provider := range providers {
		if provider.Definition().Type == parser.AgentClaude {
			watched = provider
		}
	}
	require.NotNil(t, watched)
	result, err := rawwatch.NewAuditor(store, rawcapture.New(store), 64).AuditProviderFull(t.Context(), watched)

	require.NoError(t, err)
	assert.Positive(t, result.Unchanged)
	assert.Zero(t, result.Captured, "watch must not upload a transcript backfill already sent")
}
