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
	"go.kenn.io/agentsview/internal/db"
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
	full := []mirror.Record{
		listing,
		record(mirror.KindTurn, `{"id":"turn-1","status":"completed","startedAt":1759320000}`),
		record(mirror.KindItem, `{"turnId":"turn-1","startedAtMs":1759320000000,"completedAtMs":1759320000500,`+
			`"item":{"type":"userMessage","id":"u1","content":[{"type":"text","text":"Fix the build","text_elements":[]}]}}`),
		record(mirror.KindItem, `{"turnId":"turn-1","startedAtMs":1759320001000,"completedAtMs":1759320001500,`+
			`"item":{"type":"agentMessage","id":"a1","text":"Fixed.","phase":"final_answer"}}`),
		record(mirror.KindTokenUsage, `{"join_id":"join-a","turn_id":"turn-1",`+
			`"total":{"input":1000,"output":100,"total":1100},"last":{"input":1000,"output":100,"total":1100},`+
			`"observed_at":"2026-10-01T12:05:00Z"}`),
	}
	_, err := mirror.Append(path, 0, full)
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

	// Once the fetcher refills the file and the chat moves on, the
	// mirror replaces the archived copy again.
	require.NoError(t, os.Remove(path))
	_, err = mirror.Append(path, 0, append(slices.Clone(full),
		record(mirror.KindItem, `{"turnId":"turn-1","startedAtMs":1759320002000,"completedAtMs":1759320002500,`+
			`"item":{"type":"agentMessage","id":"a2","text":"Also fixed the lint.","phase":"final_answer"}}`),
	))
	require.NoError(t, err)
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))
	messages, err = database.GetAllMessages(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	assert.Len(t, messages, 3, "the refilled mirror replaces the archive")
}

// An archived chat written by an older parser version is reparsed even when
// the new parse has fewer messages, so a parser change can take effect.
func TestCodexCloudOlderParserVersionReplacesArchive(t *testing.T) {
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
	head := []mirror.Record{
		record(mirror.KindThread, `{"id":"thread-a","createdAt":1759320000,"updatedAt":1759323600,"cwd":"/workspace/project-a"}`),
		record(mirror.KindTurn, `{"id":"turn-1","status":"completed","startedAt":1759320000}`),
		record(mirror.KindItem, `{"turnId":"turn-1","startedAtMs":1759320000000,"completedAtMs":1759320000500,`+
			`"item":{"type":"userMessage","id":"u1","content":[{"type":"text","text":"Fix the build","text_elements":[]}]}}`),
	}
	_, err := mirror.Append(path, 0, append(slices.Clone(head),
		record(mirror.KindItem, `{"turnId":"turn-1","startedAtMs":1759320001000,"completedAtMs":1759320001500,`+
			`"item":{"type":"agentMessage","id":"a1","text":"Fixed.","phase":"final_answer"}}`),
	))
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

	require.NoError(t, database.SetSessionDataVersion(ctx, "codex-cloud:thread-a",
		db.AgentDataVersion(parser.AgentCodexCloud)-1))
	// A deleted mirror rebuilt around a usage record holds no messages,
	// which no parser change produces, so the archive stays.
	require.NoError(t, os.Remove(path))
	_, err = mirror.Append(path, 0, head[:1])
	require.NoError(t, err)
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))
	messages, err = database.GetAllMessages(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	assert.Len(t, messages, 2, "an empty mirror never replaces the archive")

	require.NoError(t, os.Remove(path))
	_, err = mirror.Append(path, 0, head)
	require.NoError(t, err)
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))

	messages, err = database.GetAllMessages(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	assert.Len(t, messages, 1, "the older parser's copy is replaced")
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
	session, err := database.GetSessionFull(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 100, session.TotalOutputTokens, "the archived token totals stay")

	_, err = mirror.Append(path, 0, []mirror.Record{tokens(1500, 150, "2026-10-01T13:00:00Z")})
	require.NoError(t, err)
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))
	assert.Equal(t, []int{1500}, inputTokens(), "the new running total replaces them")
}

