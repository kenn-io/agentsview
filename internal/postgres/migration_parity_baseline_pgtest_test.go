//go:build pgtest

package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	g_runtime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

type parityCaptureScenario struct {
	runtime  projectionFixture
	baseline projectionFixture
	store    *MigrationParityStore
	reader   *sql.DB
	lease    rawderive.ParityLease
	binding  rawderive.ParityBinding
	sourceID string
}

func newParityCaptureScenario(t *testing.T, runID string) parityCaptureScenario {
	t.Helper()
	runtime := newProjectionFixture(t)
	baseline := newProjectionFixture(t)
	runtimeManifest, _ := runtime.acceptScoped(t, "device-a", "capture-a", "", parser.AgentCodex, "root-a", "source-a")
	baselineManifest, _ := baseline.acceptScoped(t, "device-a", "capture-a", "", parser.AgentCodex, "root-a", "source-a")
	require.NoError(t, runtime.sink.Project(t.Context(), runtime.lease(t, runtimeManifest), runtimeManifest, parityOutcomeWithResult("runtime", "old")))
	require.NoError(t, baseline.sink.Project(t.Context(), baseline.lease(t, baselineManifest), baselineManifest, parityOutcomeWithResult("baseline", "old")))
	store, err := NewMigrationParityStore(t.Context(), runtime.runtime, MigrationParityOptions{Schema: runtime.schema, Tenant: runtime.tenant})
	require.NoError(t, err)
	request := rawderive.ParityRequest{RunID: runID, RuntimeID: "00000000-0000-4000-8000-000000000052", BaselineProfile: "before", Cohort: rawderive.ParityCohort{DeviceID: "device-a", Provider: parser.AgentCodex, RootID: "root-a"}}
	_, err = store.CreateOrResumeParity(t.Context(), request, 2)
	require.NoError(t, err)
	lease := claimedParityLease(t, runtime.hostedFixture, request, time.Hour)
	reader := newParityReadRole(t, baseline.hostedFixture)
	var baselineID string
	require.NoError(t, reader.QueryRowContext(t.Context(), `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&baselineID))
	binding := parityPhysicalBinding(runtime.tenant)
	binding.Request, binding.BaselineID = request, baselineID
	require.NoError(t, baseline.runtime.QueryRowContext(t.Context(), `SELECT data_version,quality_signal_version,secrets_rules_version FROM sessions LIMIT 1`).Scan(&binding.Versions.Data, &binding.Versions.Quality, &binding.Versions.SecretRules))
	lease, err = store.BindParity(t.Context(), lease, binding)
	require.NoError(t, err)
	return parityCaptureScenario{runtime: runtime, baseline: baseline, store: store, reader: reader, lease: lease, binding: binding, sourceID: rawSourceID(runtimeManifest)}
}

func TestParityBaselineSealsAcrossSeparatelyConfiguredSchemas(t *testing.T) {
	runtime := newProjectionFixture(t)
	baseline := newProjectionFixture(t)
	require.NotEqual(t, runtime.schema, baseline.schema)
	runtimeManifest, _ := runtime.acceptScoped(t, "device-a", "capture-a", "", parser.AgentCodex, "root-a", "source-a")
	baselineManifest, _ := baseline.acceptScoped(t, "device-a", "capture-a", "", parser.AgentCodex, "root-a", "source-a")
	require.Equal(t, runtimeManifest.ManifestID, baselineManifest.ManifestID)
	require.NoError(t, runtime.sink.Project(t.Context(), runtime.lease(t, runtimeManifest), runtimeManifest, projectionOutcome("runtime")))
	require.NoError(t, baseline.sink.Project(t.Context(), baseline.lease(t, baselineManifest), baselineManifest, projectionOutcome("baseline")))

	store, err := NewMigrationParityStore(t.Context(), runtime.runtime, MigrationParityOptions{Schema: runtime.schema, Tenant: runtime.tenant})
	require.NoError(t, err)
	request := rawderive.ParityRequest{
		RunID: "00000000-0000-4000-8000-000000000031", RuntimeID: "00000000-0000-4000-8000-000000000032", BaselineProfile: "before",
		Cohort: rawderive.ParityCohort{DeviceID: "device-a", Provider: parser.AgentCodex, RootID: "root-a"},
	}
	_, err = store.CreateOrResumeParity(t.Context(), request, 2)
	require.NoError(t, err)
	lease := claimedParityLease(t, runtime.hostedFixture, request, time.Hour)
	reader := newParityReadRole(t, baseline.hostedFixture)
	var baselineID string
	require.NoError(t, reader.QueryRowContext(t.Context(), `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&baselineID))
	binding := parityPhysicalBinding(runtime.tenant)
	binding.Request = request
	binding.BaselineID = baselineID
	require.NoError(t, baseline.runtime.QueryRowContext(t.Context(), `SELECT data_version,quality_signal_version,secrets_rules_version FROM sessions LIMIT 1`).Scan(
		&binding.Versions.Data, &binding.Versions.Quality, &binding.Versions.SecretRules,
	))
	lease, err = store.BindParity(t.Context(), lease, binding)
	require.NoError(t, err)

	require.NoError(t, store.CaptureParityBaseline(t.Context(), lease, reader, binding))
	report, err := store.ReadParityReport(t.Context(), request.RunID)
	require.NoError(t, err)
	assert.True(t, report.BaselineSealed)
	assert.NotNil(t, report.BaselineObservedAt)
	assert.NotNil(t, report.RuntimeObservedAt)
	var sourceCount, memberCount, historyCount int
	require.NoError(t, runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_sources WHERE run_id=$1`, request.RunID).Scan(&sourceCount))
	require.NoError(t, runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_members WHERE run_id=$1`, request.RunID).Scan(&memberCount))
	require.NoError(t, runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_dependencies WHERE run_id=$1 AND kind='manifest'`, request.RunID).Scan(&historyCount))
	assert.Equal(t, 1, sourceCount)
	assert.Equal(t, 1, memberCount)
	assert.Equal(t, 1, historyCount)
	sources, err := store.NextParitySources(t.Context(), lease, "", 2)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, rawSourceID(runtimeManifest), sources[0].ID)
	assert.Equal(t, runtime.tenant, sources[0].Identity.TenantID)
	history, err := store.NextParityHistory(t.Context(), lease, sources[0].ID, 0, 2)
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, runtimeManifest.ManifestID, history[0].ManifestID)
	assert.Empty(t, history[0].ParentReceipt)

	var physicalID string
	require.NoError(t, baseline.runtime.QueryRowContext(t.Context(), `SELECT physical_session_id FROM session_sources LIMIT 1`).Scan(&physicalID))
	for _, statement := range []string{
		`DELETE FROM tool_result_events WHERE session_id=$1`,
		`DELETE FROM tool_calls WHERE session_id=$1`,
		`DELETE FROM messages WHERE session_id=$1`,
		`DELETE FROM usage_events WHERE session_id=$1`,
		`DELETE FROM session_sources WHERE physical_session_id=$1`,
		`DELETE FROM sessions WHERE id=$1`,
	} {
		_, err = baseline.runtime.ExecContext(t.Context(), statement, physicalID)
		require.NoError(t, err)
	}
	require.NoError(t, runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_members WHERE run_id=$1`, request.RunID).Scan(&memberCount))
	assert.Equal(t, 1, memberCount, "sealed evidence has no corpus foreign key")
}

