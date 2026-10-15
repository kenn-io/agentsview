package codexcloud

import (
	"bytes"
	"encoding/json/jsontext"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/codexcloudtest"
	"go.kenn.io/agentsview/internal/codexcloud/mirror"
)

// loseAccount removes account.json, or replaces it with bytes that do not
// decode.
func (e *usageEnv) loseAccount(t *testing.T, corrupt bool) {
	t.Helper()
	path, err := e.syncer.Store.path(passKey, "account", ".json")
	require.NoError(t, err)
	if corrupt {
		require.NoError(t, os.WriteFile(path, []byte("{garbage"), 0o600))
		return
	}
	require.NoError(t, os.Remove(path))
}

// requestedSince lists every root asked about in plan usage requests
// since from, sorted.
func (e *usageEnv) requestedSince(from int) []string {
	var roots []string
	for _, r := range e.requestedRoots(from) {
		roots = append(roots, r...)
	}
	slices.Sort(roots)
	return roots
}

func TestLostAccountStateRebuildsPlanUsageQueue(t *testing.T) {
	env := newUsageEnv(t)
	for _, id := range []string{"partial", "available", "leftout"} {
		env.threads.Set(finishedThread(id, ago(time.Hour), message(id+"-m", "hi")))
	}
	env.usage.SetRow("partial", codexcloudtest.Row("partial", "partial", pct(1), nil))
	env.usage.SetRow("available", codexcloudtest.Row("available", "available", pct(2), nil))
	env.run(t)
	acct := env.account(t)
	require.Contains(t, acct.UsagePending, "partial")
	require.Contains(t, acct.UsagePending, "leftout")
	require.Contains(t, acct.Refresh, "available")

	env.loseAccount(t, false)
	env.clock.Advance(time.Hour)
	from, fetchFrom := len(env.usage.Requests()), len(env.host.Requests())
	env.run(t)

	assert.Equal(t, []string{"leftout", "partial"}, env.requestedSince(from),
		"every pending root is asked about again; the complete row waits for its refresh")
	assert.Empty(t, env.fetchedThreads(fetchFrom), "no chat is fetched again")
	acct = env.account(t)
	assert.Equal(t, fetchNow, acct.UsagePending["partial"].LastChange, "the stored change time, not now")
	assert.Equal(t, fetchNow, acct.UsagePending["leftout"].LastChange)
	assert.Equal(t, fetchNow, acct.Refresh["available"].LastChange)

	// Lost again after the settle point: each root settles on its first
	// answer.
	env.loseAccount(t, true)
	env.clock.Set(fetchNow.Add(settleAfter + time.Hour))
	from = len(env.usage.Requests())
	env.run(t)
	assert.Equal(t, []string{"leftout", "partial"}, env.requestedSince(from),
		"the complete row drops out with no request")
	partial := env.plan(t, "partial")
	assert.True(t, partial.rows[len(partial.rows)-1].Final, "the partial row settles")
	leftOut := env.plan(t, "leftout")
	assert.Equal(t, mirror.OutcomeSettledNoRow, leftOut.outcomes[len(leftOut.outcomes)-1].Kind)
	acct = env.account(t)
	assert.NotContains(t, acct.UsagePending, "partial")
	assert.NotContains(t, acct.UsagePending, "leftout")
	assert.NotContains(t, acct.Refresh, "available")

	// Settled roots stay settled when the account state is lost once more.
	env.loseAccount(t, false)
	env.clock.Advance(time.Hour)
	from = len(env.usage.Requests())
	env.run(t)
	requested := env.requestedSince(from)
	assert.NotContains(t, requested, "partial")
	assert.NotContains(t, requested, "leftout")
}

