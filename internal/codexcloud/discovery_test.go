package codexcloud

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/codexcloudtest"
	"go.kenn.io/agentsview/internal/codexcloud/mirror"
	"go.kenn.io/agentsview/internal/config"
)

// liveGates lets a test change the gates between discovery runs.
type liveGates struct {
	mu    sync.Mutex
	gates Gates
}

func (g *liveGates) get() Gates {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gates
}

func (g *liveGates) set(change func(*Gates)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	change(&g.gates)
}

func newDiscoveryEnv(t *testing.T) (*monitorEnv, *liveGates) {
	t.Helper()
	env := newMonitorEnv(t)
	gates := &liveGates{gates: Gates{
		CodexCloud: config.CodexCloudConfig{Present: true, Enabled: true, LiveTokens: true, Endpoint: env.host.URL()},
		Homes:      env.homes,
	}}
	env.monitor.Gates = gates.get
	env.syncer.Gates = gates.get
	return env, gates
}

func (e *monitorEnv) discover(t *testing.T) {
	t.Helper()
	require.NoError(t, e.monitor.Discover(t.Context()))
}

// turnChecks lists the threads whose newest turn was read, in order.
func (e *monitorEnv) turnChecks() []string {
	var ids []string
	for _, r := range e.host.Requests() {
		if r.Method != "thread/turns/list" {
			continue
		}
		var p struct {
			ThreadID string `json:"threadId"`
			Limit    int    `json:"limit"`
		}
		_ = json.Unmarshal(r.Params, &p)
		if p.Limit == 1 {
			ids = append(ids, p.ThreadID)
		}
	}
	return ids
}

func TestDiscoveryFindsIdleListedRunning(t *testing.T) {
	env, _ := newDiscoveryEnv(t)
	thread := runningThread("a", "warm")
	thread.Status = "idle"
	env.threads.Set(thread)

	env.discover(t)

	assert.Equal(t, []string{"a"}, env.monitor.Joined(passKey))
	for _, r := range env.host.Requests() {
		if r.Method == "thread/list" {
			assert.JSONEq(t, `{"limit":50,"sortKey":"updated_at","sortDirection":"desc","archived":false}`,
				string(r.Params), "the non-archived default listing, with no sourceKinds")
		}
	}
	env.discover(t)
	assert.Equal(t, 1, env.resumes(), "a joined thread is not joined again")
}

func TestDiscoveryIgnoresOldThreads(t *testing.T) {
	env, _ := newDiscoveryEnv(t)
	thread := runningThread("a", "warm")
	thread.UpdatedAt = fetchNow.Add(-6 * time.Minute).Unix()
	thread.Status = "active"
	env.threads.Set(thread)

	env.discover(t)

	assert.Empty(t, env.turnChecks(), "no turn check for a thread updated 6 minutes ago, even when listed active")
	assert.Zero(t, env.resumes())
}

func TestDiscoveryJoinsDelegatedChild(t *testing.T) {
	env, _ := newDiscoveryEnv(t)
	thread := runningThread("child", "warm")
	thread.ThreadSource = "aeon_child"
	thread.Source = map[string]string{"custom": "codex-cloud-agent"}
	env.threads.Set(thread)

	env.discover(t)

	assert.Equal(t, []string{"child"}, env.monitor.Joined(passKey))
}

func TestDiscoverySkipsSubAgents(t *testing.T) {
	env, _ := newDiscoveryEnv(t)
	for id, change := range map[string]func(*codexcloudtest.Thread){
		"parent": func(th *codexcloudtest.Thread) { th.ParentThreadID = "p" },
		"source": func(th *codexcloudtest.Thread) { th.ThreadSource = "subagent" },
		"spawned": func(th *codexcloudtest.Thread) {
			th.Source = map[string]any{"subAgent": map[string]any{"thread_spawn": map[string]string{"parent_thread_id": "p"}}}
		},
		"review": func(th *codexcloudtest.Thread) { th.Source = map[string]any{"subAgent": "review"} },
	} {
		thread := runningThread(id, "warm")
		change(&thread)
		env.threads.Set(thread)
	}

	env.discover(t)

	assert.Empty(t, env.turnChecks(), "a sub-agent thread gets no turn check")
	assert.Zero(t, env.resumes())
}

