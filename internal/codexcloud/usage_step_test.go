package codexcloud

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
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

type usageEnv struct {
	*passEnv
	usage *codexcloudtest.UsageHost
}

func newUsageEnv(t *testing.T, homes ...string) *usageEnv {
	t.Helper()
	env := &usageEnv{passEnv: newPassEnv(t, homes...), usage: codexcloudtest.NewUsageHost(t, fetchNow.Add(-time.Hour))}
	endpoint, usageEndpoint, envHomes := env.host.URL(), env.usage.URL(), env.homes
	env.syncer.Gates = func() Gates {
		return Gates{
			CodexCloud: config.CodexCloudConfig{
				Present: true, Enabled: true, Endpoint: endpoint, UsageEndpoint: usageEndpoint,
			},
			Homes: envHomes,
		}
	}
	return env
}

func pct(f float64) *float64 { return new(f) }
func dec(s string) *string   { return new(s) }

type planRecords struct {
	rows     []mirror.PlanUsageRowData
	outcomes []mirror.PlanUsageOutcomeData
	kinds    []mirror.Kind
}

func (e *usageEnv) plan(t *testing.T, id string) planRecords {
	t.Helper()
	records, err := mirror.ReadAll(t.Context(), e.mirrorPath(t, id))
	require.NoError(t, err)
	var out planRecords
	for _, r := range records {
		switch r.Kind {
		case mirror.KindPlanUsageRow:
			var row mirror.PlanUsageRowData
			require.NoError(t, json.Unmarshal(r.Data, &row))
			out.rows = append(out.rows, row)
		case mirror.KindPlanUsageOutcome:
			var outcome mirror.PlanUsageOutcomeData
			require.NoError(t, json.Unmarshal(r.Data, &outcome))
			out.outcomes = append(out.outcomes, outcome)
		default:
			continue
		}
		out.kinds = append(out.kinds, r.Kind)
	}
	return out
}

func (e *usageEnv) planLine(t *testing.T) string {
	t.Helper()
	row, ok := e.syncer.Status.Row(string(passKey))
	require.True(t, ok)
	return row.PlanUsage
}

// requestedRoots lists the roots of every plan usage request since from.
func (e *usageEnv) requestedRoots(from int) [][]string {
	var out [][]string
	for _, r := range e.usage.Requests()[from:] {
		var roots []string
		for _, g := range r.Threads {
			roots = append(roots, g.ThreadID)
		}
		out = append(out, roots)
	}
	return out
}

func TestUsageRowStoredAfterImport(t *testing.T) {
	env := newUsageEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.usage.SetRow("a", codexcloudtest.Row("a", "available", pct(1.5), dec("0")))

	env.run(t)

	plan := env.plan(t, "a")
	require.Len(t, plan.rows, 1)
	assert.Equal(t, fetchNow.Add(-time.Hour), plan.rows[0].DataAsOf)
	assert.InDelta(t, 1.5, *plan.rows[0].Row.WeeklyPercent, 0)
	assert.False(t, plan.rows[0].Final)
	acct := env.account(t)
	assert.Empty(t, acct.UsagePending)
	assert.Equal(t, RefreshEntry{DataAsOf: fetchNow.Add(-time.Hour), LastAttempt: fetchNow, LastChange: fetchNow},
		acct.Refresh["a"])
	assert.Equal(t, "Plan usage as of "+fetchNow.Add(-time.Hour).Local().Format("Jan 2 15:04"), env.planLine(t))
	row, _ := env.syncer.Status.Row(string(passKey))
	assert.Equal(t, StateUpToDate, row.State)
}

func TestFailedUsageRetriedUntilUsable(t *testing.T) {
	env := newUsageEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.usage.Refuse(http.StatusInternalServerError, nil, "")

	env.run(t)
	acct := env.account(t)
	require.Contains(t, acct.UsagePending, "a")
	assert.Equal(t, fetchNow.Add(5*time.Minute), acct.UsagePending["a"].NextAttempt)
	assert.Equal(t, "Plan usage: "+Message(FailServer, time.Time{}), env.planLine(t))
	row, _ := env.syncer.Status.Row(string(passKey))
	assert.Equal(t, StateUpToDate, row.State, "a plan usage failure leaves thread status alone")

	env.clock.Advance(time.Minute)
	from := len(env.usage.Requests())
	env.run(t)
	assert.Len(t, env.usage.Requests(), from, "not before the next attempt")

	env.usage.Accept()
	env.usage.SetRow("a", codexcloudtest.Row("a", "available", pct(2), nil))
	env.clock.Set(fetchNow.Add(5 * time.Minute))
	env.run(t)
	assert.Len(t, env.plan(t, "a").rows, 1)
	assert.NotContains(t, env.account(t).UsagePending, "a")
}

