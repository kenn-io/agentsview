//go:build pgtest

package postgres

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
)

func boundParityFixture(t *testing.T) (hostedFixture, *MigrationParityStore, rawderive.ParityLease, rawderive.ParityBinding) {
	t.Helper()
	f := newHostedFixture(t, "tenant-a")
	store, err := NewMigrationParityStore(t.Context(), f.runtime, MigrationParityOptions{Schema: f.schema, Tenant: f.tenant})
	require.NoError(t, err)
	request := parityTestRequest()
	_, err = store.CreateOrResumeParity(t.Context(), request, 4)
	require.NoError(t, err)
	lease := claimedParityLease(t, f, request, time.Hour)
	binding := parityPhysicalBinding(f.tenant)
	binding.Request = request
	lease, err = store.BindParity(t.Context(), lease, binding)
	require.NoError(t, err)
	return f, store, lease, binding
}

func TestParityEvidenceRecordsTheExactMemberUnionIdempotently(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000081")
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	sources, err := scenario.store.NextParitySources(t.Context(), scenario.lease, "", 4)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	var encodedKey, encodedFingerprint []byte
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT member_key,baseline_fingerprint FROM migration_parity_members WHERE run_id=$1 AND source_id=$2`, scenario.lease.RunID, sources[0].ID).Scan(&encodedKey, &encodedFingerprint))
	key, err := decodeParityMemberKey(encodedKey)
	require.NoError(t, err)
	expected, err := decodeParityFingerprint(encodedFingerprint)
	require.NoError(t, err)

	candidateOnly := rawderive.ParityMemberKey{SourceID: sources[0].ID, LogicalKey: "member-b", Kind: "session"}
	result := rawderive.ParitySourceResult{Complete: true, Members: []rawderive.ParityMemberResult{
		{Key: key, Fingerprint: &expected},
		{Key: candidateOnly, Fingerprint: &rawderive.ParityFingerprint{Semantic: rawderive.ParityDigest{2}}},
	}}
	require.NoError(t, scenario.store.ReserveParitySource(t.Context(), scenario.lease, sources[0]))
	require.NoError(t, scenario.store.RecordParitySource(t.Context(), scenario.lease, sources[0], result))
	require.NoError(t, scenario.store.RecordParitySource(t.Context(), scenario.lease, sources[0], result), "completed retries replace evidence instead of incrementing counts")

	report, err := scenario.store.ReadParityReport(t.Context(), scenario.lease.RunID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, report.Members.Matched)
	assert.EqualValues(t, 1, report.Members.Missing)
	assert.EqualValues(t, 1, report.Sources.Missing)
	var rows int
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_members WHERE run_id=$1`, scenario.lease.RunID).Scan(&rows))
	assert.Equal(t, 2, rows)
}

func TestParityEvidenceRejectsUnreservedPublicationWithoutMutation(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000136")
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	sources, err := scenario.store.NextParitySources(t.Context(), scenario.lease, "", 1)
	require.NoError(t, err)
	require.Len(t, sources, 1)

	err = scenario.store.RecordParitySource(t.Context(), scenario.lease, sources[0], rawderive.ParitySourceResult{
		Complete: true,
		Members: []rawderive.ParityMemberResult{{
			Key:         rawderive.ParityMemberKey{SourceID: sources[0].ID, LogicalKey: "unreserved", Kind: "session"},
			Fingerprint: &rawderive.ParityFingerprint{Semantic: rawderive.ParityDigest{9}},
		}},
	})
	require.ErrorContains(t, err, "reservation")
	var generation int64
	var complete bool
	var candidateOnly int
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT evidence_generation,candidate_complete FROM migration_parity_sources WHERE run_id=$1 AND source_id=$2`, scenario.lease.RunID, sources[0].ID).Scan(&generation, &complete))
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_members WHERE run_id=$1 AND source_id=$2 AND inventory_kind='candidate_only'`, scenario.lease.RunID, sources[0].ID).Scan(&candidateOnly))
	assert.Zero(t, generation)
	assert.False(t, complete)
	assert.Zero(t, candidateOnly)
}