func TestDiscoverySecondPage(t *testing.T) {
	env, _ := newDiscoveryEnv(t)
	recent := fetchNow.Add(-time.Minute).Unix()
	for i := range listPageSize + 2 {
		env.threads.Set(finishedThread("r"+strconv.Itoa(i), recent))
	}
	for i := range listPageSize {
		env.threads.Set(finishedThread("o"+strconv.Itoa(i), fetchNow.Add(-time.Hour).Unix()))
	}

	env.discover(t)

	assert.Equal(t, 2, env.calls("thread/list"),
		"a first page of recent threads leads to a second; a page ending with an older thread is the last")
	assert.Len(t, env.turnChecks(), listPageSize+2)
}

func TestDiscoveryGatesCloseJoins(t *testing.T) {
	env, gates := newDiscoveryEnv(t)
	env.threads.Set(runningThread("a", "warm"))
	env.discover(t)
	require.Equal(t, []string{"a"}, env.monitor.Joined(passKey))

	gates.set(func(g *Gates) { g.CodexCloud.LiveTokens = false })
	env.discover(t)

	assert.Empty(t, env.monitor.Joined(passKey))
	require.Eventually(t, func() bool { return env.host.Joined("a") == 0 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, "Live tokens: off, because live_tokens = false under [agents.codex-cloud].", env.live(t))
	from := len(env.host.Requests())
	env.discover(t)
	assert.Len(t, env.host.Requests(), from, "live_tokens = false sends nothing")
}

func TestDiscoveryOffWithoutSourceSync(t *testing.T) {
	env, _ := newDiscoveryEnv(t)
	env.monitor.NoSync = true
	env.threads.Set(runningThread("a", "warm"))

	env.discover(t)

	assert.Empty(t, env.host.Handshakes())
	assert.Equal(t, "Live tokens: off, because this daemon does not sync sources.", env.live(t))
}

func TestDiscoveryOffWhenSyncIsOff(t *testing.T) {
	env, gates := newDiscoveryEnv(t)
	gates.set(func(g *Gates) { g.CodexCloud.Enabled = false })

	env.discover(t)

	assert.Equal(t, "Live tokens: off, because Codex cloud sync is off.", env.live(t))
}

func TestSync429HoldsDiscovery(t *testing.T) {
	env, _ := newDiscoveryEnv(t)
	env.threads.Set(runningThread("a", "warm"))
	env.host.RefuseHandshake(http.StatusTooManyRequests, http.Header{"Retry-After": {"600"}}, "")
	env.run(t)
	require.Equal(t, FailRateLimited.String(), env.account(t).Wait.Failure)
	handshakes := len(env.host.Handshakes())
	env.host.AcceptHandshakes()

	env.discover(t)

	assert.Len(t, env.host.Handshakes(), handshakes, "the account wait set by sync holds discovery")
}

func TestDiscovery5xxDelaysSync(t *testing.T) {
	env, _ := newDiscoveryEnv(t)
	env.threads.Set(runningThread("a", "warm"))
	env.host.RefuseHandshake(http.StatusServiceUnavailable, nil, "")

	env.discover(t)

	acct := env.account(t)
	assert.Equal(t, FailServer.String(), acct.Wait.Failure)
	handshakes := len(env.host.Handshakes())
	env.host.AcceptHandshakes()
	env.run(t)
	assert.Len(t, env.host.Handshakes(), handshakes, "the wait set by discovery holds sync")
}

func TestDiscoveryProtocolChangeStopsCaptureOnly(t *testing.T) {
	env, _ := newDiscoveryEnv(t)
	env.threads.Set(runningThread("a", "warm"))
	env.host.Handle("thread/turns/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		var p struct {
			Limit int `json:"limit"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Limit == 1 {
			return nil, &codexcloudtest.Error{Code: -32601, Message: "unsupported app-server method: thread/turns/list"}
		}
		return env.threads.TurnsList(params)
	})

	env.discover(t)

	assert.Contains(t, env.live(t), "Live tokens: stopped.")
	assert.Empty(t, env.account(t).Wait.Failure, "the sync job's wait is not set")
	from := env.calls("thread/list")
	env.discover(t)
	assert.Equal(t, from, env.calls("thread/list"), "capture stays off for the account")
	env.run(t)
	assert.Greater(t, env.calls("thread/list"), from, "sync carries on")
}

func TestDiscoveryThreadErrorSkipsThread(t *testing.T) {
	env, _ := newDiscoveryEnv(t)
	env.threads.Set(runningThread("a", "warm"))
	env.threads.Set(runningThread("b", "warm"))
	env.host.Handle("thread/turns/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(params, &p)
		if p.ThreadID == "a" {
			return nil, &codexcloudtest.Error{Code: -32600, Message: "no rollout found for thread id a"}
		}
		return env.threads.TurnsList(params)
	})

	env.discover(t)

	assert.Equal(t, []string{"b"}, env.monitor.Joined(passKey))
	assert.NotContains(t, env.live(t), "stopped")
}

func TestFailedFinalFlushIsSavedOnTheNextRun(t *testing.T) {
	env, _ := newDiscoveryEnv(t)
	env.threads.Set(runningThread("a", "warm"))
	env.discover(t)
	require.Equal(t, []string{"a"}, env.monitor.Joined(passKey))
	env.notify(t, "a", tokenUsageMethod, tokenParams("a", "a-turn-2", [6]float64{10, 0, 0, 5, 0, 15}, [6]float64{10, 0, 0, 5, 0, 15}))

	// A directory in place of the state file makes every save of the chat
	// fail until it is put back.
	state := filepath.Join(env.root, string(passKey), "a.state.json")
	require.NoError(t, os.Rename(state, state+".saved"))
	require.NoError(t, os.Mkdir(state, 0o700))
	env.complete(t, "a", "a-turn-2")
	env.waitLeft(t, "a")
	assert.Empty(t, tokenRecords(t, env.records(t, "a")))
	assert.Contains(t, env.live(t), "not saved yet")

	require.NoError(t, os.Remove(state))
	require.NoError(t, os.Rename(state+".saved", state))
	env.discover(t)

	records := env.records(t, "a")
	assert.Len(t, tokenRecords(t, records), 1)
	assert.Equal(t, mirror.KindTurnCompleted, records[len(records)-1].Kind)
	assert.Equal(t, "a-turn-2", env.thread(t, "a").LastHandledTurnID)
	assert.Equal(t, 1, env.resumes(), "the completed turn is not joined again")
	assert.NotContains(t, env.live(t), "not saved yet")
}

func TestPendingRecordsAreKeptWhileLiveTokensAreOff(t *testing.T) {
	env, gates := newDiscoveryEnv(t)
	j := &join{key: passKey, threadID: "a", id: "join-1"}
	restore := env.failSaves(t, "a")
	require.Error(t, env.monitor.flush(passKey, "a", []mirror.Record{liveTokenRecord(t, j, 100, fetchNow)}, ""))
	gates.set(func(g *Gates) { g.CodexCloud.LiveTokens = false })

	env.discover(t)

	live := env.live(t)
	assert.Contains(t, live, "live_tokens = false")
	assert.Contains(t, live, "Token counts for 1 chat are not saved yet")
	assert.Contains(t, live, "restarting it before a save works loses them")

	restore()
	gates.set(func(g *Gates) { g.CodexCloud.Present = false })
	env.discover(t)
	assert.Equal(t, []int64{100}, tokenTotals(t, env.records(t, "a")),
		"pending records are saved even when the source is no longer configured")
}

func TestDiscoverySuccessResetsAccountWait(t *testing.T) {
	env, _ := newDiscoveryEnv(t)
	env.host.RefuseHandshake(http.StatusServiceUnavailable, nil, "")
	env.discover(t)
	require.Equal(t, 1, env.account(t).Wait.Attempts)

	env.clock.Advance(2 * time.Minute)
	env.host.AcceptHandshakes()
	env.discover(t)
	assert.Empty(t, env.account(t).Wait.Failure, "a successful listing ends the wait")

	env.host.RefuseHandshake(http.StatusServiceUnavailable, nil, "")
	env.discover(t)
	wait := env.account(t).Wait
	assert.Equal(t, 1, wait.Attempts, "the next failure starts from the first wait")
	assert.Equal(t, env.clock.Now().Add(time.Minute), wait.Until)
}