func TestParityBaselineCensusesRuntimeHeadsAndBaselineSourcesIndependently(t *testing.T) {
	runtime := newProjectionFixture(t)
	baseline := newProjectionFixture(t)
	runtimeShared, _ := runtime.acceptScoped(t, "device-a", "runtime-shared", "", parser.AgentCodex, "root-a", "shared")
	baselineShared, _ := baseline.acceptScoped(t, "device-a", "baseline-shared", "", parser.AgentCodex, "root-a", "shared")
	require.NoError(t, runtime.sink.Project(t.Context(), runtime.lease(t, runtimeShared), runtimeShared, projectionOutcome("runtime shared")))
	require.NoError(t, baseline.sink.Project(t.Context(), baseline.lease(t, baselineShared), baselineShared, projectionOutcome("baseline shared")))
	unprojected, _ := runtime.acceptScoped(t, "device-a", "runtime-unprojected", "", parser.AgentCodex, "root-a", "unprojected")
	baselineOnly, _ := baseline.acceptScoped(t, "device-a", "baseline-only", "", parser.AgentCodex, "root-a", "baseline-only")
	require.NoError(t, baseline.sink.Project(t.Context(), baseline.lease(t, baselineOnly), baselineOnly, projectionOutcome("baseline only")))

	store, err := NewMigrationParityStore(t.Context(), runtime.runtime, MigrationParityOptions{Schema: runtime.schema, Tenant: runtime.tenant})
	require.NoError(t, err)
	request := rawderive.ParityRequest{RunID: "00000000-0000-4000-8000-000000000071", RuntimeID: "00000000-0000-4000-8000-000000000072", BaselineProfile: "before", Cohort: rawderive.ParityCohort{DeviceID: "device-a", Provider: parser.AgentCodex, RootID: "root-a"}}
	_, err = store.CreateOrResumeParity(t.Context(), request, 8)
	require.NoError(t, err)
	lease := claimedParityLease(t, runtime.hostedFixture, request, time.Hour)
	reader := newParityReadRole(t, baseline.hostedFixture)
	var baselineID string
	require.NoError(t, reader.QueryRowContext(t.Context(), `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&baselineID))
	binding := parityPhysicalBinding(runtime.tenant)
	binding.Request, binding.BaselineID = request, baselineID
	require.NoError(t, baseline.runtime.QueryRowContext(t.Context(), `SELECT data_version,quality_signal_version,secrets_rules_version FROM sessions LIMIT 1`).Scan(&binding.Versions.Data, &binding.Versions.Quality, &binding.Versions.SecretRules))
	lease, err = store.BindParity(t.Context(), lease, binding)
	require.NoError(t, err)
	require.NoError(t, store.CaptureParityBaseline(t.Context(), lease, reader, binding))

	var sources, members int
	require.NoError(t, runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_sources WHERE run_id=$1`, request.RunID).Scan(&sources))
	require.NoError(t, runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_members WHERE run_id=$1`, request.RunID).Scan(&members))
	assert.Equal(t, 3, sources)
	assert.Equal(t, 2, members)
	var verdict, code string
	require.NoError(t, runtime.runtime.QueryRowContext(t.Context(), `SELECT verdict,code FROM migration_parity_sources WHERE run_id=$1 AND source_id=$2`, request.RunID, rawSourceID(unprojected)).Scan(&verdict, &code))
	assert.Equal(t, string(rawderive.ParityPartial), verdict)
	assert.Equal(t, "missing_object", code)
	require.NoError(t, runtime.runtime.QueryRowContext(t.Context(), `SELECT verdict,code FROM migration_parity_sources WHERE run_id=$1 AND source_id=$2`, request.RunID, rawSourceID(baselineOnly)).Scan(&verdict, &code))
	assert.Equal(t, string(rawderive.ParityMissing), verdict)
	assert.Equal(t, "missing_object", code)
}

func TestParityBaselinePreflightsOversizedUnprojectedManifestBeforeLoading(t *testing.T) {
	runtime := newProjectionFixture(t)
	baseline := newProjectionFixture(t)
	identity, err := rawsync.NewAuthIdentity(runtime.tenant, "device-a")
	require.NoError(t, err)
	source := rawsync.CanonicalManifest{Identity: identity, Manifest: rawsync.Manifest{Provider: parser.AgentCodex, ConfiguredRootID: "root-a", SourceKey: "oversized-source"}}
	sourceHash := fmt.Sprintf("%x", sha256.Sum256([]byte(source.Manifest.SourceKey)))
	manifestID := strings.Repeat("a", 64)
	receipt := strings.Repeat("b", 64)
	_, err = runtime.runtime.ExecContext(t.Context(), `INSERT INTO raw_devices(device_id,tenant_id,display_name,credential_sha256,created_at) VALUES('device-a',$1,'device-a',$2,clock_timestamp())`, runtime.tenant, make([]byte, 32))
	require.NoError(t, err)
	_, err = runtime.runtime.ExecContext(t.Context(), `INSERT INTO raw_source_heads(tenant_id,device_id,provider,configured_root_id,source_key,source_key_sha256,generation) VALUES($1,'device-a','codex','root-a','oversized-source',$2,0)`, runtime.tenant, sourceHash)
	require.NoError(t, err)
	_, err = runtime.runtime.ExecContext(t.Context(), `INSERT INTO raw_manifests(tenant_id,manifest_id,device_id,provider,configured_root_id,source_key,source_key_sha256,capture_id,parent_receipt,receipt,generation,kind,captured_at,accepted_at,canonical_json)
		VALUES($1,$2,'device-a','codex','root-a','oversized-source',$3,'oversized-unprojected','',$4,1,'snapshot',clock_timestamp(),clock_timestamp(),convert_to(repeat('x',$5),'UTF8'))`, runtime.tenant, manifestID, sourceHash, receipt, parityPhysicalMaxBytes*2)
	require.NoError(t, err)
	_, err = runtime.runtime.ExecContext(t.Context(), `UPDATE raw_source_heads SET manifest_id=$1,receipt=$2,generation=1 WHERE source_key_sha256=$3`, manifestID, receipt, sourceHash)
	require.NoError(t, err)
	var stored, expanded int64
	require.NoError(t, runtime.runtime.QueryRowContext(t.Context(), `SELECT pg_column_size(canonical_json),octet_length(canonical_json) FROM raw_manifests WHERE manifest_id=$1`, manifestID).Scan(&stored, &expanded))
	assert.Less(t, stored, expanded, "fixture must exercise compressed oversized metadata")

	store, err := NewMigrationParityStore(t.Context(), runtime.runtime, MigrationParityOptions{Schema: runtime.schema, Tenant: runtime.tenant})
	require.NoError(t, err)
	request := rawderive.ParityRequest{RunID: "00000000-0000-4000-8000-000000000126", RuntimeID: "00000000-0000-4000-8000-000000000127", BaselineProfile: "before", Cohort: rawderive.ParityCohort{DeviceID: "device-a", Provider: parser.AgentCodex, RootID: "root-a"}}
	_, err = store.CreateOrResumeParity(t.Context(), request, 4)
	require.NoError(t, err)
	lease := claimedParityLease(t, runtime.hostedFixture, request, time.Hour)
	reader := newParityReadRole(t, baseline.hostedFixture)
	var baselineID string
	require.NoError(t, reader.QueryRowContext(t.Context(), `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&baselineID))
	binding := parityPhysicalBinding(runtime.tenant)
	binding.Request, binding.BaselineID = request, baselineID
	lease, err = store.BindParity(t.Context(), lease, binding)
	require.NoError(t, err)

	g_runtime.GC()
	var before, after g_runtime.MemStats
	g_runtime.ReadMemStats(&before)
	require.NoError(t, store.CaptureParityBaseline(t.Context(), lease, reader, binding))
	g_runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	assert.Less(t, allocated, uint64(parityPhysicalMaxBytes), "oversized compressed manifest must be rejected before its expanded payload is loaded")
	var verdict, code string
	require.NoError(t, runtime.runtime.QueryRowContext(t.Context(), `SELECT verdict,code FROM migration_parity_sources WHERE run_id=$1 AND source_id=$2`, request.RunID, rawderive.SourceID(source)).Scan(&verdict, &code))
	assert.Equal(t, string(rawderive.ParityPartial), verdict)
	assert.Equal(t, "limit_exceeded", code)
}