func TestParityEvidenceCandidateOnlyExplicitBlockerOutranksMissing(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000138")
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	sources, err := scenario.store.NextParitySources(t.Context(), scenario.lease, "", 1)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.NoError(t, scenario.store.ReserveParitySource(t.Context(), scenario.lease, sources[0]))
	key := rawderive.ParityMemberKey{SourceID: sources[0].ID, LogicalKey: "provider-excluded", Kind: "exclusion"}
	require.NoError(t, scenario.store.RecordParitySource(t.Context(), scenario.lease, sources[0], rawderive.ParitySourceResult{
		Complete: true, Blocker: rawderive.ParityPartial, Code: "exclusion_provenance_unavailable",
		Members: []rawderive.ParityMemberResult{{Key: key, Verdict: rawderive.ParityPartial}},
	}))
	var verdict string
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT verdict FROM migration_parity_members WHERE run_id=$1 AND source_id=$2 AND kind='exclusion'`, scenario.lease.RunID, sources[0].ID).Scan(&verdict))
	assert.Equal(t, string(rawderive.ParityPartial), verdict)
}

func TestParityQueueReservationSurvivesCrashAndExplicitResumeRetries(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000131")
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	_, err := scenario.runtime.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET batch_size=1,budget_sources=1 WHERE run_id=$1`, scenario.lease.RunID)
	require.NoError(t, err)
	sources, err := scenario.store.NextParitySources(t.Context(), scenario.lease, "", 1)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.NoError(t, scenario.store.ReserveParitySource(t.Context(), scenario.lease, sources[0]))
	require.ErrorContains(t, scenario.store.ReserveParitySource(t.Context(), scenario.lease, sources[0]), "stale")
	remaining, err := scenario.store.NextParitySources(t.Context(), scenario.lease, "", 1)
	require.NoError(t, err)
	assert.Empty(t, remaining, "a crash before Record must not select the source again")
	var budget int
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT budget_sources FROM migration_parity_runs WHERE run_id=$1`, scenario.lease.RunID).Scan(&budget))
	assert.Zero(t, budget)

	_, err = scenario.runtime.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE run_id=$1`, scenario.lease.RunID)
	require.NoError(t, err)
	reclaimed, err := scenario.store.ClaimParity(t.Context(), "owner-restart", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, reclaimed)
	assert.Zero(t, reclaimed.BatchSize)
	require.NoError(t, scenario.store.FinishParityRequest(t.Context(), *reclaimed, "pending"))
	_, err = scenario.store.CreateOrResumeParity(t.Context(), reclaimed.Request, 1)
	require.NoError(t, err)
	retry, err := scenario.store.ClaimParity(t.Context(), "owner-resume", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, retry)
	assert.Equal(t, int64(2), retry.RequestGeneration)
	retrySources, err := scenario.store.NextParitySources(t.Context(), *retry, "", 1)
	require.NoError(t, err)
	require.Len(t, retrySources, 1, "an explicit new generation may retry the incomplete source")
}

func TestParityQueueBatchOneRestartDoesNotSpendSecondSource(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000133")
	runtimeManifest, _ := scenario.runtime.acceptScoped(t, "device-a", "capture-b", "", parser.AgentCodex, "root-a", "source-b")
	baselineManifest, _ := scenario.baseline.acceptScoped(t, "device-a", "capture-b", "", parser.AgentCodex, "root-a", "source-b")
	require.NoError(t, scenario.runtime.sink.Project(t.Context(), scenario.runtime.lease(t, runtimeManifest), runtimeManifest, parityOutcomeWithResult("runtime-b", "old")))
	require.NoError(t, scenario.baseline.sink.Project(t.Context(), scenario.baseline.lease(t, baselineManifest), baselineManifest, parityOutcomeWithResult("baseline-b", "old")))
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	_, err := scenario.runtime.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET batch_size=1,budget_sources=1 WHERE run_id=$1`, scenario.lease.RunID)
	require.NoError(t, err)
	sources, err := scenario.store.NextParitySources(t.Context(), scenario.lease, "", 1)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.NoError(t, scenario.store.ReserveParitySource(t.Context(), scenario.lease, sources[0]))
	require.NoError(t, scenario.store.RecordParitySource(t.Context(), scenario.lease, sources[0], rawderive.ParitySourceResult{Complete: true}))
	_, err = scenario.runtime.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE run_id=$1`, scenario.lease.RunID)
	require.NoError(t, err)
	reclaimed, err := scenario.store.ClaimParity(t.Context(), "owner-restart", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, reclaimed)
	assert.Zero(t, reclaimed.BatchSize, "restart must not authorize the second source")
	require.NoError(t, scenario.store.FinishParityRequest(t.Context(), *reclaimed, "pending"))
	_, err = scenario.store.CreateOrResumeParity(t.Context(), reclaimed.Request, 1)
	require.NoError(t, err)
	retry, err := scenario.store.ClaimParity(t.Context(), "owner-resume", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, retry)
	next, err := scenario.store.NextParitySources(t.Context(), *retry, "", 1)
	require.NoError(t, err)
	require.Len(t, next, 1)
	assert.NotEqual(t, sources[0].ID, next[0].ID)
}

