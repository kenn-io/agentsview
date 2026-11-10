package clickhouse

import "iter"

// This file keeps the selection a range in progress used before it kept
// survivors: select the candidates' rows and their Claude peers from every
// kept row, then select survivors among them. The tests compare the kept
// survivors with it.

// claudeUsageKey is a Claude message and request ID pair.
type claudeUsageKey struct{ message, request string }

// activityUsageSelection returns the kept rows a read of the candidate
// sessions returns, in report order, and how many there are: every row of
// a candidate session, and every message row of another session that
// carries the Claude message and request IDs of a candidate row. Those
// peers are what complete-snapshot selection compares the candidates with.
func activityUsageSelection(k *activityUsageKept, candidates []string) (iter.Seq[*clickActivityReportUsageRow], int) {
	selected := make(map[string]bool, len(candidates))
	for _, id := range candidates {
		selected[id] = true
	}
	candidate := make([]bool, len(k.dict))
	others := false
	for _, session := range k.sessions {
		candidate[session] = selected[k.dict[session]]
		others = others || !candidate[session]
	}
	if !others {
		return k.all(), k.count
	}
	peerKey := func(r *keptUsageRow) (claudeUsageKey, bool) {
		return claudeUsageKey{r.claudeMessageID, r.claudeRequestID}, r.claudeMessageID != ""
	}
	// The other sessions' message rows are grouped by key first; a
	// candidate row carrying the key then selects the whole group.
	type peerGroup struct {
		rows     int
		selected bool
	}
	peers := map[claudeUsageKey]*peerGroup{}
	count := 0
	for r := range k.rows() {
		if candidate[r.refs[0]] {
			count++
			continue
		}
		if key, ok := peerKey(r); ok && r.source == "message" {
			group := peers[key]
			if group == nil {
				group = &peerGroup{}
				peers[key] = group
			}
			group.rows++
		}
	}
	if len(peers) > 0 {
		for r := range k.rows() {
			if !candidate[r.refs[0]] {
				continue
			}
			if key, ok := peerKey(r); ok {
				if group := peers[key]; group != nil && !group.selected {
					group.selected = true
					count += group.rows
				}
			}
		}
	}
	return func(yield func(*clickActivityReportUsageRow) bool) {
		for r := range k.rows() {
			if !candidate[r.refs[0]] {
				key, ok := peerKey(r)
				if !ok || r.source != "message" {
					continue
				}
				if group := peers[key]; group == nil || !group.selected {
					continue
				}
			}
			if !yield(&r.clickActivityReportUsageRow) {
				return
			}
		}
	}, count
}
