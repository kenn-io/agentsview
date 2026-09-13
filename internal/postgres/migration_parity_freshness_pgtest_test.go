//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
)

func parityOutcomeWithResult(content, result string) rawderive.ParsedManifest {
	parsed := projectionOutcome(content)
	call := &parsed.Outcome.Results[0].Result.Messages[1].ToolCalls[0]
	call.ResultEvents = []parser.ParsedToolResultEvent{{ToolUseID: "call-1", Source: "tool_result", Status: "completed", Content: result}}
	parsed.Outcome.Results[0].Result.Messages[1].HasToolUse = true
	return parsed
}

func markParityScenarioMatched(t *testing.T, scenario parityCaptureScenario) {
	t.Helper()
	_, err := scenario.runtime.runtime.ExecContext(t.Context(), `UPDATE migration_parity_members SET candidate_fingerprint=baseline_fingerprint,verdict='matched' WHERE run_id=$1`, scenario.lease.RunID)
	require.NoError(t, err)
	_, err = scenario.runtime.runtime.ExecContext(t.Context(), `UPDATE migration_parity_sources SET verdict='matched',candidate_complete=TRUE,evidence_generation=1 WHERE run_id=$1`, scenario.lease.RunID)
	require.NoError(t, err)
}

func TestParityBindingFailureAfterValidationCannotPublishPassingReport(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000132")
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	markParityScenarioMatched(t, scenario)
	require.NoError(t, scenario.store.ValidateParityEvidence(t.Context(), scenario.lease, scenario.reader, scenario.binding, 4))
	var validation string
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT validation_state FROM migration_parity_runs WHERE run_id=$1`, scenario.lease.RunID).Scan(&validation))
	assert.Equal(t, "checked", validation)
	require.NoError(t, scenario.store.FinishParityRequest(t.Context(), scenario.lease, "binding_conflict"))
	report, err := scenario.store.ReadParityRequestReport(t.Context(), scenario.lease.RunID, scenario.lease.RequestGeneration)
	require.NoError(t, err)
	assert.Equal(t, "binding_conflict", report.Code)
	assert.Equal(t, "incomplete", report.Freshness)
	assert.False(t, report.Passing)
}

func TestParityFreshnessRestartsFullValidationAndIgnoresCorpusCounter(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000061")
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	_, err := scenario.runtime.runtime.ExecContext(t.Context(), `UPDATE migration_parity_members SET candidate_fingerprint=baseline_fingerprint,verdict='matched' WHERE run_id=$1`, scenario.lease.RunID)
	require.NoError(t, err)
	_, err = scenario.runtime.runtime.ExecContext(t.Context(), `UPDATE migration_parity_sources SET verdict='matched',candidate_complete=TRUE,evidence_generation=1 WHERE run_id=$1`, scenario.lease.RunID)
	require.NoError(t, err)
	_, err = scenario.baseline.runtime.ExecContext(t.Context(), `UPDATE raw_corpus_state SET corpus_revision=corpus_revision+1 WHERE singleton=1`)
	require.NoError(t, err)

	interrupted := errors.New("synthetic validation interruption")
	scenario.store.parityEvidencePageHook = func(_ context.Context, phase string) error {
		if phase == "validation_member" {
			return interrupted
		}
		return nil
	}
	require.ErrorIs(t, scenario.store.ValidateParityEvidence(t.Context(), scenario.lease, scenario.reader, scenario.binding, 1), interrupted)
	var firstEpoch int64
	var state string
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT validation_epoch,validation_state FROM migration_parity_runs WHERE run_id=$1`, scenario.lease.RunID).Scan(&firstEpoch, &state))
	assert.Equal(t, "validating", state)

	scenario.store.parityEvidencePageHook = nil
	require.NoError(t, scenario.store.ValidateParityEvidence(t.Context(), scenario.lease, scenario.reader, scenario.binding, 1))
	var secondEpoch int64
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT validation_epoch,validation_state FROM migration_parity_runs WHERE run_id=$1`, scenario.lease.RunID).Scan(&secondEpoch, &state))
	assert.Equal(t, firstEpoch+1, secondEpoch)
	assert.Equal(t, "checked", state)
	report, err := scenario.store.ReadParityReport(t.Context(), scenario.lease.RunID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, report.Sources.Matched)
	assert.EqualValues(t, 1, report.Members.Matched)
}

func TestParityFreshnessRestartRemovesAbandonedSourceDelta(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000129")
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	markParityScenarioMatched(t, scenario)
	added, _ := scenario.runtime.acceptScoped(t, "device-a", "runtime-validation-delta", "", parser.AgentCodex, "root-a", "runtime-validation-delta")
	interrupted := errors.New("interrupt after source delta staging")
	scenario.store.parityEvidencePageHook = func(ctx context.Context, phase string) error {
		if phase != "validation_source" {
			return nil
		}
		var deltas int
		if err := scenario.runtime.runtime.QueryRowContext(ctx, `SELECT count(*) FROM migration_parity_sources WHERE run_id=$1 AND inventory_kind='validation_delta'`, scenario.lease.RunID).Scan(&deltas); err != nil {
			return err
		}
		if deltas > 0 {
			return interrupted
		}
		return nil
	}
	require.ErrorIs(t, scenario.store.ValidateParityEvidence(t.Context(), scenario.lease, scenario.reader, scenario.binding, 1), interrupted)
	var staged int
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_sources WHERE run_id=$1 AND inventory_kind='validation_delta'`, scenario.lease.RunID).Scan(&staged))
	assert.Equal(t, 1, staged)
	_, err := scenario.runtime.runtime.ExecContext(t.Context(), `UPDATE raw_source_heads SET manifest_id=NULL,receipt=NULL,generation=0 WHERE source_key=$1`, added.Manifest.SourceKey)
	require.NoError(t, err)
	scenario.store.parityEvidencePageHook = nil
	require.NoError(t, scenario.store.ValidateParityEvidence(t.Context(), scenario.lease, scenario.reader, scenario.binding, 1))
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_sources WHERE run_id=$1 AND inventory_kind='validation_delta'`, scenario.lease.RunID).Scan(&staged))
	assert.Zero(t, staged)
	report, err := scenario.store.ReadParityReport(t.Context(), scenario.lease.RunID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, report.Sources.Matched)
	assert.EqualValues(t, 1, report.Members.Matched)
}

func TestParityFreshnessStalesOnlyTheChangedSource(t *testing.T) {
	runtime := newProjectionFixture(t)
	baseline := newProjectionFixture(t)
	var runtimeSources, baselineSources []string
	for i, sourceKey := range []string{"source-a", "source-b"} {
		capture := "capture-" + sourceKey
		runtimeManifest, _ := runtime.acceptScoped(t, "device-a", capture, "", parser.AgentCodex, "root-a", sourceKey)
		baselineManifest, _ := baseline.acceptScoped(t, "device-a", capture, "", parser.AgentCodex, "root-a", sourceKey)
		require.NoError(t, runtime.sink.Project(t.Context(), runtime.lease(t, runtimeManifest), runtimeManifest, parityOutcomeWithResult("runtime-"+sourceKey, "result")))
		require.NoError(t, baseline.sink.Project(t.Context(), baseline.lease(t, baselineManifest), baselineManifest, parityOutcomeWithResult("baseline-"+sourceKey, "result")))
		runtimeSources = append(runtimeSources, rawSourceID(runtimeManifest))
		baselineSources = append(baselineSources, rawSourceID(baselineManifest))
		assert.Equal(t, runtimeSources[i], baselineSources[i])
	}

	store, err := NewMigrationParityStore(t.Context(), runtime.runtime, MigrationParityOptions{Schema: runtime.schema, Tenant: runtime.tenant})
	require.NoError(t, err)
	request := rawderive.ParityRequest{
		RunID: "00000000-0000-4000-8000-000000000041", RuntimeID: "00000000-0000-4000-8000-000000000042", BaselineProfile: "before",
		Cohort: rawderive.ParityCohort{DeviceID: "device-a", Provider: parser.AgentCodex, RootID: "root-a"},
	}
	_, err = store.CreateOrResumeParity(t.Context(), request, 2)
	require.NoError(t, err)
	lease := claimedParityLease(t, runtime.hostedFixture, request, time.Hour)
	reader := newParityReadRole(t, baseline.hostedFixture)
	var baselineID string
	require.NoError(t, reader.QueryRowContext(t.Context(), `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&baselineID))
	binding := parityPhysicalBinding(runtime.tenant)
	binding.Request, binding.BaselineID = request, baselineID
	require.NoError(t, baseline.runtime.QueryRowContext(t.Context(), `SELECT data_version,quality_signal_version,secrets_rules_version FROM sessions LIMIT 1`).Scan(
		&binding.Versions.Data, &binding.Versions.Quality, &binding.Versions.SecretRules,
	))
	lease, err = store.BindParity(t.Context(), lease, binding)
	require.NoError(t, err)
	require.NoError(t, store.CaptureParityBaseline(t.Context(), lease, reader, binding))
	var untouchedPhysical string
	var untouchedKeyBytes, untouchedStoredPhysical []byte
	require.NoError(t, runtime.runtime.QueryRowContext(t.Context(), `SELECT baseline_ref,member_key,physical_fingerprint FROM migration_parity_members WHERE run_id=$1 AND source_id=$2`, request.RunID, baselineSources[1]).Scan(&untouchedPhysical, &untouchedKeyBytes, &untouchedStoredPhysical))
	untouchedKey, err := decodeParityMemberKey(untouchedKeyBytes)
	require.NoError(t, err)
	untouchedTx, err := reader.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	untouchedRead, err := readParityPhysicalMember(t.Context(), untouchedTx, binding, untouchedPhysical, untouchedKey)
	require.NoError(t, err)
	require.NoError(t, untouchedTx.Commit())
	assert.Equal(t, untouchedStoredPhysical, untouchedRead.Physical[:], "unchanged physical evidence is deterministic across snapshots")
	pageTx, err := reader.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	untouchedPage, _, err := readParityRawMemberPage(t.Context(), pageTx, binding, baselineSources[1], "", 1)
	require.NoError(t, err)
	require.NoError(t, pageTx.Commit())
	require.Len(t, untouchedPage, 1)
	var storedDependency []byte
	require.NoError(t, runtime.runtime.QueryRowContext(t.Context(), `SELECT expected FROM migration_parity_dependencies WHERE run_id=$1 AND source_id=$2 AND kind='member'`, request.RunID, baselineSources[1]).Scan(&storedDependency))
	computedDependency := parityCapturedMemberDependency(untouchedPage[0])
	assert.Equal(t, storedDependency, computedDependency[:], "member dependency is stable across census pages")
	_, err = runtime.runtime.ExecContext(t.Context(), `UPDATE migration_parity_members SET candidate_fingerprint=baseline_fingerprint,verdict='matched' WHERE run_id=$1`, request.RunID)
	require.NoError(t, err)
	_, err = runtime.runtime.ExecContext(t.Context(), `UPDATE migration_parity_sources SET verdict='matched',candidate_complete=TRUE,evidence_generation=1 WHERE run_id=$1`, request.RunID)
	require.NoError(t, err)

	var changedPhysical string
	require.NoError(t, baseline.runtime.QueryRowContext(t.Context(), `SELECT ss.physical_session_id FROM session_sources ss WHERE ss.source_id=$1`, baselineSources[0]).Scan(&changedPhysical))
	_, err = baseline.runtime.ExecContext(t.Context(), `UPDATE tool_result_events SET content='changed',content_length=7 WHERE session_id=$1`, changedPhysical)
	require.NoError(t, err)
	require.NoError(t, store.ValidateParityEvidence(t.Context(), lease, reader, binding, 1))
	report, err := store.ReadParityReport(t.Context(), lease.RunID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, report.Sources.Stale)
	assert.EqualValues(t, 1, report.Sources.Matched)
	assert.EqualValues(t, 1, report.Members.Stale)
	assert.EqualValues(t, 1, report.Members.Matched)
	assert.False(t, report.Passing)
	assert.NotNil(t, report.BaselineObservedAt)
	assert.NotNil(t, report.RuntimeObservedAt)
}

