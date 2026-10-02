package main

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

// These costs are hand-calculated from the pinned LiteLLM standard rates.
// Pi's input excludes cache reads/writes; all three count toward each request's
// long-context threshold, and separate requests must never share that threshold.
func TestEmbeddedGPT6PricingCLI(t *testing.T) {
	for _, tt := range []struct {
		name, model                          string
		input, output, write, read, requests int
		want                                 int64
	}{
		{"astra-base", "gpt-6-astra", 100_000, 10_000, 0, 0, 1, 1_500_000},
		{"sol-base", "gpt-6-sol", 100_000, 10_000, 0, 0, 1, 300_000},
		{"sol61-base", "gpt-6.1-sol", 100_000, 10_000, 0, 0, 1, 300_000},
		{"astra-cache", "gpt-6-astra", 100_000, 10_000, 10_000, 10_000, 1, 1_635_000},
		{"sol-cache", "gpt-6-sol", 100_000, 10_000, 10_000, 10_000, 1, 327_000},
		{"sol61-cache", "gpt-6.1-sol", 100_000, 10_000, 10_000, 10_000, 1, 326_000},
		{"astra-at-threshold", "gpt-6-astra", 262_000, 10_000, 0, 10_000, 1, 3_130_000},
		{"sol-at-threshold", "gpt-6-sol", 262_000, 10_000, 0, 10_000, 1, 626_000},
		{"sol61-at-threshold", "gpt-6.1-sol", 262_000, 10_000, 0, 10_000, 1, 625_000},
		{"astra-cache-crossing", "gpt-6-astra", 262_000, 10_000, 1, 10_000, 1, 6_010_025},
		{"sol-cache-crossing", "gpt-6-sol", 262_000, 10_000, 1, 10_000, 1, 1_202_005},
		{"sol61-cache-crossing", "gpt-6.1-sol", 262_000, 10_000, 1, 10_000, 1, 1_200_005},
		{"astra-above-threshold", "gpt-6-astra", 272_001, 10_000, 0, 0, 1, 6_190_020},
		{"sol-above-threshold", "gpt-6-sol", 272_001, 10_000, 0, 0, 1, 1_238_004},
		{"sol61-above-threshold", "gpt-6.1-sol", 272_001, 10_000, 0, 0, 1, 1_238_004},
		{"astra-separate-requests", "gpt-6-astra", 150_000, 10_000, 0, 0, 2, 4_000_000},
		{"sol-separate-requests", "gpt-6-sol", 150_000, 10_000, 0, 0, 2, 800_000},
		{"sol61-separate-requests", "gpt-6.1-sol", 150_000, 10_000, 0, 0, 2, 800_000},
		{"namespaced-astra", "openai.gpt-6-astra", 100_000, 10_000, 0, 0, 1, 1_650_000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedUnpricedPiSession(t, tt.model, tt.input, tt.output, tt.write, tt.read, tt.requests)
			assertEmbeddedCLIUsage(t, tt.model, tt.want)
		})
	}
}

func TestEmbeddedGPT6PricingCLICustomOverride(t *testing.T) {
	dataDir := seedUnpricedPiSession(t, "gpt-6-astra", 272_001, 10_000, 0, 0, 1)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(`
[custom_model_pricing."gpt-6-astra"]
input_microdollars_per_mtok = 1000000
output_microdollars_per_mtok = 2000000
`), 0o600))
	assertEmbeddedCLIUsage(t, "gpt-6-astra", 292_001)
}

func seedUnpricedPiSession(t *testing.T, model string, input, output, write, read, requests int) string {
	t.Helper()
	dataDir := testDataDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	root := t.TempDir()
	path := filepath.Join(root, "2026-10-01T10-00-00-000Z_fixture.jsonl")
	var content strings.Builder
	content.WriteString(`{"type":"session","version":3,"id":"fixture","timestamp":"2026-10-01T10:00:00Z","cwd":"/tmp/demo"}` + "\n")
	for i := range requests {
		fmt.Fprintf(&content, `{"type":"message","id":"u%d","timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`+"\n", i)
		fmt.Fprintf(&content, `{"type":"message","id":"a%d","parentId":"u%d","timestamp":"2026-10-01T10:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"model":%q,"usage":{"input":%d,"output":%d,"cacheRead":%d,"cacheWrite":%d}}}`+"\n", i, i, model, input, output, read, write)
	}
	require.NoError(t, os.WriteFile(path, []byte(content.String()), 0o600))
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentPi: {root}}, Machine: "local",
	})
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
	engine.Close()
	seeded, err := database.HasModelPricingRows(t.Context())
	require.NoError(t, err)
	require.False(t, seeded, "fixture must reproduce an unseeded archive")
	require.NoError(t, database.Close())
	return dataDir
}

func assertEmbeddedCLIUsage(t *testing.T, model string, want int64) {
	t.Helper()
	stdout, stderr, err := executeExportSessionsCommand(newRootCommand(),
		"export", "sessions", "--all", "--include-one-shot", "--format", "json",
		"--date-from", "2026-09-30", "--date-to", "2026-10-02")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	doc := decodeExportSessionsDocument(t, stdout)
	require.Len(t, doc.Sessions, 1)
	usage := doc.Sessions[0].ModelUsage
	require.NotNil(t, usage)
	assert.True(t, usage.HasCost)
	assert.Equal(t, want, usage.Cost.Microdollars)
	require.Contains(t, usage.ByModel, model, "reported model name must survive pricing")
	assert.Equal(t, want, usage.ByModel[model].Cost.Microdollars)

	cmd := newRootCommand()
	cmd.SetArgs([]string{
		"usage", "daily", "--json", "--offline", "--no-sync",
		"--since", "2026-10-01", "--until", "2026-10-01", "--timezone", "UTC",
	})
	stdout = captureStdout(t, func() { _, err = cmd.ExecuteC() })
	require.NoError(t, err)
	var report db.DailyUsageResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &report))
	assert.Equal(t, want, report.Totals.TotalCost.Microdollars)
}