func TestPartialRowBacksOffAndSettles(t *testing.T) {
	env := newUsageEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.usage.SetRow("a", codexcloudtest.Row("a", "partial", pct(1), nil))

	env.run(t)
	assert.Equal(t, fetchNow.Add(5*time.Minute), env.account(t).UsagePending["a"].NextAttempt)
	env.clock.Set(fetchNow.Add(5 * time.Minute))
	env.run(t)
	assert.Equal(t, fetchNow.Add(15*time.Minute), env.account(t).UsagePending["a"].NextAttempt, "unresolved attempts back off")
	assert.Len(t, env.plan(t, "a").rows, 1, "an unchanged row is not stored again")

	env.clock.Set(fetchNow.Add(7*24*time.Hour + time.Minute))
	env.run(t)
	plan := env.plan(t, "a")
	require.Len(t, plan.rows, 2)
	assert.True(t, plan.rows[1].Final, "a partial row settles 7 days after the last change")
	assert.InDelta(t, 1, *plan.rows[1].Row.WeeklyPercent, 0)
	assert.NotContains(t, env.account(t).UsagePending, "a")

	env.threads.Update("a", func(th *codexcloudtest.Thread) { th.UpdatedAt = env.clock.Now().Unix() - 600 })
	env.run(t)
	assert.Contains(t, env.account(t).UsagePending, "a", "a later thread change reopens the root")
}

func TestCouldNotCheckAfterSevenDaysOfFailures(t *testing.T) {
	env := newUsageEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.usage.SetRow("a", codexcloudtest.Row("a", "partial", pct(1), nil))
	env.run(t)

	changed := fetchNow.Add(time.Hour)
	env.clock.Set(changed)
	env.threads.Update("a", func(th *codexcloudtest.Thread) { th.UpdatedAt = changed.Unix() - 600 })
	env.usage.Refuse(http.StatusBadGateway, nil, "")
	for env.clock.Now().Before(changed.Add(8 * 24 * time.Hour)) {
		entry, ok := env.account(t).UsagePending["a"]
		if !ok {
			break
		}
		env.clock.Set(entry.NextAttempt)
		env.run(t)
	}

	assert.NotContains(t, env.account(t).UsagePending, "a", "retries stop")
	plan := env.plan(t, "a")
	require.NotEmpty(t, plan.outcomes)
	assert.Equal(t, mirror.OutcomeCouldNotCheck, plan.outcomes[len(plan.outcomes)-1].Kind)
	for _, row := range plan.rows {
		assert.False(t, row.Final, "the partial row stays unfinal")
	}
}

func TestLeftOutRootNotReportedThenSettles(t *testing.T) {
	env := newUsageEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.threads.Set(finishedThread("b", ago(2*time.Hour), message("n", "ho")))
	env.usage.SetRow("b", codexcloudtest.Row("b", "available", pct(3), nil))
	env.run(t)
	assert.Equal(t, []mirror.Kind{mirror.KindPlanUsageOutcome}, env.plan(t, "a").kinds)
	assert.Equal(t, mirror.OutcomeLeftOut, env.plan(t, "a").outcomes[0].Kind)

	// b re-enters with a usable row, then the report stops including it.
	env.usage.SetRow("b", nil)
	env.clock.Set(fetchNow.Add(7*24*time.Hour + time.Hour))
	for _, id := range []string{"a", "b"} {
		acct := env.account(t)
		entry := acct.UsagePending[id]
		if id == "b" {
			entry = PendingEntry{Since: fetchNow, NextAttempt: fetchNow, LastChange: fetchNow, HasRow: true}
			delete(acct.Refresh, "b")
		}
		entry.NextAttempt = fetchNow
		acct.UsagePending[id] = entry
		require.NoError(t, env.syncer.Store.SaveAccount(passKey, acct))
	}
	env.run(t)

	a := env.plan(t, "a")
	assert.Equal(t, mirror.OutcomeSettledNoRow, a.outcomes[len(a.outcomes)-1].Kind, "always left out: not reported")
	b := env.plan(t, "b")
	require.Len(t, b.rows, 2)
	assert.True(t, b.rows[1].Final, "a usable row left out at settling is stored again as final")
	assert.InDelta(t, 3, *b.rows[1].Row.WeeklyPercent, 0)
	assert.Empty(t, env.account(t).UsagePending)
}

