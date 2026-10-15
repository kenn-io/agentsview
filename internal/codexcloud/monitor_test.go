package codexcloud

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/codexcloudtest"
	"go.kenn.io/agentsview/internal/codexcloud/mirror"
	"go.kenn.io/agentsview/internal/parser"
)

type monitorEnv struct {
	*passEnv
	monitor  *Monitor
	triggers atomic.Int64
	logs     *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newMonitorEnv(t *testing.T) *monitorEnv {
	t.Helper()
	env := &monitorEnv{passEnv: newPassEnv(t), logs: &syncBuffer{}}
	s := env.syncer
	env.monitor = &Monitor{
		Store: s.Store, Waits: s.Waits, Status: s.Status, Now: s.Now, Dial: s.Dial, Gates: s.Gates,
		ReadLogin: s.ReadLogin, Version: "test", Log: log.New(io.MultiWriter(env.logs), "", 0),
		Trigger: func() error {
			env.triggers.Add(1)
			return nil
		},
	}
	t.Cleanup(env.monitor.CloseAll)
	return env
}

func (e *monitorEnv) login() Login { return e.logins[e.homes[0]] }

// runningThread is a thread updated 30 seconds ago whose newest turn is
// in progress.
func runningThread(id string, kind string) codexcloudtest.Thread {
	updated := fetchNow.Add(-30 * time.Second).Unix()
	return codexcloudtest.Thread{
		ID: id, Name: "thread " + id, CreatedAt: updated - 3600, UpdatedAt: updated,
		Model: "gpt-a", ResumeKind: kind,
		Turns: []codexcloudtest.Turn{
			{ID: id + "-turn-1", Status: "completed", StartedAt: updated - 3600, CompletedAt: updated - 1800},
			{ID: id + "-turn-2", Status: "inProgress", StartedAt: updated - 60},
		},
	}
}

func (e *monitorEnv) join(t *testing.T, id string) {
	t.Helper()
	thread := e.threadUpdated(id)
	require.NoError(t, e.monitor.Join(t.Context(), passKey, e.login(), id, thread))
}

func (e *monitorEnv) threadUpdated(id string) int64 {
	var updated int64
	e.threads.Update(id, func(th *codexcloudtest.Thread) { updated = th.UpdatedAt })
	return updated
}

func (e *monitorEnv) waitLeft(t *testing.T, id string) {
	t.Helper()
	require.Eventually(t, func() bool {
		return !slices.Contains(e.monitor.Joined(passKey), id)
	}, 10*time.Second, 5*time.Millisecond)
}

func (e *monitorEnv) resumes() int { return e.calls("thread/resume") }

// tokenParams builds a thread/tokenUsage/updated notification with counters
// in the host's fractional form, such as 4603049.0.
func tokenParams(threadID, turnID string, total, last [6]float64) jsontext.Value {
	counts := func(c [6]float64) string {
		return fmt.Sprintf(`{"inputTokens":%s,"cachedInputTokens":%s,"cacheWriteInputTokens":%s,`+
			`"outputTokens":%s,"reasoningOutputTokens":%s,"totalTokens":%s}`,
			num(c[0]), num(c[1]), num(c[2]), num(c[3]), num(c[4]), num(c[5]))
	}
	return jsontext.Value(fmt.Sprintf(
		`{"threadId":%q,"turnId":%q,"tokenUsage":{"total":%s,"last":%s,"modelContextWindow":272000.0}}`,
		threadID, turnID, counts(total), counts(last)))
}

func num(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10) + ".0"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func (e *monitorEnv) notify(t *testing.T, id, method string, params any) {
	t.Helper()
	require.NoError(t, e.host.NotifyThread(t.Context(), id, method, params))
}

func (e *monitorEnv) complete(t *testing.T, id, turnID string) {
	t.Helper()
	e.notify(t, id, turnCompletedMethod, map[string]any{
		"threadId": id, "turn": map[string]any{"id": turnID, "status": "completed", "items": []any{}},
	})
}

func (e *monitorEnv) records(t *testing.T, id string) []mirror.Record {
	t.Helper()
	records, err := mirror.ReadAll(t.Context(), e.mirrorPath(t, id))
	require.NoError(t, err)
	return records
}

func tokenRecords(t *testing.T, records []mirror.Record) []mirror.TokenUsageData {
	t.Helper()
	var out []mirror.TokenUsageData
	for _, r := range records {
		if r.Kind == mirror.KindTokenUsage {
			var data mirror.TokenUsageData
			require.NoError(t, json.Unmarshal(r.Data, &data))
			out = append(out, data)
		}
	}
	return out
}

func (e *monitorEnv) thread(t *testing.T, id string) ThreadState {
	t.Helper()
	st, err := e.syncer.Store.LoadThread(passKey, id)
	require.NoError(t, err)
	return st
}

func (e *monitorEnv) live(t *testing.T) string {
	t.Helper()
	e.syncer.Status.Set(HomeStatus{Key: string(passKey), State: StateUpToDate})
	row, ok := e.syncer.Status.Row(string(passKey))
	require.True(t, ok)
	return row.Live
}

func TestWarmJoinRecordsUntilTurnCompleted(t *testing.T) {
	env := newMonitorEnv(t)
	env.threads.Set(runningThread("a", "warm"))

	env.join(t, "a")
	assert.Equal(t, []string{"a"}, env.monitor.Joined(passKey))
	assert.Equal(t, "Live tokens: listening in on 1 running chat.", env.live(t))
	env.notify(t, "a", tokenUsageMethod, tokenParams("a", "a-turn-2",
		[6]float64{4603049, 4000000, 1000, 20000, 5000, 4623049}, [6]float64{90000, 80000, 0, 700, 100, 90700}))
	env.notify(t, "a", "item/agentMessage/delta", map[string]any{"threadId": "a", "delta": "hi"})
	env.notify(t, "a", modelReroutedMethod, map[string]any{
		"threadId": "a", "turnId": "a-turn-2", "fromModel": "gpt-a", "toModel": "gpt-b", "reason": "highRiskCyberActivity",
	})
	env.notify(t, "a", tokenUsageMethod, tokenParams("a", "a-turn-2",
		[6]float64{4700000, 4080000, 1000, 21000, 5200, 4721000}, [6]float64{96951, 80000, 0, 1000, 200, 97951}))
	env.complete(t, "a", "a-turn-2")
	env.waitLeft(t, "a")

	records := env.records(t, "a")
	require.Equal(t, mirror.KindThreadResume, records[0].Kind, "a file the monitor starts begins with metadata")
	assert.NotContains(t, string(records[0].Data), "updatedAt")
	assert.NotContains(t, string(records[0].Data), "recencyAt")
	tokens := tokenRecords(t, records)
	require.Len(t, tokens, 2)
	assert.Equal(t, mirror.TokenCounts{
		Input: 4603049, CachedInput: 4000000, CacheWriteInput: 1000, Output: 20000, ReasoningOutput: 5000, Total: 4623049,
	}, tokens[0].Total)
	assert.Equal(t, int64(97951), tokens[1].Last.Total)
	assert.Equal(t, int64(272000), tokens[0].ModelContextWindow)
	assert.Equal(t, "gpt-a", tokens[0].Model, "the model comes from the resume reply")
	assert.Equal(t, "gpt-b", tokens[1].Model, "model/rerouted changes later records")
	last := records[len(records)-1]
	require.Equal(t, mirror.KindTurnCompleted, last.Kind)
	var completed mirror.TurnCompletedData
	require.NoError(t, json.Unmarshal(last.Data, &completed))
	assert.Equal(t, mirror.TurnCompletedData{JoinID: tokens[0].JoinID, TurnID: "a-turn-2", ObservedAt: fetchNow}, completed)
	assert.NotEmpty(t, tokens[0].JoinID)

	assert.Equal(t, int64(1), env.monitor.flushes.Load(), "updates within a minute are one append")
	assert.Equal(t, "a-turn-2", env.thread(t, "a").LastHandledTurnID)
	require.Eventually(t, func() bool { return env.triggers.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	var resume ResumeParams
	for _, r := range env.host.Requests() {
		if r.Method == "thread/resume" {
			assert.JSONEq(t, `{"threadId":"a","excludeTurns":true}`, string(r.Params))
			require.NoError(t, json.Unmarshal(r.Params, &resume))
		}
	}
	assert.Equal(t, "a", resume.ThreadID)
	assert.Equal(t, "Live tokens: on, not listening in on any running chat right now.", env.live(t))

	from := env.resumes()
	env.join(t, "a")
	assert.Equal(t, from, env.resumes(), "a turn still listed as inProgress after turn/completed is not joined again")
}

func TestColdResumeOnceUntilNewerTurn(t *testing.T) {
	env := newMonitorEnv(t)
	env.threads.Set(runningThread("a", "cold"))

	env.join(t, "a")
	assert.Empty(t, env.monitor.Joined(passKey))
	require.Eventually(t, func() bool { return env.host.Joined("a") == 0 }, 5*time.Second, 5*time.Millisecond,
		"a cold resume closes the connection at once")
	assert.Equal(t, "a-turn-2", env.thread(t, "a").LastHandledTurnID)
	assert.Contains(t, env.logs.String(), "had ended when AgentsView connected")

	env.join(t, "a")
	assert.Equal(t, 1, env.resumes(), "a stuck turn causes one cold resume")

	env.threads.Update("a", func(th *codexcloudtest.Thread) {
		th.Turns = append(th.Turns, codexcloudtest.Turn{ID: "a-turn-3", Status: "inProgress", StartedAt: th.UpdatedAt})
	})
	env.join(t, "a")
	assert.Equal(t, 2, env.resumes(), "a newer turn is joined")
}

func TestUnknownResumeKindStopsCapture(t *testing.T) {
	for name, kind := range map[string]string{"unknown": "lukewarm", "missing": ""} {
		t.Run(name, func(t *testing.T) {
			env := newMonitorEnv(t)
			env.threads.Set(runningThread("a", kind))
			env.threads.Set(runningThread("b", "warm"))

			env.join(t, "a")
			assert.Empty(t, env.monitor.Joined(passKey))
			assert.Equal(t, "Live tokens: stopped. "+Message(FailProtocol, time.Time{})+
				" After updating AgentsView or fixing the endpoint, restart AgentsView to listen in again. Without"+
				" a restart, set live_tokens = false under [agents.codex-cloud], wait until this line says"+
				" \"Live tokens: off\", then set it back to true.", env.live(t))
			assert.True(t, env.liveWarn(t), "a stop marks the status")
			handshakes := len(env.host.Handshakes())
			env.join(t, "b")
			assert.Len(t, env.host.Handshakes(), handshakes, "capture stays off for the account")
			acct := env.account(t)
			assert.Empty(t, acct.Wait.Failure, "the sync job's wait is not set")
		})
	}
}

func TestRefreshRequestFlushesClosesAndWaits(t *testing.T) {
	env := newMonitorEnv(t)
	env.threads.Set(runningThread("a", "warm"))
	env.threads.Set(runningThread("b", "warm"))
	env.join(t, "a")
	env.join(t, "b")
	env.notify(t, "a", tokenUsageMethod, tokenParams("a", "a-turn-2", [6]float64{10, 0, 0, 5, 0, 15}, [6]float64{10, 0, 0, 5, 0, 15}))

	require.NoError(t, env.host.ServerRequestThread(t.Context(), "a", refreshMethod, map[string]any{"reason": "unauthorized"}))
	env.waitLeft(t, "a")
	env.waitLeft(t, "b")

	assert.Len(t, tokenRecords(t, env.records(t, "a")), 1, "buffered records are flushed")
	assert.False(t, env.syncer.Waits.HomeReady(env.homes[0], env.login().AuthModTime))
	for _, r := range env.host.Requests() {
		assert.NotEmpty(t, r.Method, "the refresh request is never answered")
	}
	from := env.resumes()
	env.join(t, "a")
	assert.Equal(t, from, env.resumes(), "no join follows through the waiting home")
}

func TestServerRequestsAreNeverAnswered(t *testing.T) {
	env := newMonitorEnv(t)
	env.threads.Set(runningThread("a", "warm"))
	env.join(t, "a")

	require.NoError(t, env.host.ServerRequestThread(t.Context(), "a", "item/commandExecution/requestApproval",
		map[string]any{"threadId": "a", "turnId": "a-turn-2", "itemId": "x"}))
	env.complete(t, "a", "a-turn-2")
	env.waitLeft(t, "a")

	for _, r := range env.host.Requests() {
		assert.NotEmpty(t, r.Method, "AgentsView sent a reply to a server request")
	}
}

func TestJoinCapPerAccount(t *testing.T) {
	env := newMonitorEnv(t)
	for i := range maxJoins + 1 {
		env.threads.Set(runningThread("t"+strconv.Itoa(i), "warm"))
	}
	for i := range maxJoins + 1 {
		env.join(t, "t"+strconv.Itoa(i))
	}
	assert.Len(t, env.monitor.Joined(passKey), maxJoins)
	assert.Equal(t, maxJoins, env.resumes())
	assert.Equal(t, "Live tokens: listening in on 10 running chats.", env.live(t))
}

func TestJoinLimitLeavesAndRejoins(t *testing.T) {
	env := newMonitorEnv(t)
	env.monitor.joinLimit = 100 * time.Millisecond
	env.threads.Set(runningThread("a", "warm"))
	env.join(t, "a")
	env.notify(t, "a", tokenUsageMethod, tokenParams("a", "a-turn-2", [6]float64{10, 0, 0, 5, 0, 15}, [6]float64{10, 0, 0, 5, 0, 15}))

	env.waitLeft(t, "a")
	assert.Len(t, tokenRecords(t, env.records(t, "a")), 1)
	assert.Empty(t, env.thread(t, "a").LastHandledTurnID, "a turn left unfinished is joined again")
	env.join(t, "a")
	assert.Equal(t, 2, env.resumes())
}

func TestFlushWhileJoined(t *testing.T) {
	env := newMonitorEnv(t)
	env.monitor.flushEvery = 20 * time.Millisecond
	env.threads.Set(runningThread("a", "warm"))
	env.join(t, "a")
	env.notify(t, "a", tokenUsageMethod, tokenParams("a", "a-turn-2", [6]float64{10, 0, 0, 5, 0, 15}, [6]float64{10, 0, 0, 5, 0, 15}))

	require.Eventually(t, func() bool { return len(tokenRecords(t, env.records(t, "a"))) == 1 },
		5*time.Second, 5*time.Millisecond)
	assert.Equal(t, []string{"a"}, env.monitor.Joined(passKey))
}

func TestDroppedJoinRejoins(t *testing.T) {
	env := newMonitorEnv(t)
	env.threads.Set(runningThread("a", "warm"))
	env.join(t, "a")
	env.notify(t, "a", tokenUsageMethod, tokenParams("a", "a-turn-2", [6]float64{10, 0, 0, 5, 0, 15}, [6]float64{10, 0, 0, 5, 0, 15}))

	env.host.DropThread("a")
	env.waitLeft(t, "a")
	assert.Len(t, tokenRecords(t, env.records(t, "a")), 1)
	env.join(t, "a")
	assert.Equal(t, 2, env.resumes())
}

func TestResumeErrors(t *testing.T) {
	cases := map[string]struct {
		err     *codexcloudtest.Error
		stopped bool
		wait    string
	}{
		"environment terminated": {
			err: &codexcloudtest.Error{
				Code: -32600, Message: "thread environment ended",
				Data: map[string]string{"errorType": "environment_terminated"},
			},
		},
		"ordinary error": {err: &codexcloudtest.Error{Code: -32600, Message: "no rollout found for thread id a"}},
		"unsupported method": {
			err:     &codexcloudtest.Error{Code: -32601, Message: "unsupported app-server method: thread/resume"},
			stopped: true,
		},
		"invalid request": {
			err:     &codexcloudtest.Error{Code: -32600, Message: "Invalid request: unknown field"},
			stopped: true,
		},
		"overloaded": {err: &codexcloudtest.Error{Code: -32001, Message: "overloaded"}, wait: FailServer.String()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := newMonitorEnv(t)
			env.threads.Set(runningThread("a", "warm"))
			env.host.Handle("thread/resume", func(jsontext.Value) (any, *codexcloudtest.Error) { return nil, tc.err })

			env.join(t, "a")
			assert.Empty(t, env.monitor.Joined(passKey))
			assert.Equal(t, tc.wait, env.account(t).Wait.Failure)
			if tc.stopped {
				assert.Contains(t, env.live(t), "Live tokens: stopped.")
				return
			}
			assert.NotContains(t, env.live(t), "stopped")
			if tc.wait == "" {
				env.join(t, "a")
				assert.Equal(t, 2, env.resumes(), "the next run tries again")
			}
		})
	}
}

func TestNonIntegralCounterStopsCapture(t *testing.T) {
	env := newMonitorEnv(t)
	env.threads.Set(runningThread("a", "warm"))
	env.join(t, "a")

	env.notify(t, "a", tokenUsageMethod, tokenParams("a", "a-turn-2", [6]float64{1.5, 0, 0, 5, 0, 15}, [6]float64{10, 0, 0, 5, 0, 15}))
	env.waitLeft(t, "a")
	assert.Contains(t, env.live(t), "Live tokens: stopped.")
	assert.Empty(t, tokenRecords(t, env.records(t, "a")))
}

func TestOldThreadIsNotResumed(t *testing.T) {
	env := newMonitorEnv(t)
	thread := runningThread("a", "warm")
	thread.UpdatedAt = fetchNow.Add(-6 * time.Minute).Unix()
	env.threads.Set(thread)

	env.join(t, "a")
	assert.Empty(t, env.host.Handshakes(), "an inProgress turn updated 6 minutes ago is never checked or resumed")
}

func TestIdleNewestTurnIsNotResumed(t *testing.T) {
	env := newMonitorEnv(t)
	thread := runningThread("a", "warm")
	thread.Turns[1].Status = "completed"
	env.threads.Set(thread)

	env.join(t, "a")
	assert.Zero(t, env.resumes())
}

func TestCloseAllEndsJoins(t *testing.T) {
	env := newMonitorEnv(t)
	env.threads.Set(runningThread("a", "warm"))
	env.threads.Set(runningThread("b", "warm"))
	env.join(t, "a")

	env.monitor.CloseAll()
	assert.Empty(t, env.monitor.Joined(passKey))
	require.Eventually(t, func() bool { return env.host.Joined("a") == 0 }, 5*time.Second, 5*time.Millisecond)
	env.join(t, "b")
	assert.Equal(t, 1, env.resumes(), "a closed monitor joins nothing")
}

// liveTokenRecord is a token record the monitor would buffer for j.
func liveTokenRecord(t *testing.T, j *join, total int64, at time.Time) mirror.Record {
	t.Helper()
	data := mirror.TokenUsageData{
		JoinID: j.id, TurnID: "turn-1", ObservedAt: at,
		Total: mirror.TokenCounts{Input: total, Total: total},
		Last:  mirror.TokenCounts{Input: 100, Total: 100},
	}
	var buf []mirror.Record
	require.NoError(t, appendRecord(&buf, mirror.KindTokenUsage, data, at))
	return buf[0]
}

func TestRetriedFlushAppendsRecordsOnce(t *testing.T) {
	env := newMonitorEnv(t)
	m := env.monitor
	j := &join{key: passKey, threadID: "a", id: "join-1"}
	require.NoError(t, m.flush(j.key, j.threadID, []mirror.Record{liveTokenRecord(t, j, 100, fetchNow)}, ""))
	buf := []mirror.Record{
		liveTokenRecord(t, j, 200, fetchNow.Add(10*time.Second)),
		liveTokenRecord(t, j, 300, fetchNow.Add(20*time.Second)),
	}
	// An earlier flush of buf wrote its records, then failed to save the
	// thread's state, so the stored offset is still before them.
	st := env.thread(t, "a")
	_, err := mirror.Append(env.mirrorPath(t, "a"), st.Offset, buf)
	require.NoError(t, err)

	require.NoError(t, m.flush(j.key, j.threadID, buf, ""))

	tokens := tokenRecords(t, env.records(t, "a"))
	totals := make([]int64, len(tokens))
	for i, rec := range tokens {
		totals[i] = rec.Total.Total
	}
	assert.Equal(t, []int64{100, 200, 300}, totals)
	for digest := range env.thread(t, "a").ItemDigests {
		assert.False(t, strings.HasPrefix(digest, liveKeyPrefix), "live record keys are dropped once the records are saved")
	}
}

// failSaves puts a directory in place of the chat's state file, so every
// save of the chat fails until the returned function puts the file back.
func (e *monitorEnv) failSaves(t *testing.T, id string) (restore func()) {
	t.Helper()
	state := filepath.Join(e.root, string(passKey), id+".state.json")
	_, err := os.Stat(state)
	existed := err == nil
	if existed {
		require.NoError(t, os.Rename(state, state+".saved"))
	}
	require.NoError(t, os.MkdirAll(state, 0o700))
	return func() {
		require.NoError(t, os.Remove(state))
		if existed {
			require.NoError(t, os.Rename(state+".saved", state))
		}
	}
}

func tokenTotals(t *testing.T, records []mirror.Record) []int64 {
	t.Helper()
	var totals []int64
	for _, rec := range tokenRecords(t, records) {
		totals = append(totals, rec.Total.Total)
	}
	return totals
}

func (e *monitorEnv) pendingRecords(id string) int {
	e.monitor.mu.Lock()
	defer e.monitor.mu.Unlock()
	return len(e.monitor.pending[threadKey{passKey, id}].records)
}

func TestPendingRecordsAreSavedBeforeNewerOnes(t *testing.T) {
	env := newMonitorEnv(t)
	m := env.monitor
	first := &join{key: passKey, threadID: "a", id: "join-1"}
	second := &join{key: passKey, threadID: "a", id: "join-2"}
	restore := env.failSaves(t, "a")
	require.Error(t, m.flush(passKey, "a", []mirror.Record{
		liveTokenRecord(t, first, 100, fetchNow),
		liveTokenRecord(t, first, 200, fetchNow.Add(10*time.Second)),
	}, ""))
	restore()

	require.NoError(t, m.flush(passKey, "a", []mirror.Record{
		liveTokenRecord(t, second, 300, fetchNow.Add(time.Minute)),
	}, ""))

	assert.Equal(t, []int64{100, 200, 300}, tokenTotals(t, env.records(t, "a")))
	assert.Zero(t, env.pendingRecords("a"))
}

func TestColdJoinSavesPendingRecordsFirst(t *testing.T) {
	env := newMonitorEnv(t)
	j := &join{key: passKey, threadID: "a", id: "join-1"}
	restore := env.failSaves(t, "a")
	require.Error(t, env.monitor.flush(passKey, "a", []mirror.Record{liveTokenRecord(t, j, 100, fetchNow)}, "a-turn-1"))
	restore()
	env.threads.Set(runningThread("a", "cold"))

	env.join(t, "a")

	assert.Equal(t, []int64{100}, tokenTotals(t, env.records(t, "a")))
	assert.Equal(t, "a-turn-2", env.thread(t, "a").LastHandledTurnID,
		"the pending batch's older handled turn does not replace the cold join's")
}

func TestPendingTokenRecordsStayBounded(t *testing.T) {
	env := newMonitorEnv(t)
	m := env.monitor
	j := &join{key: passKey, threadID: "a", id: "join-1"}
	restore := env.failSaves(t, "a")
	at := fetchNow
	add := func(total int64) {
		at = at.Add(time.Second)
		require.Error(t, m.flush(passKey, "a", []mirror.Record{liveTokenRecord(t, j, total, at)}, ""))
	}
	for i := range int64(2 * pendingCompactAt) {
		add((i + 1) * 100)
	}
	// The server's counter went back down: the total before it holds
	// tokens the later records do not.
	add(50)
	add(150)
	assert.LessOrEqual(t, env.pendingRecords("a"), pendingCompactAt)
	restore()

	require.NoError(t, m.flush(passKey, "a", nil, ""))

	totals := tokenTotals(t, env.records(t, "a"))
	require.NotEmpty(t, totals)
	assert.Contains(t, totals, int64(2*pendingCompactAt*100), "the total before the reset is kept")
	assert.Equal(t, []int64{50, 150}, totals[len(totals)-2:])
	assert.True(t, slices.IsSorted(totals[:len(totals)-2]), "kept totals stay in order")
}

// TestCompactedRecordsParseToTheSameUsage parses a chat's token records
// with and without compaction: the per-model token counts and the peak
// context must not change.
func TestCompactedRecordsParseToTheSameUsage(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour)))
	env.run(t)
	synced, err := mirror.ReadAll(t.Context(), env.mirrorPath(t, "a"))
	require.NoError(t, err)

	at := fetchNow
	var tokens []mirror.Record
	add := func(model string, total, last int64) {
		at = at.Add(time.Second)
		data := mirror.TokenUsageData{
			JoinID: "join-1", TurnID: "a-turn-1", Model: model, ObservedAt: at,
			Total: mirror.TokenCounts{Input: total, Total: total},
			Last:  mirror.TokenCounts{Input: last, Total: last},
		}
		require.NoError(t, appendRecord(&tokens, mirror.KindTokenUsage, data, at))
	}
	add("gpt-a", 100, 100)
	add("gpt-a", 200, 900) // the chat's largest context
	add("gpt-a", 300, 100)
	add("gpt-a", 50, 50) // the counter went back down
	add("gpt-a", 400, 100)
	add("gpt-b", 450, 50) // covered by 500, and neither a peak nor its predecessor
	add("gpt-b", 500, 100)
	add("gpt-b", 600, 100)

	parse := func(records []mirror.Record) (map[string]int, int) {
		path := filepath.Join(t.TempDir(), "a.jsonl")
		_, err := mirror.Append(path, 0, slices.Concat(synced, records))
		require.NoError(t, err)
		result, ok, err := parser.ParseCodexCloudFile(t.Context(), path, "m")
		require.NoError(t, err)
		require.True(t, ok)
		byModel := map[string]int{}
		for _, ev := range result.UsageEvents {
			byModel[ev.Model] += ev.InputTokens
		}
		return byModel, result.Session.PeakContextTokens
	}
	wantTokens, wantPeak := parse(tokens)
	require.Equal(t, map[string]int{"gpt-a": 700, "gpt-b": 200}, wantTokens)
	require.Equal(t, 900, wantPeak)

	compacted := compactPending(slices.Clone(tokens))
	// Only gpt-b 450 is dropped. gpt-b 500 stays as the record before its
	// run's peak, so the peak stays priced as one call.
	assert.Len(t, compacted, len(tokens)-1)
	gotTokens, gotPeak := parse(compacted)

	assert.Equal(t, wantTokens, gotTokens)
	assert.Equal(t, wantPeak, gotPeak)
}