func TestParityBaselineRejectsChangedBindingDigest(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000073")
	changed := scenario.binding
	changed.Versions.Policy[0]++
	err := scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, changed)
	require.ErrorContains(t, err, "binding_conflict")
}

func TestParityBaselineLegacyCollisionMissingOriginalAndFreshness(t *testing.T) {
	for _, test := range []struct {
		name      string
		runID     string
		sourceID  string
		collision bool
		expected  rawderive.ParityVerdict
	}{
		{name: "missing original identifier", runID: "00000000-0000-4000-8000-000000000073", expected: rawderive.ParityLegacyOnly},
		{name: "public alias collision", runID: "00000000-0000-4000-8000-000000000074", sourceID: "source-session", collision: true, expected: rawderive.ParityAmbiguous},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := newHostedFixture(t, "tenant-a")
			baseline := newHostedFixture(t, "tenant-a")
			seedParityPhysicalMember(t, baseline)
			_, err := baseline.runtime.ExecContext(t.Context(), `UPDATE sessions SET provenance_kind='legacy',agent='claude',source_session_id=$1 WHERE id='physical'`, test.sourceID)
			require.NoError(t, err)
			if test.collision {
				_, err = baseline.runtime.ExecContext(t.Context(), `INSERT INTO raw_session_groups(group_id,provider,logical_key,base_alias) VALUES('group-a','claude','logical','logical');
					INSERT INTO raw_session_public_aliases(alias_id,group_id) VALUES('source-session','group-a')`)
				require.NoError(t, err)
			}
			store, err := NewMigrationParityStore(t.Context(), runtime.runtime, MigrationParityOptions{Schema: runtime.schema, Tenant: runtime.tenant})
			require.NoError(t, err)
			request := rawderive.ParityRequest{RunID: test.runID, RuntimeID: "00000000-0000-4000-8000-000000000075", BaselineProfile: "before", Cohort: rawderive.ParityCohort{DeviceID: "device-a", Provider: parser.AgentClaude, RootID: "root-a"}}
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
			var sourceVerdict, memberVerdict string
			var memberKeyBytes []byte
			require.NoError(t, runtime.runtime.QueryRowContext(t.Context(), `SELECT s.verdict,m.verdict,m.member_key FROM migration_parity_sources s JOIN migration_parity_members m USING(tenant_id,run_id,init_epoch,source_id) WHERE s.run_id=$1`, request.RunID).Scan(&sourceVerdict, &memberVerdict, &memberKeyBytes))
			memberKey, err := decodeParityMemberKey(memberKeyBytes)
			require.NoError(t, err)
			assert.Equal(t, string(test.expected), sourceVerdict)
			assert.Equal(t, string(test.expected), memberVerdict)
			if !test.collision {
				assert.Equal(t, "physical", memberKey.LogicalKey, "missing source_session_id falls back to the physical ID")
				for _, statement := range []string{
					`DELETE FROM tool_result_events WHERE session_id='physical'`,
					`DELETE FROM tool_calls WHERE session_id='physical'`,
					`DELETE FROM messages WHERE session_id='physical'`,
					`DELETE FROM usage_events WHERE session_id='physical'`,
					`DELETE FROM secret_findings WHERE session_id='physical'`,
					`DELETE FROM sessions WHERE id='physical'`,
				} {
					_, err = baseline.runtime.ExecContext(t.Context(), statement)
					require.NoError(t, err)
				}
				require.NoError(t, store.ValidateParityEvidence(t.Context(), lease, reader, binding, 4))
				require.NoError(t, runtime.runtime.QueryRowContext(t.Context(), `SELECT verdict FROM migration_parity_sources WHERE run_id=$1`, request.RunID).Scan(&sourceVerdict))
				assert.Equal(t, string(rawderive.ParityStale), sourceVerdict, "deleted legacy member stales its required source")
			}
		})
	}
}

