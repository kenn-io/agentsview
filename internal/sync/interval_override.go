package sync

import (
	"math"
	"slices"
	gosync "sync"
	"time"
)

// IntervalOverride shortens the minimum time between watcher-triggered syncs
// while something needs changes sooner, such as a connected change stream or
// an open web UI. Watchers that share it use the smallest of their configured
// interval, the shortest interval currently held, and, while anything is
// held, their live interval (WatcherOptions.LiveInterval). The zero value
// holds no override and is ready to use.
type IntervalOverride struct {
	mu      gosync.Mutex
	holds   []time.Duration
	changed chan struct{}
}

// LiveInterval is the hold for a client that wants each watcher's live
// interval and no shorter cap. Each watcher resolves it from its own options,
// so a hold that is already open follows a watcher rebuilt with new settings.
const LiveInterval = time.Duration(math.MaxInt64)

// Hold asks for syncs at most d apart until release runs, and wakes every
// watcher that uses the override so a pending sync is rescheduled. Zero or
// negative d holds nothing. release is safe to call more than once.
func (o *IntervalOverride) Hold(d time.Duration) (release func()) {
	if d <= 0 {
		return func() {}
	}
	o.update(func() { o.holds = append(o.holds, d) })
	return gosync.OnceFunc(func() {
		o.update(func() {
			if i := slices.Index(o.holds, d); i >= 0 {
				o.holds = slices.Delete(o.holds, i, i+1)
			}
		})
	})
}

func (o *IntervalOverride) update(change func()) {
	o.mu.Lock()
	defer o.mu.Unlock()
	before := o.valueLocked()
	change()
	if o.valueLocked() != before && o.changed != nil {
		close(o.changed)
		o.changed = nil
	}
}

func (o *IntervalOverride) valueLocked() time.Duration {
	if len(o.holds) == 0 {
		return 0
	}
	return slices.Min(o.holds)
}

// Value returns the shortest held interval, zero when nothing is held and
// LiveInterval when only live holds are.
func (o *IntervalOverride) Value() time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.valueLocked()
}

// Current returns the shortest held interval, zero when nothing is held, and
// a channel that closes at its next change.
func (o *IntervalOverride) Current() (time.Duration, <-chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.changed == nil {
		o.changed = make(chan struct{})
	}
	return o.valueLocked(), o.changed
}

// effectiveInterval returns the smaller positive interval. A configured
// interval of zero has no minimum, so an override cannot add one.
func effectiveInterval(configured, override time.Duration) time.Duration {
	if override > 0 && override < configured {
		return override
	}
	return configured
}
