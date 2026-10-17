package codexcloud

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/codexcloudtest"
	"go.kenn.io/agentsview/internal/codexcloud/mirror"
	"go.kenn.io/agentsview/internal/parser"
)

var fetchNow = time.Date(2027, 1, 15, 12, 0, 0, 0, time.UTC)

const fetchKey AccountKey = "fedcba9876543210fedcba9876543210"

type fetchEnv struct {
	host    *codexcloudtest.Host
	threads *codexcloudtest.Threads
	conn    *Conn
	store   *Store
}

func newFetchEnv(t *testing.T) *fetchEnv {
	t.Helper()
	host := codexcloudtest.NewHost(t)
	threads := codexcloudtest.NewThreads()
	host.ServeThreads(threads)
	return &fetchEnv{host: host, threads: threads, conn: listConn(t, host), store: NewStore(t.TempDir())}
}

func ago(d time.Duration) int64   { return fetchNow.Add(-d).Unix() }
func agoMs(d time.Duration) int64 { return fetchNow.Add(-d).UnixMilli() }

func listingRecord(id string, updated int64) jsontext.Value {
	return jsontext.Value(`{"id":"` + id + `","name":"n","createdAt":` + strconv.FormatInt(updated-100, 10) +
		`,"updatedAt":` + strconv.FormatInt(updated, 10) + `,"status":{"type":"idle"}}`)
}

// pass fetches and commits one thread at now, as the sync pass does.
func (e *fetchEnv) pass(t *testing.T, id string, st ThreadState, updated int64, now time.Time) (ThreadState, fetchResult) {
	t.Helper()
	result, err := fetchThread(t.Context(), e.conn, id, st, listingRecord(id, updated), now)
	require.NoError(t, err)
	next, _, err := commitThread(e.store, fetchKey, id, result.Next, result.Records, now)
	require.NoError(t, err)
	return next, result
}

func (e *fetchEnv) mirrorBytes(t *testing.T, id string) []byte {
	t.Helper()
	path, err := e.store.MirrorPath(fetchKey, id)
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

// itemCalls returns the params of every thread/items/list call so far.
func (e *fetchEnv) itemCalls(t *testing.T) []itemsParams {
	t.Helper()
	var calls []itemsParams
	for _, r := range e.host.Requests() {
		if r.Method == "thread/items/list" {
			var p itemsParams
			require.NoError(t, json.Unmarshal(r.Params, &p))
			calls = append(calls, p)
		}
	}
	return calls
}

func (e *fetchEnv) turnCalls(t *testing.T) []map[string]jsontext.Value {
	t.Helper()
	var calls []map[string]jsontext.Value
	for _, r := range e.host.Requests() {
		if r.Method == "thread/turns/list" {
			var p map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(r.Params, &p))
			calls = append(calls, p)
		}
	}
	return calls
}

func message(id string, text string) codexcloudtest.Item {
	return codexcloudtest.Item{ID: id, Type: "agentMessage", Text: text}
}

func command(id, status string, completedAtMs int64) codexcloudtest.Item {
	return codexcloudtest.Item{ID: id, Type: "commandExecution", Status: status, CompletedAtMs: completedAtMs}
}

func recordedItemIDs(records []mirror.Record) []string {
	var ids []string
	for _, r := range records {
		if r.Kind == mirror.KindItem {
			ids = append(ids, rowItemID(r.Data))
		}
	}
	return ids
}

func TestFirstImportFinishedThreadOneFetch(t *testing.T) {
	env := newFetchEnv(t)
	env.threads.Set(codexcloudtest.Thread{ID: "t", Turns: []codexcloudtest.Turn{{
		ID: "turn-1", Status: "completed", StartedAt: ago(time.Hour), CompletedAt: ago(13 * time.Minute),
		Items: []codexcloudtest.Item{message("a", "one"), message("b", "two")},
	}}})

	st, result := env.pass(t, "t", ThreadState{}, ago(13*time.Minute), fetchNow)

	assert.False(t, result.Unfinished)
	assert.Equal(t, "turn-1", st.ResumeTurnID)
	assert.Equal(t, "b", st.ResumeItemID)
}