// TestFinishedChatListedActiveKeepsItsSchedule covers a finished chat the
// listing still shows as active: it is fetched every pass, yet fetches
// that find nothing new leave its plan usage schedule alone.
func TestFinishedChatListedActiveKeepsItsSchedule(t *testing.T) {
	for _, status := range []string{"available", "partial"} {
		t.Run(status, func(t *testing.T) {
			env := newUsageEnv(t)
			thread := finishedThread("a", ago(time.Hour), message("m", "hi"))
			thread.Status = "active"
			env.threads.Set(thread)
			env.usage.SetRow("a", codexcloudtest.Row("a", status, pct(1), nil))

			// Past the settle point by more than the longest retry wait.
			for env.clock.Now().Before(fetchNow.Add(settleAfter + usageRetryLimit + 2*time.Hour)) {
				env.run(t)
				env.clock.Advance(4 * time.Hour)
			}

			assert.Greater(t, len(env.fetchedThreads(0)), 40, "the chat is fetched every pass")
			requests := len(env.usage.Requests())
			if status == "available" {
				// The first answer, then one refresh a day until 7 days
				// after the import.
				assert.Equal(t, 7, requests, "asked on the refresh schedule")
			} else {
				assert.Less(t, requests, 40, "unresolved answers back off, up to 6 hours apart")
				plan := env.plan(t, "a")
				assert.True(t, plan.rows[len(plan.rows)-1].Final, "the partial row settles 7 days after the import")
			}
			acct := env.account(t)
			assert.NotContains(t, acct.UsagePending, "a")
			assert.NotContains(t, acct.Refresh, "a")
			from := len(env.usage.Requests())
			env.run(t)
			assert.Empty(t, env.requestedRoots(from), "a settled chat is not asked about again")

			env.threads.Update("a", func(th *codexcloudtest.Thread) {
				th.UpdatedAt = env.clock.Now().Unix() - 60
				th.Turns[0].Items = append(th.Turns[0].Items, message("m2", "more"))
			})
			from = len(env.usage.Requests())
			env.run(t)
			assert.Equal(t, [][]string{{"a"}}, env.requestedRoots(from), "a real change asks about the chat again")
			acct = env.account(t)
			lastChange := acct.Refresh["a"].LastChange
			if status == "partial" {
				lastChange = acct.UsagePending["a"].LastChange
			}
			assert.Equal(t, env.clock.Now(), lastChange, "the 7 days start again from the change")
		})
	}
}

// TestChangeLostWithTheAccountSaveIsMarkedAgain covers a crash after a
// fetch committed a chat's change but before the account state that marks
// its root pending was saved: the next fetch, which finds nothing new,
// still marks the root.
func TestChangeLostWithTheAccountSaveIsMarkedAgain(t *testing.T) {
	env := newUsageEnv(t)
	thread := finishedThread("a", ago(time.Hour), message("m", "hi"))
	thread.Status = "active"
	env.threads.Set(thread)
	env.usage.SetRow("a", codexcloudtest.Row("a", "available", pct(1), nil))
	env.run(t)
	require.Equal(t, fetchNow, env.account(t).Refresh["a"].LastChange)
	before := env.account(t)
	st, err := env.syncer.Store.LoadThread(passKey, "a")
	require.NoError(t, err)
	usageChange := st.UsageChange

	env.clock.Advance(time.Hour)
	changed := env.clock.Now()
	env.threads.Update("a", func(th *codexcloudtest.Thread) {
		th.UpdatedAt = changed.Unix() - 60
		th.Turns[0].Items = append(th.Turns[0].Items, message("m2", "more"))
	})
	env.run(t)
	// The crash: the account save and the mark saved after it are lost;
	// the chat's committed state stays.
	require.NoError(t, env.syncer.Store.SaveAccount(passKey, before))
	st, err = env.syncer.Store.LoadThread(passKey, "a")
	require.NoError(t, err)
	st.UsageChange = usageChange
	require.NoError(t, env.syncer.Store.SaveThread(passKey, "a", st))

	env.clock.Advance(5 * time.Minute)
	from := len(env.usage.Requests())
	env.run(t)

	assert.Equal(t, [][]string{{"a"}}, env.requestedRoots(from), "the root is asked about again")
	assert.Equal(t, changed, env.account(t).Refresh["a"].LastChange, "with the committed change time")
}

