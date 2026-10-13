package codexcloud

import (
	"fmt"
	"sync"
	"time"
)

// homeWaitLimit ends a signed-out or refresh-requested wait even when the
// home's auth.json never changes.
const homeWaitLimit = time.Hour

// Waits times retries after failures. Account waits live in the account's
// persisted state, shared by the sync job and live discovery; home waits
// live in memory and end on daemon restart.
type Waits struct {
	now func() time.Time

	mu    sync.Mutex
	homes map[string]homeWait
	// usageHomes holds plan usage 401s, which skip a home for plan usage
	// only; thread sync keeps using it.
	usageHomes map[string]homeWait
}

type homeWait struct {
	failure   Failure
	until     time.Time
	authMTime time.Time
}

// NewWaits times waits on now.
func NewWaits(now func() time.Time) *Waits {
	return &Waits{now: now, homes: map[string]homeWait{}, usageHomes: map[string]homeWait{}}
}

// accountSchedule is the first wait and the cap for each account failure.
// Each consecutive failure of the same kind doubles the wait.
var accountSchedule = map[Failure]struct{ first, limit time.Duration }{
	FailRefused:     {time.Hour, 24 * time.Hour},
	FailTooLarge:    {time.Hour, 24 * time.Hour},
	FailProtocol:    {time.Hour, 24 * time.Hour},
	FailRateLimited: {5 * time.Minute, time.Hour},
	FailNetwork:     {time.Minute, time.Hour},
	FailServer:      {time.Minute, time.Hour},
}

// RecordAccount starts or extends the account's wait. A 429's Retry-After
// replaces the schedule. The caller saves st.
func (w *Waits) RecordAccount(st *AccountState, f Failure, retryAfter time.Duration) error {
	schedule, ok := accountSchedule[f]
	if !ok {
		return fmt.Errorf("codex-cloud: %s is not an account failure", f)
	}
	attempts := 1
	if st.Wait.Failure == f.String() {
		attempts = st.Wait.Attempts + 1
	}
	wait := schedule.first
	for range attempts - 1 {
		if wait >= schedule.limit {
			break
		}
		wait *= 2
	}
	wait = min(wait, schedule.limit)
	if f == FailRateLimited && retryAfter > 0 {
		wait = retryAfter
	}
	st.Wait = WaitState{Failure: f.String(), Attempts: attempts, Until: w.now().Add(wait)}
	return nil
}

// AccountReady reports whether the account's wait has ended.
func (w *Waits) AccountReady(st *AccountState) bool {
	return !w.now().Before(st.Wait.Until)
}

// ResetAccount clears the account's wait after a successful request. The
// caller saves st.
func (w *Waits) ResetAccount(st *AccountState) {
	st.Wait = WaitState{}
}

// RecordHome makes a Codex home wait until its auth.json changes from
// authMTime, or an hour passes.
func (w *Waits) RecordHome(home string, f Failure, authMTime time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.homes[home] = homeWait{failure: f, until: w.now().Add(homeWaitLimit), authMTime: authMTime}
}

// HomeReady reports whether home may be used, given its auth.json's
// current modification time. A ready home's wait is forgotten.
func (w *Waits) HomeReady(home string, authMTime time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	wait, ok := w.homes[home]
	if !ok {
		return true
	}
	if !authMTime.Equal(wait.authMTime) || !w.now().Before(wait.until) {
		delete(w.homes, home)
		return true
	}
	return false
}

// HomeFailure returns the failure a waiting home recorded, or FailNone
// when the home is not waiting.
func (w *Waits) HomeFailure(home string) Failure {
	w.mu.Lock()
	defer w.mu.Unlock()
	if wait, ok := w.homes[home]; ok {
		return wait.failure
	}
	return FailNone
}

// RecordUsageHome remembers a plan usage 401 for home until its auth.json
// changes from authMTime or an hour passes.
func (w *Waits) RecordUsageHome(home string, authMTime time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.usageHomes[home] = homeWait{failure: FailSignedOut, until: w.now().Add(homeWaitLimit), authMTime: authMTime}
}

// UsageHomeReady reports whether plan usage may use home, given its
// auth.json's current modification time.
func (w *Waits) UsageHomeReady(home string, authMTime time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	wait, ok := w.usageHomes[home]
	if !ok {
		return true
	}
	if !authMTime.Equal(wait.authMTime) || !w.now().Before(wait.until) {
		delete(w.usageHomes, home)
		return true
	}
	return false
}
