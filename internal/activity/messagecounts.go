package activity

import "time"

// MessageAccumulator counts normalized transcript messages independently of
// the adjacent-message pairs used to estimate active time. Storage supplies
// only non-system user/assistant rows, excluding tool-result messages.
type MessageAccumulator struct {
	windows      []BucketWindow
	effectiveEnd time.Time
	artifacts    *CandidateArtifacts
	interactive  map[string]bool
}

func NewMessageAccumulator(q Query, artifacts *CandidateArtifacts) *MessageAccumulator {
	interactive := make(map[string]bool, len(artifacts.Sessions))
	for _, session := range artifacts.Sessions {
		interactive[session.SessionID] = !session.IsAutomated && !session.IsSubagent
	}
	return &MessageAccumulator{
		windows: rangeWindows(Params{
			RangeStart: q.RangeStart, RangeEnd: q.RangeEnd,
			Loc: q.Loc, Bucket: q.Bucket,
		}),
		effectiveEnd: q.EffectiveEnd,
		artifacts:    artifacts,
		interactive:  interactive,
	}
}

// Add counts one message in its half-open bucket and includes its session in
// bucket drill-down even when the message has no active interval.
func (a *MessageAccumulator) Add(sessionID, role string, timestamp time.Time) {
	if !timestamp.Before(a.effectiveEnd) {
		return
	}
	index := windowIndex(a.windows, timestamp)
	if index < 0 {
		return
	}
	bucket := &a.artifacts.Report.Buckets[index]
	switch role {
	case "user":
		// User messages measure human prompts, excluding instructions generated
		// by automation or delegated into subagent sessions.
		if !a.interactive[sessionID] {
			return
		}
		bucket.UserMessages++
	case "assistant":
		bucket.AssistantMessages++
	default:
		return
	}
	bits := a.artifacts.Membership[sessionID]
	if bits == nil {
		bits = make(BucketMembership, (len(a.windows)+63)/64)
		a.artifacts.Membership[sessionID] = bits
	}
	bits.add(index)
}
