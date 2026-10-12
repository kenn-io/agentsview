package codexcloud

import (
	"encoding/json/v2"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
)

const (
	// saveLag is how long saved items may trail the live work. It is a
	// guess from the probes.
	saveLag = 10 * time.Minute
	// clockAllowance covers the difference between OpenAI's clock and
	// this machine's.
	clockAllowance = 2 * time.Minute
)

func turnEnded(status string) bool {
	switch status {
	case "completed", "interrupted", "failed":
		return true
	default:
		return false
	}
}

func itemStatusTerminal(status string) bool {
	switch status {
	case "completed", "failed", "declined", "interrupted":
		return true
	default:
		return false
	}
}

// marginPassed reports whether the save lag has passed since an event the
// server stamped at serverTime and this fetcher first saw at firstSeen.
// Server times also allow for clock difference.
func marginPassed(serverTime *time.Time, firstSeen, now time.Time) bool {
	if serverTime != nil && !serverTime.After(now.Add(-saveLag-clockAllowance)) {
		return true
	}
	return !firstSeen.IsZero() && !firstSeen.After(now.Add(-saveLag))
}

// turnMarginPassed reports whether nothing more can be saved into turn:
// the save lag has passed for its end, or for the start of later.
func turnMarginPassed(turn TurnState, later *TurnState, now time.Time) bool {
	if turnEnded(turn.Status) && marginPassed(turn.CompletedAt, turn.EndedFirstSeen, now) {
		return true
	}
	return later != nil && marginPassed(later.StartedAt, later.FirstSeen, now)
}

// itemFinished reports whether an item can no longer change, so the resume
// point may pass it. An archived thread counts only once the save lag has
// passed since the fetcher first saw it archived: a user can archive a
// thread seconds after its turn ends.
func itemFinished(
	record mirror.ItemRecord, turn TurnState, later *TurnState, archivedFirstSeen, now time.Time,
) bool {
	var header mirror.ItemHeader
	_ = json.Unmarshal(record.Item, &header) // an unreadable item stays unfinished
	var status string
	_ = json.Unmarshal(header.Status, &status)
	if record.CompletedAtMs != nil && (len(header.Status) == 0 || itemStatusTerminal(status)) {
		return true
	}
	if turnMarginPassed(turn, later, now) {
		return true
	}
	return !archivedFirstSeen.IsZero() && !archivedFirstSeen.After(now.Add(-saveLag))
}

// advanceResume moves the resume point over the longest run of finished
// items. turns run oldest first from the current resume turn; items holds
// each turn's items after the resume point, in order. The point stops at
// the first unfinished item, because parallel tool calls can finish out of
// order, and never leaves a turn until nothing more can be saved into it.
func advanceResume(
	st ThreadState, turns []TurnState, items map[string][]mirror.ItemRecord, now time.Time,
) (turnID, itemID string) {
	turnID, itemID = st.ResumeTurnID, st.ResumeItemID
	for i, turn := range turns {
		var later *TurnState
		if i+1 < len(turns) {
			later = &turns[i+1]
		}
		if i > 0 && !turnMarginPassed(turns[i-1], &turns[i], now) {
			return turnID, itemID
		}
		for _, record := range items[turn.ID] {
			if !itemFinished(record, turn, later, st.ArchivedFirstSeen, now) {
				return turnID, itemID
			}
			var header mirror.ItemHeader
			if json.Unmarshal(record.Item, &header) != nil || header.ID == "" {
				return turnID, itemID
			}
			turnID, itemID = turn.ID, header.ID
		}
	}
	return turnID, itemID
}

// threadUnfinished reports whether a thread must be fetched again next
// pass: a turn still runs, an item after the resume point has not
// finished, or the newest turn could still receive saved items.
func threadUnfinished(
	st ThreadState, turns []TurnState, items map[string][]mirror.ItemRecord, now time.Time,
) (unfinished, inProgress bool) {
	for _, turn := range turns {
		if turn.Status == "inProgress" {
			inProgress = true
		}
	}
	if len(turns) == 0 {
		return inProgress, inProgress
	}
	last := turns[len(turns)-1]
	if inProgress || !turnMarginPassed(last, nil, now) {
		return true, inProgress
	}
	turnID, itemID := advanceResume(st, turns, items, now)
	for _, turn := range slices.Backward(turns) {
		turnItems := items[turn.ID]
		if len(turnItems) == 0 {
			continue
		}
		var header mirror.ItemHeader
		_ = json.Unmarshal(turnItems[len(turnItems)-1].Item, &header)
		return turnID != turn.ID || itemID != header.ID, false
	}
	return false, false
}