func TestParityEvidencePreservesCapturedAmbiguityWithoutFingerprint(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000082")
	_, err := scenario.baseline.runtime.ExecContext(t.Context(), `DELETE FROM session_sources WHERE source_id=$1`, scenario.sourceID)
	require.NoError(t, err)
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	sources, err := scenario.store.NextParitySources(t.Context(), scenario.lease, "", 4)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.NoError(t, scenario.store.ReserveParitySource(t.Context(), scenario.lease, sources[0]))
	var encodedKey []byte
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT member_key FROM migration_parity_members WHERE run_id=$1 AND source_id=$2`, scenario.lease.RunID, sources[0].ID).Scan(&encodedKey))
	key, err := decodeParityMemberKey(encodedKey)
	require.NoError(t, err)
	candidate := rawderive.ParityFingerprint{Semantic: rawderive.ParityDigest{1}}
	require.NoError(t, scenario.store.RecordParitySource(t.Context(), scenario.lease, sources[0], rawderive.ParitySourceResult{
		Complete: true,
		Members:  []rawderive.ParityMemberResult{{Key: key, Fingerprint: &candidate}},
	}))
	var sourceVerdict, memberVerdict string
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT s.verdict,m.verdict FROM migration_parity_sources s JOIN migration_parity_members m USING(tenant_id,run_id,init_epoch,source_id) WHERE s.run_id=$1 AND s.source_id=$2`, scenario.lease.RunID, sources[0].ID).Scan(&sourceVerdict, &memberVerdict))
	assert.Equal(t, string(rawderive.ParityAmbiguous), sourceVerdict)
	assert.Equal(t, string(rawderive.ParityAmbiguous), memberVerdict)
}

func TestParityEvidencePreservesCapturedUnresolvedLinkWithFingerprint(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000121")
	_, err := scenario.baseline.runtime.ExecContext(t.Context(), `UPDATE sessions SET parser_parent_session_id='missing-parser-parent' WHERE provenance_kind='raw'`)
	require.NoError(t, err)
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	sources, err := scenario.store.NextParitySources(t.Context(), scenario.lease, "", 4)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.NoError(t, scenario.store.ReserveParitySource(t.Context(), scenario.lease, sources[0]))
	var encodedKey, encodedFingerprint []byte
	var mapping, capturedVerdict string
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT member_key,baseline_fingerprint,mapping_state,verdict FROM migration_parity_members WHERE run_id=$1 AND source_id=$2`, scenario.lease.RunID, sources[0].ID).Scan(&encodedKey, &encodedFingerprint, &mapping, &capturedVerdict))
	assert.Equal(t, "exact", mapping)
	assert.NotEmpty(t, encodedFingerprint)
	assert.Equal(t, string(rawderive.ParityAmbiguous), capturedVerdict)
	key, err := decodeParityMemberKey(encodedKey)
	require.NoError(t, err)
	fingerprint, err := decodeParityFingerprint(encodedFingerprint)
	require.NoError(t, err)
	require.NoError(t, scenario.store.RecordParitySource(t.Context(), scenario.lease, sources[0], rawderive.ParitySourceResult{
		Complete: true,
		Members:  []rawderive.ParityMemberResult{{Key: key, Fingerprint: &fingerprint}},
	}))
	var memberVerdict, sourceVerdict string
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT m.verdict,s.verdict FROM migration_parity_members m JOIN migration_parity_sources s USING(tenant_id,run_id,init_epoch,source_id) WHERE m.run_id=$1 AND m.source_id=$2`, scenario.lease.RunID, sources[0].ID).Scan(&memberVerdict, &sourceVerdict))
	assert.Equal(t, string(rawderive.ParityAmbiguous), memberVerdict)
	assert.Equal(t, string(rawderive.ParityAmbiguous), sourceVerdict)
}

