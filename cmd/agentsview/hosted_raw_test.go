package main

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/vector"
	"hegel.dev/go/hegel"
)

func TestHostedRawArchiveOpenPreservesStoredState(t *testing.T) {
	check := func(ht require.TestingT, present, stale bool) {
		dir := t.TempDir()
		cfg := config.Config{DataDir: dir, DBPath: filepath.Join(dir, "sessions.db"), RawSync: config.RawSyncConfig{Hosted: true}}
		var before []byte
		if present {
			d, err := db.OpenIsolated(t.Context(), cfg.DBPath)
			require.NoError(ht, err)
			require.NoError(ht, d.UpsertSession(t.Context(), db.Session{ID: "fixture", Agent: "claude", Project: "fixture", Machine: "device", FirstMessage: new("hello")}))
			require.NoError(ht, d.Update(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), "UPDATE sessions SET is_automated=1")
				return err
			}))
			if stale {
				err = d.Update(t.Context(), func(tx *sql.Tx) error { _, err := tx.ExecContext(t.Context(), "PRAGMA user_version = 1"); return err })
				require.NoError(ht, err)
			}
			require.NoError(ht, d.Close())
			before, err = os.ReadFile(cfg.DBPath)
			require.NoError(ht, err)
		}
		d, lock, err := openServeDBWith(t.Context(), cfg, nil)
		require.Nil(ht, lock)
		require.NoError(ht, err)
		require.True(ht, d.ReadOnly())
		require.Equal(ht, present && stale, d.NeedsResync())
		require.ErrorIs(ht, d.RequireSourceSync(t.Context()), config.ErrHostedRawMode)
		only, err := d.IsArchiveOnly(t.Context())
		require.NoError(ht, err)
		require.False(ht, only)

		if present {
			session, err := d.GetSession(t.Context(), "fixture")
			require.NoError(ht, err)
			require.True(ht, session.IsAutomated, "hosted startup must preserve the stored classification")
		}
		require.NoError(ht, d.Close())
		if present {
			after, err := os.ReadFile(cfg.DBPath)
			require.NoError(ht, err)
			require.True(ht, bytes.Equal(before, after))
			local, err := db.OpenIsolated(t.Context(), cfg.DBPath)
			require.NoError(ht, err)
			session, err := local.GetSession(t.Context(), "fixture")
			require.NoError(ht, err)
			require.False(ht, session.IsAutomated, "ordinary startup must audit this intentionally stale classification")
			require.NoError(ht, local.Close())
		} else {
			_, err = os.Stat(cfg.DBPath)
			require.True(ht, os.IsNotExist(err))
		}
	}
	t.Run("missing-archive-regression", func(t *testing.T) { check(t, false, false) })
	hegel.Test(t, func(ht *hegel.T) { check(ht, hegel.Draw(ht, hegel.Booleans()), hegel.Draw(ht, hegel.Booleans())) }, hegel.WithTestCases(8), hegel.WithDatabase(""))
}

func TestHostedRawRefusesWritableCLIWorkBeforeOpeningArchive(t *testing.T) {
	t.Run("resync-build-disabled-features-regression", func(t *testing.T) {
		var output bytes.Buffer
		cfg := config.Config{RawSync: config.RawSyncConfig{Hosted: true}}
		err := runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "resync-build"}, &output)
		require.ErrorIs(t, err, config.ErrHostedRawMode)
		var line workerLine
		require.NoError(t, json.Unmarshal(output.Bytes(), &line))
		require.NotNil(t, line.Result)
		require.Equal(t, "failed", line.Result.Status)
		require.Equal(t, config.ErrHostedRawMode.Error(), line.Result.Error)
	})
	t.Run("disabled-features-writable-open-regression", func(t *testing.T) {
		cfg := config.Config{RawSync: config.RawSyncConfig{Hosted: true}}
		_, _, err := openWriteDB(t.Context(), cfg)
		require.ErrorIs(t, err, config.ErrHostedRawMode)
	})
	hegel.Test(t, func(ht *hegel.T) {
		dir := t.TempDir()
		cfg := config.Config{DataDir: dir, DBPath: filepath.Join(dir, "sessions.db"), RawSync: config.RawSyncConfig{Hosted: true}}
		cfg.Vector.Enabled = hegel.Draw(ht, hegel.Booleans())
		cfg.Recall.Extract.Enabled = hegel.Draw(ht, hegel.Booleans())
		which := hegel.Draw(ht, hegel.Integers(0, 7))
		var err error
		switch which {
		case 0:
			_, _, err = openWriteDB(t.Context(), cfg)
		case 1:
			err = runEmbeddingsBuildDirect(t.Context(), io.Discard, cfg, vector.BuildRequest{FullRebuild: hegel.Draw(ht, hegel.Booleans())})
		case 2:
			_, _, err = openWritableExtractDB(t.Context(), cfg)
		case 3:
			err = requireDaemonSourceSync(t.Context(), cfg)
		default:
			err = runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: []string{"startup", "sync", "audit", "resync-build"}[which-4]}, io.Discard)
		}
		require.ErrorIs(ht, err, config.ErrHostedRawMode)
		_, err = os.Stat(cfg.DBPath)
		require.True(ht, os.IsNotExist(err))
	}, hegel.WithTestCases(20), hegel.WithDatabase(""))
}

