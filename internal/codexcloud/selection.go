package codexcloud

import (
	"maps"
	"slices"
	"time"
)

// buildSelection returns each thread to fetch this pass, once: threads the
// listings found changed, then listed-active and unfinished threads that
// the 24-hour stall rule has not stopped, check-again threads, and due
// retries. A retry entry not yet due removes its thread however it was
// selected, so a failing thread at the top of a listing still waits.
func buildSelection(now time.Time, acct *AccountState, changed []ListedThread, active []string) []string {
	var selected []string
	seen := map[string]bool{}
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			selected = append(selected, id)
		}
	}
	for _, thread := range changed {
		add(thread.ID)
	}
	for _, id := range active {
		if !acct.Unfinished[id].Stalled {
			add(id)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(acct.Unfinished)) {
		if !acct.Unfinished[id].Stalled {
			add(id)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(acct.CheckAgain)) {
		// A stalled turn is fetched again only when its thread changes.
		if acct.CheckAgain[id] && !acct.Unfinished[id].Stalled {
			add(id)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(acct.Retry)) {
		add(id)
	}
	return slices.DeleteFunc(selected, func(id string) bool {
		retry, ok := acct.Retry[id]
		return ok && now.Before(retry.NextAttempt)
	})
}