// TestItemsAppendedBeforeACrashMarkTheRootAgain covers a crash after a
// fetch appended new items to the mirror file but before it saved the
// chat's state or the account. The listing time did not move, so only the
// recovered items show the change, and they must mark the root again.
func TestItemsAppendedBeforeACrashMarkTheRootAgain(t *testing.T) {
	env := newUsageEnv(t)
	thread := finishedThread("a", ago(time.Hour), message("m", "hi"))
	thread.Status = "active"
	env.threads.Set(thread)
	env.usage.SetRow("a", codexcloudtest.Row("a", "available", pct(1), nil))
	env.run(t)
	require.Equal(t, fetchNow, env.account(t).Refresh["a"].LastChange)
	before := env.account(t)
	saved, err := env.syncer.Store.LoadThread(passKey, "a")
	require.NoError(t, err)

	env.clock.Advance(time.Hour)
	appended := env.clock.Now()
	env.threads.Update("a", func(th *codexcloudtest.Thread) {
		th.Turns[0].Items = append(th.Turns[0].Items, message("m2", "more"))
	})
	env.run(t)
	// The crash: the new items stay in the mirror file, but the chat's
	// state and the account are as they were before the fetch.
	require.NoError(t, env.syncer.Store.SaveThread(passKey, "a", saved))
	require.NoError(t, env.syncer.Store.SaveAccount(passKey, before))

	env.clock.Advance(5 * time.Minute)
	from := len(env.usage.Requests())
	env.run(t)

	assert.Equal(t, [][]string{{"a"}}, env.requestedRoots(from), "the root is asked about again")
	assert.Equal(t, appended, env.account(t).Refresh["a"].LastChange, "with the recovered items' fetch time")
}

// TestFailingHistoryKeepsTheRootsChangeTime covers a chat whose history
// read keeps failing while its listing time stays put: each retry leaves
// the root's last change at the listing time, so the root settles 7 days
// after it, and only a newer listing time marks it again.
func TestFailingHistoryKeepsTheRootsChangeTime(t *testing.T) {
	env := newUsageEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.usage.SetRow("a", codexcloudtest.Row("a", "partial", pct(1), nil))
	fail, mu := true, &sync.Mutex{}
	env.failThread("a", &fail, mu)
	listed := time.Unix(ago(time.Hour), 0)

	env.run(t)
	require.Contains(t, env.account(t).Retry, "a")
	require.WithinDuration(t, listed, env.account(t).UsagePending["a"].LastChange, 0)
	for range 4 {
		env.clock.Advance(time.Hour)
		env.run(t)
		assert.WithinDuration(t, listed, env.account(t).UsagePending["a"].LastChange, 0, "a failed retry is not a change")
	}

	env.clock.Set(listed.Add(settleAfter + 30*time.Minute))
	env.run(t)
	require.Contains(t, env.account(t).Retry, "a", "the history read still fails")
	assert.NotContains(t, env.account(t).UsagePending, "a", "the root settles 7 days after the listing time")

	env.clock.Advance(time.Hour)
	relisted := env.clock.Now().Add(-time.Minute).Truncate(time.Second)
	env.threads.Update("a", func(th *codexcloudtest.Thread) { th.UpdatedAt = relisted.Unix() })
	env.run(t)
	assert.WithinDuration(t, relisted, env.account(t).UsagePending["a"].LastChange, 0, "a newer listing time marks the root again")
}

