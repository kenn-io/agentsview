package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/postgres"
)

func TestLedgerStatusShowsPushState(t *testing.T) {
	dataDir := testDataDir(t)
	writeLedgerTestConfig(t, dataDir, "[ledger]\nenabled = true\nsource = \"host-a\"\n")
	_, err := runLedger(t, "append", "--class", "health")
	require.NoError(t, err)

	database, err := db.Open(t.Context(), filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.SetSyncState(t.Context(), postgres.LedgerPushStatusKeyPrefix+"hub",
		`{"at":"2026-09-22T10:00:00.000000Z","zones":{"default":{"pushed":3,"identical":1,"held_back":2}},`+
			`"failures":[["default","host-a","4","integrity check failed: checksum mismatch for host-a-000004.json"],["ops","host-b","1","x"]]}`))
	require.NoError(t, database.Close())

	out, err := runLedger(t, "status")
	require.NoError(t, err)
	assert.Contains(t, out, "  push [hub] at 2026-09-22T10:00:00.000000Z: 3 pushed, 1 already present, 2 held back, 1 refused\n")
	assert.Contains(t, out, "    refused host-a-000004: integrity check failed: checksum mismatch for host-a-000004.json\n")
	assert.NotContains(t, out, "host-b", "other zones' failures stay with their zone")

	out, err = runLedger(t, "status", "--json")
	require.NoError(t, err)
	var reports []struct {
		Push []struct {
			Target   string      `json:"target"`
			Pushed   int         `json:"pushed"`
			Failures [][4]string `json:"failures"`
		} `json:"push"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &reports))
	require.Len(t, reports, 1)
	require.Len(t, reports[0].Push, 1)
	assert.Equal(t, "hub", reports[0].Push[0].Target)
	assert.Equal(t, 3, reports[0].Push[0].Pushed)
	assert.Equal(t, [][4]string{{"default", "host-a", "4", "integrity check failed: checksum mismatch for host-a-000004.json"}}, reports[0].Push[0].Failures)
}

func writeLedgerTestConfig(t *testing.T, dataDir, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(body), 0o600))
}

func runLedger(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return executeCommand(newRootCommand(), append([]string{"ledger"}, args...)...)
}

func TestNewLedgerCommand_RegistersSubcommands(t *testing.T) {
	root := newRootCommand()
	cmd, _, err := root.Find([]string{"ledger"})
	require.NoError(t, err)
	assert.Equal(t, groupData, cmd.GroupID)
	var names []string
	for _, sub := range cmd.Commands() {
		names = append(names, sub.Name())
	}
	assert.ElementsMatch(t, []string{"append", "status", "verify", "import", "export", "rebuild-index"}, names)
}

func TestLedgerAppendStatusVerify(t *testing.T) {
	dataDir := testDataDir(t)
	writeLedgerTestConfig(t, dataDir, "[ledger]\nenabled = true\nsource = \"host-a\"\n")

	out, err := runLedger(t, "append", "--class", "state-change", "--subsystem", "cli", "--summary", "hello")
	require.NoError(t, err)
	assert.Equal(t, "appended 1 event(s) to default as host-a-000001.json\n", out)
	out, err = runLedger(t, "append", "--class", "health", "--json")
	require.NoError(t, err)
	var appended map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &appended))
	assert.Equal(t, "host-a", appended["source"])
	assert.EqualValues(t, 2, appended["source_seq"])

	out, err = runLedger(t, "status")
	require.NoError(t, err)
	assert.Contains(t, out, "zone default: 2 segment(s), 2 event(s)\n")
	assert.Contains(t, out, "  source host-a: latest seq 2\n")

	out, err = runLedger(t, "status", "--json")
	require.NoError(t, err)
	var status []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &status))
	require.Len(t, status, 1)
	assert.Equal(t, "default", status[0]["zone"])
	assert.EqualValues(t, 2, status[0]["events"])

	out, err = runLedger(t, "verify")
	require.NoError(t, err)
	assert.Equal(t, "verify [default]: 2 newly verified, 0 skipped, 0 failure(s)\n", out)
	out, err = runLedger(t, "verify")
	require.NoError(t, err)
	assert.Equal(t, "verify [default]: 0 newly verified, 2 skipped, 0 failure(s)\n", out)
}

func TestLedgerCommandErrors(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		args    []string
		wantErr string
	}{
		{"disabled_append", "", []string{"append", "--class", "health"}, "the event ledger is off"},
		{"disabled_import", "", []string{"import", "--zone", "default", "--segments", "x"}, "the event ledger is off"},
		{"missing_class", "[ledger]\nenabled = true\n", []string{"append"}, `required flag(s) "class" not set`},
		{"bad_class", "[ledger]\nenabled = true\n", []string{"append", "--class", "review"}, "unknown event class 'review'"},
		{"bad_payload", "[ledger]\nenabled = true\n", []string{"append", "--class", "health", "--payload", "{"}, "--payload is not valid JSON"},
		{"summary_needs_object", "[ledger]\nenabled = true\n", []string{"append", "--class", "health", "--payload", "[1]", "--summary", "x"}, "need --payload to be a JSON object"},
		{"unknown_zone", "[ledger]\nenabled = true\n", []string{"append", "--class", "health", "--zone", "ops"}, `zone "ops" is not configured (configured: default)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := testDataDir(t)
			writeLedgerTestConfig(t, dataDir, tt.config)
			_, err := runLedger(t, tt.args...)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestLedgerImportExportRebuild(t *testing.T) {
	dataDir := testDataDir(t)
	writeLedgerTestConfig(t, dataDir, "[ledger]\nenabled = true\n")
	fixtures := filepath.Join("..", "..", "internal", "ledger", "testdata", "segments")

	out, err := runLedger(t, "import", "--zone", "default", "--segments", fixtures)
	require.ErrorContains(t, err, "ledger import finished with failed segments")
	assert.Contains(t, out, "ledger import [default]: 5 segment(s) and 19 event(s) indexed, 0 skipped, 1 failed\n")
	assert.Contains(t, out, "  fixture-bigseq-000001: projection error: an event's source_seq exceeds i64::MAX")

	out, err = runLedger(t, "import", "--zone", "default", "--segments", fixtures)
	require.Error(t, err, "the refused segment is retried and refused again")
	assert.Contains(t, out, "0 segment(s) and 0 event(s) indexed, 5 skipped, 1 failed")

	exportDir := t.TempDir()
	out, err = runLedger(t, "export", "--zone", "default", "--dir", exportDir)
	require.NoError(t, err)
	assert.Equal(t, "ledger export [default]: 5 written, 0 already identical, 0 failed\n", out)
	entries, err := os.ReadDir(exportDir)
	require.NoError(t, err)
	for _, e := range entries {
		want, err := os.ReadFile(filepath.Join(fixtures, e.Name()))
		require.NoError(t, err)
		got, err := os.ReadFile(filepath.Join(exportDir, e.Name()))
		require.NoError(t, err)
		assert.Equal(t, string(want), string(got), e.Name())
	}

	out, err = runLedger(t, "rebuild-index", "--zone", "default")
	require.NoError(t, err)
	assert.Equal(t, "ledger rebuild-index [default]: 19 event(s) projected from 5 segment(s)\n", out)
}

func TestLedgerImportJob(t *testing.T) {
	importRoot := t.TempDir()
	segments := filepath.Join(importRoot, "segments")
	require.NoError(t, os.MkdirAll(segments, 0o755))
	src := filepath.Join("..", "..", "internal", "ledger", "testdata", "segments", "fixture-a-000001.json")
	raw, err := os.ReadFile(src)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(segments, "fixture-a-000001.json"), raw, 0o644))

	tests := []struct {
		name   string
		ledger config.LedgerConfig
		want   bool
	}{
		{"disabled", config.LedgerConfig{Zones: []config.LedgerZoneConfig{{ID: "default", ImportPath: importRoot}}}, false},
		{"no_import_path", config.LedgerConfig{Enabled: true}, false},
		{"import_path", config.LedgerConfig{Enabled: true, Zones: []config.LedgerZoneConfig{{ID: "default", ImportPath: importRoot}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			job, ok := ledgerImportJob(config.Config{Ledger: tt.ledger}, database, nil)
			require.Equal(t, tt.want, ok)
			if !ok {
				return
			}
			assert.Equal(t, "ledger-import", job.Name)
			require.NoError(t, job.Run(t.Context()))
			st, err := database.LedgerStatus(t.Context(), "default")
			require.NoError(t, err)
			assert.Equal(t, 1, st.Segments)
			state, err := database.GetLedgerImportState(t.Context(), "default", segments)
			require.NoError(t, err)
			require.NotNil(t, state)
			assert.Contains(t, state.LastReportJSON, `"segments_indexed":1`, state.LastReportJSON)
		})
	}
}