func TestParityInvalidLegacyMemberRemainsFreshWhenUnchanged(t *testing.T) {
	runtime := newHostedFixture(t, "tenant-a")
	baseline := newHostedFixture(t, "tenant-a")
	seedParityPhysicalMember(t, baseline)
	_, err := baseline.runtime.ExecContext(t.Context(), `UPDATE sessions SET provenance_kind='legacy',agent='claude',source_session_id='' WHERE id='physical';
		INSERT INTO tool_calls(session_id,message_ordinal,call_index,tool_name,category) VALUES('physical',99,0,'orphan','read')`)
	require.NoError(t, err)
	store, err := NewMigrationParityStore(t.Context(), runtime.runtime, MigrationParityOptions{Schema: runtime.schema, Tenant: runtime.tenant})
	require.NoError(t, err)
	request := rawderive.ParityRequest{RunID: "00000000-0000-4000-8000-000000000134", RuntimeID: "00000000-0000-4000-8000-000000000135", BaselineProfile: "before", Cohort: rawderive.ParityCohort{DeviceID: "device-a", Provider: parser.AgentClaude, RootID: "root-a"}}
	_, err = store.CreateOrResumeParity(t.Context(), request, 1)
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
	var memberVerdict, sourceVerdict, validation string
	require.NoError(t, runtime.runtime.QueryRowContext(t.Context(), `SELECT m.verdict,s.verdict,r.validation_state FROM migration_parity_members m JOIN migration_parity_sources s USING(tenant_id,run_id,init_epoch,source_id) JOIN migration_parity_runs r USING(tenant_id,run_id,init_epoch) WHERE m.run_id=$1`, request.RunID).Scan(&memberVerdict, &sourceVerdict, &validation))
	assert.Equal(t, string(rawderive.ParityPartial), memberVerdict)
	assert.NotEqual(t, string(rawderive.ParityStale), sourceVerdict)
	assert.Equal(t, "checked", validation)
}