// seedRoots makes roots usage-pending (or due for refresh), each with n
// mirrored descendants, without a thread host.
func (e *usageEnv) seedRoots(t *testing.T, ids []string, n int, refresh bool) {
	t.Helper()
	s := e.syncer.Store
	acct := e.account(t)
	for i, root := range ids {
		mirrored(t, s, root)
		var desc []string
		for d := range n {
			desc = append(desc, root+"-d"+strconv.Itoa(d))
		}
		require.NoError(t, s.SaveDescendants(passKey, root, desc))
		at := fetchNow.Add(time.Duration(i) * time.Minute)
		if refresh {
			acct.Refresh[root] = RefreshEntry{
				DataAsOf: at.Add(-48 * time.Hour), LastAttempt: at.Add(-25 * time.Hour), LastChange: fetchNow,
			}
		} else {
			acct.UsagePending[root] = PendingEntry{Since: at, NextAttempt: fetchNow, LastChange: fetchNow}
		}
	}
	require.NoError(t, s.SaveAccount(passKey, acct))
}

func TestUsageBudgetPendingFirst(t *testing.T) {
	env := newUsageEnv(t)
	// 500 descendants each: two roots exceed 1,000 IDs, so each request
	// carries one root.
	env.seedRoots(t, []string{"p1", "p2", "p3", "p4"}, 500, false)
	env.seedRoots(t, []string{"r1", "r2", "r3"}, 500, true)

	require.NoError(t, env.syncer.runUsage(t.Context(), env.syncer.Gates(), passKey, []Login{env.logins["/homes/a"]}))
	assert.Equal(t, [][]string{{"p1"}, {"p2"}, {"p3"}, {"p4"}, {"r1"}}, env.requestedRoots(0),
		"at most 5 requests, pending roots first")

	require.NoError(t, env.syncer.runUsage(t.Context(), env.syncer.Gates(), passKey, []Login{env.logins["/homes/a"]}))
	assert.Equal(t, [][]string{{"r2"}, {"r3"}}, env.requestedRoots(5), "leftover refresh roots run on the next pass")
}

func TestUsageBacklogPacksGroups(t *testing.T) {
	env := newUsageEnv(t)
	var ids []string
	for i := range 520 {
		ids = append(ids, "t"+strconv.Itoa(i))
	}
	env.seedRoots(t, ids, 0, false)

	require.NoError(t, env.syncer.runUsage(t.Context(), env.syncer.Gates(), passKey, []Login{env.logins["/homes/a"]}))

	requests := env.requestedRoots(0)
	require.Len(t, requests, usageBudget, "a large backlog sends at most 5 requests")
	for _, r := range requests {
		assert.Len(t, r, maxUsageGroups)
	}
	untried := 0
	for _, entry := range env.account(t).UsagePending {
		if entry.Attempts == 0 {
			untried++
		}
	}
	assert.Equal(t, 20, untried, "the rest wait for the next pass")
}

func TestOversizedGroupBoundary(t *testing.T) {
	env := newUsageEnv(t)
	env.seedRoots(t, []string{"fits"}, 999, false)
	env.seedRoots(t, []string{"huge"}, 1000, false)

	require.NoError(t, env.syncer.runUsage(t.Context(), env.syncer.Gates(), passKey, []Login{env.logins["/homes/a"]}))

	assert.Equal(t, [][]string{{"fits"}}, env.requestedRoots(0), "999 descendants and the root are 1,000 IDs")
	huge := env.plan(t, "huge")
	require.Len(t, huge.outcomes, 1)
	assert.Equal(t, mirror.OutcomeTooManyDescendants, huge.outcomes[0].Kind)
	assert.NotContains(t, env.account(t).UsagePending, "huge")
}