func TestRecentEndRefetchUntilMargin(t *testing.T) {
	env := newFetchEnv(t)
	env.threads.Set(codexcloudtest.Thread{ID: "t", Turns: []codexcloudtest.Turn{{
		ID: "turn-1", Status: "completed", StartedAt: ago(time.Hour), CompletedAt: ago(11 * time.Minute),
		Items: []codexcloudtest.Item{message("a", "one")},
	}}})

	st, result := env.pass(t, "t", ThreadState{}, ago(11*time.Minute), fetchNow)
	assert.True(t, result.Unfinished, "11 minutes is inside the 12-minute margin")
	assert.Empty(t, st.ResumeItemID)

	_, result = env.pass(t, "t", st, ago(11*time.Minute), fetchNow.Add(time.Minute))
	assert.False(t, result.Unfinished, "at 12 minutes the margin has passed")
}

func TestArchivedDuringSaveLag(t *testing.T) {
	env := newFetchEnv(t)
	env.threads.Set(codexcloudtest.Thread{ID: "t", Turns: []codexcloudtest.Turn{{
		ID: "turn-1", Status: "inProgress", StartedAt: ago(time.Hour),
		Items: []codexcloudtest.Item{message("a", "one"), command("c", "inProgress", 0)},
	}}})
	st := ThreadState{ArchivedFirstSeen: fetchNow.Add(-30 * time.Second)}

	st, _ = env.pass(t, "t", st, ago(time.Minute), fetchNow)
	assert.Empty(t, st.ResumeItemID, "archiving alone does not end the save lag")

	st, _ = env.pass(t, "t", st, ago(time.Minute), fetchNow.Add(9*time.Minute))
	assert.Empty(t, st.ResumeItemID)

	st, _ = env.pass(t, "t", st, ago(time.Minute), fetchNow.Add(10*time.Minute))
	assert.Equal(t, "c", st.ResumeItemID, "10 minutes after archiving, nothing more is saved")
}

func TestResumeStaysInTurnUntilMargin(t *testing.T) {
	env := newFetchEnv(t)
	env.threads.Set(codexcloudtest.Thread{ID: "t", Turns: []codexcloudtest.Turn{
		{
			ID: "turn-1", Status: "completed", StartedAt: ago(time.Hour), CompletedAt: ago(time.Minute),
			Items: []codexcloudtest.Item{command("a", "completed", agoMs(2*time.Minute))},
		},
		{
			ID: "turn-2", Status: "completed", StartedAt: ago(30 * time.Second), CompletedAt: ago(20 * time.Second),
			Items: []codexcloudtest.Item{command("b", "completed", agoMs(20*time.Second))},
		},
	}})

	st, _ := env.pass(t, "t", ThreadState{}, ago(20*time.Second), fetchNow)
	assert.Equal(t, "turn-1", st.ResumeTurnID, "the resume point does not leave a turn inside its margin")
	assert.Equal(t, "a", st.ResumeItemID)

	env.threads.Update("t", func(thread *codexcloudtest.Thread) {
		thread.Turns[0].Items = append(thread.Turns[0].Items, message("late", "saved after the end"))
	})
	_, result := env.pass(t, "t", st, ago(20*time.Second), fetchNow.Add(time.Minute))
	assert.Contains(t, recordedItemIDs(result.Records), "late")
}

func TestItemSavedAfterNextTurnStarts(t *testing.T) {
	env := newFetchEnv(t)
	env.threads.Set(codexcloudtest.Thread{ID: "t", Turns: []codexcloudtest.Turn{
		{ID: "turn-1", Status: "inProgress", StartedAt: ago(time.Hour), Items: []codexcloudtest.Item{message("a", "one")}},
		{ID: "turn-2", Status: "inProgress", StartedAt: ago(time.Minute), Items: []codexcloudtest.Item{message("b", "two")}},
	}})

	st, _ := env.pass(t, "t", ThreadState{}, ago(time.Minute), fetchNow)
	assert.Empty(t, st.ResumeItemID, "turn 1 never ended and turn 2 started inside the margin")

	env.threads.Update("t", func(thread *codexcloudtest.Thread) {
		thread.Turns[0].Items = append(thread.Turns[0].Items, message("late", "mid-flight"))
	})
	_, result := env.pass(t, "t", st, ago(time.Minute), fetchNow.Add(5*time.Minute))
	assert.Contains(t, recordedItemIDs(result.Records), "late")
}