func TestParityFreshnessMergesCandidateOnlyMemberObservation(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000084")
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	sources, err := scenario.store.NextParitySources(t.Context(), scenario.lease, "", 4)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.NoError(t, scenario.store.ReserveParitySource(t.Context(), scenario.lease, sources[0]))
	var baselineKeyBytes, baselineFingerprintBytes []byte
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT member_key,baseline_fingerprint FROM migration_parity_members WHERE run_id=$1 AND source_id=$2`, scenario.lease.RunID, sources[0].ID).Scan(&baselineKeyBytes, &baselineFingerprintBytes))
	baselineKey, err := decodeParityMemberKey(baselineKeyBytes)
	require.NoError(t, err)
	baselineFingerprint, err := decodeParityFingerprint(baselineFingerprintBytes)
	require.NoError(t, err)
	candidateKey := rawderive.ParityMemberKey{SourceID: sources[0].ID, LogicalKey: "new-member", Kind: "session"}
	candidateFingerprint := rawderive.ParityFingerprint{Semantic: rawderive.ParityDigest{9}}
	require.NoError(t, scenario.store.RecordParitySource(t.Context(), scenario.lease, sources[0], rawderive.ParitySourceResult{Complete: true, Members: []rawderive.ParityMemberResult{
		{Key: baselineKey, Fingerprint: &baselineFingerprint},
		{Key: candidateKey, Fingerprint: &candidateFingerprint},
	}}))

	epoch, err := scenario.store.beginParityValidation(t.Context(), scenario.lease)
	require.NoError(t, err)
	physical := rawderive.ParityDigest{7}
	require.NoError(t, scenario.store.stageParityMemberObservations(t.Context(), scenario.lease, epoch, sources[0].ID, []parityCapturedMember{{
		key: candidateKey, physicalID: "new-physical", mappingState: "exact", physical: &physical,
	}}))
	require.NoError(t, scenario.store.finishParityValidation(t.Context(), scenario.lease, epoch, rawderive.ParityDigest{8}, time.Now(), time.Now()))
	var count int
	var verdict string
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*),min(verdict) FROM migration_parity_members WHERE run_id=$1 AND source_id=$2 AND member_digest=$3`, scenario.lease.RunID, sources[0].ID, parityTestSHA(mustEncodeParityMemberKey(t, candidateKey))).Scan(&count, &verdict))
	assert.Equal(t, 1, count)
	assert.Equal(t, string(rawderive.ParityStale), verdict)
}

