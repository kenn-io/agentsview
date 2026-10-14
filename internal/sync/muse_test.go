package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
)

const (
	museSyncParentID = "0199a1b2-6c3d-7e4f-8a5b-000000000001"
	museSyncChildID  = "0199a1b2-6c3d-7e4f-8a5b-000000000002"
)

// copyMuseFixture copies the parser's Muse fixture tree into a writable root.
func copyMuseFixture(t *testing.T) (root, parentLog string) {
	t.Helper()
	root = t.TempDir()
	src := filepath.Join("..", "parser", "testdata", "muse", "sessions")
	require.NoError(t, os.CopyFS(root, os.DirFS(src)))
	parentLog = filepath.Join(root, "2026", "09", "10", museSyncParentID, "session.jsonl")
	return root, parentLog
}

func TestMuseSyncStoresSessionsSubagentsAndUsage(t *testing.T) {
	root, parentLog := copyMuseFixture(t)
	database := openTestDB(t)
	// Both rows share a canonical name, so a bare muse-spark-1.3 prices
	// only through its fixed alias to Meta's row.
	museRates := db.ModelPricing{
		InputPerMTok:     money.MustParseDollars("1.25"),
		OutputPerMTok:    money.MustParseDollars("4.25"),
		CacheReadPerMTok: money.MustParseDollars("0.15"),
	}
	metaRow, openRouterRow := museRates, museRates
	metaRow.ModelPattern = "meta/muse-spark-1.3"
	openRouterRow.ModelPattern = "openrouter/meta/muse-spark-1.3"
	require.NoError(t, database.UpsertModelPricing([]db.ModelPricing{metaRow, openRouterRow}))
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentMuse: {root}},
		Machine:   "local", DisableSignalRecomputation: true,
	})
	t.Cleanup(engine.Close)

	stats := engine.SyncAllForceParse(t.Context(), nil)
	assert.GreaterOrEqual(t, stats.Synced, 2)

	parentID := "muse:" + museSyncParentID
	parent, err := database.GetSessionFull(t.Context(), parentID)
	require.NoError(t, err)
	require.NotNil(t, parent)
	assert.Equal(t, "muse", parent.Agent)
	// Tool results fold into the calls they answer.
	assert.Equal(t, 3, parent.MessageCount)
	assert.Equal(t, 120, parent.TotalOutputTokens)

	child, err := database.GetSessionFull(t.Context(), "muse:"+museSyncChildID)
	require.NoError(t, err)
	require.NotNil(t, child)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, parentID, *child.ParentSessionID)
	assert.Equal(t, "subagent", child.RelationshipType)

	messages, err := database.GetAllMessages(t.Context(), parentID)
	require.NoError(t, err)
	require.Len(t, messages, 3)
	require.Len(t, messages[1].ToolCalls, 2)
	assert.Equal(t, "tool failed: exit status 1", messages[1].ToolCalls[1].ResultContent)
	require.Len(t, messages[1].ToolCalls[1].ResultEvents, 1)
	assert.Equal(t, "errored", messages[1].ToolCalls[1].ResultEvents[0].Status)

	events, err := database.GetUsageEvents(t.Context(), parentID)
	require.NoError(t, err)
	assert.Empty(t, events)

	// Two replies, with cached reads billed once at the cached rate:
	// (200*1.25 + 1000*0.15 + 80*4.25) + (200*1.25 + 1300*0.15 + 40*4.25)
	// = 1355 microdollars.
	usage, err := database.GetSessionUsage(t.Context(), parentID, false)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, money.MustParseDollars("0.001355"), usage.Cost)

	// Later records extend an existing reply and start a new turn. An
	// ordinary sync must rewrite the stored reply, not only append.
	f, err := os.OpenFile(parentLog, os.O_RDWR, 0)
	require.NoError(t, err)
	info, err := f.Stat()
	require.NoError(t, err)
	// Drop the fixture's partial last line before appending whole records.
	require.NoError(t, f.Truncate(info.Size()-int64(len(museFixturePartialTail))))
	_, err = f.Seek(0, 2)
	require.NoError(t, err)
	_, err = f.WriteString(
		`{"schema_version":1,"stream":{"kind":"session","id":"` + museSyncParentID + `"},"sequence":29,"recorded_at":1789000006500000,"payload_type":"runtime.session","payload":{"kind":"run","run_id":"run-1","event":{"kind":"assistant_message_committed","response_id":"resp-2","text":"Tests pass now."}}}` + "\n" +
			`{"schema_version":1,"stream":{"kind":"session","id":"` + museSyncParentID + `"},"sequence":30,"recorded_at":1789000007000000,"payload_type":"runtime.session","payload":{"kind":"run","run_id":"run-2","event":{"kind":"started","prompt":"Now add a regression test"}}}` + "\n",
	)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	engine.SyncAll(t.Context(), nil)
	messages, err = database.GetAllMessages(t.Context(), parentID)
	require.NoError(t, err)
	require.Len(t, messages, 4)
	assert.Equal(t, "The test expected a trimmed username. I fixed the trim.\nTests pass now.",
		messages[2].Content)
	assert.Equal(t, "Now add a regression test", messages[3].Content)
	events, err = database.GetUsageEvents(t.Context(), parentID)
	require.NoError(t, err)
	assert.Empty(t, events)
}