func TestParallelToolCallsFinishOutOfOrder(t *testing.T) {
	env := newFetchEnv(t)
	env.threads.Set(codexcloudtest.Thread{ID: "t", Turns: []codexcloudtest.Turn{{
		ID: "turn-1", Status: "inProgress", StartedAt: ago(time.Hour),
		Items: []codexcloudtest.Item{
			command("first", "completed", agoMs(time.Minute)),
			command("slow", "inProgress", 0),
			command("fast", "completed", agoMs(30*time.Second)),
		},
	}}})

	st, _ := env.pass(t, "t", ThreadState{}, ago(time.Minute), fetchNow)
	assert.Equal(t, "first", st.ResumeItemID, "the resume point stops at the first unfinished item")

	env.threads.Update("t", func(thread *codexcloudtest.Thread) {
		thread.Turns[0].Items[1] = command("slow", "completed", agoMs(0))
	})
	st, result := env.pass(t, "t", st, ago(time.Minute), fetchNow.Add(time.Minute))
	assert.Equal(t, "fast", st.ResumeItemID)
	path, err := env.store.MirrorPath(fetchKey, "t")
	require.NoError(t, err)
	records, err := mirror.ReadAll(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, []string{"first", "slow", "fast", "slow"}, recordedItemIDs(records),
		"only the changed item is appended again")
	assert.ElementsMatch(t, []string{"slow", "fast"}, recordedItemIDs(result.Records))
}

func TestSilentCommandStillFinal(t *testing.T) {
	env := newFetchEnv(t)
	env.threads.Set(codexcloudtest.Thread{ID: "t", Turns: []codexcloudtest.Turn{{
		ID: "turn-1", Status: "inProgress", StartedAt: ago(time.Hour),
		Items: []codexcloudtest.Item{command("c", "inProgress", 0)},
	}}})
	st := ThreadState{}
	for i := range 3 {
		var result fetchResult
		st, result = env.pass(t, "t", st, ago(time.Hour), fetchNow.Add(time.Duration(i)*5*time.Minute))
		assert.True(t, result.Unfinished, "pass %d", i)
	}
	env.threads.Update("t", func(thread *codexcloudtest.Thread) {
		thread.Turns[0].Status = "completed"
		thread.Turns[0].CompletedAt = fetchNow.Add(15 * time.Minute).Unix()
		thread.Turns[0].Items[0] = codexcloudtest.Item{
			ID: "c", Type: "commandExecution", Status: "completed", Text: "final output",
			CompletedAtMs: fetchNow.Add(15 * time.Minute).UnixMilli(),
		}
	})
	_, result := env.pass(t, "t", st, ago(time.Hour), fetchNow.Add(16*time.Minute))
	require.Equal(t, []string{"c"}, recordedItemIDs(result.Records))
	assert.Contains(t, string(result.Records[len(result.Records)-1].Data), "final output")
}

func TestLaterTurnFetchedInFull(t *testing.T) {
	env := newFetchEnv(t)
	env.threads.Set(codexcloudtest.Thread{ID: "t", Turns: []codexcloudtest.Turn{{
		ID: "turn-1", Status: "completed", StartedAt: ago(time.Hour), CompletedAt: ago(30 * time.Minute),
		Items: []codexcloudtest.Item{message("a", "one")},
	}}})
	st, _ := env.pass(t, "t", ThreadState{}, ago(30*time.Minute), fetchNow)
	env.threads.Update("t", func(thread *codexcloudtest.Thread) {
		thread.Turns = append(thread.Turns, codexcloudtest.Turn{
			ID: "turn-2", Status: "completed", StartedAt: ago(20 * time.Minute), CompletedAt: ago(15 * time.Minute),
			Items: []codexcloudtest.Item{message("b", "two"), message("c", "three"), message("d", "four")},
		})
	})
	before := len(env.itemCalls(t))

	_, result := env.pass(t, "t", st, ago(15*time.Minute), fetchNow)

	assert.Equal(t, []string{"b", "c", "d"}, recordedItemIDs(result.Records))
	calls := env.itemCalls(t)[before:]
	require.Len(t, calls, 2)
	assert.Equal(t, "turn-1", calls[0].TurnID)
	assert.JSONEq(t, `{"type":"item","itemId":"a"}`, string(calls[0].Cursor), "the resume turn reads from its anchor")
	assert.Equal(t, "turn-2", calls[1].TurnID)
	assert.Empty(t, calls[1].Cursor, "a later turn is read in full")
}