func TestNestedSubAgentsGroupedUnderRoot(t *testing.T) {
	env := newUsageEnv(t)
	root := finishedThread("p", ago(3*time.Hour), message("m", "root"))
	child := finishedThread("c", ago(2*time.Hour), message("n", "child"))
	child.ParentThreadID = "p"
	grand := finishedThread("gc", ago(time.Hour), message("o", "grandchild"))
	grand.ParentThreadID = "c"
	for _, th := range []codexcloudtest.Thread{root, child, grand} {
		env.threads.Set(th)
	}
	env.usage.SetRow("p", codexcloudtest.Row("p", "available", pct(4), nil))

	env.run(t)

	requests := env.usage.Requests()
	require.Len(t, requests, 1)
	require.Len(t, requests[0].Threads, 1, "child and grandchild are never their own group")
	assert.Equal(t, "p", requests[0].Threads[0].ThreadID)
	assert.ElementsMatch(t, []string{"c", "gc"}, requests[0].Threads[0].Descendants)
	for _, id := range []string{"c", "gc"} {
		plan := env.plan(t, id)
		require.NotEmpty(t, plan.outcomes)
		assert.Equal(t, mirror.PlanUsageOutcomeData{Kind: mirror.OutcomeCountedUnder, At: fetchNow, Root: "p"},
			plan.outcomes[len(plan.outcomes)-1], "the newest record names the final root")
	}

	env.clock.Advance(time.Hour)
	env.threads.Update("gc", func(th *codexcloudtest.Thread) { th.UpdatedAt = env.clock.Now().Unix() - 600 })
	env.run(t)
	assert.Equal(t, [][]string{{"p"}}, env.requestedRoots(1), "a grandchild change asks again for the root")
}

func TestRefreshAtLaterOfDataAsOfAndLastAttempt(t *testing.T) {
	env := newUsageEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.usage.SetDataAsOf(fetchNow.Add(-6 * time.Hour))
	env.usage.SetRow("a", codexcloudtest.Row("a", "available", pct(1), nil))
	env.run(t)
	require.Contains(t, env.account(t).Refresh, "a")

	env.clock.Set(fetchNow.Add(23 * time.Hour))
	from := len(env.usage.Requests())
	env.run(t)
	assert.Len(t, env.usage.Requests(), from,
		"data_as_of + 1 day has passed, but the last attempt was less than a day ago")

	env.usage.SetDataAsOf(fetchNow.Add(20 * time.Hour))
	env.usage.SetRow("a", codexcloudtest.Row("a", "available", pct(2), nil))
	env.clock.Set(fetchNow.Add(24 * time.Hour))
	env.run(t)
	assert.Len(t, env.usage.Requests(), from+1)
	assert.Len(t, env.plan(t, "a").rows, 2, "a newer row is stored")
	assert.Equal(t, fetchNow.Add(20*time.Hour), env.account(t).Refresh["a"].DataAsOf)

	env.clock.Set(fetchNow.Add(7*24*time.Hour + time.Minute))
	from = len(env.usage.Requests())
	env.run(t)
	assert.Len(t, env.usage.Requests(), from, "no refresh 7 days after the last change")
	assert.NotContains(t, env.account(t).Refresh, "a")
}

func TestUsageUnavailableAndRateLimited(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			env := newUsageEnv(t)
			env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
			env.usage.Refuse(status, nil, "")

			env.run(t)

			assert.Equal(t, "Usage isn't available for this account", env.planLine(t))
			plan := env.plan(t, "a")
			require.Len(t, plan.outcomes, 1)
			assert.Equal(t, mirror.OutcomeAccountUnavailable, plan.outcomes[0].Kind)
			row, _ := env.syncer.Status.Row(string(passKey))
			assert.Equal(t, StateUpToDate, row.State, "thread sync status is unchanged")
			from := len(env.usage.Requests())
			env.clock.Advance(23 * time.Hour)
			env.run(t)
			assert.Len(t, env.usage.Requests(), from, "no usage requests for 24 hours")
		})
	}
	t.Run("429", func(t *testing.T) {
		env := newUsageEnv(t)
		env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
		env.usage.Refuse(http.StatusTooManyRequests, http.Header{"Retry-After": {"600"}}, "")

		env.run(t)

		assert.Equal(t, UsagePauseState{Until: fetchNow.Add(10 * time.Minute), Reason: pauseRateLimited, Attempts: 1},
			env.account(t).UsagePause)
		env.usage.Accept()
		env.clock.Advance(9 * time.Minute)
		from := len(env.usage.Requests())
		env.run(t)
		assert.Len(t, env.usage.Requests(), from)
		env.clock.Advance(2 * time.Minute)
		env.run(t)
		assert.Greater(t, len(env.usage.Requests()), from)
	})
}