func TestParityEvidenceRecordCannotClearValidatedStaleMember(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000122")
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	sources, err := scenario.store.NextParitySources(t.Context(), scenario.lease, "", 4)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.NoError(t, scenario.store.ReserveParitySource(t.Context(), scenario.lease, sources[0]))
	var encodedKey, encodedFingerprint, physical []byte
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT member_key,baseline_fingerprint,physical_fingerprint FROM migration_parity_members WHERE run_id=$1 AND source_id=$2`, scenario.lease.RunID, sources[0].ID).Scan(&encodedKey, &encodedFingerprint, &physical))
	key, err := decodeParityMemberKey(encodedKey)
	require.NoError(t, err)
	fingerprint, err := decodeParityFingerprint(encodedFingerprint)
	require.NoError(t, err)
	markParityScenarioMatched(t, scenario)
	_, err = scenario.baseline.runtime.ExecContext(t.Context(), `UPDATE tool_result_events SET content='stale',content_length=5 WHERE session_id=(SELECT physical_session_id FROM session_sources WHERE source_id=$1)`, scenario.sourceID)
	require.NoError(t, err)
	require.NoError(t, scenario.store.ValidateParityEvidence(t.Context(), scenario.lease, scenario.reader, scenario.binding, 1))
	require.NoError(t, scenario.store.RecordParitySource(t.Context(), scenario.lease, sources[0], rawderive.ParitySourceResult{
		Complete: true,
		Members:  []rawderive.ParityMemberResult{{Key: key, Fingerprint: &fingerprint}},
	}))
	var memberVerdict, sourceVerdict string
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT m.verdict,s.verdict FROM migration_parity_members m JOIN migration_parity_sources s USING(tenant_id,run_id,init_epoch,source_id) WHERE m.run_id=$1 AND m.source_id=$2`, scenario.lease.RunID, sources[0].ID).Scan(&memberVerdict, &sourceVerdict))
	assert.Equal(t, string(rawderive.ParityStale), memberVerdict)
	assert.Equal(t, string(rawderive.ParityStale), sourceVerdict)
}