func TestLongThreadReadsOnlyNewTurns(t *testing.T) {
	env := newFetchEnv(t)
	thread := codexcloudtest.Thread{ID: "t"}
	for i := range 120 {
		thread.Turns = append(thread.Turns, codexcloudtest.Turn{
			ID: "turn-" + strconv.Itoa(i), Status: "completed",
			StartedAt: ago(time.Duration(200-i) * time.Hour), CompletedAt: ago(time.Duration(199-i) * time.Hour),
			Items: []codexcloudtest.Item{message("item-"+strconv.Itoa(i), "x")},
		})
	}
	env.threads.Set(thread)
	st := ThreadState{ResumeTurnID: "turn-118", ResumeItemID: "item-118"}
	before := len(env.turnCalls(t))

	_, result := env.pass(t, "t", st, ago(time.Hour), fetchNow)

	assert.Len(t, env.turnCalls(t)[before:], 1, "the turn listing stops at the resume turn")
	assert.Equal(t, []string{"item-119"}, recordedItemIDs(result.Records))
}

func TestTurnEndingBetweenCallsNotFinished(t *testing.T) {
	env := newFetchEnv(t)
	env.threads.Set(codexcloudtest.Thread{ID: "t", Turns: []codexcloudtest.Turn{{
		ID: "turn-1", Status: "inProgress", StartedAt: ago(time.Hour),
		Items: []codexcloudtest.Item{message("a", "one")},
	}}})
	// The turn ends once its items are read. Statuses read first still show it
	// running; statuses read after the items would show it ended.
	env.host.Handle("thread/items/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		reply, rpcErr := env.threads.ItemsList(params)
		env.threads.Update("t", func(thread *codexcloudtest.Thread) {
			thread.Turns[0].Status = "completed"
			thread.Turns[0].CompletedAt = ago(time.Hour)
		})
		return reply, rpcErr
	})

	st, result := env.pass(t, "t", ThreadState{}, ago(time.Hour), fetchNow)

	assert.True(t, result.Unfinished)
	assert.Empty(t, st.ResumeItemID, "the turn's end is not trusted until the next pass sees it first")
}

func TestUnchangedRefetchByteIdentical(t *testing.T) {
	env := newFetchEnv(t)
	env.threads.Set(codexcloudtest.Thread{ID: "t", Turns: []codexcloudtest.Turn{{
		ID: "turn-1", Status: "inProgress", StartedAt: ago(time.Hour),
		Items: []codexcloudtest.Item{message("a", "one"), command("c", "inProgress", 0)},
	}}})
	st, _ := env.pass(t, "t", ThreadState{}, ago(time.Hour), fetchNow)
	first := env.mirrorBytes(t, "t")

	next, _ := env.pass(t, "t", st, ago(time.Hour), fetchNow.Add(5*time.Minute))

	assert.Equal(t, first, env.mirrorBytes(t, "t"))
	assert.Equal(t, st.Offset, next.Offset)
}

// A crash after the append but before the state save leaves frames the
// stored offset does not cover. The next run must neither lose nor repeat
// them.
func TestCrashBetweenAppendAndSave(t *testing.T) {
	env := newFetchEnv(t)
	env.threads.Set(codexcloudtest.Thread{ID: "t", Turns: []codexcloudtest.Turn{{
		ID: "turn-1", Status: "inProgress", StartedAt: ago(time.Hour),
		Items: []codexcloudtest.Item{message("a", "one")},
	}}})
	saved, _ := env.pass(t, "t", ThreadState{}, ago(time.Hour), fetchNow)
	env.threads.Update("t", func(thread *codexcloudtest.Thread) {
		thread.Turns[0].Items = append(thread.Turns[0].Items, message("b", "two"))
	})
	crashed, err := fetchThread(t.Context(), env.conn, "t", saved, listingRecord("t", ago(time.Hour)), fetchNow.Add(time.Minute))
	require.NoError(t, err)
	path, err := env.store.MirrorPath(fetchKey, "t")
	require.NoError(t, err)
	_, err = mirror.Append(path, saved.Offset, crashed.Records)
	require.NoError(t, err)
	afterCrash := env.mirrorBytes(t, "t")

	recovered, _ := env.pass(t, "t", saved, ago(time.Hour), fetchNow.Add(2*time.Minute))

	assert.Equal(t, afterCrash, env.mirrorBytes(t, "t"), "recovered records are not appended again")
	assert.Equal(t, fetchNow.Add(time.Minute), recovered.changedAt(),
		"the recovered items are the chat's newest change, though the listing time did not move")
	result, ok, err := parser.ParseCodexCloudFile(t.Context(), path, "m")
	require.NoError(t, err)
	require.True(t, ok)
	var texts []string
	for _, msg := range result.Messages {
		texts = append(texts, msg.Content)
	}
	assert.Equal(t, []string{"one", "two"}, texts)
}