func TestRepeatedSaveFailureIsLoggedOnce(t *testing.T) {
	env := newMonitorEnv(t)
	j := &join{key: passKey, threadID: "a", id: "join-1"}
	env.failSaves(t, "a")
	require.Error(t, env.monitor.flush(passKey, "a", []mirror.Record{liveTokenRecord(t, j, 100, fetchNow)}, ""))

	env.monitor.flushPending()
	env.monitor.flushPending()

	assert.Equal(t, 1, strings.Count(env.logs.String(), "save token records"))
}

func TestUnreadableMessageStopsCapture(t *testing.T) {
	env := newMonitorEnv(t)
	// readLoop ends a connection this way when a message does not decode.
	conn := &Conn{done: make(chan struct{})}
	conn.setErr(fmt.Errorf("%w: unreadable message from the thread host", ErrProtocolChange))
	j := &join{key: passKey, threadID: "a", login: env.login(), conn: conn, stop: make(chan struct{})}

	env.monitor.ended(j)

	assert.Contains(t, env.live(t), "Live tokens: stopped.")
	assert.Contains(t, env.logs.String(), "chat a: a message AgentsView cannot read ended the connection",
		"the log names the chat that ended capture")
	env.threads.Set(runningThread("b", "warm"))
	env.join(t, "b")
	assert.Empty(t, env.host.Handshakes(), "no chat is listened in on until capture starts again")
}

