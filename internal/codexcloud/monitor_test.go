package codexcloud

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"log"
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
	assert.Equal(t, "Live tokens: watching 1 running threads", env.live(t))
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
	assert.Equal(t, "Live tokens: watching 0 running threads", env.live(t))

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
	assert.Contains(t, env.logs.String(), "had ended when AgentsView joined")
	assert.Equal(t, "Live tokens: watching 0 running threads (1 joins arrived after the turn ended)", env.live(t))

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
			assert.Equal(t, "Live tokens: stopped. "+Message(FailProtocol, time.Time{}), env.live(t))
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
	thread.UpdatedAt = fetchNow.Add(-3 * time.Minute).Unix()
	env.threads.Set(thread)

	env.join(t, "a")
	assert.Empty(t, env.host.Handshakes(), "an inProgress turn updated 3 minutes ago is never checked or resumed")
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
	require.NoError(t, m.flush(j, []mirror.Record{liveTokenRecord(t, j, 100, fetchNow)}))
	buf := []mirror.Record{
		liveTokenRecord(t, j, 200, fetchNow.Add(10*time.Second)),
		liveTokenRecord(t, j, 300, fetchNow.Add(20*time.Second)),
	}
	// An earlier flush of buf wrote its records, then failed to save the
	// thread's state, so the stored offset is still before them.
	st := env.thread(t, "a")
	_, err := mirror.Append(env.mirrorPath(t, "a"), st.Offset, buf)
	require.NoError(t, err)

	require.NoError(t, m.flush(j, buf))

	tokens := tokenRecords(t, env.records(t, "a"))
	totals := make([]int64, len(tokens))
	for i, rec := range tokens {
		totals[i] = rec.Total.Total
	}
	assert.Equal(t, []int64{100, 200, 300}, totals)
}
