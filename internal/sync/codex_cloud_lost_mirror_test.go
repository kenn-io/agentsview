package sync_test

import (
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// A mirror file that lost part of a chat keeps the archived transcript and
// live token counts until the fetcher has read the chat again.
func TestCodexCloudShorterMirrorKeepsArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := t.Context()
	root := t.TempDir()
	path := filepath.Join(root, "account-key", "thread-a.jsonl.zstd")
	fetched := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	record := func(kind mirror.Kind, data string) mirror.Record {
		return mirror.Record{Kind: kind, FetchedAt: fetched, Data: jsontext.Value(data)}
	}
	listing := record(mirror.KindThread, `{"id":"thread-a","createdAt":1759320000,"updatedAt":1759323600,"cwd":"/workspace/project-a"}`)
	_, err := mirror.Append(path, 0, []mirror.Record{
		listing,
		record(mirror.KindTurn, `{"id":"turn-1","status":"completed","startedAt":1759320000}`),
		record(mirror.KindItem, `{"turnId":"turn-1","startedAtMs":1759320000000,"completedAtMs":1759320000500,`+
			`"item":{"type":"userMessage","id":"u1","content":[{"type":"text","text":"Fix the build","text_elements":[]}]}}`),
		record(mirror.KindItem, `{"turnId":"turn-1","startedAtMs":1759320001000,"completedAtMs":1759320001500,`+
			`"item":{"type":"agentMessage","id":"a1","text":"Fixed.","phase":"final_answer"}}`),
		record(mirror.KindTokenUsage, `{"join_id":"join-a","turn_id":"turn-1",`+
			`"total":{"input":1000,"output":100,"total":1100},"last":{"input":1000,"output":100,"total":1100},`+
			`"observed_at":"2026-10-01T12:05:00Z"}`),
	})
	require.NoError(t, err)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(ctx, database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodexCloud: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	engine.SyncAll(ctx, nil)
	messages, err := database.GetAllMessages(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	events, err := database.GetUsageEvents(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	require.Len(t, events, 1)

	require.NoError(t, os.Remove(path))
	_, err = mirror.Append(path, 0, []mirror.Record{listing})
	require.NoError(t, err)
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))

	messages, err = database.GetAllMessages(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	assert.Len(t, messages, 2, "the archived transcript stays")
	events, err = database.GetUsageEvents(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	assert.Len(t, events, 1, "the archived live token counts stay")
}

// A mirror rebuilt from server history has no token counts. The archived
// ones stay until new live records arrive, and the new running total then
// replaces them instead of adding to them.
func TestCodexCloudRefetchedMirrorKeepsTokenCounts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := t.Context()
	root := t.TempDir()
	path := filepath.Join(root, "account-key", "thread-a.jsonl.zstd")
	fetched := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	record := func(kind mirror.Kind, data string) mirror.Record {
		return mirror.Record{Kind: kind, FetchedAt: fetched, Data: jsontext.Value(data)}
	}
	history := []mirror.Record{
		record(mirror.KindThread, `{"id":"thread-a","createdAt":1759320000,"updatedAt":1759323600,"cwd":"/workspace/project-a"}`),
		record(mirror.KindTurn, `{"id":"turn-1","status":"completed","startedAt":1759320000}`),
		record(mirror.KindItem, `{"turnId":"turn-1","startedAtMs":1759320000000,"completedAtMs":1759320000500,`+
			`"item":{"type":"userMessage","id":"u1","content":[{"type":"text","text":"Fix the build","text_elements":[]}]}}`),
		record(mirror.KindItem, `{"turnId":"turn-1","startedAtMs":1759320001000,"completedAtMs":1759320001500,`+
			`"item":{"type":"agentMessage","id":"a1","text":"Fixed.","phase":"final_answer"}}`),
	}
	tokens := func(input, output int, observed string) mirror.Record {
		counts := `{"input":` + strconv.Itoa(input) + `,"output":` + strconv.Itoa(output) +
			`,"total":` + strconv.Itoa(input+output) + `}`
		return record(mirror.KindTokenUsage, `{"join_id":"join-a","turn_id":"turn-1","total":`+counts+
			`,"last":`+counts+`,"observed_at":"`+observed+`"}`)
	}
	_, err := mirror.Append(path, 0, append(slices.Clone(history), tokens(1000, 100, "2026-10-01T12:05:00Z")))
	require.NoError(t, err)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(ctx, database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodexCloud: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	engine.SyncAll(ctx, nil)
	inputTokens := func() []int {
		t.Helper()
		events, err := database.GetUsageEvents(ctx, "codex-cloud:thread-a")
		require.NoError(t, err)
		var inputs []int
		for _, ev := range events {
			inputs = append(inputs, ev.InputTokens)
		}
		return inputs
	}
	require.Equal(t, []int{1000}, inputTokens())

	require.NoError(t, os.Remove(path))
	_, err = mirror.Append(path, 0, history)
	require.NoError(t, err)
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))
	assert.Equal(t, []int{1000}, inputTokens(), "the archived token counts stay")

	_, err = mirror.Append(path, 0, []mirror.Record{tokens(1500, 150, "2026-10-01T13:00:00Z")})
	require.NoError(t, err)
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))
	assert.Equal(t, []int{1500}, inputTokens(), "the new running total replaces them")
}
