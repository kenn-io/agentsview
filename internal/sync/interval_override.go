package sync

import (
	gosync "sync"
	"time"
)

// IntervalOverride shortens the minimum time between watcher-triggered syncs
// while something needs changes sooner, such as a connected change stream.
// Watchers that share it use the smaller of their configured interval and the
// override. The zero value holds no override and is ready to use.
type IntervalOverride struct {
	mu      gosync.Mutex
	value   time.Duration
	changed chan struct{}
}

// Set replaces the override and wakes every watcher that uses it so a
// pending sync is rescheduled. Zero or negative removes the override.
func (o *IntervalOverride) Set(d time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if d < 0 {
		d = 0
	}
	if d == o.value {
		return
	}
	o.value = d
	if o.changed != nil {
		close(o.changed)
		o.changed = nil
	}
}

// Current returns the override, zero when none is set, and a channel that
// closes at its next change.
func (o *IntervalOverride) Current() (time.Duration, <-chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.changed == nil {
		o.changed = make(chan struct{})
	}
	return o.value, o.changed
}

// effectiveInterval returns the smaller positive interval. A configured
// interval of zero has no minimum, so an override cannot add one.
func effectiveInterval(configured, override time.Duration) time.Duration {
	if override > 0 && override < configured {
		return override
	}
	return configured
}