const museFixturePartialTail = `{"schema_version":1,"id":"rec-partial","stream":{"kind":"session"`

// TestMuseSyncMovesEarlyUsageOntoLaterToolCalls covers a live log read
// between a tool-only response's usage and its tool calls, which Muse logs
// in that order. The first sync prices the usage as a standalone event; the
// next moves it onto the reply without changing the cost.
func TestMuseSyncMovesEarlyUsageOntoLaterToolCalls(t *testing.T) {
	const sessionID = "0199a1b2-6c3d-7e4f-8a5b-000000000003"
	root := t.TempDir()
	logPath := filepath.Join(root, "2026", "09", "11", sessionID, "session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o755))
	record := func(sequence int, payloadType, payload string) string {
		return fmt.Sprintf(
			`{"schema_version":1,"stream":{"kind":"session","id":%q},"sequence":%d,"recorded_at":%d,"payload_type":%q,"payload":%s}`+"\n",
			sessionID, sequence, 1789100000000000+int64(sequence)*1000000, payloadType, payload,
		)
	}
	require.NoError(t, os.WriteFile(logPath, []byte(
		record(1, "runtime.session.metadata", `{"record":{"workspace_root":"/work/demo-app","model_id":"muse-spark-1.3"}}`)+
			record(2, "runtime.session", `{"kind":"run","event":{"kind":"started","prompt":"List the files"}}`)+
			record(3, "runtime.session", `{"kind":"run","event":{"kind":"model_response_created","response_id":"resp-a"}}`)+
			record(4, "runtime.session", `{"kind":"run","event":{"kind":"model_completed","model":"muse-spark-1.3","usage":{"input_tokens":1000,"output_tokens":100,"cache_read_tokens":0,"cache_write_tokens":0,"reasoning_tokens":40}}}`),
	), 0o600))

	database := openTestDB(t)
	rates := db.ModelPricing{
		ModelPattern:  "meta/muse-spark-1.3",
		InputPerMTok:  money.MustParseDollars("1.25"),
		OutputPerMTok: money.MustParseDollars("4.25"),
	}
	require.NoError(t, database.UpsertModelPricing([]db.ModelPricing{rates}))
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentMuse: {root}},
		Machine:   "local", DisableSignalRecomputation: true,
	})
	t.Cleanup(engine.Close)
	fullID := "muse:" + sessionID
	// 1000*1.25 + 100*4.25 = 1675 microdollars.
	wantCost := money.MustParseDollars("0.001675")

	engine.SyncAll(t.Context(), nil)
	events, err := database.GetUsageEvents(t.Context(), fullID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, 100, events[0].OutputTokens)
	usage, err := database.GetSessionUsage(t.Context(), fullID, false)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, wantCost, usage.Cost)

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(record(5, "runtime.session",
		`{"kind":"run","event":{"kind":"assistant_tool_calls_committed","response_id":"resp-a","tool_calls":[{"call_id":"call-ls","name":"bash","args":"{\"command\":\"ls\"}"}]}}`))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	engine.SyncAll(t.Context(), nil)
	events, err = database.GetUsageEvents(t.Context(), fullID)
	require.NoError(t, err)
	assert.Empty(t, events)
	messages, err := database.GetAllMessages(t.Context(), fullID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, 100, messages[1].OutputTokens)
	usage, err = database.GetSessionUsage(t.Context(), fullID, false)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, wantCost, usage.Cost)
}