func TestParityBaselineUsesOnePinnedSnapshotAcrossPageHooks(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000051")
	var physicalID, logicalKey string
	require.NoError(t, scenario.baseline.runtime.QueryRowContext(t.Context(), `SELECT ss.physical_session_id,g.logical_key FROM session_sources ss JOIN raw_session_groups g ON g.group_id=ss.group_id WHERE ss.source_id=$1`, scenario.sourceID).Scan(&physicalID, &logicalKey))
	key := rawderive.ParityMemberKey{SourceID: scenario.sourceID, LogicalKey: logicalKey, Kind: "session"}
	tx, err := scenario.reader.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	before, err := readParityPhysicalMember(t.Context(), tx, scenario.binding, physicalID, key)
	require.NoError(t, err)
	expected, err := rawderive.FingerprintParity(t.Context(), scenario.binding, before.Graph)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	mutated := false
	scenario.store.parityEvidencePageHook = func(ctx context.Context, phase string) error {
		if phase != "baseline_source" || mutated {
			return nil
		}
		mutated = true
		_, updateErr := scenario.baseline.runtime.ExecContext(ctx, `UPDATE tool_result_events SET content='changed',content_length=7 WHERE session_id=$1`, physicalID)
		return updateErr
	}
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	assert.True(t, mutated)
	var stored []byte
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT baseline_fingerprint FROM migration_parity_members WHERE run_id=$1 AND source_id=$2`, scenario.lease.RunID, scenario.sourceID).Scan(&stored))
	assert.Equal(t, encodeParityFingerprint(expected), stored, "the capture keeps the snapshot opened before the page hook mutation")
}

func TestParityBaselinePinnedCaptureExcludesInsertionBeforeConsumedCursor(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000128")
	var firstReceipt string
	require.NoError(t, scenario.baseline.runtime.QueryRowContext(t.Context(), `SELECT receipt FROM raw_source_heads WHERE source_key='source-a'`).Scan(&firstReceipt))
	bulkManifest, bulkCommit := scenario.baseline.acceptScoped(t, "device-a", "capture-bulk", firstReceipt, parser.AgentCodex, "root-a", "source-a")
	bulk := projectionOutcome("bulk-000")
	bulk.Outcome.Results[0].Result.Session.ID = "codex:bulk-000"
	bulk.Outcome.Results[0].Result.Session.SourceSessionID = "bulk-000"
	for i := 1; i < parityInventoryPageSize+1; i++ {
		logical := fmt.Sprintf("bulk-%03d", i)
		result := projectionOutcome(logical).Outcome.Results[0]
		result.Result.Session.ID = "codex:" + logical
		result.Result.Session.SourceSessionID = logical
		bulk.Outcome.Results = append(bulk.Outcome.Results, result)
	}
	require.Len(t, bulk.Outcome.Results, parityInventoryPageSize+1)
	require.NoError(t, scenario.baseline.sink.Project(t.Context(), scenario.baseline.lease(t, bulkManifest), bulkManifest, bulk))
	var consumedCursor string
	require.NoError(t, scenario.baseline.runtime.QueryRowContext(t.Context(), `SELECT group_id FROM raw_session_branches WHERE source_id=$1 AND active ORDER BY group_id OFFSET $2 LIMIT 1`, scenario.sourceID, parityInventoryPageSize-1).Scan(&consumedCursor))
	nextManifest, _ := scenario.baseline.acceptScoped(t, "device-a", "capture-insert", bulkCommit.Receipt, parser.AgentCodex, "root-a", "source-a")
	insertedLogical := ""
	for i := 0; i < 10000; i++ {
		candidate := fmt.Sprintf("inserted-before-%d", i)
		groupID, _ := rawderive.GroupID(nextManifest, db.Session{ID: "codex:" + candidate, Agent: string(parser.AgentCodex), SourceSessionID: candidate})
		if groupID < consumedCursor {
			insertedLogical = candidate
			break
		}
	}
	require.NotEmpty(t, insertedLogical)
	next := bulk
	inserted := projectionOutcome("inserted").Outcome.Results[0]
	inserted.Result.Session.ID = "codex:" + insertedLogical
	inserted.Result.Session.SourceSessionID = insertedLogical
	next.Outcome.Results = append(next.Outcome.Results, inserted)
	mutated := false
	scenario.store.parityEvidencePageHook = func(ctx context.Context, phase string) error {
		if phase != "baseline_member" || mutated {
			return nil
		}
		mutated = true
		return scenario.baseline.sink.Project(ctx, scenario.baseline.lease(t, nextManifest), nextManifest, next)
	}
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	require.True(t, mutated)
	var sealedMembers, insertedMembers int
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_members WHERE run_id=$1`, scenario.lease.RunID).Scan(&sealedMembers))
	assert.Equal(t, parityInventoryPageSize+1, sealedMembers)
	insertedKey, insertedDigest := encodeParityMemberKey(rawderive.ParityMemberKey{SourceID: scenario.sourceID, LogicalKey: insertedLogical, Kind: "session"})
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_members WHERE run_id=$1 AND member_digest=$2 AND member_key=$3`, scenario.lease.RunID, insertedDigest[:], insertedKey).Scan(&insertedMembers))
	assert.Zero(t, insertedMembers, "the pinned capture excludes a member inserted behind its consumed cursor")
	scenario.store.parityEvidencePageHook = nil
	require.NoError(t, scenario.store.ValidateParityEvidence(t.Context(), scenario.lease, scenario.reader, scenario.binding, 128))
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_members WHERE run_id=$1 AND member_digest=$2 AND member_key=$3 AND inventory_kind='validation_delta'`, scenario.lease.RunID, insertedDigest[:], insertedKey).Scan(&insertedMembers))
	assert.Equal(t, 1, insertedMembers, "the next full snapshot observes the insertion")
}