// A mirror rebuilt from server history after the chat moved on has more
// messages but no token counts. The new messages import and the archived
// token counts and capture coverage stay.
func TestCodexCloudRebuiltMirrorWithNewMessagesKeepsTokenCounts(t *testing.T) {
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
	_, err := mirror.Append(path, 0, append(slices.Clone(history),
		record(mirror.KindTokenUsage, `{"join_id":"join-a","turn_id":"turn-1",`+
			`"total":{"input":1000,"output":100,"total":1100},"last":{"input":1000,"output":100,"total":1100},`+
			`"observed_at":"2026-10-01T12:05:00Z"}`),
	))
	require.NoError(t, err)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(ctx, database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodexCloud: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	engine.SyncAll(ctx, nil)
	before, err := database.GetSessionPlanUsage(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	require.NotNil(t, before)
	require.True(t, before.HasCoverage)

	require.NoError(t, os.Remove(path))
	_, err = mirror.Append(path, 0, append(slices.Clone(history),
		record(mirror.KindItem, `{"turnId":"turn-1","startedAtMs":1759320002000,"completedAtMs":1759320002500,`+
			`"item":{"type":"agentMessage","id":"a2","text":"Also fixed the lint.","phase":"final_answer"}}`),
	))
	require.NoError(t, err)
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))

	messages, err := database.GetAllMessages(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	assert.Len(t, messages, 3, "the new message imports")
	events, err := database.GetUsageEvents(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	require.Len(t, events, 1, "the archived usage event stays")
	assert.Equal(t, 1000, events[0].InputTokens)
	session, err := database.GetSessionFull(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.True(t, session.HasTotalOutputTokens)
	assert.Equal(t, 100, session.TotalOutputTokens, "the archived token totals stay")
	after, err := database.GetSessionPlanUsage(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.True(t, after.HasCoverage, "the archived capture coverage stays")
	assert.Equal(t, "2026-10-01T12:05:00Z", after.LastCaptureAt)
}

// A mirror rebuilt from server history has no plan usage records. The
// archived plan figures stay, and newer ones from the rebuilt file replace
// them while the archived token counts stay.
func TestCodexCloudRebuiltMirrorKeepsPlanUsage(t *testing.T) {
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
	planRow := func(weekly, asOf string) mirror.Record {
		return record(mirror.KindPlanUsageRow, `{"row":{"thread_id":"thread-a","data_status":"available","usage_source":"wham",`+
			`"weekly_limit_percent":`+weekly+`,"five_hour_limit_percent":null,"balance_usage_credits":"0","groups":[]},`+
			`"data_as_of":"`+asOf+`"}`)
	}
	_, err := mirror.Append(path, 0, append(slices.Clone(history), planRow("2.25", "2026-10-02T09:30:00Z")))
	require.NoError(t, err)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(ctx, database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodexCloud: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	engine.SyncAll(ctx, nil)
	weekly := func() *float64 {
		t.Helper()
		plan, err := database.GetSessionPlanUsage(ctx, "codex-cloud:thread-a")
		require.NoError(t, err)
		require.NotNil(t, plan)
		require.True(t, plan.HasPlanUsage)
		return plan.WeeklyPercent
	}
	require.Equal(t, new(2.25), weekly())

	require.NoError(t, os.Remove(path))
	_, err = mirror.Append(path, 0, append(slices.Clone(history),
		record(mirror.KindItem, `{"turnId":"turn-1","startedAtMs":1759320002000,"completedAtMs":1759320002500,`+
			`"item":{"type":"agentMessage","id":"a2","text":"Also fixed the lint.","phase":"final_answer"}}`),
	))
	require.NoError(t, err)
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))
	messages, err := database.GetAllMessages(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	assert.Len(t, messages, 3, "the new message imports")
	assert.Equal(t, new(2.25), weekly(), "the archived plan figures stay")

	// An outcome written after the rebuild applies even when its time,
	// from the local clock, is before the report time from OpenAI's.
	_, err = mirror.Append(path, 0, []mirror.Record{
		record(mirror.KindPlanUsageOutcome, `{"kind":"left_out","at":"2026-10-02T08:00:00Z"}`),
	})
	require.NoError(t, err)
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))
	skewed, err := database.GetSessionPlanUsage(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	require.NotNil(t, skewed)
	assert.Equal(t, "left_out", skewed.OutcomeKind, "the outcome follows the report in the file")
	assert.Equal(t, new(2.25), weekly())

	_, err = mirror.Append(path, 0, []mirror.Record{planRow("3.5", "2026-10-03T09:30:00Z")})
	require.NoError(t, err)
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))
	assert.Equal(t, new(3.5), weekly(), "newer plan figures replace them")
}

// A refetched mirror with the same messages and no token counts still
// imports its newer plan usage, and the archived token counts stay.
func TestCodexCloudRefetchedMirrorImportsNewerPlanUsage(t *testing.T) {
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
	_, err := mirror.Append(path, 0, append(slices.Clone(history),
		record(mirror.KindTokenUsage, `{"join_id":"join-a","turn_id":"turn-1",`+
			`"total":{"input":1000,"output":100,"total":1100},"last":{"input":1000,"output":100,"total":1100},`+
			`"observed_at":"2026-10-01T12:05:00Z"}`),
	))
	require.NoError(t, err)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(ctx, database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodexCloud: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	engine.SyncAll(ctx, nil)

	require.NoError(t, os.Remove(path))
	_, err = mirror.Append(path, 0, append(slices.Clone(history),
		record(mirror.KindPlanUsageOutcome, `{"kind":"left_out","at":"2026-10-03T08:00:00Z"}`),
	))
	require.NoError(t, err)
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))

	plan, err := database.GetSessionPlanUsage(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, "left_out", plan.OutcomeKind, "the newer plan usage imports")
	assert.True(t, plan.HasCoverage, "the archived capture coverage stays")
	session, err := database.GetSessionFull(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 100, session.TotalOutputTokens, "the archived token totals stay")
}

