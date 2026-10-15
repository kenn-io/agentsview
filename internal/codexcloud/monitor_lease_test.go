package codexcloud

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
)

// workLeases stands in for the daemon's idle tracker: it counts the work
// leases the monitor holds, and refuses new ones while draining.
type workLeases struct {
	held     atomic.Int64
	draining atomic.Bool
	// onRelease runs as a lease is released.
	onRelease func()
}

func (w *workLeases) begin() (func(), bool) {
	if w.draining.Load() {
		return func() {}, false
	}
	w.held.Add(1)
	var released atomic.Bool
	return func() {
		if released.Swap(true) {
			return
		}
		if w.onRelease != nil {
			w.onRelease()
		}
		w.held.Add(-1)
	}, true
}

func newLeasedMonitorEnv(t *testing.T) (*monitorEnv, *workLeases) {
	t.Helper()
	env := newMonitorEnv(t)
	leases := &workLeases{}
	env.monitor.BeginWork = leases.begin
	return env, leases
}

func TestJoinHoldsAWorkLeaseUntilItLeaves(t *testing.T) {
	env, leases := newLeasedMonitorEnv(t)
	// Whether the mirror holds the turn's records when the lease is
	// released. onRelease runs on the join's goroutine, so it only records.
	path := env.mirrorPath(t, "a")
	var savedAtRelease atomic.Bool
	leases.onRelease = func() {
		records, err := mirror.ReadAll(t.Context(), path)
		savedAtRelease.Store(err == nil && slices.ContainsFunc(records, func(r mirror.Record) bool {
			return r.Kind == mirror.KindTokenUsage
		}) && records[len(records)-1].Kind == mirror.KindTurnCompleted)
	}
	env.threads.Set(runningThread("a", "warm"))

	env.join(t, "a")
	assert.Equal(t, int64(1), leases.held.Load(), "a running join keeps the daemon awake")
	env.notify(t, "a", tokenUsageMethod, tokenParams("a", "a-turn-2",
		[6]float64{1000, 0, 0, 10, 0, 1010}, [6]float64{1000, 0, 0, 10, 0, 1010}))
	env.complete(t, "a", "a-turn-2")
	env.waitLeft(t, "a")

	require.Eventually(t, func() bool { return leases.held.Load() == 0 }, 5*time.Second, 5*time.Millisecond,
		"the lease is released when the join leaves")
	assert.True(t, savedAtRelease.Load(), "the join saves its records before it releases the lease")
}

func TestCloseAllReleasesJoinWorkLeases(t *testing.T) {
	env, leases := newLeasedMonitorEnv(t)
	env.threads.Set(runningThread("a", "warm"))
	env.threads.Set(runningThread("b", "warm"))
	env.join(t, "a")
	env.join(t, "b")
	require.Equal(t, int64(2), leases.held.Load(), "each joined chat holds its own lease")

	env.monitor.CloseAll()

	assert.Zero(t, leases.held.Load(), "CloseAll returns after every join released its lease")
}

func TestRefusedWorkLeaseRefusesTheJoin(t *testing.T) {
	env, leases := newLeasedMonitorEnv(t)
	env.threads.Set(runningThread("a", "warm"))
	leases.draining.Store(true)

	env.join(t, "a")

	assert.Empty(t, env.monitor.Joined(passKey), "a draining daemon joins nothing")
	assert.Zero(t, leases.held.Load())
	require.Eventually(t, func() bool { return env.host.Joined("a") == 0 }, 5*time.Second, 5*time.Millisecond,
		"the refused join closes its connection")

	leases.draining.Store(false)
	env.join(t, "a")
	assert.Equal(t, []string{"a"}, env.monitor.Joined(passKey), "the refused join gave back its slot")
}