func TestLostMirrorFileRewritesEverything(t *testing.T) {
	env := newFetchEnv(t)
	env.threads.Set(codexcloudtest.Thread{ID: "t", Turns: []codexcloudtest.Turn{{
		ID: "turn-1", Status: "inProgress", StartedAt: ago(time.Hour),
		Items: []codexcloudtest.Item{message("a", "one"), command("c", "inProgress", 0)},
	}}})
	st, _ := env.pass(t, "t", ThreadState{}, ago(time.Hour), fetchNow)
	path, err := env.store.MirrorPath(fetchKey, "t")
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))

	_, _ = env.pass(t, "t", st, ago(time.Hour), fetchNow.Add(time.Minute))

	records, err := mirror.ReadAll(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, mirror.KindThread, records[0].Kind, "the metadata record is written again")
	assert.Equal(t, []string{"a", "c"}, recordedItemIDs(records))
}

func TestEveryTurnsListCarriesItemsView(t *testing.T) {
	env := newFetchEnv(t)
	thread := codexcloudtest.Thread{ID: "t"}
	for i := range 60 {
		thread.Turns = append(thread.Turns, codexcloudtest.Turn{ID: "turn-" + strconv.Itoa(i), Status: "inProgress"})
	}
	env.threads.Set(thread)

	_, _ = env.pass(t, "t", ThreadState{}, ago(time.Hour), fetchNow)

	calls := env.turnCalls(t)
	require.Len(t, calls, 2, "two pages")
	for _, call := range calls {
		assert.JSONEq(t, `"notLoaded"`, string(call["itemsView"]))
	}
}

func TestAnchoredReadErrorRereadsTurn(t *testing.T) {
	env := newFetchEnv(t)
	env.threads.Set(codexcloudtest.Thread{ID: "t", Turns: []codexcloudtest.Turn{{
		ID: "turn-1", Status: "inProgress", StartedAt: ago(time.Hour),
		Items: []codexcloudtest.Item{
			command("a", "completed", agoMs(time.Hour)), command("b", "completed", agoMs(time.Hour)),
			command("c", "inProgress", 0),
		},
	}}})
	st, _ := env.pass(t, "t", ThreadState{}, ago(time.Hour), fetchNow)
	require.Equal(t, "b", st.ResumeItemID)
	env.threads.RejectAnchor()

	result, err := fetchThread(t.Context(), env.conn, "t", st, nil, fetchNow.Add(time.Minute))

	require.NoError(t, err)
	assert.Equal(t, []string{"c"}, recordedItemIDs(result.Records), "items through the resume point are skipped")
}

// The spec counts completedAtMs only when an item's own status, if it has
// one, is terminal.
func TestCompletedTimeWithRunningStatusNotFinished(t *testing.T) {
	env := newFetchEnv(t)
	env.threads.Set(codexcloudtest.Thread{ID: "t", Turns: []codexcloudtest.Turn{{
		ID: "turn-1", Status: "inProgress", StartedAt: ago(time.Hour),
		Items: []codexcloudtest.Item{
			command("a", "completed", agoMs(time.Hour)), command("b", "inProgress", agoMs(time.Hour)),
		},
	}}})

	st, _ := env.pass(t, "t", ThreadState{}, ago(time.Hour), fetchNow)

	assert.Equal(t, "a", st.ResumeItemID)
}
