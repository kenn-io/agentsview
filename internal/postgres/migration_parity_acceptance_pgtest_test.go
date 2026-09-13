//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawtest"
)

// These rows represent unrelated devices' captured heads. Fixtures are inserted
// before allocation measurements, and source/history calls use the real store.
func TestMigrationParityAcceptanceSelectedSourceAllocationsIgnoreUnrelatedCorpus(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000151")
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	var small float64
	for _, size := range []int{10, 10000} {
		_, err := scenario.runtime.runtime.ExecContext(t.Context(), `INSERT INTO raw_source_heads(tenant_id,device_id,provider,configured_root_id,source_key,source_key_sha256,generation)
   SELECT current_setting('agentsview.tenant_id'),'device-a','codex','unrelated-root','unrelated-'||n,repeat(md5('unrelated-'||n),2),0 FROM generate_series(1,$1) n ON CONFLICT DO NOTHING`, size)
		require.NoError(t, err)
		allocations := testing.AllocsPerRun(5, func() {
			sources, err := scenario.store.NextParitySources(t.Context(), scenario.lease, "", 1)
			require.NoError(t, err)
			require.Len(t, sources, 1)
			require.Equal(t, scenario.sourceID, sources[0].ID)
			history, err := scenario.store.NextParityHistory(t.Context(), scenario.lease, sources[0].ID, 0, 128)
			require.NoError(t, err)
			require.Len(t, history, 1)
		})
		if size == 10 {
			small = allocations
		} else {
			assert.LessOrEqual(t, allocations, small+64, "one-source work grew with unrelated corpus")
		}
		t.Logf("unrelated sources=%d allocations/op=%.0f source/history rows per operation=1/1", size, allocations)
	}
}

func TestMigrationParityAcceptanceCohortPagesAndReportsStayBounded(t *testing.T) {
	f, store, lease, binding := boundParityFixture(t)
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO raw_devices(device_id,display_name,credential_sha256,created_at) VALUES($1,'fixture',decode(repeat('00',32),'hex'),clock_timestamp())`, binding.Request.Cohort.DeviceID)
	require.NoError(t, err)
	var smallPeak uint64
	for _, size := range []int{10, 10000} {
		_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO raw_source_heads(tenant_id,device_id,provider,configured_root_id,source_key,source_key_sha256,generation)
   SELECT current_setting('agentsview.tenant_id'),$2,$3,$4,'cohort-'||n,repeat(md5('cohort-'||n),2),0 FROM generate_series(1,$1) n ON CONFLICT DO NOTHING`, size, binding.Request.Cohort.DeviceID, binding.Request.Cohort.Provider, binding.Request.Cohort.RootID)
		require.NoError(t, err)
		_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO raw_manifests(tenant_id,manifest_id,device_id,provider,configured_root_id,source_key,source_key_sha256,capture_id,parent_receipt,receipt,generation,kind,captured_at,canonical_json)
   SELECT current_setting('agentsview.tenant_id'),repeat(md5('manifest-'||n),2),$2,$3,$4,'cohort-'||n,repeat(md5('cohort-'||n),2),'capture-'||n,'',repeat(md5('receipt-'||n),2),1,'snapshot',clock_timestamp(),convert_to('{}','UTF8') FROM generate_series(1,$1)n ON CONFLICT DO NOTHING`, size, binding.Request.Cohort.DeviceID, binding.Request.Cohort.Provider, binding.Request.Cohort.RootID)
		require.NoError(t, err)
		_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_source_heads h SET manifest_id=m.manifest_id,receipt=m.receipt,generation=1 FROM raw_manifests m WHERE h.source_key_sha256=m.source_key_sha256`)
		require.NoError(t, err)
		_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_sources(run_id,init_epoch,source_id,source_kind,device_id,provider,root_id,head_manifest,head_generation,head_receipt,source_key_sha256,dependency_digest,code)
    SELECT $1,$2,'source-'||n,'raw',$4,$5,$6,repeat(md5('manifest-'||n),2),1,repeat(md5('receipt-'||n),2),repeat(md5('cohort-'||n),2),decode(repeat('00',32),'hex'),'pending' FROM generate_series(1,$3)n ON CONFLICT DO NOTHING`, lease.RunID, lease.InitEpoch, size, binding.Request.Cohort.DeviceID, binding.Request.Cohort.Provider, binding.Request.Cohort.RootID)
		require.NoError(t, err)

		tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		require.NoError(t, err)
		after := ""
		delivered, pages, peakRows := 0, 0, 0
		var peak uint64
		for {
			var before, afterMemory runtime.MemStats
			runtime.ReadMemStats(&before)
			page, err := readParityCohortSources(t.Context(), tx, binding, after, 128)
			runtime.ReadMemStats(&afterMemory)
			require.NoError(t, err)
			delta := afterMemory.TotalAlloc - before.TotalAlloc
			if delta > peak {
				peak = delta
			}
			if len(page) > peakRows {
				peakRows = len(page)
			}
			require.LessOrEqual(t, len(page), 128)
			if len(page) == 0 {
				break
			}
			pages++
			delivered += len(page)
			after = page[len(page)-1].cursor
		}
		require.NoError(t, tx.Rollback())
		assert.Equal(t, size, delivered)
		assert.Equal(t, (size+127)/128, pages)
		assert.Equal(t, min(size, 128), peakRows)
		if size == 10 {
			smallPeak = peak
		} else {
			assert.LessOrEqual(t, peak, smallPeak*16+(128<<10), "page allocations grew beyond one 128-row page")
		}
		// Public output contains fixed counters, not a materialized source listing.
		report, err := store.ReadParityReport(t.Context(), lease.RunID)
		require.NoError(t, err)
		require.EqualValues(t, size, report.PendingSources)
		encoded, err := json.Marshal(report)
		require.NoError(t, err)
		assert.Less(t, len(encoded), 4096)
		t.Logf("cohort=%d delivered=%d pages=%d peak page rows=%d peak allocated bytes=%d report bytes=%d", size, delivered, pages, peakRows, peak, len(encoded))
	}
}

func TestMigrationParityAcceptanceCancellationBetweenCensusPages(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000152")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	callbacks := 0
	scenario.store.parityEvidencePageHook = func(_ context.Context, phase string) error {
		if phase == "baseline_source" {
			callbacks++
			cancel()
		}
		return ctx.Err()
	}
	require.ErrorIs(t, scenario.store.CaptureParityBaseline(ctx, scenario.lease, scenario.reader, scenario.binding), context.Canceled)
	assert.Equal(t, 1, callbacks)
	report, err := scenario.store.ReadParityReport(t.Context(), scenario.lease.RunID)
	require.NoError(t, err)
	assert.False(t, report.BaselineSealed)
	assert.False(t, report.Passing)
}

func TestMigrationParityAcceptancePhysicalChildCeiling(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	seedParityPhysicalMember(t, f)
	// One existing message, call, result and usage count toward the combined
	// ceiling. Exceed it with small valid children: byte ceilings cannot mask it.
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO usage_events(session_id,source,model,input_tokens,output_tokens,cost_status,cost_source,dedup_key)
 SELECT 'physical','fixture','gpt-5.4',0,0,'unpriced','fixture','extra-'||n FROM generate_series(1,100001)n`)
	require.NoError(t, err)
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback()
	member, err := readParityPhysicalMember(t.Context(), tx, parityPhysicalBinding(f.tenant), "physical", rawderive.ParityMemberKey{SourceID: "source-a", LogicalKey: "logical-a", Kind: "session"})
	require.NoError(t, err)
	assert.Equal(t, "limit_exceeded", member.InvalidCode)
	assert.Empty(t, member.Graph.Prepared.UsageEvents)
}

