package parser

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/usagefacts"
)

func requireRecordedCost(t *testing.T, msg ParsedMessage, want int64) {
	t.Helper()
	cost, ok := msg.RecordedCost()
	require.True(t, ok, "recorded cost missing from %s", msg.TokenUsage)
	assert.Equal(t, money.Money{Microdollars: want}, cost)
}

func TestPiKeepsRecordedCost(t *testing.T) {
	header := `{"type":"session","id":"rc-sess","timestamp":"2025-01-01T10:00:00Z","cwd":"/tmp"}` + "\n"
	priced := `{"type":"message","id":"a1","timestamp":"2025-01-01T10:00:01Z","message":{"role":"assistant","content":"one","model":"claude-opus-4-5","usage":{"input":100,"output":50,"cacheRead":10,"cacheWrite":0,"cost":{"input":0.0005,"output":0.00125,"cacheRead":0.000005,"cacheWrite":0,"total":0.001755}}}}` + "\n"
	custom := `{"type":"message","id":"a2","timestamp":"2025-01-01T10:00:02Z","message":{"role":"assistant","content":"two","model":"my-local-model","usage":{"input":10,"output":5,"cost":{"input":0,"output":0,"total":0}}}}` + "\n"
	noCost := `{"type":"message","id":"a3","timestamp":"2025-01-01T10:00:03Z","message":{"role":"assistant","content":"three","model":"claude-opus-4-5","usage":{"input":1,"output":1}}}`

	_, msgs := runPiParserTest(t, header+priced+custom+noCost)
	require.Len(t, msgs, 3)
	requireRecordedCost(t, msgs[0], 1_755)
	requireRecordedCost(t, msgs[1], 0)
	_, ok := msgs[2].RecordedCost()
	assert.False(t, ok, "a message without usage.cost records no cost")
	// The recorded cost leaves the token counters intact.
	usage := usagefacts.ParseTokenUsage(string(msgs[0].TokenUsage))
	assert.Equal(t, int64(100), usage.InputTokens)
	assert.Equal(t, int64(50), usage.OutputTokens)
	assert.Equal(t, int64(10), usage.CacheReadTokens)
}

func TestOpenClawKeepsRecordedCost(t *testing.T) {
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"rc-1","timestamp":"2026-04-30T12:00:00Z","cwd":"/home/user/proj"}`,
		`{"type":"message","id":"u1","timestamp":"2026-04-30T12:00:01Z","message":{"role":"user","content":[{"type":"text","text":"do a thing"}],"timestamp":"2026-04-30T12:00:01Z"}}`,
		`{"type":"message","id":"a1","timestamp":"2026-04-30T12:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"timestamp":"2026-04-30T12:00:02Z","provider":"openrouter","model":"vendor/some-model","usage":{"input":3,"output":91,"cacheRead":0,"cacheWrite":9612,"cost":{"input":0.000009,"output":0.001365,"cacheRead":0,"cacheWrite":0.036045,"total":0.042,"totalOrigin":"provider-billed"}}}}`,
	)
	_, msgs, err := parseOpenClawSessionForTest(t, path, "", "test")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	requireRecordedCost(t, msgs[1], 42_000)
	assert.Equal(t, int64(9612),
		usagefacts.ParseTokenUsage(string(msgs[1].TokenUsage)).CacheCreationTokens)
}

func TestShelleyKeepsGatewayCost(t *testing.T) {
	var msg ParsedMessage
	applyShelleyUsage(&msg,
		`{"input_tokens":12,"cache_creation_input_tokens":0,"cache_read_input_tokens":3,`+
			`"output_tokens":7,"cost_usd":0.0123456,"model":"claude-sonnet-4-6"}`, "")
	requireRecordedCost(t, msg, 12_346)
	usage := usagefacts.ParseTokenUsage(string(msg.TokenUsage))
	assert.Equal(t, int64(12), usage.InputTokens)
	assert.Equal(t, int64(7), usage.OutputTokens)
	assert.Equal(t, "claude-sonnet-4-6", msg.Model)
}

func TestFxKeepsGatewayCostOnlyWhenBillingIsComplete(t *testing.T) {
	tests := []struct {
		billing  string
		wantCost *int64
	}{
		{"complete", new(int64(1_500_000))},
		{"pending", nil},
		{"incomplete", nil},
		{"", nil},
	}
	for _, tt := range tests {
		t.Run("billing="+tt.billing, func(t *testing.T) {
			dir := t.TempDir()
			usage := `{"schema_version":1,"session_id":"sess","snapshot":{"schema_version":3,"billing":"` +
				tt.billing + `","total_cost":1.5,"models":[{"model":"openai/gpt-5","total_cost":1.5,` +
				`"input_tokens":1000,"output_tokens":100,"cache_read_tokens":200,"cache_write_tokens":0}]}}`
			require.NoError(t, os.WriteFile(filepath.Join(dir, "usage-v2.json"), []byte(usage), 0o644))

			events := fxUsageEvents(dir, "sess", "fx:sess", time.Time{}, time.Time{})
			require.Len(t, events, 1)
			event := events[0]
			assert.Equal(t, 800, event.InputTokens, "fresh input excludes cache reads")
			if tt.wantCost == nil {
				assert.Nil(t, event.Cost)
				assert.Empty(t, event.CostStatus)
				return
			}
			require.NotNil(t, event.Cost)
			assert.Equal(t, *tt.wantCost, event.Cost.Microdollars)
			assert.Equal(t, usagefacts.CostStatusRecorded, event.CostStatus)
		})
	}
}