func TestJoinAllowsClockDrift(t *testing.T) {
	env := newMonitorEnv(t)
	thread := runningThread("a", "warm")
	// OpenAI's clock runs 4 minutes behind this machine's.
	thread.UpdatedAt = fetchNow.Add(-4 * time.Minute).Unix()
	env.threads.Set(thread)

	env.join(t, "a")

	assert.Equal(t, []string{"a"}, env.monitor.Joined(passKey))
}

func TestJoinRuleIsCheckedAgainBeforeResume(t *testing.T) {
	env := newMonitorEnv(t)
	env.threads.Set(runningThread("a", "warm"))
	// A slow dial carries the chat past the join window.
	env.monitor.Dial = func(ctx context.Context, o DialOptions) (*Conn, error) {
		conn, err := Dial(ctx, o)
		env.clock.Advance(joinWindow)
		return conn, err
	}

	env.join(t, "a")

	assert.Zero(t, env.resumes())
	assert.Empty(t, env.monitor.Joined(passKey))
}

func TestJoinSetUpDuringARefreshRequestIsNotKept(t *testing.T) {
	env := newMonitorEnv(t)
	env.threads.Set(runningThread("a", "warm"))
	// Another connection through the same home gets a refresh request
	// while this one is being set up.
	env.monitor.Dial = func(ctx context.Context, o DialOptions) (*Conn, error) {
		conn, err := Dial(ctx, o)
		env.syncer.Waits.RecordHome(o.Login.Home, FailRefreshRequested, o.Login.AuthModTime)
		return conn, err
	}

	env.join(t, "a")

	assert.Empty(t, env.monitor.Joined(passKey))
	require.Eventually(t, func() bool { return env.host.Joined("a") == 0 }, 5*time.Second, 5*time.Millisecond)
}