func TestParityFreshnessDetectsHeadRelationshipAliasExclusionRemovalAndReinsert(t *testing.T) {
	tests := []struct {
		name   string
		runID  string
		mutate func(*testing.T, parityCaptureScenario)
	}{
		{
			name:  "runtime head",
			runID: "00000000-0000-4000-8000-000000000101",
			mutate: func(t *testing.T, scenario parityCaptureScenario) {
				var receipt string
				require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT receipt FROM raw_source_heads WHERE source_key='source-a'`).Scan(&receipt))
				scenario.runtime.acceptScoped(t, "device-a", "next-head", receipt, parser.AgentCodex, "root-a", "source-a")
			},
		},
		{
			name:  "relationship",
			runID: "00000000-0000-4000-8000-000000000102",
			mutate: func(t *testing.T, scenario parityCaptureScenario) {
				_, err := scenario.baseline.runtime.ExecContext(t.Context(), `INSERT INTO raw_session_links(branch_id,kind,ordinal,call_index,event_index,target_alias)
					SELECT branch_id,'parent',-1,-1,-1,'new-parent' FROM raw_session_branches WHERE source_id=$1`, scenario.sourceID)
				require.NoError(t, err)
			},
		},
		{
			name:  "public alias",
			runID: "00000000-0000-4000-8000-000000000103",
			mutate: func(t *testing.T, scenario parityCaptureScenario) {
				_, err := scenario.baseline.runtime.ExecContext(t.Context(), `INSERT INTO raw_session_public_aliases(alias_id,group_id) SELECT 'new-alias',group_id FROM raw_session_branches WHERE source_id=$1`, scenario.sourceID)
				require.NoError(t, err)
			},
		},
		{
			name:  "curation exclusion",
			runID: "00000000-0000-4000-8000-000000000104",
			mutate: func(t *testing.T, scenario parityCaptureScenario) {
				_, err := scenario.baseline.runtime.ExecContext(t.Context(), `INSERT INTO raw_curation(group_id,branch_id,field,value) SELECT group_id,branch_id,'excluded','true' FROM raw_session_branches WHERE source_id=$1`, scenario.sourceID)
				require.NoError(t, err)
			},
		},
		{
			name:  "removed member",
			runID: "00000000-0000-4000-8000-000000000105",
			mutate: func(t *testing.T, scenario parityCaptureScenario) {
				_, err := scenario.baseline.runtime.ExecContext(t.Context(), `UPDATE raw_session_branches SET active=FALSE WHERE source_id=$1`, scenario.sourceID)
				require.NoError(t, err)
			},
		},
		{
			name:  "deleted and reinserted proof with changed identity",
			runID: "00000000-0000-4000-8000-000000000106",
			mutate: func(t *testing.T, scenario parityCaptureScenario) {
				_, err := scenario.baseline.runtime.ExecContext(t.Context(), `DELETE FROM session_sources WHERE source_id=$1`, scenario.sourceID)
				require.NoError(t, err)
				_, err = scenario.baseline.runtime.ExecContext(t.Context(), `INSERT INTO session_sources(branch_id,group_id,source_id,session_id,physical_session_id,manifest_id,content_revision,processing_version,projection_generation)
					SELECT branch_id,group_id,source_id,session_id,NULL,manifest_id,content_revision,processing_version,projection_generation FROM raw_session_branches WHERE source_id=$1`, scenario.sourceID)
				require.NoError(t, err)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scenario := newParityCaptureScenario(t, test.runID)
			require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
			markParityScenarioMatched(t, scenario)
			test.mutate(t, scenario)
			require.NoError(t, scenario.store.ValidateParityEvidence(t.Context(), scenario.lease, scenario.reader, scenario.binding, 1))
			report, err := scenario.store.ReadParityReport(t.Context(), scenario.lease.RunID)
			require.NoError(t, err)
			assert.EqualValues(t, 1, report.Sources.Stale)
			assert.False(t, report.Passing)
		})
	}
}

func TestParityFreshnessFindsNewBaselineOnlySource(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000107")
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	markParityScenarioMatched(t, scenario)
	added, _ := scenario.baseline.acceptScoped(t, "device-a", "baseline-added", "", parser.AgentCodex, "root-a", "baseline-added")
	addedOutcome := projectionOutcome("added")
	addedOutcome.Outcome.Results[0].Result.Session.ID = "codex:baseline-added"
	addedOutcome.Outcome.Results[0].Result.Session.SourceSessionID = "baseline-added"
	require.NoError(t, scenario.baseline.sink.Project(t.Context(), scenario.baseline.lease(t, added), added, addedOutcome))
	require.NoError(t, scenario.store.ValidateParityEvidence(t.Context(), scenario.lease, scenario.reader, scenario.binding, 1))
	report, err := scenario.store.ReadParityReport(t.Context(), scenario.lease.RunID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, report.Sources.Matched)
	assert.EqualValues(t, 1, report.Sources.Stale)
	var staged int
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_sources WHERE run_id=$1 AND inventory_kind='validation_delta'`, scenario.lease.RunID).Scan(&staged))
	assert.Equal(t, 1, staged)
}

func TestParityFreshnessFindsMemberInsertedBeforePreviousCursor(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000108")
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	markParityScenarioMatched(t, scenario)
	var receipt, oldGroup string
	require.NoError(t, scenario.baseline.runtime.QueryRowContext(t.Context(), `SELECT h.receipt,b.group_id FROM raw_source_heads h JOIN raw_source_projections p USING(device_id,provider,configured_root_id,source_key_sha256) JOIN raw_session_branches b ON b.source_id=p.source_id WHERE p.source_id=$1`, scenario.sourceID).Scan(&receipt, &oldGroup))
	next, _ := scenario.baseline.acceptScoped(t, "device-a", "baseline-next", receipt, parser.AgentCodex, "root-a", "source-a")
	logical := ""
	for i := 0; i < 10000; i++ {
		candidate := fmt.Sprintf("inserted-%d", i)
		group, _ := rawderive.GroupID(next, db.Session{ID: "codex:" + candidate, Agent: string(parser.AgentCodex), SourceSessionID: candidate})
		if group < oldGroup {
			logical = candidate
			break
		}
	}
	require.NotEmpty(t, logical)
	nextOutcome := parityOutcomeWithResult("baseline", "old")
	added := projectionOutcome("added").Outcome.Results[0]
	added.Result.Session.ID = "codex:" + logical
	added.Result.Session.SourceSessionID = logical
	nextOutcome.Outcome.Results = append(nextOutcome.Outcome.Results, added)
	require.NoError(t, scenario.baseline.sink.Project(t.Context(), scenario.baseline.lease(t, next), next, nextOutcome))
	require.NoError(t, scenario.store.ValidateParityEvidence(t.Context(), scenario.lease, scenario.reader, scenario.binding, 1))
	var deltas int
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_members WHERE run_id=$1 AND inventory_kind='validation_delta'`, scenario.lease.RunID).Scan(&deltas))
	assert.Equal(t, 1, deltas, "a full census discovers a key inserted before the prior page cursor")
}

func TestParityFreshnessFindsMemberRemovedBeforePageCursor(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000118")
	var receipt string
	require.NoError(t, scenario.baseline.runtime.QueryRowContext(t.Context(), `SELECT receipt FROM raw_source_heads WHERE source_key='source-a'`).Scan(&receipt))
	next, _ := scenario.baseline.acceptScoped(t, "device-a", "baseline-two-members", receipt, parser.AgentCodex, "root-a", "source-a")
	outcome := parityOutcomeWithResult("baseline", "old")
	second := projectionOutcome("second").Outcome.Results[0]
	second.Result.Session.ID = "codex:second"
	second.Result.Session.SourceSessionID = "second"
	outcome.Outcome.Results = append(outcome.Outcome.Results, second)
	require.NoError(t, scenario.baseline.sink.Project(t.Context(), scenario.baseline.lease(t, next), next, outcome))
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	markParityScenarioMatched(t, scenario)
	var removedGroup, removedLogical string
	require.NoError(t, scenario.baseline.runtime.QueryRowContext(t.Context(), `SELECT b.group_id,g.logical_key FROM raw_session_branches b JOIN raw_session_groups g USING(group_id) WHERE b.source_id=$1 AND b.active ORDER BY b.group_id LIMIT 1`, scenario.sourceID).Scan(&removedGroup, &removedLogical))
	_, err := scenario.baseline.runtime.ExecContext(t.Context(), `UPDATE raw_session_branches SET active=FALSE WHERE source_id=$1 AND group_id=$2`, scenario.sourceID, removedGroup)
	require.NoError(t, err)
	require.NoError(t, scenario.store.ValidateParityEvidence(t.Context(), scenario.lease, scenario.reader, scenario.binding, 1))
	report, err := scenario.store.ReadParityReport(t.Context(), scenario.lease.RunID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, report.Members.Stale, int64(1))
	assert.EqualValues(t, 1, report.Sources.Stale)
	removedKey, removedDigest := encodeParityMemberKey(rawderive.ParityMemberKey{SourceID: scenario.sourceID, LogicalKey: removedLogical, Kind: "session"})
	var removedVerdict string
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT verdict FROM migration_parity_members WHERE run_id=$1 AND source_id=$2 AND member_digest=$3 AND member_key=$4`, scenario.lease.RunID, scenario.sourceID, removedDigest[:], removedKey).Scan(&removedVerdict))
	assert.Equal(t, string(rawderive.ParityStale), removedVerdict)
}