func TestOpenCodeKeepsStepFinishCosts(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(root, "storage", "session", "global", "ses_rc.json")
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id": "ses_rc", "directory": "/home/user/code/myapp", "title": "Cost Session",
		"time": map[string]any{"created": 1700000000000, "updated": 1700000060000},
	})
	// The message cost includes 0.5 of child subagent cost that Kilo rolls
	// into the parent; only the two step-finish costs belong to this message.
	writeOpenCodeStorageFile(t, filepath.Join(root, "storage", "message", "ses_rc", "msg_1.json"), map[string]any{
		"id": "msg_1", "sessionID": "ses_rc", "role": "assistant", "modelID": "gpt-5.2-codex",
		"cost": 0.53, "time": map[string]any{"created": 1700000000000},
	})
	parts := []map[string]any{
		{"id": "prt_1", "type": "text", "text": "reply"},
		{
			"id": "prt_2", "type": "step-finish", "cost": 0.01,
			"tokens": map[string]any{"input": 11, "output": 7, "cache": map[string]any{"read": 3, "write": 2}},
		},
		{
			"id": "prt_3", "type": "step-finish", "cost": 0.02,
			"tokens": map[string]any{"input": 13, "output": 5, "cache": map[string]any{"read": 4, "write": 0}},
		},
	}
	for i, part := range parts {
		part["sessionID"], part["messageID"] = "ses_rc", "msg_1"
		part["time"] = map[string]any{"created": 1700000000000 + int64(i)}
		writeOpenCodeStorageFile(t, filepath.Join(
			root, "storage", "part", "msg_1", part["id"].(string)+".json"), part)
	}

	_, msgs, err := parseOpenCodeStorageFile(sessionPath, "testmachine")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	requireRecordedCost(t, msgs[0], 30_000)
}

func TestQClawKeepsRecordedCost(t *testing.T) {
	path, _ := writeQClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"rc-q","timestamp":"2026-04-30T12:00:00Z","cwd":"/home/user/proj"}`,
		`{"type":"message","id":"u1","timestamp":"2026-04-30T12:00:01Z","message":{"role":"user","content":[{"type":"text","text":"do a thing"}],"timestamp":"2026-04-30T12:00:01Z"}}`,
		`{"type":"message","id":"a1","timestamp":"2026-04-30T12:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"timestamp":"2026-04-30T12:00:02Z","model":"vendor/some-model","usage":{"input":3,"output":91,"cost":{"total":0.0042}}}}`,
	)
	_, msgs, err := parseQClawSessionForTest(t, path, "", "test")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	requireRecordedCost(t, msgs[1], 4_200)
}

func TestShelleyKeepsChargedRowWithoutTokens(t *testing.T) {
	_, dbPath, db := newShelleyTestDB(t)
	seedShelleyConversation(
		t, db, "cRC1", "charged", "/home/user/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:40Z",
	)
	seedShelleyMessage(t, db, "cRC1", 1, 1, "user",
		`{"Role":0,"Content":[{"Type":2,"Text":"go"}]}`,
		"", "", "2026-06-15T10:00:00Z")
	seedShelleyMessage(t, db, "cRC1", 2, 1, "error", `{not json`, "",
		`{"input_tokens":0,"output_tokens":0,"cost_usd":0.25,"model":"custom-model"}`,
		"2026-06-15T10:00:40Z")
	seedShelleyMessage(t, db, "cRC1", 3, 1, "error", `{not json`, "",
		`{"input_tokens":0,"output_tokens":0,"cost_usd":0,"model":"custom-model"}`,
		"2026-06-15T10:00:41Z")
	info, err := os.Stat(dbPath)
	require.NoError(t, err)
	result, err := parseShelleyConversationDirectForTest(t, dbPath, "cRC1", "m", info)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Len(t, result.Messages, 2, "the charged row is kept; the all-zero row is not")
	charged := result.Messages[1]
	requireRecordedCost(t, charged, 250_000)
	assert.Equal(t, "custom-model", charged.Model)
	assert.False(t, charged.HasContextTokens, "no token counts are known")
	assert.False(t, charged.HasOutputTokens, "no token counts are known")
}
