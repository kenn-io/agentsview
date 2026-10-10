package clickhouse

import (
	"unsafe"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/readbase"
)

// Byte caps of the memos whose entries can be large. The two report memos
// together hold as much as the server's own 256 MiB report cache; a range
// in progress keeps every session's usage rows, so a year of a large mirror
// still fits.
const (
	activityReportMemoBytes = 128 << 20
	activityUsageRangeBytes = 256 << 20
	activityUsageRowsBytes  = 128 << 20
	dailyUsageRowsBytes     = 128 << 20
)

// The estimates below count a value's own bytes and the bytes of the
// strings and slices it holds. They leave out allocator and map overhead,
// so they bound a memo's memory roughly rather than exactly.

func stringBytes(values ...string) int64 {
	var n int64
	for _, v := range values {
		n += int64(len(v))
	}
	return n
}

func optionalStringBytes(values ...*string) int64 {
	var n int64
	for _, v := range values {
		if v != nil {
			n += int64(len(*v))
		}
	}
	return n
}

func aggregateRowBytes(r chUsageAggregateRow) int64 {
	return int64(unsafe.Sizeof(r)) + stringBytes(r.date, r.ts, r.pricingTS, r.sessionID, r.project,
		r.agent, r.machine, r.model, r.providerID, r.priceModel, r.source, r.displayName, r.startedAt, r.groupKey, r.sessionName)
}

func dailyUsageRowBytes(r chDailyUsageGroupRow) int64 {
	return aggregateRowBytes(r.chUsageAggregateRow) + int64(unsafe.Sizeof(r)-unsafe.Sizeof(r.chUsageAggregateRow)) +
		stringBytes(r.kind, r.contextID, r.priceError)
}

func usageSessionRowBytes(r chUsageSessionRow) int64 {
	return int64(unsafe.Sizeof(r)) + stringBytes(r.sessionID, r.project, r.agent)
}

func topSessionBytes(e db.TopSessionEntry) int64 {
	return int64(unsafe.Sizeof(e)) + stringBytes(e.SessionID, e.DisplayName, e.Agent, e.Project, e.StartedAt, e.Machine, e.GroupKey, e.GroupLabel, e.SessionName)
}

func analyticsSessionBytes(r readbase.AnalyticsSession) int64 {
	return int64(unsafe.Sizeof(r)) +
		stringBytes(r.ID, r.Project, r.Machine, r.Agent, r.StartedAt, r.EndedAt, r.CreatedAt,
			r.Outcome, r.OutcomeConfidence) +
		optionalStringBytes(r.FirstMessage, r.DisplayName, r.TerminationStatus, r.HealthGrade)
}

func sessionListingBytes(l activitySessionListing) int64 {
	n := int64(unsafe.Sizeof(l))
	for _, s := range l.sessions {
		n += int64(unsafe.Sizeof(s)) + stringBytes(s.SessionID, s.Title, s.Project, s.ProjectKey,
			s.Agent, s.Machine, s.StartedAt, s.EndedAt)
	}
	for _, id := range l.ids {
		n += int64(unsafe.Sizeof(id)) + int64(len(id))
	}
	for id := range l.versions {
		n += int64(len(id)) + 24
	}
	return n
}

func keptUsageBytes(k *activityUsageKept) int64 {
	if k == nil {
		return 0
	}
	n := int64(unsafe.Sizeof(*k)) + int64(len(k.arena)) + int64(len(k.data)) + 8*int64(len(k.sessions))
	for _, v := range k.dict {
		n += int64(unsafe.Sizeof(v)) + int64(len(v))
	}
	return n
}

func usageRangeBytes(r activityUsageRange) int64 {
	n := keptUsageBytes(r.rows)
	for session, revision := range r.revisions {
		n += int64(len(session)+len(revision)) + 32
	}
	return n
}

func reportEntryBytes(e activityReportEntry) int64 {
	return activity.EstimatedArtifactBytes(e.artifacts)
}