func TestHostedRawDiagnosticsReportUnsupportedEnabledProviders(t *testing.T) {
	dir := t.TempDir()
	hegel.Test(t, func(ht *hegel.T) {
		cfg := config.Config{DataDir: dir}
		for _, def := range parser.Registry {
			if def.Type != parser.AgentClaude && def.Type != parser.AgentGemini {
				cfg.DisabledAgents = append(cfg.DisabledAgents, def.Type)
			}
		}
		disabled := hegel.Draw(ht, hegel.Booleans())
		if disabled {
			cfg.DisabledAgents = append(cfg.DisabledAgents, parser.AgentGemini)
		}
		cfg.RawSync.Hosted = hegel.Draw(ht, hegel.Booleans())
		var output bytes.Buffer
		require.NoError(ht, runRawSyncStatusWith(t.Context(), &output, cfg))
		var status struct {
			Unsupported []string `json:"unsupported_enabled_providers"`
		}
		require.NoError(ht, json.Unmarshal(output.Bytes(), &status))
		got := status.Unsupported
		output.Reset()
		writeDoctorSyncReport(&output, doctorSyncReport{Config: cfg})
		require.Equal(ht, !disabled, strings.Contains(output.String(), "Raw sync unsupported enabled providers: gemini"))
		if disabled {
			require.Empty(ht, got)
		} else {
			require.Equal(ht, []string{"gemini"}, got)
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
}

func TestHostedRawStartupSchedulersStayIdle(t *testing.T) {
	database, err := db.OpenIsolated(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	defer database.Close()
	t.Run("enabled-extraction-no-model-regression", func(t *testing.T) {
		cfg := config.Config{RawSync: config.RawSyncConfig{Hosted: true}}
		cfg.Recall.Extract.Enabled = true
		scheduler, err := setupRecallExtraction(cfg, database, nil)
		require.NoError(t, err)
		require.Nil(t, scheduler)
	})
	hegel.Test(t, func(ht *hegel.T) {
		cfg := vectorTestConfig(t.TempDir())
		cfg.RawSync.Hosted = true
		cfg.Vector.Enabled = hegel.Draw(ht, hegel.Booleans())
		cfg.Recall.Extract.Enabled = hegel.Draw(ht, hegel.Booleans())
		vs, err := setupVectorServing(t.Context(), cfg, database, nil)
		if vs.Close != nil {
			defer vs.Close()
		}
		require.NoError(ht, err)
		require.Nil(ht, vs.Scheduler)
		require.Nil(ht, vs.RecallScheduler)
		extraction, err := setupRecallExtraction(cfg, database, nil)
		require.NoError(ht, err)
		require.Nil(ht, extraction)
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
}

func TestHostedRawCLIDaemonTransportRefused(t *testing.T) {
	dir := t.TempDir()
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntimeWithAuthAndNoSync(dir, host, port, "test", "", false, false, false, nil)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })
	t.Run("ordinary-daemon-no-sync-false-regression", func(t *testing.T) {
		cfg := config.Config{DataDir: dir, RawSync: config.RawSyncConfig{Hosted: true}}
		_, _, err := resolveArchiveWriteBackend(t.Context(), cfg, transportIntentArchiveWrite)
		require.ErrorIs(t, err, config.ErrHostedRawMode)
	})
	hegel.Test(t, func(ht *hegel.T) {
		cfg := config.Config{DataDir: dir, RawSync: config.RawSyncConfig{Hosted: true}, NoSync: hegel.Draw(ht, hegel.Booleans())}
		var err error
		if hegel.Draw(ht, hegel.Booleans()) {
			_, _, err = syncService(t.Context(), cfg, transport{Mode: transportHTTP})
		} else {
			_, _, err = resolveArchiveWriteBackend(t.Context(), cfg, transportIntentArchiveWrite)
		}
		require.ErrorIs(ht, err, config.ErrHostedRawMode)
	}, hegel.WithTestCases(8), hegel.WithDatabase(""))
}
