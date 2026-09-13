package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
)

const parityTestRunID = "00000000-0000-4000-8000-000000000051"

func paritySubmitArgs() []string {
	return []string{
		"--runtime-target", "runtime",
		"--baseline-target", "baseline",
		"--run-id", parityTestRunID,
		"--device", "device-a",
		"--provider", "claude",
		"--root", "root-a",
	}
}

func executeParityCommand(t *testing.T, args ...string) (pgMigrationParityOptions, string, error) {
	t.Helper()
	var captured pgMigrationParityOptions
	format := ""
	cmd := newPGMigrationParityCommandWithRunner(func(_ context.Context, _ io.Writer, options pgMigrationParityOptions, selectedFormat string) error {
		captured = options
		format = selectedFormat
		return nil
	})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs(args)
	return captured, format, cmd.Execute()
}

func TestMigrationParityCommandContract(t *testing.T) {
	migration := newPGMigrationCommand()
	parity, _, err := migration.Find([]string{"parity"})
	require.NoError(t, err)
	assert.Equal(t, "parity", parity.Use)
	assert.Equal(t, "32", parity.Flag("batch-size").DefValue)
	assert.Equal(t, "0s", parity.Flag("wait").DefValue)
	assert.Equal(t, "false", parity.Flag("status").DefValue)
	assert.Equal(t, "0", parity.Flag("request-generation").DefValue)
	assert.NotNil(t, parity.Flag("format"))
	assert.NotNil(t, parity.Flag("json"))
}

func TestMigrationParityRequiresExplicitSelectors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "runtime", args: []string{"--baseline-target", "baseline", "--run-id", parityTestRunID, "--device", "device-a", "--provider", "claude", "--root", "root-a"}, want: "--runtime-target is required"},
		{name: "baseline", args: []string{"--runtime-target", "runtime", "--run-id", parityTestRunID, "--device", "device-a", "--provider", "claude", "--root", "root-a"}, want: "--baseline-target is required"},
		{name: "run", args: []string{"--runtime-target", "runtime", "--baseline-target", "baseline", "--device", "device-a", "--provider", "claude", "--root", "root-a"}, want: "--run-id is required"},
		{name: "device", args: []string{"--runtime-target", "runtime", "--baseline-target", "baseline", "--run-id", parityTestRunID, "--provider", "claude", "--root", "root-a"}, want: "--device is required"},
		{name: "provider", args: []string{"--runtime-target", "runtime", "--baseline-target", "baseline", "--run-id", parityTestRunID, "--device", "device-a", "--root", "root-a"}, want: "--provider is required"},
		{name: "root", args: []string{"--runtime-target", "runtime", "--baseline-target", "baseline", "--run-id", parityTestRunID, "--device", "device-a", "--provider", "claude"}, want: "--root is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := executeParityCommand(t, tt.args...)
			require.EqualError(t, err, tt.want)
		})
	}
}

func TestMigrationParityValidatesCanonicalSelectors(t *testing.T) {
	tests := []struct {
		name  string
		flag  string
		value string
		want  string
	}{
		{name: "uppercase run UUID", flag: "--run-id", value: "00000000-0000-4000-8000-00000000005A", want: "--run-id must be a canonical lowercase UUID"},
		{name: "device path", flag: "--device", value: "device/a", want: "--device is not a safe opaque identifier"},
		{name: "root path", flag: "--root", value: `root\a`, want: "--root is not a safe opaque identifier"},
		{name: "unknown provider", flag: "--provider", value: "unknown-agent", want: "--provider must name a raw-sync provider"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := paritySubmitArgs()
			for i := range args {
				if args[i] == tt.flag {
					args[i+1] = tt.value
				}
			}
			_, _, err := executeParityCommand(t, args...)
			require.EqualError(t, err, tt.want)
		})
	}
}

func TestMigrationParityBatchBounds(t *testing.T) {
	for _, tt := range []struct {
		value string
		valid bool
	}{
		{value: "0"},
		{value: "1", valid: true},
		{value: "32", valid: true},
		{value: "128", valid: true},
		{value: "129"},
	} {
		t.Run(tt.value, func(t *testing.T) {
			args := append(paritySubmitArgs(), "--batch-size", tt.value)
			options, _, err := executeParityCommand(t, args...)
			if tt.valid {
				require.NoError(t, err)
				assert.Equal(t, tt.value, strconv.Itoa(options.BatchSize))
				return
			}
			require.EqualError(t, err, "--batch-size must be between 1 and 128")
		})
	}
}

