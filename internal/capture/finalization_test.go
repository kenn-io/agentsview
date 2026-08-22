package capture

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestOversizedResultWritesBoundedFailureAndKeepsPriorFailure(t *testing.T) {
	tests := []struct {
		name        string
		priorReason ReasonCode
		wantReason  ReasonCode
	}{
		{name: "new failure", wantReason: ReasonResultSizeLimit},
		{
			name:        "prior failure",
			priorReason: ReasonNoSession, wantReason: ReasonNoSession,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limits := DefaultLimits()
			limits.MaxResultBytes = minResultBytes
			state := &captureState{
				dir: t.TempDir(),
				manifest: manifest{
					OccurrenceID:      "oversized-result",
					Provider:          string(ProviderClaude),
					ProviderSessionID: "11111111-1111-4111-8111-111111111111",
					Limits:            limits,
				},
			}
			var priorData []byte
			if test.priorReason != "" {
				prior := failureResult(state.manifest, test.priorReason, "test")
				var err error
				priorData, err = encodeResult(prior, limits.MaxResultBytes)
				require.NoError(t, err)
				require.NoError(t, state.storeAttempt(priorData, false))
			}
			oversized := baseResult(state.manifest, "test")
			oversized.Reporting = ReportingOutcome{Outcome: ReportingComplete}
			for i := range 512 {
				oversized.Provider.IncludedSessionIDs = append(
					oversized.Provider.IncludedSessionIDs,
					strings.Repeat("s", 250)+fmt.Sprintf("%03d", i),
				)
			}

			result, data, err := encodeAndStoreResult(state, oversized, nil)

			require.Error(t, err)
			assert.Equal(t, ReportingFailed, result.Reporting.Outcome)
			assert.Equal(t, test.wantReason, result.Reporting.Reason)
			require.NotEmpty(t, data)
			assert.LessOrEqual(t, len(data), limits.MaxResultBytes)
			stored, readErr := os.ReadFile(state.sealedPath())
			require.NoError(t, readErr)
			assert.Equal(t, data, stored)
			if priorData != nil {
				assert.Equal(t, priorData, data)
			}
		})
	}
}

func TestCompletedAttemptBecomesRecoverableTimeoutAtSealDeadline(t *testing.T) {
	state := &captureState{
		dir: t.TempDir(),
		manifest: manifest{
			OccurrenceID: "seal-timeout", Limits: DefaultLimits(),
		},
		finalizationDeadline: time.Now().Add(-time.Second),
	}

	result, data, err := encodeAndStoreResult(state, Result{
		Schema:       Schema{Name: ResultSchemaName, Version: ResultSchemaVersion},
		OccurrenceID: "seal-timeout",
		Reporting: ReportingOutcome{
			Outcome: ReportingComplete,
		},
		Producer: ProducerMetadata{AgentsViewVersion: "test"},
	}, nil)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, ReportingFailed, result.Reporting.Outcome)
	assert.Equal(t, ReasonFinalizationTimeout, result.Reporting.Reason)
	assert.Empty(t, state.manifest.SealedDigest)
	stored, readErr := os.ReadFile(state.sealedPath())
	require.NoError(t, readErr)
	assert.Equal(t, data, stored)
}

func TestCompletedRetryKeepsPriorFailureWhenSealingExpires(t *testing.T) {
	state := &captureState{
		dir: t.TempDir(),
		manifest: manifest{
			OccurrenceID: "retry-seal-timeout", Provider: string(ProviderClaude),
			Limits: DefaultLimits(),
		},
	}
	first := failureResult(state.manifest, ReasonNoSession, "test")
	firstData, err := encodeResult(first, state.manifest.Limits.MaxResultBytes)
	require.NoError(t, err)
	require.NoError(t, state.storeAttempt(firstData, false))
	state.finalizationDeadline = time.Now().Add(-time.Second)

	result, data, err := encodeAndStoreResult(state, Result{
		Schema:       Schema{Name: ResultSchemaName, Version: ResultSchemaVersion},
		OccurrenceID: state.manifest.OccurrenceID,
		Provider:     ProviderIdentity{Name: string(ProviderClaude)},
		Reporting:    ReportingOutcome{Outcome: ReportingComplete},
		Producer:     ProducerMetadata{AgentsViewVersion: "test"},
	}, nil)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, ReasonNoSession, result.Reporting.Reason)
	assert.Equal(t, firstData, data)
	stored, readErr := os.ReadFile(state.sealedPath())
	require.NoError(t, readErr)
	assert.Equal(t, firstData, stored)
	assert.Empty(t, state.manifest.SealedDigest)
}

