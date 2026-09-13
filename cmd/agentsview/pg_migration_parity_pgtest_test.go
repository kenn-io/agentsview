//go:build pgtest

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/rawtest"
)

type parityCommandFixture struct {
	owner                   *pgMigrationParityOwner
	request                 rawderive.ParityRequest
	baselineDB              *sql.DB
	runtimeDB               *sql.DB
	archivePath             string
	sourceRoot              string
	runtimeCfg, baselineCfg config.Config
	custody                 *pgRawSyncCustody
	capture                 *directParityCapture
	generations             []directCapturedGeneration
}

func newParityCommandFixture(t *testing.T) *parityCommandFixture {
	t.Helper()
	return newParityCommandProviderFixture(t, parser.AgentCodex, func(t *testing.T, root string) rawtest.ParityFixture {
		rawtest.CodexTools(t, root)
		return rawtest.ParityFixture{Root: root, Sessions: []rawtest.ParitySession{{ID: "codex:" + rawtest.CodexToolsID, First: "Run the build.", Roles: []string{"user", "assistant", "assistant"}, Output: 9, HasOutput: true, ToolName: "exec_command", ToolResult: "Error: synthetic failure"}}}
	})
}
func newParityCommandProviderFixture(t *testing.T, agent parser.AgentType, build func(*testing.T, string) rawtest.ParityFixture) *parityCommandFixture {
	t.Helper()
	sourceFixture := build(t, t.TempDir())
	oracle, engine := rawtest.Oracle(t, agent, sourceFixture.Root, config.ArchiveContentFull, "")
	require.Positive(t, engine.SyncAll(t.Context(), nil).Synced)
	sourceFixture.AssertOracle(t, oracle)
	runtimeCfg, _ := hostedRuntimeConfig(t)
	baselineCfg, baselineAdmin := hostedRuntimeConfig(t)
	runtimeDB, err := postgres.OpenHosted(runtimeCfg.PG.URL, runtimeCfg.PG.Schema, runtimeCfg.PG.RawTenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtimeDB.Close()) })
	baselineDB, err := postgres.OpenHosted(baselineCfg.PG.URL, baselineCfg.PG.Schema, baselineCfg.PG.RawTenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, baselineDB.Close()) })

	runtimeCustody := newDirectParityCustody(t, runtimeCfg, runtimeDB)
	baselineCustody := newDirectParityCustody(t, baselineCfg, baselineDB)
	root := sourceFixture.Root
	device := "00000000-0000-4000-8000-000000000140"
	runtimeCapture := newDirectParityCapture(t, runtimeDB, runtimeCustody, agent, root, device)
	runtimeGenerations := runtimeCapture.captureAll(t)
	require.NotEmpty(t, runtimeGenerations)
	registerDirectParityDevice(t, baselineDB, runtimeCapture.identity)
	baselineCapture := &directParityCapture{identity: runtimeCapture.identity, custody: baselineCustody}
	baselineGenerations := replicateDirectParityGenerations(t, runtimeCapture, baselineCapture, runtimeGenerations)
	require.Len(t, baselineGenerations, len(runtimeGenerations))
	requireHostedSandbox(t)
	runDirectRawDerivation(t, runtimeCfg, runtimeDB, runtimeCustody, len(runtimeGenerations))
	runDirectRawDerivation(t, baselineCfg, baselineDB, baselineCustody, len(baselineGenerations))

	baselinePublic, err := postgres.NewHostedStore(baselineCfg.PG.URL, baselineCfg.PG.Schema, baselineCfg.PG.RawTenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, baselinePublic.Close()) })
	rawtest.EqualStored(t, t.Context(), oracle, baselinePublic, sourceFixture.IDs()...)
	projection, err := postgres.NewRawProjectionStore(baselineDB, postgres.RawProjectionOptions{Tenant: baselineCfg.PG.RawTenant})
	require.NoError(t, err)
	for _, id := range sourceFixture.IDs() {
		resolved, err := projection.Resolve(t.Context(), id)
		require.NoError(t, err)
		rawtest.EqualUsageEvents(t, t.Context(), oracle, baselineDB, id, resolved.SessionID)
	}
	// A real, closed archive at the configured command path detects even a read
	// that accidentally opens SQLite writable or starts a local ingestion worker.
	engine.Close()
	require.NoError(t, oracle.Close())
	runtimeCfg.DBPath = filepath.Join(runtimeCfg.DataDir, "sessions.db")
	archiveBytes, err := os.ReadFile(oracle.Path())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(runtimeCfg.DBPath, archiveBytes, 0600))
	baselineReadCfg := restrictedDirectParityBaseline(t, baselineCfg, baselineAdmin)
	owner, _, request := configureDirectParityOwner(t, runtimeCfg, baselineReadCfg, runtimeDB, runtimeCustody)
	var baselineID string
	require.NoError(t, baselineDB.QueryRowContext(t.Context(), `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&baselineID))
	writeParityCommandConfig(t, runtimeCfg, baselineReadCfg, baselineID)

	t.Setenv("AGENTSVIEW_DATA_DIR", runtimeCfg.DataDir)
	t.Setenv("AGENT_VIEWER_DATA_DIR", "")
	t.Setenv("AGENTSVIEW_PG_URL", "")
	t.Setenv("AGENTSVIEW_PG_SCHEMA", "")
	t.Setenv("AGENTSVIEW_PG_MACHINE", "")
	return &parityCommandFixture{owner: owner, request: request, baselineDB: baselineDB, runtimeDB: runtimeDB, archivePath: runtimeCfg.DBPath, sourceRoot: root, runtimeCfg: runtimeCfg, baselineCfg: baselineReadCfg, custody: runtimeCustody, capture: runtimeCapture, generations: runtimeGenerations}
}

func writeParityCommandConfig(t *testing.T, runtimeCfg, baselineCfg config.Config, baselineID string) {
	t.Helper()
	body := fmt.Sprintf(`require_auth = true
auth_token = "synthetic-test-token"
cursor_secret = "c3ludGhldGljLWN1cnNvci1zZWNyZXQ="
default_pg = "runtime"

[pg.runtime]
url = %q
schema = %q
raw_tenant = %q
parity_enabled = true
parity_poll_seconds = 1
parity_attempt_seconds = 20
parity_snapshot_seconds = 20

[pg.runtime.parity_baselines.before]
target = "baseline"
identity = %q

[pg.baseline]
url = %q
schema = %q
raw_tenant = %q
`, runtimeCfg.PG.URL, runtimeCfg.PG.Schema, runtimeCfg.PG.RawTenant, baselineID, baselineCfg.PG.URL, baselineCfg.PG.Schema, baselineCfg.PG.RawTenant)
	require.NoError(t, os.WriteFile(filepath.Join(runtimeCfg.DataDir, "config.toml"), []byte(body), 0o600))
}

func (f *parityCommandFixture) CommandReport(t *testing.T, args ...string) rawderive.ParityReport {
	t.Helper()
	cmd := newPGCommand()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{"migration", "parity"}, args...))
	require.NoError(t, cmd.Execute())
	var report rawderive.ParityReport
	require.NoError(t, json.Unmarshal(output.Bytes(), &report))
	return report
}

func (f *parityCommandFixture) SubmitAndCompleteRun(t *testing.T, batchSize int) int64 {
	t.Helper()
	requested := f.CommandReport(t,
		"--runtime-target", "runtime",
		"--baseline-target", f.request.BaselineProfile,
		"--run-id", f.request.RunID,
		"--device", f.request.Cohort.DeviceID,
		"--provider", string(f.request.Cohort.Provider),
		"--root", f.request.Cohort.RootID,
		"--batch-size", strconv.Itoa(batchSize),
		"--wait", "0",
		"--json",
	)
	assert.False(t, requested.Passing)
	require.Positive(t, requested.RequestGeneration)
	require.NoError(t, f.owner.runOne(t.Context()))
	return requested.RequestGeneration
}

func (f *parityCommandFixture) ChangeBaselineToolEvent(t *testing.T) {
	t.Helper()
	var sourceID string
	require.NoError(t, f.baselineDB.QueryRowContext(t.Context(), `SELECT source_id FROM raw_source_projections WHERE device_id=$1 AND provider=$2 AND configured_root_id=$3`,
		f.request.Cohort.DeviceID, f.request.Cohort.Provider, f.request.Cohort.RootID).Scan(&sourceID))
	var eventCount, eventID int64
	require.NoError(t, f.baselineDB.QueryRowContext(t.Context(), `SELECT count(*),COALESCE(min(e.id),0) FROM tool_result_events e JOIN session_sources ss ON ss.physical_session_id=e.session_id WHERE ss.source_id=$1`, sourceID).Scan(&eventCount, &eventID))
	require.EqualValues(t, 1, eventCount)
	require.Positive(t, eventID)
	result, err := f.baselineDB.ExecContext(t.Context(), `UPDATE tool_result_events SET content=$2,content_length=octet_length($2) WHERE id=$1`, eventID, "changed baseline tool event")
	require.NoError(t, err)
	affected, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, affected)
}

func TestParityStatusConsumesOnlyNamedObservation(t *testing.T) {
	fixture := newParityCommandFixture(t)
	generation := fixture.SubmitAndCompleteRun(t, 1)

	ordinary := fixture.CommandReport(t,
		"--runtime-target", "runtime",
		"--run-id", fixture.request.RunID,
		"--status",
		"--json",
	)
	assert.Equal(t, "historical_evidence", ordinary.Code)
	assert.False(t, ordinary.Passing)

	named := fixture.CommandReport(t,
		"--runtime-target", "runtime",
		"--run-id", fixture.request.RunID,
		"--status",
		"--request-generation", strconv.FormatInt(generation, 10),
		"--json",
	)
	assert.Equal(t, "historical_checked", named.Freshness)
	assert.True(t, named.Passing)
	require.NotNil(t, named.BaselineObservedAt)
	require.NotNil(t, named.RuntimeObservedAt)
	assert.False(t, named.BaselineObservedAt.IsZero())
	assert.False(t, named.RuntimeObservedAt.IsZero())

	fixture.ChangeBaselineToolEvent(t)
	staleGeneration := fixture.SubmitAndCompleteRun(t, 1)
	require.Equal(t, generation+1, staleGeneration)
	stale := fixture.CommandReport(t,
		"--runtime-target", "runtime",
		"--run-id", fixture.request.RunID,
		"--status",
		"--request-generation", strconv.FormatInt(staleGeneration, 10),
		"--json",
	)
	assert.Equal(t, "stale", stale.Freshness)
	assert.False(t, stale.Passing)
	assert.Equal(t, staleGeneration, stale.CompletedGeneration)
	assert.EqualValues(t, 1, stale.Members.Stale)
	assert.EqualValues(t, 1, stale.Sources.Stale)
}

// All schema tables are included by catalog discovery. Only the four mutable
// evidence tables are omitted; identity and every raw-ingest lease column stay.
func parityApplicationSnapshot(t *testing.T, database *sql.DB) map[string][32]byte {
	t.Helper()
	rows, err := database.QueryContext(t.Context(), `SELECT tablename FROM pg_tables WHERE schemaname=current_schema() ORDER BY tablename`)
	require.NoError(t, err)
	var tables []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		switch name {
		case "migration_parity_runs", "migration_parity_sources", "migration_parity_members", "migration_parity_dependencies":
			continue
		}
		tables = append(tables, name)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Contains(t, tables, "raw_ingest_jobs")
	require.Contains(t, tables, "migration_parity_identity")
	require.Contains(t, tables, "tool_result_events")
	result := make(map[string][32]byte, len(tables))
	for _, table := range tables {
		query := `SELECT row_to_json(r)::text FROM "` + strings.ReplaceAll(table, `"`, `""`) + `" r ORDER BY row_to_json(r)::text COLLATE "C"`
		rows, err := database.QueryContext(t.Context(), query)
		require.NoError(t, err)
		hash := sha256.New()
		for rows.Next() {
			var row []byte
			require.NoError(t, rows.Scan(&row))
			var length [8]byte
			binary.BigEndian.PutUint64(length[:], uint64(len(row)))
			_, err = hash.Write(length[:])
			require.NoError(t, err)
			_, err = hash.Write(row)
			require.NoError(t, err)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		var digest [32]byte
		copy(digest[:], hash.Sum(nil))
		result[table] = digest
	}
	return result
}
func (f *parityCommandFixture) assertNoApplicationWrites(t *testing.T) func() {
	t.Helper()
	beforeRuntime, beforeBaseline := parityApplicationSnapshot(t, f.runtimeDB), parityApplicationSnapshot(t, f.baselineDB)
	archive, err := os.ReadFile(f.archivePath)
	require.NoError(t, err)
	beforeArchive := sha256.Sum256(archive)
	return func() {
		afterRuntime, afterBaseline := parityApplicationSnapshot(t, f.runtimeDB), parityApplicationSnapshot(t, f.baselineDB)
		require.Len(t, afterRuntime, len(beforeRuntime))
		require.Len(t, afterBaseline, len(beforeBaseline))
		for name, digest := range beforeRuntime {
			assert.True(t, digest == afterRuntime[name], "runtime application table changed: %s", name)
		}
		for name, digest := range beforeBaseline {
			assert.True(t, digest == afterBaseline[name], "baseline application table changed: %s", name)
		}
		archive, err := os.ReadFile(f.archivePath)
		require.NoError(t, err)
		assert.True(t, beforeArchive == sha256.Sum256(archive), "archive bytes changed")
		for _, suffix := range []string{"-wal", "-shm"} {
			_, err := os.Stat(f.archivePath + suffix)
			assert.True(t, errors.Is(err, os.ErrNotExist), "command opened archive sidecar")
		}
	}
}
func (f *parityCommandFixture) submitArgs(batch int, wait string) []string {
	return []string{"--runtime-target", "runtime", "--baseline-target", f.request.BaselineProfile, "--run-id", f.request.RunID, "--device", f.request.Cohort.DeviceID, "--provider", string(f.request.Cohort.Provider), "--root", f.request.Cohort.RootID, "--batch-size", strconv.Itoa(batch), "--wait", wait, "--json"}
}
func (f *parityCommandFixture) submitWait(t *testing.T, batch int) rawderive.ParityReport {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-ticker.C:
				if err := f.owner.runOne(ctx); err != nil {
					if ctx.Err() != nil {
						done <- nil
					} else {
						done <- err
					}
					return
				}
			}
		}
	}()
	// Always join the actual configured owner, including assertion failures.
	defer func() { cancel(); require.NoError(t, <-done) }()
	return f.CommandReport(t, f.submitArgs(batch, "20s")...)
}
func TestMigrationParityAcceptanceProvidersAndImmutableApplication(t *testing.T) {
	fixtures := rawtest.ParityFixtures()
	for _, agent := range []parser.AgentType{parser.AgentClaude, parser.AgentCodex, parser.AgentEvener, parser.AgentGoose, parser.AgentForge, parser.AgentPiebald, parser.AgentWarp, parser.AgentZCode} {
		t.Run(string(agent), func(t *testing.T) {
			f := newParityCommandProviderFixture(t, agent, fixtures[agent])
			unchanged := f.assertNoApplicationWrites(t)
			report := f.submitWait(t, 128)
			assert.True(t, report.Passing)
			assert.Equal(t, "checked", report.Freshness)
			assert.True(t, report.Complete)
			assert.Zero(t, report.PendingSources)
			assert.Positive(t, report.Members.Matched)
			assert.Zero(t, report.Members.Mismatched)
			historical := f.CommandReport(t, "--runtime-target", "runtime", "--run-id", f.request.RunID, "--status", "--json")
			assert.False(t, historical.Passing)
			assert.Equal(t, "historical_evidence", historical.Code)
			unchanged()
		})
	}
}
func TestMigrationParityAcceptanceMismatchAndLegacy(t *testing.T) {
	for _, scenario := range []string{"mismatch", "legacy", "ambiguous legacy", "unsupported"} {
		t.Run(scenario, func(t *testing.T) {
			f := newParityCommandFixture(t)
			switch scenario {
			case "mismatch":
				f.ChangeBaselineToolEvent(t)
			case "legacy", "ambiguous legacy":
				sourceID := "missing-original"
				if scenario == "ambiguous legacy" {
					sourceID = "codex:" + rawtest.CodexToolsID
					var aliases int
					require.NoError(t, f.baselineDB.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_session_public_aliases WHERE alias_id=$1`, sourceID).Scan(&aliases))
					require.Equal(t, 1, aliases, "legacy fixture must collide with an existing public alias")
				}
				_, err := f.baselineDB.ExecContext(t.Context(), `INSERT INTO sessions SELECT (jsonb_populate_record(NULL::sessions,to_jsonb(s)||jsonb_build_object('id','legacy-acceptance','source_session_id',$1::text,'provenance_kind','legacy'))).* FROM sessions s LIMIT 1`, sourceID)
				require.NoError(t, err)
			case "unsupported":
				// An otherwise valid physical baseline from another preparation version is
				// unavailable evidence, never a fabricated semantic match.
				_, err := f.baselineDB.ExecContext(t.Context(), `UPDATE sessions SET data_version=0`)
				require.NoError(t, err)
			}
			unchanged := f.assertNoApplicationWrites(t)
			report := f.submitWait(t, 128)
			assert.False(t, report.Passing)
			switch scenario {
			case "mismatch":
				assert.EqualValues(t, 1, report.Members.Mismatched)
			case "legacy":
				assert.EqualValues(t, 1, report.Members.LegacyOnly)
			case "ambiguous legacy":
				assert.EqualValues(t, 1, report.Members.Ambiguous)
			case "unsupported":
				assert.EqualValues(t, 1, report.Members.PartialUnsupported)
			}
			unchanged()
		})
	}
}
func TestMigrationParityAcceptanceMissingCustodyAndParseFailureAreReadOnly(t *testing.T) {
	for _, scenario := range []string{"missing custody", "parse failure"} {
		t.Run(scenario, func(t *testing.T) {
			var f *parityCommandFixture
			if scenario == "missing custody" {
				f = newParityCommandFixture(t)
				quarantineDirectParityObject(t, f.custody, f.capture.identity.TenantID, f.generations[0].manifest.Entries[0].Objects[0])
			} else {
				f = newParityCommandProviderFixture(t, parser.AgentEvener, rawtest.EvenerParity)
				path := filepath.Join(f.sourceRoot, "sessions", "primary.transcript.jsonl")
				file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				require.NoError(t, err)
				_, err = file.WriteString("{bad}\n")
				require.NoError(t, err)
				require.NoError(t, file.Close())
				f.capture.captureAll(t)
			}
			unchanged := f.assertNoApplicationWrites(t)
			report := f.submitWait(t, 128)
			assert.False(t, report.Passing)
			assert.EqualValues(t, 1, report.Sources.PartialUnsupported)
			var code string
			require.NoError(t, f.runtimeDB.QueryRowContext(t.Context(), `SELECT code FROM migration_parity_sources WHERE run_id=$1 LIMIT 1`, f.request.RunID).Scan(&code))
			if scenario == "parse failure" {
				assert.Equal(t, "parse_failed", code)
			} else {
				assert.Equal(t, "missing_object", code)
			}
			unchanged()
		})
	}
}

type parityCancellationMaterializer struct {
	rawderive.SourceMaterializer
	ready chan struct{}
}

func (m parityCancellationMaterializer) Materialize(ctx context.Context, manifest rawsync.CanonicalManifest) (*rawderive.Materialization, error) {
	materialized, err := m.SourceMaterializer.Materialize(ctx, manifest)
	if err != nil {
		return nil, err
	}
	close(m.ready)
	<-ctx.Done()
	// The worker owns cleanup even when cancellation arrives after acquisition.
	return materialized, nil
}
func TestMigrationParityAcceptanceCancellationPreservesApplication(t *testing.T) {
	f := newParityCommandFixture(t)
	unchanged := f.assertNoApplicationWrites(t)
	requested := f.CommandReport(t, f.submitArgs(1, "0")...)
	assert.False(t, requested.Passing)
	ready := make(chan struct{})
	f.owner.materializer = parityCancellationMaterializer{SourceMaterializer: f.owner.materializer, ready: ready}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.owner.runOne(ctx) }()
	select {
	case <-ready:
	case err := <-done:
		require.NoError(t, err)
		t.Fatal("owner stopped before retained materialization")
	case <-time.After(20 * time.Second):
		cancel()
		<-done
		t.Fatal("owner did not reach materialization")
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	report := f.CommandReport(t, "--runtime-target", "runtime", "--run-id", f.request.RunID, "--status", "--json")
	assert.False(t, report.Passing)
	assert.False(t, report.Complete)
	unchanged()
}
func TestMigrationParityAcceptanceBatchResumeUsesPersistedClock(t *testing.T) {
	f := newParityCommandProviderFixture(t, parser.AgentCodex, rawtest.ParityFixtures()[parser.AgentCodex])
	unchanged := f.assertNoApplicationWrites(t)
	first := f.submitWait(t, 1)
	assert.False(t, first.Passing)
	assert.False(t, first.Complete)
	assert.EqualValues(t, 2, first.PendingSources)
	var observed time.Time
	require.NoError(t, f.runtimeDB.QueryRowContext(t.Context(), `SELECT observed_at FROM migration_parity_runs WHERE run_id=$1`, f.request.RunID).Scan(&observed))
	assert.Zero(t, observed.Nanosecond()%1000)
	require.NoError(t, f.owner.parser.Close())
	restarted, _, _ := configureDirectParityOwner(t, f.runtimeCfg, f.baselineCfg, f.runtimeDB, f.custody)
	f.owner = restarted
	second := f.submitWait(t, 1)
	assert.False(t, second.Passing)
	assert.EqualValues(t, 1, second.PendingSources)
	assert.Equal(t, first.RequestGeneration+1, second.RequestGeneration)
	third := f.submitWait(t, 1)
	assert.True(t, third.Passing)
	assert.EqualValues(t, 3, third.Members.Matched)
	assert.Zero(t, third.PendingSources)
	var resumed time.Time
	require.NoError(t, f.runtimeDB.QueryRowContext(t.Context(), `SELECT observed_at FROM migration_parity_runs WHERE run_id=$1`, f.request.RunID).Scan(&resumed))
	assert.True(t, observed.Equal(resumed), "restart changed persisted observation clock")
	unchanged()
}

func TestMigrationParityAcceptanceHeadAndExclusionChangesBecomeStale(t *testing.T) {
	for _, change := range []string{"head", "exclusion"} {
		t.Run(change, func(t *testing.T) {
			f := newParityCommandFixture(t)
			first := f.submitWait(t, 1)
			require.True(t, first.Passing)
			if change == "head" {
				path := filepath.Join(f.sourceRoot, "2026", "07", "06", "rollout-2026-07-06T12-00-00-"+rawtest.CodexToolsID+".jsonl")
				file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				require.NoError(t, err)
				_, err = file.WriteString(`{"type":"response_item","timestamp":"2026-07-06T12:01:00Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Later observation."}]}}
`)
				require.NoError(t, err)
				require.NoError(t, file.Close())
				f.capture.captureAll(t)
			} else {
				var group string
				require.NoError(t, f.baselineDB.QueryRowContext(t.Context(), `SELECT group_id FROM raw_session_public_aliases WHERE alias_id=$1`, "codex:"+rawtest.CodexToolsID).Scan(&group))
				_, err := f.baselineDB.ExecContext(t.Context(), `INSERT INTO raw_curation(group_id,branch_id,field,value) VALUES($1,'','excluded','true') ON CONFLICT(group_id,branch_id,field) DO UPDATE SET value='true'`, group)
				require.NoError(t, err)
			}
			unchanged := f.assertNoApplicationWrites(t)
			later := f.submitWait(t, 1)
			assert.False(t, later.Passing)
			assert.Equal(t, "stale", later.Freshness)
			assert.EqualValues(t, 1, later.Sources.Stale)
			if change == "head" {
				// A new retained head invalidates source reconstruction; the physical
				// baseline member has not changed and keeps its historical match.
				assert.Zero(t, later.Members.Stale)
				assert.EqualValues(t, 1, later.Members.Matched)
				var changedHeads, changedMembers, observedMembers int
				require.NoError(t, f.runtimeDB.QueryRowContext(t.Context(), `SELECT
 count(*) FILTER(WHERE kind='head' AND observed IS DISTINCT FROM expected),
 count(*) FILTER(WHERE kind='member' AND observed IS DISTINCT FROM expected),
 count(*) FILTER(WHERE kind='member' AND observed=expected)
 FROM migration_parity_dependencies WHERE run_id=$1 AND validation_epoch=(SELECT validation_epoch FROM migration_parity_runs WHERE run_id=$1)`, f.request.RunID).Scan(&changedHeads, &changedMembers, &observedMembers))
				assert.Equal(t, 1, changedHeads)
				assert.Zero(t, changedMembers)
				assert.Equal(t, 1, observedMembers)
			} else {
				assert.EqualValues(t, 1, later.Members.Stale)
			}
			unchanged()
		})
	}
}