func TestMigrationParityWaitBounds(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  time.Duration
	}{
		{value: "-1s"},
		{value: "0s", want: 0},
		{value: "60s", want: 60 * time.Second},
		{value: "61s"},
	} {
		t.Run(tt.value, func(t *testing.T) {
			args := append(paritySubmitArgs(), "--wait="+tt.value)
			options, _, err := executeParityCommand(t, args...)
			if tt.value == "0s" || tt.want > 0 {
				require.NoError(t, err)
				assert.Equal(t, tt.want, options.Wait)
				return
			}
			require.EqualError(t, err, "--wait must be between 0s and 60s")
		})
	}
}

func TestMigrationParityStatusSelectors(t *testing.T) {
	base := []string{"--runtime-target", "runtime", "--run-id", parityTestRunID, "--status"}
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "baseline", args: []string{"--baseline-target", "baseline"}, want: "--baseline-target cannot be used with --status"},
		{name: "device", args: []string{"--device", "device-a"}, want: "--device cannot be used with --status"},
		{name: "provider", args: []string{"--provider", "claude"}, want: "--provider cannot be used with --status"},
		{name: "root", args: []string{"--root", "root-a"}, want: "--root cannot be used with --status"},
		{name: "batch", args: []string{"--batch-size", "32"}, want: "--batch-size cannot be used with --status"},
		{name: "wait", args: []string{"--wait", "0s"}, want: "--wait cannot be used with --status"},
		{name: "zero generation", args: []string{"--request-generation", "0"}, want: "--request-generation must be a positive integer"},
		{name: "negative generation", args: []string{"--request-generation=-1"}, want: "--request-generation must be a positive integer"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := executeParityCommand(t, append(base, tt.args...)...)
			require.EqualError(t, err, tt.want)
		})
	}

	options, _, err := executeParityCommand(t, append(base, "--request-generation", "7")...)
	require.NoError(t, err)
	assert.True(t, options.Status)
	assert.Equal(t, int64(7), options.RequestGeneration)
}

func TestMigrationParityRequestGenerationRequiresStatus(t *testing.T) {
	_, _, err := executeParityCommand(t, append(paritySubmitArgs(), "--request-generation", "1")...)
	require.EqualError(t, err, "--request-generation requires --status")
}

func TestMigrationParityOutputFlags(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{args: []string{"--json"}, want: "json"},
		{args: []string{"--format", "json"}, want: "json"},
		{args: []string{"--format", "human"}, want: "human"},
	} {
		_, format, err := executeParityCommand(t, append(paritySubmitArgs(), tt.args...)...)
		require.NoError(t, err)
		assert.Equal(t, tt.want, format)
	}
}

func TestFormatParityReportUsesFixedSafeJSONShape(t *testing.T) {
	baseline := time.Date(2026, 9, 12, 1, 2, 3, 4000, time.UTC)
	runtime := time.Date(2026, 9, 12, 1, 3, 4, 5000, time.UTC)
	report := rawderive.ParityReport{
		RunID: parityTestRunID, State: "complete", Code: "pending",
		Members:        rawderive.ParityCounts{Matched: 1, Mismatched: 2, Ambiguous: 3, LegacyOnly: 4, Missing: 5, PartialUnsupported: 6, Stale: 7},
		Sources:        rawderive.ParityCounts{Matched: 8, Mismatched: 9, Ambiguous: 10, LegacyOnly: 11, Missing: 12, PartialUnsupported: 13, Stale: 14},
		PendingSources: 15, BaselineSealed: true, Complete: true, Freshness: "historical_checked", Passing: true,
		RequestGeneration: 16, CompletedGeneration: 16, BaselineObservedAt: &baseline, RuntimeObservedAt: &runtime,
	}
	var output bytes.Buffer
	require.NoError(t, formatParityReport(&output, "json", report))
	assert.JSONEq(t, `{
		"run_id":"00000000-0000-4000-8000-000000000051",
		"state":"complete",
		"code":"pending",
		"members":{"matched":1,"mismatched":2,"ambiguous":3,"legacy_only":4,"missing":5,"partial_unsupported":6,"stale":7},
		"sources":{"matched":8,"mismatched":9,"ambiguous":10,"legacy_only":11,"missing":12,"partial_unsupported":13,"stale":14},
		"pending_sources":15,
		"baseline_sealed":true,
		"complete":true,
		"freshness":"historical_checked",
		"passing":true,
		"request_generation":16,
		"completed_generation":16,
		"baseline_observed_at":"2026-09-12T01:02:03.000004Z",
		"runtime_observed_at":"2026-09-12T01:03:04.000005Z"
	}`, output.String())

	var fields map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &fields))
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{"baseline_observed_at", "baseline_sealed", "code", "complete", "completed_generation", "freshness", "members", "passing", "pending_sources", "request_generation", "run_id", "runtime_observed_at", "sources", "state"}, keys)
	for _, forbidden := range []string{"tenant", "device", "root", "host", "schema", "profile", "hash", "transcript", "path", "parser_error"} {
		assert.NotContains(t, output.String(), forbidden)
	}
}