// TestLostAccountStateRestoresFollowUpFetches covers chats that were due
// to be fetched again when account.json was lost: one with a turn still
// running, and one updated seconds ago whose late items may not be saved
// yet. Neither has a newer listing time, so only the rebuild brings them
// back.
func TestLostAccountStateRestoresFollowUpFetches(t *testing.T) {
	env := newUsageEnv(t)
	running := finishedThread("running", ago(time.Hour), message("r", "hi"))
	running.Turns[0].Status = "inProgress"
	env.threads.Set(running)
	recent := finishedThread("recent", ago(30*time.Second), message("n", "hi"))
	recent.Turns[0].CompletedAt = ago(time.Hour)
	env.threads.Set(recent)
	env.threads.Set(finishedThread("settled", ago(time.Hour), message("s", "hi")))
	env.run(t)
	acct := env.account(t)
	require.Contains(t, acct.Unfinished, "running")
	require.True(t, acct.CheckAgain["recent"])

	env.loseAccount(t, false)
	env.clock.Advance(30 * time.Second)
	from := len(env.host.Requests())
	env.run(t)

	var fetched []string
	for _, r := range env.host.Requests()[from:] {
		if r.Method != "thread/turns/list" {
			continue
		}
		for _, id := range []string{"running", "recent", "settled"} {
			if strings.Contains(string(r.Params), `"`+id+`"`) {
				fetched = append(fetched, id)
			}
		}
	}
	slices.Sort(fetched)
	assert.Equal(t, []string{"recent", "running"}, slices.Compact(fetched),
		"both waiting chats are fetched again; the settled one is not")
}

// TestDamagedDescendantsListIsRebuilt covers a root whose descendants
// file does not decode: its next plan usage request still sends the
// sub-agent chats, taken from the chats that count under it.
func TestDamagedDescendantsListIsRebuilt(t *testing.T) {
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
	refresh := env.account(t).Refresh["p"]
	require.False(t, refresh.DataAsOf.IsZero())

	path, err := env.syncer.Store.path(passKey, "p", ".descendants.json")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("{garbage"), 0o600))
	env.clock.Set(refreshDue(refresh))
	from := len(env.usage.Requests())
	env.run(t)

	requests := env.usage.Requests()[from:]
	require.Len(t, requests, 1)
	require.Len(t, requests[0].Threads, 1)
	assert.Equal(t, "p", requests[0].Threads[0].ThreadID)
	assert.Equal(t, []string{"c", "gc"}, requests[0].Threads[0].Descendants, "the sub-agent chats are still sent")
	ids, err := env.syncer.Store.LoadDescendants(passKey, "p")
	require.NoError(t, err)
	assert.Equal(t, []string{"c", "gc"}, ids, "the rebuilt list is saved")
}

// TestLostAccountStateRestoresRefreshRoots covers a root whose newest
// plan usage record is a complete row: after account.json is lost it
// goes back on the refresh schedule, not the pending queue, so it is not
// asked about early, and it drops out without a request once its chat
// has gone 7 days without a change.
func TestLostAccountStateRestoresRefreshRoots(t *testing.T) {
	env := newUsageEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.usage.SetRow("a", codexcloudtest.Row("a", "available", pct(2), nil))
	env.run(t)
	dataAsOf := env.account(t).Refresh["a"].DataAsOf
	require.False(t, dataAsOf.IsZero())

	env.loseAccount(t, false)
	env.clock.Advance(time.Hour)
	from := len(env.usage.Requests())
	env.run(t)
	assert.Empty(t, env.requestedRoots(from), "a complete row is not asked about before its refresh is due")
	acct := env.account(t)
	assert.NotContains(t, acct.UsagePending, "a")
	assert.Equal(t, RefreshEntry{DataAsOf: dataAsOf, LastChange: fetchNow}, acct.Refresh["a"])

	env.clock.Set(dataAsOf.Add(refreshEvery))
	env.run(t)
	assert.Equal(t, [][]string{{"a"}}, env.requestedRoots(from), "refreshed a day after its data")

	// Lost after the settle point, with the server now answering partial:
	// the root drops out with no request, and its complete row stays the
	// newest.
	env.usage.SetRow("a", codexcloudtest.Row("a", "partial", pct(3), nil))
	env.loseAccount(t, true)
	env.clock.Set(fetchNow.Add(settleAfter + time.Hour))
	from = len(env.usage.Requests())
	env.run(t)
	assert.Empty(t, env.requestedRoots(from))
	assert.NotContains(t, env.account(t).Refresh, "a")
	plan := env.plan(t, "a")
	last := plan.rows[len(plan.rows)-1]
	assert.Equal(t, mirror.UsageAvailable, last.Row.DataStatus)
	assert.False(t, last.Final)
}