func TestUsage401UsesNextHome(t *testing.T) {
	env := newUsageEnv(t, "/homes/a", "/homes/b")
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.usage.SetRow("a", codexcloudtest.Row("a", "available", pct(1), nil))
	env.usage.RefuseBearer("token-0", http.StatusUnauthorized)

	env.run(t)

	requests := env.usage.Requests()
	require.Len(t, requests, 2)
	assert.Equal(t, "Bearer token-1", requests[1].Header.Get("Authorization"))
	assert.Len(t, env.plan(t, "a").rows, 1)
	row, _ := env.syncer.Status.Row(string(passKey))
	assert.Equal(t, StateUpToDate, row.State, "a plan usage 401 does not touch thread sync")
	assert.True(t, env.syncer.Waits.HomeReady("/homes/a", env.logins["/homes/a"].AuthModTime),
		"a plan usage 401 does not start the thread host's wait")
}

func TestUsage401OnEveryHomePausesUntilLoginChanges(t *testing.T) {
	env := newUsageEnv(t, "/homes/a", "/homes/b")
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.usage.RefuseBearer("token-0", http.StatusUnauthorized)
	env.usage.RefuseBearer("token-1", http.StatusUnauthorized)

	env.run(t)
	assert.Equal(t, "Plan usage: signed out of ChatGPT in this Codex home. Run `codex login`.", env.planLine(t))
	from := len(env.usage.Requests())
	env.clock.Advance(10 * time.Minute)
	env.run(t)
	assert.Len(t, env.usage.Requests(), from, "paused until a login changes")

	login := env.logins["/homes/b"]
	login.AuthModTime = env.clock.Now()
	login.Token = NewToken("token-2")
	env.logins["/homes/b"] = login
	env.usage.SetRow("a", codexcloudtest.Row("a", "available", pct(1), nil))
	env.run(t)
	assert.Len(t, env.plan(t, "a").rows, 1, "a new login on any home on the key resumes plan usage")
}

func TestUsageRunsWhileThreadHostWaits(t *testing.T) {
	env := newUsageEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.usage.Refuse(http.StatusInternalServerError, nil, "")
	env.run(t)
	env.usage.Accept()
	env.usage.SetRow("a", codexcloudtest.Row("a", "available", pct(1), nil))
	acct := env.account(t)
	require.NoError(t, env.syncer.Waits.RecordAccount(&acct, FailRateLimited, time.Hour))
	require.NoError(t, env.syncer.Store.SaveAccount(passKey, acct))
	handshakes := len(env.host.Handshakes())

	env.clock.Advance(5 * time.Minute)
	env.run(t)

	assert.Len(t, env.host.Handshakes(), handshakes, "the thread host is still waiting")
	assert.Len(t, env.plan(t, "a").rows, 1, "plan usage ran anyway")
}

func TestRowStoredAgainAfterNewerOutcome(t *testing.T) {
	env := newUsageEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	row := mirror.PlanUsageRowData{Row: mirror.UsageRow{ThreadID: "a", DataStatus: "available"}, DataAsOf: fetchNow}
	env.run(t)
	require.NoError(t, env.syncer.appendPlan(passKey, "a", fetchNow, mirror.KindPlanUsageRow, row))
	// A crash after appending an outcome, before its state was saved.
	outcome, err := planRecord(mirror.KindPlanUsageOutcome,
		mirror.PlanUsageOutcomeData{Kind: mirror.OutcomeLeftOut, At: fetchNow}, fetchNow)
	require.NoError(t, err)
	st, err := env.syncer.Store.LoadThread(passKey, "a")
	require.NoError(t, err)
	_, err = mirror.Append(env.mirrorPath(t, "a"), st.Offset, []mirror.Record{outcome})
	require.NoError(t, err)

	require.NoError(t, env.syncer.appendPlan(passKey, "a", fetchNow, mirror.KindPlanUsageRow, row))

	plan := env.plan(t, "a")
	assert.Equal(t, []mirror.Kind{
		mirror.KindPlanUsageOutcome, mirror.KindPlanUsageRow, mirror.KindPlanUsageOutcome, mirror.KindPlanUsageRow,
	}, plan.kinds, "the recovered outcome is newest, so the same row is stored again")
}

