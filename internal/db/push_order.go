package db

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

// SortSessionsNewestFirst orders sessions by their latest activity, newest
// first, ties broken by ID. Mirror pushes use it so a push that stops partway
// has already published the most recent sessions.
//
// A Codex revert page (<thread>_<rollout>) needs its thread's row published
// first: mirrors decide whether a page is trashed from that row. Pages
// therefore sort with their thread, which goes first, and the group takes the
// recency of its newest member.
func SortSessionsNewestFirst(sessions []Session) {
	type orderKey struct {
		group        string
		groupRecency time.Time
		recency      time.Time
	}
	keys := make(map[string]orderKey, len(sessions))
	groupRecency := make(map[string]time.Time, len(sessions))
	for _, sess := range sessions {
		k := orderKey{group: codexThreadSessionID(sess), recency: sessionRecency(sess)}
		keys[sess.ID] = k
		if k.recency.After(groupRecency[k.group]) {
			groupRecency[k.group] = k.recency
		}
	}
	for id, k := range keys {
		k.groupRecency = groupRecency[k.group]
		keys[id] = k
	}
	slices.SortFunc(sessions, func(a, b Session) int {
		ka, kb := keys[a.ID], keys[b.ID]
		return cmp.Or(
			kb.groupRecency.Compare(ka.groupRecency),
			strings.Compare(ka.group, kb.group),
			// The thread row precedes its pages.
			cmpBool(b.ID == kb.group, a.ID == ka.group),
			kb.recency.Compare(ka.recency),
			strings.Compare(a.ID, b.ID),
		)
	})
}

// codexThreadSessionID returns the session ID of the thread a Codex revert
// page belongs to, and the session's own ID for any other session.
func codexThreadSessionID(sess Session) string {
	switch parser.AgentType(sess.Agent) {
	case parser.AgentCodex, parser.AgentTraeX, parser.AgentAugureCode:
	default:
		return sess.ID
	}
	prefixEnd := strings.LastIndex(sess.ID, ":") + 1
	return sess.ID[:prefixEnd] + parser.CodexThreadIDFromSessionKey(sess.ID[prefixEnd:])
}

// cmpBool orders false before true.
func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	default:
		return -1
	}
}

// sessionRecency is the session's end time, falling back to its start and
// then its creation time. A session with none of them parseable sorts last.
func sessionRecency(sess Session) time.Time {
	for _, v := range []*string{sess.EndedAt, sess.StartedAt, &sess.CreatedAt} {
		if v == nil {
			continue
		}
		if t, ok := ParseStoredTimestamp(*v); ok {
			return t
		}
	}
	return time.Time{}
}