// TestLostAccountStateKeepsAnOutstandingCheck covers a root with a
// complete row that changed after the row was fetched, whose request for
// that change failed before account.json was lost. The rebuild must keep
// the check outstanding, so a restart more than 7 days after the change
// still asks once, where the refresh schedule would drop it unasked.
func TestLostAccountStateKeepsAnOutstandingCheck(t *testing.T) {
	env := newUsageEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.usage.SetRow("a", codexcloudtest.Row("a", "available", pct(1), nil))
	env.run(t)
	require.Contains(t, env.account(t).Refresh, "a")

	env.clock.Advance(time.Hour)
	changed := env.clock.Now()
	env.threads.Update("a", func(th *codexcloudtest.Thread) {
		th.UpdatedAt = changed.Unix()
		th.Turns[0].Items = append(th.Turns[0].Items, message("m2", "more"))
	})
	env.usage.Refuse(http.StatusInternalServerError, nil, "")
	env.run(t)
	require.Contains(t, env.account(t).UsagePending, "a")

	env.usage.Accept()
	env.usage.SetRow("a", codexcloudtest.Row("a", "available", pct(2), nil))
	rows := len(env.plan(t, "a").rows)
	env.loseAccount(t, false)
	env.clock.Set(changed.Add(settleAfter + time.Hour))
	from := len(env.usage.Requests())
	env.run(t)

	assert.Equal(t, [][]string{{"a"}}, env.requestedRoots(from), "the outstanding check is sent")
	assert.Len(t, env.plan(t, "a").rows, rows+1, "the answer is stored")
}

// TestLostAccountStateTrustsAnIdenticalAnswer covers a check after a
// chat changed that OpenAI answered with exactly the stored row. That
// answer covers the change, so a rebuild past the settle point must not
// ask again.
func TestLostAccountStateTrustsAnIdenticalAnswer(t *testing.T) {
	env := newUsageEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.usage.SetRow("a", codexcloudtest.Row("a", "available", pct(1), nil))
	env.run(t)

	env.clock.Advance(time.Hour)
	changed := env.clock.Now()
	env.threads.Update("a", func(th *codexcloudtest.Thread) {
		th.UpdatedAt = changed.Unix()
		th.Turns[0].Items = append(th.Turns[0].Items, message("m2", "more"))
	})
	from := len(env.usage.Requests())
	for range 3 {
		env.run(t)
		env.clock.Advance(time.Hour)
	}
	require.Equal(t, [][]string{{"a"}}, env.requestedRoots(from), "the change is checked once")
	require.Contains(t, env.account(t).Refresh, "a", "and answered")

	env.loseAccount(t, false)
	env.clock.Set(changed.Add(settleAfter + time.Hour))
	from = len(env.usage.Requests())
	env.run(t)
	assert.Empty(t, env.requestedRoots(from), "the identical answer already covered the change")
}