// Updates that arrived before a join leaves are recorded, even when the stop
// signal and queued updates are ready at once.
func TestLeavingKeepsQueuedTokenUpdates(t *testing.T) {
	env := newMonitorEnv(t)
	env.monitor.flushEvery = time.Millisecond
	env.threads.Set(runningThread("a", "warm"))
	env.join(t, "a")
	env.monitor.mu.Lock()
	j := env.monitor.joins[passKey]["a"]
	env.monitor.mu.Unlock()
	require.NotNil(t, j)
	notes := j.conn.Notifications()

	// Holding the account lock blocks the join in its next save, so later
	// updates wait in its queue.
	// A failed assertion must not leave the lock held: CloseAll in cleanup
	// would wait forever for the blocked join.
	unlock := sync.OnceFunc(env.syncer.Store.Lock(passKey))
	defer unlock()
	sent := 0
	send := func() {
		sent++
		v := float64(sent * 100)
		env.notify(t, "a", tokenUsageMethod, tokenParams("a", "a-turn-2",
			[6]float64{v, 0, 0, 1, 0, v + 1}, [6]float64{100, 0, 0, 1, 0, 101}))
	}
	require.Eventually(t, func() bool {
		if len(notes) > 0 {
			return true
		}
		send()
		return false
	}, 5*time.Second, 20*time.Millisecond, "the join never blocked in a save")
	for range 5 {
		send()
	}
	prev := -1
	require.Eventually(t, func() bool {
		cur := len(notes)
		stable := cur == prev
		prev = cur
		return stable
	}, 5*time.Second, 50*time.Millisecond)
	require.Greater(t, len(notes), 1)

	done := make(chan struct{})
	go func() {
		env.monitor.CloseAll()
		close(done)
	}()
	require.Eventually(t, func() bool {
		select {
		case <-j.stop:
			return true
		default:
			return false
		}
	}, 5*time.Second, 5*time.Millisecond)
	unlock()
	<-done

	assert.Len(t, tokenRecords(t, env.records(t, "a")), sent)
}