func TestRefreshRequestSkipsUsage(t *testing.T) {
	// Two homes, so the other home could still serve plan usage.
	env := newUsageEnv(t, "/homes/a", "/homes/b")
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.usage.Refuse(http.StatusInternalServerError, nil, "")
	env.run(t)
	require.Contains(t, env.account(t).UsagePending, "a")
	env.usage.Accept()
	env.clock.Advance(5 * time.Minute)
	env.threads.Set(finishedThread("b", ago(time.Minute), message("n", "ho")))
	var once sync.Once
	env.host.Handle("thread/turns/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		once.Do(func() { _ = env.host.ServerRequest(t.Context(), refreshMethod, map[string]any{}) })
		return env.threads.TurnsList(params)
	})
	from := len(env.usage.Requests())

	env.run(t)

	assert.Len(t, env.usage.Requests(), from, "the pass that saw a refresh request ends before plan usage")
	assert.Contains(t, env.account(t).UsagePending, "a", "the due root waits for a later pass")
}

// TestLostAccountSaveDoesNotQueryMovedThread covers a crash after a
// waiting sub-agent moved under its parent's root but before the account
// state was saved: the sub-agent is still in the refresh set, yet it must
// not be sent as a root of its own.
func TestLostAccountSaveDoesNotQueryMovedThread(t *testing.T) {
	env := newUsageEnv(t)
	grand := finishedThread("gc", ago(2*time.Hour), message("o", "grandchild"))
	grand.ParentThreadID = "c"
	env.threads.Set(grand)
	env.usage.SetRow("gc", codexcloudtest.Row("gc", "available", pct(1), nil))
	env.run(t)
	require.Contains(t, env.account(t).Refresh, "gc", "gc is its own root until its parent arrives")
	before := env.account(t)

	env.clock.Advance(time.Hour)
	child := finishedThread("c", ago(time.Minute), message("n", "child"))
	env.threads.Set(child)
	env.usage.SetRow("c", codexcloudtest.Row("c", "available", pct(2), nil))
	env.run(t)
	require.NoError(t, env.syncer.Store.SaveAccount(passKey, before), "the account save is lost")

	env.clock.Advance(25 * time.Hour)
	from := len(env.usage.Requests())
	env.run(t)

	for _, roots := range env.requestedRoots(from) {
		assert.NotContains(t, roots, "gc", "a thread counted under another root is never its own root")
	}
	assert.NotContains(t, env.account(t).Refresh, "gc")
	assert.NotContains(t, env.account(t).UsagePending, "gc")
	plan := env.plan(t, "gc")
	require.NotEmpty(t, plan.kinds)
	assert.Equal(t, mirror.KindPlanUsageOutcome, plan.kinds[len(plan.kinds)-1])
	assert.Equal(t, mirror.OutcomeCountedUnder, plan.outcomes[len(plan.outcomes)-1].Kind)
}

// TestRestartShowsStoredPlanUsageDate checks that a new daemon, which has
// sent no plan usage request yet, shows the newest stored report date
// rather than "not checked yet".
func TestRestartShowsStoredPlanUsageDate(t *testing.T) {
	env := newUsageEnv(t)
	assert.Empty(t, env.planLineOrEmpty(t), "no row before the first pass")
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.usage.SetRow("a", codexcloudtest.Row("a", "available", pct(1), nil))
	env.run(t)
	asOf := fetchNow.Add(-time.Hour)

	env.syncer = &Syncer{
		Store: env.syncer.Store, Waits: NewWaits(env.clock.Now), Status: NewStatusBoard(), Now: env.clock.Now,
		Dial: Dial, Gates: env.syncer.Gates, ReadLogin: env.syncer.ReadLogin, Version: "test", Log: env.syncer.Log,
	}
	env.clock.Advance(time.Hour)
	env.run(t)

	assert.Equal(t, "Plan usage as of "+asOf.Local().Format("Jan 2 15:04"), env.planLine(t))
}

func TestFreshAccountShowsNotCheckedYet(t *testing.T) {
	env := newUsageEnv(t)
	env.usage.Refuse(http.StatusInternalServerError, nil, "")
	require.NoError(t, env.syncer.runUsage(t.Context(), env.syncer.Gates(), passKey, nil))
	env.syncer.Status.Set(HomeStatus{Key: string(passKey)})
	env.syncer.showUsage(passKey, fetchNow)
	assert.Equal(t, "Plan usage: not checked yet", env.planLine(t))
}

