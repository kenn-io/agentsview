package codexcloud

import (
	"maps"
	"slices"
	"time"
)

// buildSelection returns each thread to fetch this pass, once: threads the
// listings found changed, then listed-active and unfinished threads that
// the 24-hour stall rule has not stopped, except retired ones listed
// active, then check-again threads and due retries. A retry entry not yet
// due removes its thread however it was selected, so a failing thread at
// the top of a listing still waits. Due retries come last, after every
// thread without a retry entry. A thread OpenAI never answers ends the
// pass, so it must not run before the others.
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
		// A retired thread listed active has not changed since it was
		// retired; a change puts it in changed above.
		if !acct.Unfinished[id].Stalled && !acct.Retired[id] {
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
	selected = slices.DeleteFunc(selected, func(id string) bool {
		retry, ok := acct.Retry[id]
		return ok && now.Before(retry.NextAttempt)
	})
	slices.SortStableFunc(selected, func(a, b string) int {
		_, aRetry := acct.Retry[a]
		_, bRetry := acct.Retry[b]
		switch {
		case aRetry == bRetry:
			return 0
		case aRetry:
			return 1
		default:
			return -1
		}
	})
	return selected
}