func TestParityEvidenceTransientAttemptCanRecoverButCapturedBlockerCannot(t *testing.T) {
	t.Run("transient parse failure", func(t *testing.T) {
		scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000123")
		require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
		sources, err := scenario.store.NextParitySources(t.Context(), scenario.lease, "", 4)
		require.NoError(t, err)
		require.Len(t, sources, 1)
		require.NoError(t, scenario.store.ReserveParitySource(t.Context(), scenario.lease, sources[0]))
		var encodedKey, encodedFingerprint []byte
		require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT member_key,baseline_fingerprint FROM migration_parity_members WHERE run_id=$1 AND source_id=$2`, scenario.lease.RunID, sources[0].ID).Scan(&encodedKey, &encodedFingerprint))
		key, err := decodeParityMemberKey(encodedKey)
		require.NoError(t, err)
		fingerprint, err := decodeParityFingerprint(encodedFingerprint)
		require.NoError(t, err)
		require.NoError(t, scenario.store.RecordParitySource(t.Context(), scenario.lease, sources[0], rawderive.ParitySourceResult{Blocker: rawderive.ParityPartial, Code: "parse_failed"}))
		pending, err := scenario.store.NextParitySources(t.Context(), scenario.lease, "", 4)
		require.NoError(t, err)
		assert.Empty(t, pending, "an incomplete attempt is finite within one request generation")
		require.NoError(t, scenario.store.FinishParityRequest(t.Context(), scenario.lease, "pending"))
		_, err = scenario.store.CreateOrResumeParity(t.Context(), scenario.lease.Request, 1)
		require.NoError(t, err)
		retry, err := scenario.store.ClaimParity(t.Context(), "retry-owner", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, retry)
		pending, err = scenario.store.NextParitySources(t.Context(), *retry, "", 1)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		require.NoError(t, scenario.store.ReserveParitySource(t.Context(), *retry, pending[0]))
		require.NoError(t, scenario.store.RecordParitySource(t.Context(), *retry, pending[0], rawderive.ParitySourceResult{
			Complete: true,
			Members:  []rawderive.ParityMemberResult{{Key: key, Fingerprint: &fingerprint}},
		}))
		var verdict, code string
		require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT verdict,code FROM migration_parity_sources WHERE run_id=$1 AND source_id=$2`, scenario.lease.RunID, sources[0].ID).Scan(&verdict, &code))
		assert.Equal(t, string(rawderive.ParityMatched), verdict)
		assert.Equal(t, "pending", code)
	})

	t.Run("captured missing history", func(t *testing.T) {
		scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000124")
		_, err := scenario.runtime.runtime.ExecContext(t.Context(), `UPDATE raw_source_heads SET receipt=repeat('a',64) WHERE source_key='source-a'`)
		require.NoError(t, err)
		require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
		sources, err := scenario.store.NextParitySources(t.Context(), scenario.lease, "", 4)
		require.NoError(t, err)
		require.Len(t, sources, 1)
		require.NoError(t, scenario.store.ReserveParitySource(t.Context(), scenario.lease, sources[0]))
		var encodedKey, encodedFingerprint []byte
		require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT member_key,baseline_fingerprint FROM migration_parity_members WHERE run_id=$1 AND source_id=$2`, scenario.lease.RunID, sources[0].ID).Scan(&encodedKey, &encodedFingerprint))
		key, err := decodeParityMemberKey(encodedKey)
		require.NoError(t, err)
		fingerprint, err := decodeParityFingerprint(encodedFingerprint)
		require.NoError(t, err)
		require.NoError(t, scenario.store.RecordParitySource(t.Context(), scenario.lease, sources[0], rawderive.ParitySourceResult{
			Complete: true,
			Members:  []rawderive.ParityMemberResult{{Key: key, Fingerprint: &fingerprint}},
		}))
		var verdict, code string
		require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT verdict,code FROM migration_parity_sources WHERE run_id=$1 AND source_id=$2`, scenario.lease.RunID, sources[0].ID).Scan(&verdict, &code))
		assert.Equal(t, string(rawderive.ParityPartial), verdict)
		assert.Equal(t, "missing_history", code)
	})
}