// The first live record in a rebuilt mirror restores the running total,
// but holds a smaller last call than the archived peak; the peak stays.
func TestCodexCloudRebuiltMirrorKeepsPeakContext(t *testing.T) {
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
	_, err := mirror.Append(path, 0, append(slices.Clone(history),
		record(mirror.KindTokenUsage, `{"join_id":"join-a","turn_id":"turn-1",`+
			`"total":{"input":9000,"output":100,"total":9100},"last":{"input":9000,"output":100,"total":9100},`+
			`"observed_at":"2026-10-01T12:05:00Z"}`),
	))
	require.NoError(t, err)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(ctx, database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodexCloud: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	engine.SyncAll(ctx, nil)
	session, err := database.GetSessionFull(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	require.NotNil(t, session)
	peak := session.PeakContextTokens

	require.NoError(t, os.Remove(path))
	_, err = mirror.Append(path, 0, append(slices.Clone(history),
		record(mirror.KindTokenUsage, `{"join_id":"join-b","turn_id":"turn-1",`+
			`"total":{"input":9500,"output":150,"total":9650},"last":{"input":500,"output":50,"total":550},`+
			`"observed_at":"2026-10-01T13:00:00Z"}`),
	))
	require.NoError(t, err)
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))

	session, err = database.GetSessionFull(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, peak, session.PeakContextTokens, "the archived peak stays")
	assert.Equal(t, 150, session.TotalOutputTokens, "the running total restores the totals")
}

// A mirror cut short after its newest token record keeps the transcript
// and the earlier token records. The archived, larger counts stay.
func TestCodexCloudTruncatedMirrorKeepsNewestTokenCounts(t *testing.T) {
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
	tokens := func(input, output int, observed string) mirror.Record {
		counts := `{"input":` + strconv.Itoa(input) + `,"output":` + strconv.Itoa(output) +
			`,"total":` + strconv.Itoa(input+output) + `}`
		return record(mirror.KindTokenUsage, `{"join_id":"join-a","turn_id":"turn-1","total":`+counts+
			`,"last":`+counts+`,"observed_at":"`+observed+`"}`)
	}
	first, err := mirror.Append(path, 0, []mirror.Record{
		record(mirror.KindThread, `{"id":"thread-a","createdAt":1759320000,"updatedAt":1759323600,"cwd":"/workspace/project-a"}`),
		record(mirror.KindTurn, `{"id":"turn-1","status":"completed","startedAt":1759320000}`),
		record(mirror.KindItem, `{"turnId":"turn-1","startedAtMs":1759320000000,"completedAtMs":1759320000500,`+
			`"item":{"type":"userMessage","id":"u1","content":[{"type":"text","text":"Fix the build","text_elements":[]}]}}`),
		record(mirror.KindItem, `{"turnId":"turn-1","startedAtMs":1759320001000,"completedAtMs":1759320001500,`+
			`"item":{"type":"agentMessage","id":"a1","text":"Fixed.","phase":"final_answer"}}`),
		tokens(1000, 100, "2026-10-01T12:05:00Z"),
	})
	require.NoError(t, err)
	_, err = mirror.Append(path, first.Offset, []mirror.Record{tokens(1500, 150, "2026-10-01T12:06:00Z")})
	require.NoError(t, err)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(ctx, database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodexCloud: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	engine.SyncAll(ctx, nil)
	session, err := database.GetSessionFull(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	require.NotNil(t, session)
	require.Equal(t, 150, session.TotalOutputTokens)

	require.NoError(t, os.Truncate(path, first.Offset))
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))

	session, err = database.GetSessionFull(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 150, session.TotalOutputTokens, "the archived, larger totals stay")
	events, err := database.GetUsageEvents(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	assert.Len(t, events, 2, "the archived usage events stay")
}

// A mirror cut short after a record that added only input tokens has the
// archived output total, but only some of the archived usage events; the
// archived counts stay. A rebuilt mirror's first live record starts new
// usage events, so its running total still replaces them.
func TestCodexCloudTruncatedMirrorAfterInputOnlyIncreaseKeepsTokenCounts(t *testing.T) {
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
	tokens := func(input, output int, observed string) mirror.Record {
		counts := `{"input":` + strconv.Itoa(input) + `,"output":` + strconv.Itoa(output) +
			`,"total":` + strconv.Itoa(input+output) + `}`
		return record(mirror.KindTokenUsage, `{"join_id":"join-a","turn_id":"turn-1","total":`+counts+
			`,"last":`+counts+`,"observed_at":"`+observed+`"}`)
	}
	history := []mirror.Record{
		record(mirror.KindThread, `{"id":"thread-a","createdAt":1759320000,"updatedAt":1759323600,"cwd":"/workspace/project-a"}`),
		record(mirror.KindTurn, `{"id":"turn-1","status":"completed","startedAt":1759320000}`),
		record(mirror.KindItem, `{"turnId":"turn-1","startedAtMs":1759320000000,"completedAtMs":1759320000500,`+
			`"item":{"type":"userMessage","id":"u1","content":[{"type":"text","text":"Fix the build","text_elements":[]}]}}`),
		record(mirror.KindItem, `{"turnId":"turn-1","startedAtMs":1759320001000,"completedAtMs":1759320001500,`+
			`"item":{"type":"agentMessage","id":"a1","text":"Fixed.","phase":"final_answer"}}`),
	}
	first, err := mirror.Append(path, 0, append(slices.Clone(history), tokens(1000, 100, "2026-10-01T12:05:00Z")))
	require.NoError(t, err)
	_, err = mirror.Append(path, first.Offset, []mirror.Record{tokens(1500, 100, "2026-10-01T12:06:00Z")})
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
	require.Equal(t, []int{1000, 500}, inputTokens())

	require.NoError(t, os.Truncate(path, first.Offset))
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))
	assert.Equal(t, []int{1000, 500}, inputTokens(), "the archived usage events stay")

	require.NoError(t, os.Remove(path))
	_, err = mirror.Append(path, 0, append(slices.Clone(history), tokens(2000, 200, "2026-10-01T13:00:00Z")))
	require.NoError(t, err)
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))
	assert.Equal(t, []int{2000}, inputTokens(), "the new running total replaces them")
	session, err := database.GetSessionFull(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 200, session.TotalOutputTokens)
}