func TestFormatParityReportHumanUsesSameSafeFields(t *testing.T) {
	report := rawderive.ParityReport{RunID: parityTestRunID, State: "requested", Code: "pending", Freshness: "unchecked", RequestGeneration: 2}
	var output bytes.Buffer
	require.NoError(t, formatParityReport(&output, "human", report))
	assert.Contains(t, output.String(), "Run: "+parityTestRunID)
	assert.Contains(t, output.String(), "Members: matched=0 mismatched=0 ambiguous=0 legacy_only=0 missing=0 partial_unsupported=0 stale=0")
	assert.Contains(t, output.String(), "Baseline observed at: unavailable")
	assert.Contains(t, output.String(), "Passing: false")
	for _, forbidden := range []string{"tenant", "device", "root", "host", "schema", "profile", "hash", "transcript", "path", "parser error"} {
		assert.NotContains(t, output.String(), forbidden)
	}
}

type parityCommandStoreFixture struct {
	created        []rawderive.ParityRequest
	createBatches  []int
	createReport   rawderive.ParityReport
	createErr      error
	ordinaryReport rawderive.ParityReport
	requestReports []rawderive.ParityReport
	requestReads   []int64
	readDeadlines  []time.Duration
}

func (s *parityCommandStoreFixture) CreateOrResumeParity(ctx context.Context, request rawderive.ParityRequest, batch int) (rawderive.ParityReport, error) {
	s.captureDeadline(ctx)
	s.created = append(s.created, request)
	s.createBatches = append(s.createBatches, batch)
	return s.createReport, s.createErr
}

func (s *parityCommandStoreFixture) ReadParityReport(ctx context.Context, _ string) (rawderive.ParityReport, error) {
	s.captureDeadline(ctx)
	return s.ordinaryReport, nil
}

func (s *parityCommandStoreFixture) ReadParityRequestReport(ctx context.Context, _ string, generation int64) (rawderive.ParityReport, error) {
	s.captureDeadline(ctx)
	s.requestReads = append(s.requestReads, generation)
	if len(s.requestReports) == 0 {
		return rawderive.ParityReport{}, errors.New("unexpected request report read")
	}
	report := s.requestReports[0]
	if len(s.requestReports) > 1 {
		s.requestReports = s.requestReports[1:]
	}
	return report, nil
}

func (s *parityCommandStoreFixture) captureDeadline(ctx context.Context) {
	deadline, ok := ctx.Deadline()
	if !ok {
		s.readDeadlines = append(s.readDeadlines, 0)
		return
	}
	s.readDeadlines = append(s.readDeadlines, time.Until(deadline))
}

func parityCommandConfig() config.Config {
	const targetID = "00000000-0000-4000-8000-000000000052"
	return config.Config{
		RequireAuth: true,
		DefaultPG:   "runtime",
		PGTargets: map[string]config.PGConfig{
			"runtime": {
				URL: "postgres://runtime.invalid/db", Schema: "runtime_schema", RawTenant: "tenant-a", ParityEnabled: true,
				ParityBaselines: map[string]config.PGParityBaseline{"baseline": {Target: "baseline", Identity: targetID}},
			},
			"baseline": {URL: "postgres://baseline.invalid/db", Schema: "baseline_schema", RawTenant: "tenant-a"},
		},
	}
}

func parityCommandDeps(store pgMigrationParityStore) pgMigrationParityDeps {
	return pgMigrationParityDeps{
		loadConfig: func() (config.Config, error) { return parityCommandConfig(), nil },
		openRuntime: func(_ context.Context, pg config.PGConfig) (pgMigrationParityStore, string, func(), error) {
			return store, "00000000-0000-4000-8000-000000000053", func() {}, nil
		},
		pollInterval: time.Millisecond,
	}
}