func TestFinishIngestedResultClassifiesArchiveCloseFailure(t *testing.T) {
	database, err := db.OpenIsolated(filepath.Join(t.TempDir(), "capture.db"))
	require.NoError(t, err)
	rows, err := database.Reader().Query("SELECT 1")
	require.NoError(t, err)
	require.True(t, rows.Next())
	restoreTimeout := db.SetCloseDrainTimeoutForTest(10 * time.Millisecond)
	t.Cleanup(restoreTimeout)
	t.Cleanup(func() {
		require.NoError(t, rows.Close())
		require.NoError(t, database.Close())
	})

	state := &captureState{manifest: manifest{
		OccurrenceID:      "close-failure",
		Provider:          string(ProviderClaude),
		ProviderSessionID: "11111111-1111-4111-8111-111111111111",
	}}
	result, err := finishIngestedResult(
		context.Background(),
		state,
		&ingestedCapture{
			Database: database,
			Root: &db.Session{
				ID: "11111111-1111-4111-8111-111111111111",
			},
			Usage: &db.SessionUsage{},
		},
		"test",
	)

	require.Error(t, err)
	assert.Equal(t, ReportingFailed, result.Reporting.Outcome)
	assert.Equal(t, ReasonIngestFailed, result.Reporting.Reason)
}

func TestOpenCaptureEngineStopsBeforeInitializationAfterDeadline(t *testing.T) {
	state := &captureState{dir: t.TempDir(), manifest: manifest{
		Provider: string(ProviderClaude),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	database, engine, err := openCaptureEngine(ctx, state, nil)

	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, database)
	assert.Nil(t, engine)
	assert.NoFileExists(t, state.archivePath())
}

func TestOpenCaptureEngineRebuildsInterruptedScratchArchive(t *testing.T) {
	state := &captureState{dir: t.TempDir(), manifest: manifest{
		Provider: string(ProviderClaude),
	}}
	require.NoError(t, os.WriteFile(state.archivePath(), []byte("partial"), 0o600))

	database, engine, err := openCaptureEngine(t.Context(), state, nil)
	require.NoError(t, err)
	engine.Close()
	require.NoError(t, database.Close())

	info, err := os.Stat(state.archivePath())
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(len("partial")))
}

func TestFinishIngestedResultBoundsArchiveCloseByFinalizationDeadline(
	t *testing.T,
) {
	database, err := db.OpenIsolated(filepath.Join(t.TempDir(), "capture.db"))
	require.NoError(t, err)
	rows, err := database.Reader().Query("SELECT 1")
	require.NoError(t, err)
	require.True(t, rows.Next())
	restoreTimeout := db.SetCloseDrainTimeoutForTest(2 * time.Second)
	t.Cleanup(restoreTimeout)
	t.Cleanup(func() {
		require.NoError(t, rows.Close())
		require.NoError(t, database.Close())
	})

	state := &captureState{manifest: manifest{
		OccurrenceID:      "bounded-close",
		Provider:          string(ProviderClaude),
		ProviderSessionID: "11111111-1111-4111-8111-111111111111",
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := finishIngestedResult(
		ctx,
		state,
		&ingestedCapture{
			Database: database,
			Root: &db.Session{
				ID: "11111111-1111-4111-8111-111111111111",
			},
			Usage: &db.SessionUsage{},
		},
		"test",
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), 500*time.Millisecond)
	assert.Equal(t, ReportingFailed, result.Reporting.Outcome)
	assert.Equal(t, ReasonFinalizationTimeout, result.Reporting.Reason)
}

func TestCodexFinalVerificationRejectsLateChildCandidate(t *testing.T) {
	root := t.TempDir()
	anchor := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	dayDir := filepath.Join(root, filepath.FromSlash(anchor.Format("2006/01/02")))
	require.NoError(t, os.MkdirAll(dayDir, 0o700))
	rootID := "11111111-1111-4111-8111-111111111111"
	childID := "22222222-2222-4222-8222-222222222222"
	writeSource := func(name, id string) string {
		path := filepath.Join(dayDir, name+"-"+id+".jsonl")
		line := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q}}`, id)
		require.NoError(t, os.WriteFile(path, []byte(line+"\n"), 0o600))
		return path
	}
	rootPath := writeSource("rollout-root", rootID)
	childPath := writeSource("rollout-child", childID)
	limits := testLimits()
	expected, err := snapshotSources(
		context.Background(), []string{rootPath, childPath}, limits)
	require.NoError(t, err)
	writeSource("rollout-late-conflict", childID)
	state := &captureState{manifest: manifest{
		Provider: string(ProviderCodex), ProviderRoot: root, Limits: limits,
	}}

	unchanged, err := liveSourcesUnchanged(
		context.Background(), state, expected, []codexSourceSelection{
			{ID: rootID, Anchor: anchor, LivePath: rootPath},
			{ID: childID, Anchor: anchor, LivePath: childPath},
		},
	)

	require.Error(t, err)
	assert.False(t, unchanged)
	assert.Equal(t, ReasonMultipleSessions, reasonForError(err, ""))
}