// TestLostAccountStateRequeuesRootsChangedAfterSettling covers roots that
// settled and then changed, on the root itself or on a sub-agent chat,
// with the pending mark for that change lost along with account.json:
// the change is newer than the settled record, so each root is pending
// again.
func TestLostAccountStateRequeuesRootsChangedAfterSettling(t *testing.T) {
	env := newUsageEnv(t)
	env.threads.Set(finishedThread("own", ago(time.Hour), message("o", "hi")))
	env.threads.Set(finishedThread("parent", ago(time.Hour), message("p", "hi")))
	child := finishedThread("child", ago(time.Hour), message("c", "hi"))
	child.ParentThreadID = "parent"
	env.threads.Set(child)
	// "own" settles with a final row, "parent" with no row.
	env.usage.SetRow("own", codexcloudtest.Row("own", "partial", pct(1), nil))
	env.run(t)
	env.clock.Set(fetchNow.Add(settleAfter + time.Hour))
	env.run(t)
	require.True(t, env.plan(t, "own").rows[len(env.plan(t, "own").rows)-1].Final)
	parent := env.plan(t, "parent")
	require.Equal(t, mirror.OutcomeSettledNoRow, parent.outcomes[len(parent.outcomes)-1].Kind)
	require.Empty(t, env.account(t).UsagePending)

	// New activity, while plan usage requests fail, so no new record
	// follows the settled one before the account state is lost.
	env.clock.Advance(time.Hour)
	for _, id := range []string{"own", "child"} {
		env.threads.Update(id, func(th *codexcloudtest.Thread) {
			th.UpdatedAt = env.clock.Now().Unix() - 60
			th.Turns[0].Items = append(th.Turns[0].Items, message(id+"-2", "more"))
		})
	}
	env.usage.Refuse(http.StatusInternalServerError, nil, "")
	env.run(t)
	require.Contains(t, env.account(t).UsagePending, "own")
	require.Contains(t, env.account(t).UsagePending, "parent")
	env.usage.Accept()

	env.loseAccount(t, false)
	env.clock.Advance(time.Hour)
	from := len(env.usage.Requests())
	env.run(t)
	assert.Equal(t, []string{"own", "parent"}, env.requestedSince(from), "each changed root is asked about again")
}

// TestLostAccountStateSkipsUnreadableRoots covers a rebuild that meets a
// root it cannot read: that root is logged and skipped, and the others
// are queued, so the account keeps syncing.
func TestLostAccountStateSkipsUnreadableRoots(t *testing.T) {
	env := newUsageEnv(t)
	var logged bytes.Buffer
	env.syncer.Store.Log = log.New(&logged, "", 0)
	for _, id := range []string{"good", "badrow", "badmirror"} {
		env.threads.Set(finishedThread(id, ago(time.Hour), message(id+"-m", "hi")))
	}
	env.usage.SetRow("good", codexcloudtest.Row("good", "partial", pct(1), nil))
	env.usage.SetRow("badrow", codexcloudtest.Row("badrow", "partial", pct(1), nil))
	env.run(t)

	st, err := env.syncer.Store.LoadThread(passKey, "badrow")
	require.NoError(t, err)
	require.Equal(t, string(mirror.KindPlanUsageRow), st.LastPlanRecordKind)
	st.LastPlanRow = jsontext.Value(`"not a row"`)
	require.NoError(t, env.syncer.Store.SaveThread(passKey, "badrow", st))
	// Its newest record is a "left out" outcome, which is read from the
	// mirror file. Bytes that are not a frame make that read fail on every
	// platform and for every user, which a file mode does not.
	require.NoError(t, os.WriteFile(env.mirrorPath(t, "badmirror"), []byte("not a mirror frame"), 0o600))

	env.loseAccount(t, false)
	env.clock.Advance(time.Hour)
	from := len(env.usage.Requests())
	env.run(t)

	assert.Equal(t, []string{"good"}, env.requestedSince(from), "the readable root is asked about again")
	acct := env.account(t)
	assert.Contains(t, acct.UsagePending, "good")
	assert.NotContains(t, acct.UsagePending, "badrow")
	assert.NotContains(t, acct.UsagePending, "badmirror")
	_, err = os.Stat(filepath.Join(env.root, string(passKey), "account.json"))
	require.NoError(t, err, "the rebuilt account state is saved")
	assert.Contains(t, logged.String(), "chat badrow")
	assert.Contains(t, logged.String(), "chat badmirror")
}