// A later answer can bring back a chat a response left out, with the same
// report time when OpenAI has not refreshed its figures. Its row follows
// the left_out outcome in the file and clears it.
func TestCodexCloudRepeatedReportClearsLeftOut(t *testing.T) {
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
	planRow := record(mirror.KindPlanUsageRow, `{"row":{"thread_id":"thread-a","data_status":"available","usage_source":"wham",`+
		`"weekly_limit_percent":2.25,"five_hour_limit_percent":null,"balance_usage_credits":"0","groups":[]},`+
		`"data_as_of":"2026-10-02T09:30:00Z"}`)
	written, err := mirror.Append(path, 0, []mirror.Record{
		record(mirror.KindThread, `{"id":"thread-a","createdAt":1759320000,"updatedAt":1759323600,"cwd":"/workspace/project-a"}`),
		record(mirror.KindTurn, `{"id":"turn-1","status":"completed","startedAt":1759320000}`),
		record(mirror.KindItem, `{"turnId":"turn-1","startedAtMs":1759320000000,"completedAtMs":1759320000500,`+
			`"item":{"type":"userMessage","id":"u1","content":[{"type":"text","text":"Fix the build","text_elements":[]}]}}`),
		record(mirror.KindItem, `{"turnId":"turn-1","startedAtMs":1759320001000,"completedAtMs":1759320001500,`+
			`"item":{"type":"agentMessage","id":"a1","text":"Fixed.","phase":"final_answer"}}`),
		planRow,
		record(mirror.KindPlanUsageOutcome, `{"kind":"left_out","at":"2026-10-02T10:00:00Z"}`),
	})
	require.NoError(t, err)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(ctx, database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodexCloud: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	engine.SyncAll(ctx, nil)
	plan := func() *db.SessionPlanUsage {
		t.Helper()
		plan, err := database.GetSessionPlanUsage(ctx, "codex-cloud:thread-a")
		require.NoError(t, err)
		require.NotNil(t, plan)
		return plan
	}
	require.Equal(t, "left_out", plan().OutcomeKind)

	_, err = mirror.Append(path, written.Offset, []mirror.Record{planRow})
	require.NoError(t, err)
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))
	assert.Empty(t, plan().OutcomeKind, "the repeated report clears the outcome")
	assert.Equal(t, new(2.25), plan().WeeklyPercent)
}