func TestParityEvidenceRejectsChangedRuntimeDependencyAtRecord(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000083")
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	sources, err := scenario.store.NextParitySources(t.Context(), scenario.lease, "", 4)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.NoError(t, scenario.store.ReserveParitySource(t.Context(), scenario.lease, sources[0]))
	_, err = scenario.runtime.runtime.ExecContext(t.Context(), `UPDATE raw_source_projections SET diagnostics=diagnostics||' changed' WHERE source_id=$1`, sources[0].ID)
	require.NoError(t, err)
	err = scenario.store.RecordParitySource(t.Context(), scenario.lease, sources[0], rawderive.ParitySourceResult{Complete: true})
	require.ErrorContains(t, err, "dependency_changed")
	var verdict, code string
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT verdict,code FROM migration_parity_sources WHERE run_id=$1 AND source_id=$2`, scenario.lease.RunID, sources[0].ID).Scan(&verdict, &code))
	assert.Equal(t, string(rawderive.ParityStale), verdict)
	assert.Equal(t, "dependency_changed", code)
}

func TestParityEvidenceRecordLooksUpOnlyTheExactLateCohortSource(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000125")
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	sources, err := scenario.store.NextParitySources(t.Context(), scenario.lease, "", 4)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.NoError(t, scenario.store.ReserveParitySource(t.Context(), scenario.lease, sources[0]))
	var encodedKey, encodedFingerprint []byte
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT member_key,baseline_fingerprint FROM migration_parity_members WHERE run_id=$1 AND source_id=$2`, scenario.lease.RunID, sources[0].ID).Scan(&encodedKey, &encodedFingerprint))
	key, err := decodeParityMemberKey(encodedKey)
	require.NoError(t, err)
	fingerprint, err := decodeParityFingerprint(encodedFingerprint)
	require.NoError(t, err)
	var targetHash string
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT source_key_sha256 FROM raw_source_heads WHERE source_key='source-a'`).Scan(&targetHash))
	_, err = scenario.runtime.runtime.ExecContext(t.Context(), `WITH candidates AS (
		 SELECT g,repeat(md5('round2-source-'||g::text),2) source_hash FROM generate_series(1,10000) g
	), chosen AS (
		 SELECT g,source_hash,row_number() OVER (ORDER BY source_hash) n FROM candidates WHERE source_hash<$1 ORDER BY source_hash LIMIT 129
	)
	INSERT INTO raw_source_heads(tenant_id,device_id,provider,configured_root_id,source_key,source_key_sha256,generation)
	SELECT current_setting('agentsview.tenant_id'),'device-a','codex','root-a','round2-source-'||g::text,source_hash,0 FROM chosen`, targetHash)
	require.NoError(t, err)
	_, err = scenario.runtime.runtime.ExecContext(t.Context(), `WITH candidates AS (
		 SELECT g,repeat(md5('round2-source-'||g::text),2) source_hash FROM generate_series(1,10000) g
	), chosen AS (
		 SELECT g,source_hash,row_number() OVER (ORDER BY source_hash) n FROM candidates WHERE source_hash<$1 ORDER BY source_hash LIMIT 129
	)
	INSERT INTO raw_manifests(tenant_id,manifest_id,device_id,provider,configured_root_id,source_key,source_key_sha256,capture_id,parent_receipt,receipt,generation,kind,captured_at,accepted_at,canonical_json)
	SELECT current_setting('agentsview.tenant_id'),repeat(md5('round2-manifest-'||g::text),2),'device-a','codex','root-a','round2-source-'||g::text,source_hash,
	 'round2-capture-'||g::text,'',repeat(md5('round2-receipt-'||g::text),2),1,'snapshot',clock_timestamp(),clock_timestamp(),convert_to('{}','UTF8') FROM chosen`, targetHash)
	require.NoError(t, err)
	_, err = scenario.runtime.runtime.ExecContext(t.Context(), `UPDATE raw_source_heads h SET manifest_id=m.manifest_id,receipt=m.receipt,generation=m.generation FROM raw_manifests m
		WHERE h.source_key_sha256=m.source_key_sha256 AND m.capture_id LIKE 'round2-capture-%'`)
	require.NoError(t, err)
	var fillers int
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_source_heads WHERE source_key LIKE 'round2-source-%'`).Scan(&fillers))
	require.Equal(t, 129, fillers)

	probeDDL := fmt.Sprintf(`CREATE SEQUENCE %s.parity_source_lookup_probe;
		CREATE FUNCTION %s.parity_source_lookup_probe(text) RETURNS boolean LANGUAGE plpgsql VOLATILE SECURITY DEFINER AS $probe$
		BEGIN PERFORM nextval('%s.parity_source_lookup_probe'::regclass); RETURN true; END $probe$;
		CREATE POLICY parity_source_lookup_probe ON %s.raw_source_heads AS RESTRICTIVE FOR SELECT TO PUBLIC USING (%s.parity_source_lookup_probe(source_key_sha256));`,
		scenario.runtime.schema, scenario.runtime.schema, scenario.runtime.schema, scenario.runtime.schema, scenario.runtime.schema)
	_, err = scenario.runtime.admin.ExecContext(t.Context(), probeDDL)
	require.NoError(t, err)
	_, err = scenario.runtime.admin.ExecContext(t.Context(), fmt.Sprintf(`SELECT setval('%s.parity_source_lookup_probe'::regclass,1,false)`, scenario.runtime.schema))
	require.NoError(t, err)
	require.NoError(t, scenario.store.RecordParitySource(t.Context(), scenario.lease, sources[0], rawderive.ParitySourceResult{
		Complete: true,
		Members:  []rawderive.ParityMemberResult{{Key: key, Fingerprint: &fingerprint}},
	}))
	var observed int
	require.NoError(t, scenario.runtime.admin.QueryRowContext(t.Context(), fmt.Sprintf(`SELECT last_value FROM %s.parity_source_lookup_probe`, scenario.runtime.schema)).Scan(&observed))
	assert.Less(t, observed, 20, "exact source processing must not evaluate preceding cohort heads")
}