func parityRunOptions() pgMigrationParityOptions {
	return pgMigrationParityOptions{RuntimeTarget: "runtime", BaselineTarget: "baseline", RunID: parityTestRunID, Device: "device-a", Provider: parser.AgentClaude, Root: "root-a", BatchSize: 32}
}

func TestMigrationParityRunPollsOnlySubmittedGeneration(t *testing.T) {
	store := &parityCommandStoreFixture{
		createReport: rawderive.ParityReport{RunID: parityTestRunID, State: "requested", Code: "pending", RequestGeneration: 4},
		requestReports: []rawderive.ParityReport{
			{RunID: parityTestRunID, State: "running", Code: "pending", RequestGeneration: 4},
			{RunID: parityTestRunID, State: "complete", Code: "pending", RequestGeneration: 4, CompletedGeneration: 4, BaselineSealed: true, Complete: true, Freshness: "historical_checked", Passing: true, Members: rawderive.ParityCounts{Matched: 1}, Sources: rawderive.ParityCounts{Matched: 1}},
		},
	}
	options := parityRunOptions()
	options.Wait = 50 * time.Millisecond
	var output bytes.Buffer
	require.NoError(t, runPGMigrationParityWithDeps(t.Context(), &output, options, "json", parityCommandDeps(store)))
	assert.Equal(t, []int64{4, 4}, store.requestReads)
	require.Len(t, store.created, 1)
	assert.Equal(t, rawderive.ParityRequest{RunID: parityTestRunID, RuntimeID: "00000000-0000-4000-8000-000000000053", BaselineProfile: "baseline", Cohort: rawderive.ParityCohort{DeviceID: "device-a", Provider: parser.AgentClaude, RootID: "root-a"}}, store.created[0])
	assert.Equal(t, []int{32}, store.createBatches)
	assert.Contains(t, output.String(), `"freshness":"checked"`)
	assert.Contains(t, output.String(), `"passing":true`)
	for _, remaining := range store.readDeadlines {
		assert.Greater(t, remaining, time.Duration(0))
		assert.LessOrEqual(t, remaining, 10*time.Second)
	}
}

func TestMigrationParityWaitZeroDoesNotPoll(t *testing.T) {
	store := &parityCommandStoreFixture{createReport: rawderive.ParityReport{RunID: parityTestRunID, State: "requested", Code: "pending", RequestGeneration: 2}}
	var output bytes.Buffer
	require.NoError(t, runPGMigrationParityWithDeps(t.Context(), &output, parityRunOptions(), "json", parityCommandDeps(store)))
	assert.Empty(t, store.requestReads)
	assert.Contains(t, output.String(), `"passing":false`)
	assert.Contains(t, output.String(), `"baseline_observed_at":null`)
	assert.Contains(t, output.String(), `"runtime_observed_at":null`)
}

func TestMigrationParityBindingConflictDoesNotPollOrRetry(t *testing.T) {
	store := &parityCommandStoreFixture{createErr: errors.New("binding_conflict: original request differs")}
	err := runPGMigrationParityWithDeps(t.Context(), io.Discard, parityRunOptions(), "json", parityCommandDeps(store))
	require.EqualError(t, err, "binding_conflict")
	assert.Len(t, store.created, 1)
	assert.Empty(t, store.requestReads)
}

func TestParityStatusDispatchesOnlyNamedObservation(t *testing.T) {
	historical := rawderive.ParityReport{RunID: parityTestRunID, State: "complete", Code: "pending", RequestGeneration: 3, CompletedGeneration: 3, BaselineSealed: true, Complete: true, Freshness: "historical_checked", Passing: true, Members: rawderive.ParityCounts{Matched: 1}, Sources: rawderive.ParityCounts{Matched: 1}}
	store := &parityCommandStoreFixture{
		ordinaryReport: rawderive.ParityReport{RunID: parityTestRunID, State: "complete", Code: "historical_evidence", RequestGeneration: 3, CompletedGeneration: 3, Freshness: "unchecked"},
		requestReports: []rawderive.ParityReport{historical},
	}
	deps := parityCommandDeps(store)
	options := parityRunOptions()
	options.Status = true
	options.BaselineTarget = ""

	var ordinary bytes.Buffer
	require.NoError(t, runPGMigrationParityWithDeps(t.Context(), &ordinary, options, "json", deps))
	assert.Contains(t, ordinary.String(), `"code":"historical_evidence"`)
	assert.Contains(t, ordinary.String(), `"passing":false`)
	assert.Empty(t, store.requestReads)

	options.RequestGeneration = 3
	var explicit bytes.Buffer
	require.NoError(t, runPGMigrationParityWithDeps(t.Context(), &explicit, options, "json", deps))
	assert.Equal(t, []int64{3}, store.requestReads)
	assert.Contains(t, explicit.String(), `"freshness":"historical_checked"`)
	assert.Contains(t, explicit.String(), `"passing":true`)
}