func TestParityFreshnessEmptyCohortNeverPasses(t *testing.T) {
	runtime := newHostedFixture(t, "tenant-a")
	baseline := newHostedFixture(t, "tenant-a")
	store, err := NewMigrationParityStore(t.Context(), runtime.runtime, MigrationParityOptions{Schema: runtime.schema, Tenant: runtime.tenant})
	require.NoError(t, err)
	request := rawderive.ParityRequest{RunID: "00000000-0000-4000-8000-000000000109", RuntimeID: "00000000-0000-4000-8000-000000000110", BaselineProfile: "before", Cohort: rawderive.ParityCohort{DeviceID: "device-a", Provider: parser.AgentClaude, RootID: "root-a"}}
	_, err = store.CreateOrResumeParity(t.Context(), request, 4)
	require.NoError(t, err)
	lease := claimedParityLease(t, runtime, request, time.Hour)
	reader := newParityReadRole(t, baseline)
	var baselineID string
	require.NoError(t, reader.QueryRowContext(t.Context(), `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&baselineID))
	binding := parityPhysicalBinding(runtime.tenant)
	binding.Request, binding.BaselineID = request, baselineID
	lease, err = store.BindParity(t.Context(), lease, binding)
	require.NoError(t, err)
	require.NoError(t, store.CaptureParityBaseline(t.Context(), lease, reader, binding))
	require.NoError(t, store.ValidateParityEvidence(t.Context(), lease, reader, binding, 4))
	report, err := store.ReadParityReport(t.Context(), request.RunID)
	require.NoError(t, err)
	assert.True(t, report.BaselineSealed)
	assert.Equal(t, "checked", report.Freshness)
	assert.Zero(t, report.Sources)
	assert.Zero(t, report.Members)
	assert.False(t, report.Complete)
	assert.False(t, report.Passing)
}

func mustEncodeParityMemberKey(t *testing.T, key rawderive.ParityMemberKey) []byte {
	t.Helper()
	encoded, _ := encodeParityMemberKey(key)
	return encoded
}
