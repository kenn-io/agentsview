package sync

import (
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

	events, err := database.GetUsageEvents(t.Context(), parentID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, 300, events[0].InputTokens)

	// Two replies and the standalone event, with cached reads billed once
	// at the cached rate:
	// (200*1.25 + 1000*0.15 + 80*4.25) + (200*1.25 + 1300*0.15 + 40*4.25)
	// + (300*1.25 + 10*4.25) = 1772.5 microdollars.
	usage, err := database.GetSessionUsage(t.Context(), parentID, false)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, money.MustParseDollars("0.0017725"), usage.Cost)

	// A later response appends to the log; the re-parse replaces the
	// stored transcript.
	f, err := os.OpenFile(parentLog, os.O_RDWR, 0)
	require.NoError(t, err)
	info, err := f.Stat()
	require.NoError(t, err)
	// Drop the fixture's partial last line before appending whole records.
	require.NoError(t, f.Truncate(info.Size()-int64(len(museFixturePartialTail))))
	_, err = f.Seek(0, 2)
	require.NoError(t, err)
	_, err = f.WriteString(
		`{"schema_version":1,"stream":{"kind":"session","id":"` + museSyncParentID + `"},"sequence":30,"recorded_at":1789000007000000,"payload_type":"runtime.session","payload":{"kind":"run","run_id":"run-2","event":{"kind":"started","prompt":"Now add a regression test"}}}` + "\n",
	)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	engine.SyncAllForceParse(t.Context(), nil)
	messages, err = database.GetAllMessages(t.Context(), parentID)
	require.NoError(t, err)
	require.Len(t, messages, 4)
	assert.Equal(t, "Now add a regression test", messages[3].Content)
	events, err = database.GetUsageEvents(t.Context(), parentID)
	require.NoError(t, err)
	assert.Len(t, events, 1)
}

const museFixturePartialTail = `{"schema_version":1,"id":"rec-partial","stream":{"kind":"session"`
