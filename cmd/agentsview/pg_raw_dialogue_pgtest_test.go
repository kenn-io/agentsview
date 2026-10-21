//go:build pgtest

package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawProjectionDialogueRolloutRequeuesCompletedProductionVersion(t *testing.T) {
	cfg, _ := hostedRuntimeConfig(t)
	runtime, err := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close()) })
	repo, err := artifact.OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repo.Close()) })
	objects, err := rawsync.NewArtifactObjectStore(repo.Content())
	require.NoError(t, err)
	jobs, err := postgres.NewTenantRawIngestStore(runtime, cfg.PG.RawTenant)
	require.NoError(t, err)
	sink, err := postgres.NewRawProjectionStore(runtime, postgres.RawProjectionOptions{Tenant: cfg.PG.RawTenant})
	require.NoError(t, err)
	// The last released version, before parsers produced dialogue.
	const previousVersion = "parser-data-139+friction-v1"
	currentVersion := rawProcessingVersion()
	custody, err := rawsync.NewService(objects, jobs, rawsync.DefaultManifestLimits(), previousVersion)
	require.NoError(t, err)
	identity, err := rawsync.NewAuthIdentity(cfg.PG.RawTenant, "device-a")
	require.NoError(t, err)
	digest := sha256.Sum256([]byte("synthetic-device-a"))
	_, err = runtime.ExecContext(t.Context(), `INSERT INTO raw_devices(device_id,display_name,credential_sha256,created_at) VALUES($1,$1,$2,clock_timestamp())`, identity.DeviceID, digest[:])
	require.NoError(t, err)
	body := []byte("synthetic dialogue capture")
	digest = sha256.Sum256(body)
	ref, err := rawsync.NewObjectRef(hex.EncodeToString(digest[:]), int64(len(body)))
	require.NoError(t, err)
	_, err = custody.FinalizeObject(t.Context(), identity, parser.AgentCodex, ref, bytes.NewReader(body))
	require.NoError(t, err)
	manifest := rawsync.Manifest{
		SchemaVersion: rawsync.ManifestSchemaVersion, Provider: parser.AgentCodex,
		ConfiguredRootID: "root-a", SourceKey: "sessions/demo.jsonl#main",
		CaptureID: "dialogue-capture", CapturedAt: time.Now().UTC(), Kind: rawsync.ManifestSnapshot,
		Entries: []rawsync.Entry{{Path: "session.jsonl", Type: "file", Length: ref.Length, Objects: []rawsync.ObjectRef{ref}}},
	}
	m, err := rawsync.ValidateAndCanonicalize(identity, manifest, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	_, err = custody.CommitManifest(t.Context(), identity, manifest)
	require.NoError(t, err)
	_, err = sink.SelectSourceGeneration(t.Context(), m, previousVersion)
	require.NoError(t, err)
	leases, err := jobs.ClaimRawParseJobs(t.Context(), "previous-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	out := rawderive.ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, ForceReplace: true, Results: []parser.ParseResultOutcome{{Result: parser.ParseResult{
		Session:  parser.ParsedSession{ID: "codex:portable", SourceSessionID: "portable", Agent: parser.AgentCodex, Project: "synthetic", Machine: "capture-device", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		Messages: []parser.ParsedMessage{{Ordinal: 0, Role: parser.RoleUser, Content: "question"}, {Ordinal: 1, Role: parser.RoleAssistant, Content: "checking"}},
	}}}}}
	require.NoError(t, sink.Project(t.Context(), leases[0], m, out))
	var state string
	require.NoError(t, runtime.QueryRowContext(t.Context(), `SELECT state FROM raw_ingest_jobs WHERE id=$1`, leases[0].ID).Scan(&state))
	require.Equal(t, "complete", state)

	// A new run ID at the old production version leaves completed uploads alone.
	_, err = sink.ScheduleCurrentHeads(t.Context(), "previous-version", previousVersion, 64)
	require.NoError(t, err)
	leases, err = jobs.ClaimRawParseJobs(t.Context(), "reparse-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Empty(t, leases)

	_, err = sink.ScheduleCurrentHeads(t.Context(), "dialogue-upgrade", currentVersion, 64)
	require.NoError(t, err)
	leases, err = jobs.ClaimRawParseJobs(t.Context(), "reparse-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	assert.Equal(t, m.ManifestID, leases[0].ManifestID)
	assert.Equal(t, currentVersion, leases[0].ProcessingVersion)
	out.Outcome.Results[0].Result.Messages[1].DialogueText = new("")
	require.NoError(t, sink.Project(t.Context(), leases[0], m, out))
	resolved, err := sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	var dialogue sql.NullString
	require.NoError(t, runtime.QueryRowContext(t.Context(), `SELECT dialogue_text FROM messages WHERE session_id=$1 AND ordinal=1`, resolved.SessionID).Scan(&dialogue))
	assert.Equal(t, sql.NullString{Valid: true}, dialogue)
}