func TestParityEvidenceWriteFenceBlocksIndependentLeaseReplacement(t *testing.T) {
	f, store, lease, _ := boundParityFixture(t)
	tx, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, store.checkParityLeaseTx(t.Context(), tx, lease, false))
	done := make(chan error, 1)
	go func() {
		_, updateErr := f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET lease_token='00000000-0000-4000-8000-000000000099' WHERE run_id=$1`, lease.RunID)
		done <- updateErr
	}()
	select {
	case updateErr := <-done:
		require.NoError(t, updateErr)
		t.Fatal("lease replacement crossed an evidence write fence")
	case <-time.After(150 * time.Millisecond):
	}
	require.NoError(t, tx.Rollback())
	require.NoError(t, <-done)
}

func TestParityEvidenceReducesEveryVerdictThroughRecord(t *testing.T) {
	tests := []struct {
		name     string
		runID    string
		blocker  rawderive.ParityVerdict
		code     string
		mutate   bool
		expected rawderive.ParityVerdict
	}{
		{name: "matched", runID: "00000000-0000-4000-8000-000000000090", expected: rawderive.ParityMatched},
		{name: "mismatched", runID: "00000000-0000-4000-8000-000000000091", expected: rawderive.ParityMismatched},
		{name: "missing", runID: "00000000-0000-4000-8000-000000000092", expected: rawderive.ParityMissing},
		{name: "ambiguous", runID: "00000000-0000-4000-8000-000000000093", blocker: rawderive.ParityAmbiguous, expected: rawderive.ParityAmbiguous},
		{name: "legacy only", runID: "00000000-0000-4000-8000-000000000094", blocker: rawderive.ParityLegacyOnly, expected: rawderive.ParityLegacyOnly},
		{name: "provider exclusion proof unavailable", runID: "00000000-0000-4000-8000-000000000095", blocker: rawderive.ParityPartial, code: "exclusion_provenance_unavailable", expected: rawderive.ParityPartial},
		{name: "stale", runID: "00000000-0000-4000-8000-000000000096", mutate: true, expected: rawderive.ParityStale},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scenario := newParityCaptureScenario(t, test.runID)
			require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
			sources, err := scenario.store.NextParitySources(t.Context(), scenario.lease, "", 4)
			require.NoError(t, err)
			require.Len(t, sources, 1)
			require.NoError(t, scenario.store.ReserveParitySource(t.Context(), scenario.lease, sources[0]))
			var keyBytes, fingerprintBytes []byte
			require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT member_key,baseline_fingerprint FROM migration_parity_members WHERE run_id=$1 AND source_id=$2`, scenario.lease.RunID, sources[0].ID).Scan(&keyBytes, &fingerprintBytes))
			key, err := decodeParityMemberKey(keyBytes)
			require.NoError(t, err)
			fingerprint, err := decodeParityFingerprint(fingerprintBytes)
			require.NoError(t, err)
			result := rawderive.ParitySourceResult{Complete: true, Blocker: test.blocker, Code: test.code}
			switch test.expected {
			case rawderive.ParityMatched:
				result.Members = []rawderive.ParityMemberResult{{Key: key, Fingerprint: &fingerprint}}
			case rawderive.ParityMismatched:
				fingerprint.Semantic[0] ^= 0xff
				result.Members = []rawderive.ParityMemberResult{{Key: key, Fingerprint: &fingerprint}}
			case rawderive.ParityMissing:
				// A complete result without the required baseline key proves missing.
			default:
				result.Members = []rawderive.ParityMemberResult{{Key: key, Fingerprint: &fingerprint}}
			}
			if test.mutate {
				_, err = scenario.runtime.runtime.ExecContext(t.Context(), `UPDATE raw_source_projections SET diagnostics=diagnostics||' drift' WHERE source_id=$1`, sources[0].ID)
				require.NoError(t, err)
			}
			recordErr := scenario.store.RecordParitySource(t.Context(), scenario.lease, sources[0], result)
			if test.mutate {
				require.ErrorContains(t, recordErr, "dependency_changed")
			} else {
				require.NoError(t, recordErr)
			}
			var verdict, code string
			require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT verdict,code FROM migration_parity_sources WHERE run_id=$1 AND source_id=$2`, scenario.lease.RunID, sources[0].ID).Scan(&verdict, &code))
			assert.Equal(t, string(test.expected), verdict)
			if test.code != "" {
				assert.Equal(t, test.code, code)
			}
		})
	}
}

func TestParityEvidenceReportsEveryExclusiveVerdictAndSourcePrecedence(t *testing.T) {
	f, store, lease, _ := boundParityFixture(t)
	verdicts := []rawderive.ParityVerdict{
		rawderive.ParityMatched, rawderive.ParityMismatched, rawderive.ParityAmbiguous,
		rawderive.ParityLegacyOnly, rawderive.ParityMissing, rawderive.ParityPartial, rawderive.ParityStale,
	}
	for index, verdict := range verdicts {
		sourceID := fmt.Sprintf("source-%d", index)
		_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_sources(run_id,init_epoch,source_id,source_kind,device_id,provider,root_id,head_manifest,head_generation,head_receipt,dependency_digest,verdict,evidence_generation,candidate_complete)
			VALUES($1,$2,$3,'raw','device-a','claude','root-a','manifest',1,'receipt',$4,$5,1,TRUE)`, lease.RunID, lease.InitEpoch, sourceID, make([]byte, 32), verdict)
		require.NoError(t, err)
		key := rawderive.ParityMemberKey{SourceID: sourceID, LogicalKey: "member", Kind: "session"}
		encoded, digest := encodeParityMemberKey(key)
		_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_members(run_id,init_epoch,source_id,member_key,member_digest,kind,mapping_state,verdict)
			VALUES($1,$2,$3,$4,$5,'session','exact',$6)`, lease.RunID, lease.InitEpoch, sourceID, encoded, digest[:], verdict)
		require.NoError(t, err)
	}
	_, err := f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET baseline_sealed=TRUE,state='running',inventory_digest=$2,baseline_digest=$2 WHERE run_id=$1`, lease.RunID, make([]byte, 32))
	require.NoError(t, err)
	report, err := store.ReadParityReport(t.Context(), lease.RunID)
	require.NoError(t, err)
	for _, counts := range []rawderive.ParityCounts{report.Members, report.Sources} {
		assert.EqualValues(t, 1, counts.Matched)
		assert.EqualValues(t, 1, counts.Mismatched)
		assert.EqualValues(t, 1, counts.Ambiguous)
		assert.EqualValues(t, 1, counts.LegacyOnly)
		assert.EqualValues(t, 1, counts.Missing)
		assert.EqualValues(t, 1, counts.PartialUnsupported)
		assert.EqualValues(t, 1, counts.Stale)
	}
	assert.False(t, report.Passing)
	assert.Equal(t, rawderive.ParityStale, strongerParityVerdict(rawderive.ParityPartial, rawderive.ParityStale))
}

func decodeParityMemberKey(encoded []byte) (rawderive.ParityMemberKey, error) {
	const prefix = "agentsview-parity-member-key-v1\x00"
	if len(encoded) < len(prefix) || string(encoded[:len(prefix)]) != prefix {
		return rawderive.ParityMemberKey{}, fmt.Errorf("invalid parity member key")
	}
	reader := bytes.NewReader(encoded[len(prefix):])
	values := make([]string, 3)
	for i := range values {
		var err error
		values[i], err = readParityOpaqueString(reader)
		if err != nil {
			return rawderive.ParityMemberKey{}, fmt.Errorf("invalid parity member key")
		}
	}
	if reader.Len() != 0 {
		return rawderive.ParityMemberKey{}, fmt.Errorf("invalid parity member key")
	}
	return rawderive.ParityMemberKey{SourceID: values[0], LogicalKey: values[1], Kind: values[2]}, nil
}