func TestMigrationParityRuntimeOpenerLogsNoTargetDetails(t *testing.T) {
	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousWriter) })

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	runtime := config.PGConfig{
		URL:           "postgres://synthetic-user:synthetic-password@192.0.2.1:1/synthetic-db?sslmode=disable&application_name=synthetic-app",
		Schema:        "synthetic_schema",
		RawTenant:     "synthetic-tenant",
		AllowInsecure: true,
	}
	_, _, closeRuntime, err := openPGMigrationParityRuntime(ctx, runtime)
	require.Error(t, err)
	assert.Nil(t, closeRuntime)
	assert.Contains(t, logs.String(), "migration parity runtime PostgreSQL connection permits plaintext")
	for _, private := range []string{"192.0.2.1", "synthetic-user", "synthetic-password", "synthetic-db", "synthetic-app"} {
		assert.NotContains(t, logs.String(), private)
	}
}

func TestParityStatusReportsUnavailableOrSupersededGeneration(t *testing.T) {
	for _, generation := range []int64{2, 8} {
		t.Run(strconv.FormatInt(generation, 10), func(t *testing.T) {
			store := &parityCommandStoreFixture{requestReports: []rawderive.ParityReport{{RunID: parityTestRunID, State: "complete", Code: "generation_not_available", Freshness: "unchecked", RequestGeneration: 5, CompletedGeneration: 5}}}
			options := parityRunOptions()
			options.Status = true
			options.BaselineTarget = ""
			options.RequestGeneration = generation
			var output bytes.Buffer
			require.NoError(t, runPGMigrationParityWithDeps(t.Context(), &output, options, "json", parityCommandDeps(store)))
			assert.Contains(t, output.String(), `"code":"generation_not_available"`)
			assert.Contains(t, output.String(), `"passing":false`)
		})
	}
}

func TestMigrationParityUnknownOwnerReturnsPendingWithinWait(t *testing.T) {
	store := &parityCommandStoreFixture{
		createReport:   rawderive.ParityReport{RunID: parityTestRunID, State: "requested", Code: "pending", RequestGeneration: 1},
		requestReports: []rawderive.ParityReport{{RunID: parityTestRunID, State: "requested", Code: "pending", RequestGeneration: 1}},
	}
	options := parityRunOptions()
	options.Wait = 5 * time.Millisecond
	started := time.Now()
	var output bytes.Buffer
	require.NoError(t, runPGMigrationParityWithDeps(t.Context(), &output, options, "json", parityCommandDeps(store)))
	assert.Less(t, time.Since(started), 500*time.Millisecond)
	assert.NotEmpty(t, store.requestReads)
	assert.Contains(t, output.String(), `"state":"requested"`)
	assert.Contains(t, output.String(), `"passing":false`)
}

func TestMigrationParityRejectsUnknownProfileBeforeOpeningRuntime(t *testing.T) {
	opened := false
	deps := parityCommandDeps(&parityCommandStoreFixture{})
	deps.openRuntime = func(context.Context, config.PGConfig) (pgMigrationParityStore, string, func(), error) {
		opened = true
		return nil, "", nil, nil
	}
	options := parityRunOptions()
	options.BaselineTarget = "missing"
	err := runPGMigrationParityWithDeps(t.Context(), io.Discard, options, "json", deps)
	require.ErrorContains(t, err, `baseline profile "missing" is not configured`)
	assert.False(t, opened)
}

func TestMigrationParitySanitizesRuntimeConnectionFailure(t *testing.T) {
	deps := parityCommandDeps(&parityCommandStoreFixture{})
	deps.openRuntime = func(context.Context, config.PGConfig) (pgMigrationParityStore, string, func(), error) {
		return nil, "", nil, errors.New("dial postgres://private-user:private-password@private-host/private-schema?application_name=private-path")
	}
	err := runPGMigrationParityWithDeps(t.Context(), io.Discard, parityRunOptions(), "json", deps)
	require.EqualError(t, err, "opening selected migration parity runtime target failed")
	for _, private := range []string{"private-user", "private-password", "private-host", "private-schema", "private-path"} {
		assert.NotContains(t, err.Error(), private)
	}
}