func TestParityBaselinePinnedSnapshotCoversMessageToolUsageAliasExclusionAndPin(t *testing.T) {
	tests := []struct {
		name   string
		runID  string
		mutate func(*testing.T, parityCaptureScenario, string)
	}{
		{name: "message", runID: "00000000-0000-4000-8000-000000000111", mutate: func(t *testing.T, scenario parityCaptureScenario, physical string) {
			_, err := scenario.baseline.runtime.ExecContext(t.Context(), `UPDATE messages SET content='changed',content_length=7 WHERE session_id=$1 AND ordinal=0`, physical)
			require.NoError(t, err)
		}},
		{name: "tool", runID: "00000000-0000-4000-8000-000000000112", mutate: func(t *testing.T, scenario parityCaptureScenario, physical string) {
			_, err := scenario.baseline.runtime.ExecContext(t.Context(), `UPDATE tool_calls SET input_json='{"changed":true}' WHERE session_id=$1`, physical)
			require.NoError(t, err)
		}},
		{name: "usage", runID: "00000000-0000-4000-8000-000000000113", mutate: func(t *testing.T, scenario parityCaptureScenario, physical string) {
			_, err := scenario.baseline.runtime.ExecContext(t.Context(), `UPDATE usage_events SET output_tokens=99 WHERE session_id=$1`, physical)
			require.NoError(t, err)
		}},
		{name: "alias", runID: "00000000-0000-4000-8000-000000000114", mutate: func(t *testing.T, scenario parityCaptureScenario, physical string) {
			_, err := scenario.baseline.runtime.ExecContext(t.Context(), `INSERT INTO session_aliases(session_id,alias_id) VALUES($1,'late-alias')`, physical)
			require.NoError(t, err)
		}},
		{name: "exclusion", runID: "00000000-0000-4000-8000-000000000115", mutate: func(t *testing.T, scenario parityCaptureScenario, physical string) {
			_, err := scenario.baseline.runtime.ExecContext(t.Context(), `INSERT INTO excluded_sessions(id) VALUES($1)`, physical)
			require.NoError(t, err)
		}},
		{name: "pin", runID: "00000000-0000-4000-8000-000000000116", mutate: func(t *testing.T, scenario parityCaptureScenario, physical string) {
			_, err := scenario.baseline.runtime.ExecContext(t.Context(), `INSERT INTO pinned_messages(session_id,message_id,ordinal,source_uuid,note) VALUES($1,99,0,'late-pin','note')`, physical)
			require.NoError(t, err)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scenario := newParityCaptureScenario(t, test.runID)
			var physicalID, logicalKey string
			require.NoError(t, scenario.baseline.runtime.QueryRowContext(t.Context(), `SELECT ss.physical_session_id,g.logical_key FROM session_sources ss JOIN raw_session_groups g ON g.group_id=ss.group_id WHERE ss.source_id=$1`, scenario.sourceID).Scan(&physicalID, &logicalKey))
			key := rawderive.ParityMemberKey{SourceID: scenario.sourceID, LogicalKey: logicalKey, Kind: "session"}
			tx, err := scenario.reader.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
			require.NoError(t, err)
			before, err := readParityPhysicalMember(t.Context(), tx, scenario.binding, physicalID, key)
			require.NoError(t, err)
			expected, err := rawderive.FingerprintParity(t.Context(), scenario.binding, before.Graph)
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
			scenario.store.parityEvidencePageHook = func(ctx context.Context, phase string) error {
				if phase == "baseline_source" {
					test.mutate(t, scenario, physicalID)
					scenario.store.parityEvidencePageHook = nil
				}
				return nil
			}
			require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
			var storedSemantic, storedPhysical []byte
			require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT baseline_fingerprint,physical_fingerprint FROM migration_parity_members WHERE run_id=$1 AND source_id=$2`, scenario.lease.RunID, scenario.sourceID).Scan(&storedSemantic, &storedPhysical))
			assert.Equal(t, encodeParityFingerprint(expected), storedSemantic)
			assert.Equal(t, before.Physical[:], storedPhysical)
		})
	}
}

func TestParityBaselineSealedDigestsSurviveStoreReopen(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000117")
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	var beforeInventory, beforeBaseline []byte
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT inventory_digest,baseline_digest FROM migration_parity_runs WHERE run_id=$1`, scenario.lease.RunID).Scan(&beforeInventory, &beforeBaseline))
	reopened, err := NewMigrationParityStore(t.Context(), scenario.runtime.runtime, MigrationParityOptions{Schema: scenario.runtime.schema, Tenant: scenario.runtime.tenant})
	require.NoError(t, err)
	report, err := reopened.ReadParityReport(t.Context(), scenario.lease.RunID)
	require.NoError(t, err)
	assert.True(t, report.BaselineSealed)
	var afterInventory, afterBaseline []byte
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT inventory_digest,baseline_digest FROM migration_parity_runs WHERE run_id=$1`, scenario.lease.RunID).Scan(&afterInventory, &afterBaseline))
	assert.Equal(t, beforeInventory, afterInventory)
	assert.Equal(t, beforeBaseline, afterBaseline)
	assert.Len(t, beforeInventory, 32)
	assert.Len(t, beforeBaseline, 32)
}

func TestParityBaselineRestartReplacesOnlyTheUnsealedEpoch(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000053")
	interrupted := errors.New("synthetic evidence interruption")
	scenario.store.parityEvidencePageHook = func(_ context.Context, phase string) error {
		if phase == "baseline_member" {
			return interrupted
		}
		return nil
	}
	require.ErrorIs(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding), interrupted)
	var sealed bool
	var partialRows int
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT baseline_sealed FROM migration_parity_runs WHERE run_id=$1`, scenario.lease.RunID).Scan(&sealed))
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM migration_parity_sources WHERE run_id=$1)+(SELECT count(*) FROM migration_parity_members WHERE run_id=$1)`, scenario.lease.RunID).Scan(&partialRows))
	assert.False(t, sealed)
	assert.Positive(t, partialRows)

	scenario.store.parityEvidencePageHook = nil
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	var sources, members int
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_sources WHERE run_id=$1`, scenario.lease.RunID).Scan(&sources))
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_members WHERE run_id=$1`, scenario.lease.RunID).Scan(&members))
	assert.Equal(t, 1, sources)
	assert.Equal(t, 1, members)
}

func TestParityBaselineRestartReclaimsOnlyUnsealedEvidence(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000053")
	interrupted := errors.New("synthetic capture interruption")
	scenario.store.parityEvidencePageHook = func(_ context.Context, phase string) error {
		if phase == "baseline_member" {
			return interrupted
		}
		return nil
	}
	require.ErrorIs(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding), interrupted)
	var sealed bool
	var rows int
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT baseline_sealed FROM migration_parity_runs WHERE run_id=$1`, scenario.lease.RunID).Scan(&sealed))
	require.False(t, sealed)
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_members WHERE run_id=$1`, scenario.lease.RunID).Scan(&rows))
	require.Positive(t, rows)

	scenario.store.parityEvidencePageHook = nil
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT baseline_sealed FROM migration_parity_runs WHERE run_id=$1`, scenario.lease.RunID).Scan(&sealed))
	assert.True(t, sealed)
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_members WHERE run_id=$1`, scenario.lease.RunID).Scan(&rows))
	assert.Equal(t, 1, rows)
}