func (e *usageEnv) planLineOrEmpty(t *testing.T) string {
	t.Helper()
	row, _ := e.syncer.Status.Row(string(passKey))
	return row.PlanUsage
}

func TestStoredUsageLineWithOnlyPartialRows(t *testing.T) {
	acct := AccountState{UsagePending: map[string]PendingEntry{"a": {HasRow: true}, "b": {}}}
	assert.Equal(t, "Plan usage: still updating", storedUsageLine(acct))
}

// TestCouldNotCheckAfterEarlyAnswerThenFailures covers a chat answered
// once soon after its last change, then failing on every later request:
// its retries stop after the settle point instead of running forever.
func TestCouldNotCheckAfterEarlyAnswerThenFailures(t *testing.T) {
	env := newUsageEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.usage.SetRow("a", codexcloudtest.Row("a", "partial", pct(1), nil))
	env.run(t)
	require.Contains(t, env.account(t).UsagePending, "a")

	env.usage.Refuse(http.StatusBadGateway, nil, "")
	for env.clock.Now().Before(fetchNow.Add(9 * 24 * time.Hour)) {
		entry, ok := env.account(t).UsagePending["a"]
		if !ok {
			break
		}
		env.clock.Set(entry.NextAttempt)
		env.run(t)
	}

	assert.NotContains(t, env.account(t).UsagePending, "a", "retries stop")
	plan := env.plan(t, "a")
	require.NotEmpty(t, plan.outcomes)
	assert.Equal(t, mirror.OutcomeCouldNotCheck, plan.outcomes[len(plan.outcomes)-1].Kind)
	for _, row := range plan.rows {
		assert.False(t, row.Final, "the partial row stays unfinal")
	}
}

// TestSettledLeftOutRootUsesRecoveredRow covers a crash after a row was
// appended but before the thread state was saved: settling a root the
// report leaves out stores that newest row as final, not the older one
// the saved state still names.
func TestSettledLeftOutRootUsesRecoveredRow(t *testing.T) {
	env := newUsageEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.usage.SetRow("a", codexcloudtest.Row("a", "available", pct(1), nil))
	env.run(t)
	newer, err := planRecord(mirror.KindPlanUsageRow, mirror.PlanUsageRowData{
		Row:      mirror.UsageRow{ThreadID: "a", DataStatus: "available", WeeklyPercent: pct(2)},
		DataAsOf: fetchNow,
	}, fetchNow)
	require.NoError(t, err)
	st, err := env.syncer.Store.LoadThread(passKey, "a")
	require.NoError(t, err)
	_, err = mirror.Append(env.mirrorPath(t, "a"), st.Offset, []mirror.Record{newer})
	require.NoError(t, err)

	acct := env.account(t)
	delete(acct.Refresh, "a")
	acct.UsagePending["a"] = PendingEntry{Since: fetchNow, NextAttempt: fetchNow, LastChange: fetchNow, HasRow: true}
	require.NoError(t, env.syncer.Store.SaveAccount(passKey, acct))
	env.usage.SetRow("a", nil)
	env.clock.Set(fetchNow.Add(7*24*time.Hour + time.Hour))
	env.run(t)

	plan := env.plan(t, "a")
	require.NotEmpty(t, plan.rows)
	last := plan.rows[len(plan.rows)-1]
	assert.True(t, last.Final)
	assert.InDelta(t, 2, *last.Row.WeeklyPercent, 0, "the recovered row is the newest")
}

// TestNoUsableHomeNamesTheLoginProblem covers plan usage skipped because
// the only Codex home began waiting on a login problem other than a
// sign-out during the pass, as when live capture gets a token refresh
// request.
func TestNoUsableHomeNamesTheLoginProblem(t *testing.T) {
	env := newUsageEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	login := env.logins["/homes/a"]
	env.host.Handle("thread/turns/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		env.syncer.Waits.RecordHome(login.Home, FailRefreshRequested, login.AuthModTime)
		return env.threads.TurnsList(params)
	})

	env.run(t)

	assert.Empty(t, env.usage.Requests())
	assert.Equal(t, "Plan usage: "+Message(FailRefreshRequested, time.Time{}), env.planLine(t))
}