// leaveWith runs a join's leave with notes already queued, as a stop or
// the join limit finds them.
func (e *monitorEnv) leaveWith(t *testing.T, queued ...Notification) {
	t.Helper()
	j := &join{key: passKey, threadID: "a", id: "join-1", login: e.login()}
	notes := make(chan Notification, len(queued))
	for _, n := range queued {
		notes <- n
	}
	var buf []mirror.Record
	e.monitor.leave(j, notes, &buf, func() {
		_ = e.monitor.flush(j.key, j.threadID, buf, j.handled)
		buf = nil
	})
}

func TestLeavingWithAQueuedCompletionQueuesASync(t *testing.T) {
	env := newMonitorEnv(t)
	completed, err := json.Marshal(map[string]any{
		"threadId": "a", "turn": map[string]any{"id": "a-turn-2", "status": "completed", "items": []any{}},
	})
	require.NoError(t, err)

	env.leaveWith(t,
		Notification{Method: tokenUsageMethod, Params: tokenParams("a", "a-turn-2",
			[6]float64{100, 0, 0, 1, 0, 101}, [6]float64{100, 0, 0, 1, 0, 101})},
		Notification{Method: turnCompletedMethod, Params: completed},
	)

	require.Eventually(t, func() bool { return env.triggers.Load() == 1 }, 5*time.Second, 5*time.Millisecond,
		"the finished chat is synced now, not on the next schedule")
	assert.Equal(t, []int64{101}, tokenTotals(t, env.records(t, "a")))
}

func TestLeavingWithAQueuedUnreadableNoteStopsCapture(t *testing.T) {
	env := newMonitorEnv(t)

	env.leaveWith(t, Notification{Method: tokenUsageMethod, Params: jsontext.Value(`{"threadId":1}`)})

	assert.Contains(t, env.live(t), "Live tokens: stopped.")
}