func TestMigrationParityAcceptancePhysicalPreparationVersionIsUnsupported(t *testing.T) {
	for _, mutation := range []string{
		`UPDATE sessions SET data_version=0`,
		`UPDATE sessions SET quality_signal_version=0`,
		`UPDATE sessions SET secrets_rules_version='earlier-rules'`,
		`UPDATE secret_findings SET rules_version='earlier-rules'`,
	} {
		t.Run(mutation, func(t *testing.T) {
			f := newHostedFixture(t, "tenant-a")
			seedParityPhysicalMember(t, f)
			_, err := f.runtime.ExecContext(t.Context(), mutation)
			require.NoError(t, err)
			tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
			require.NoError(t, err)
			defer tx.Rollback()
			member, err := readParityPhysicalMember(t.Context(), tx, parityPhysicalBinding(f.tenant), "physical", rawderive.ParityMemberKey{SourceID: "source-a", LogicalKey: "logical-a", Kind: "session"})
			require.NoError(t, err)
			assert.Equal(t, "unsupported", member.InvalidCode)
		})
	}
}

func TestMigrationParityAcceptanceInvalidRunBindingRemainsFatal(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	seedParityPhysicalMember(t, f)
	binding := parityPhysicalBinding(f.tenant)
	binding.Versions.Comparison++
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback()
	member, err := readParityPhysicalMember(t.Context(), tx, binding, "physical", rawderive.ParityMemberKey{SourceID: "source-a", LogicalKey: "logical-a", Kind: "session"})
	require.ErrorContains(t, err, "unsupported parity binding version")
	assert.NotErrorIs(t, err, rawderive.ErrParityGraphVersion)
	assert.Empty(t, member.InvalidCode)
}

func TestMigrationParityAcceptanceUnsupportedBaselineCapturesNoFingerprint(t *testing.T) {
	scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000153")
	_, err := scenario.baseline.runtime.ExecContext(t.Context(), `UPDATE sessions SET data_version=0`)
	require.NoError(t, err)
	require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
	var members, partial, fingerprints int
	require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*),count(*) FILTER(WHERE verdict='partial_unsupported'),count(baseline_fingerprint) FROM migration_parity_members WHERE run_id=$1`, scenario.lease.RunID).Scan(&members, &partial, &fingerprints))
	assert.Equal(t, 1, members)
	assert.Equal(t, 1, partial)
	assert.Zero(t, fingerprints)
	report, err := scenario.store.ReadParityReport(t.Context(), scenario.lease.RunID)
	require.NoError(t, err)
	assert.True(t, report.BaselineSealed)
	assert.False(t, report.Passing)
	assert.EqualValues(t, 1, report.Members.PartialUnsupported)
}

func TestMigrationParityAcceptancePiebaldForkPublication(t *testing.T) {
	source := rawtest.PiebaldParity(t, t.TempDir())
	provider, ok := parser.NewProvider(parser.AgentPiebald, parser.ProviderConfig{Roots: []string{source.Root}, Machine: "fixture"})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 2)
	combined := parser.ParseOutcome{ResultSetComplete: true}
	for _, source := range sources {
		outcome, err := provider.Parse(t.Context(), parser.ParseRequest{Source: source})
		require.NoError(t, err)
		combined.Results = append(combined.Results, outcome.Results...)
	}
	require.Len(t, combined.Results, 3)
	f := newProjectionFixture(t)
	manifest, _ := f.accept(t, "device-a", "capture-piebald", "", parser.AgentPiebald)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, manifest), manifest, rawderive.ParsedManifest{Outcome: combined}))
	for _, id := range source.IDs() {
		resolved, err := f.sink.Resolve(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, RawIdentityUnique, resolved.State)
	}
	var count int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM sessions`).Scan(&count))
	assert.Equal(t, 3, count)
}
