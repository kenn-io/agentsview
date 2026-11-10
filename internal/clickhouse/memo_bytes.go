package clickhouse

import (
	"unsafe"

	"go.kenn.io/agentsview/internal/readbase"
)

// Byte caps of the memos whose entries can be large.
const (
	// A range in progress keeps its rows and their survivors. A range of
	// 1.5 million rows takes about 250 MiB, which spares deriving them again
	// on every report, a pass that allocates several times that.
	activityUsageRangeBytes = 512 << 20
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

func aggregateRowBytes(r CHUsageAggregateRow) int64 {
	return int64(unsafe.Sizeof(r)) + stringBytes(r.Date, r.TS, r.PricingTS, r.SessionID, r.Project,
		r.Agent, r.Machine, r.Model, r.ProviderID, r.PriceModel, r.Source, r.DisplayName, r.StartedAt, r.GroupKey, r.SessionName)
}

func dailyUsageRowBytes(r chDailyUsageGroupRow) int64 {
	return aggregateRowBytes(r.CHUsageAggregateRow) + int64(unsafe.Sizeof(r)-unsafe.Sizeof(r.CHUsageAggregateRow)) +
		stringBytes(r.Kind, r.ContextID, r.PriceError) +
		int64(len(r.InexactModels)+len(r.InexactProjects))*int64(unsafe.Sizeof("")) +
		stringBytes(r.InexactModels...) + stringBytes(r.InexactProjects...)
}

func usageSessionRowBytes(r chUsageSessionRow) int64 {
	return int64(unsafe.Sizeof(r)) + stringBytes(r.sessionID, r.project, r.agent)
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

// usageRangeBytes estimates a kept range. Its caller holds the range's lock.
func usageRangeBytes(r *activityUsageRange) int64 {
	n := int64(unsafe.Sizeof(*r))
	for session, revision := range r.revisions {
		n += int64(len(session)+len(revision)) + 32
	}
	// Every part of one read shares the read's dictionary; count it once.
	dicts := make(map[*string]bool)
	for _, session := range r.sessions {
		n += int64(unsafe.Sizeof(session)) + int64(len(session.id)) + 8
		k := session.rows
		if k == nil {
			continue
		}
		// Each session owns its arena and data; see splitKeptUsage.
		n += int64(unsafe.Sizeof(*k)) + int64(len(k.arena)) + int64(cap(k.data)) + 8*int64(len(k.sessions))
		if len(k.dict) > 0 && !dicts[&k.dict[0]] {
			dicts[&k.dict[0]] = true
			for _, v := range k.dict {
				n += int64(unsafe.Sizeof(v)) + int64(len(v))
			}
		}
	}
	return n + usageSurvivorsBytes(&r.survivors)
}

// usageSurvivorsBytes counts the survivors' slices by capacity: a push
// splices rows in and out of them, and a slice keeps its largest array.
func usageSurvivorsBytes(u *usageSurvivors) int64 {
	n := int64(cap(u.candidate)) +
		int64(cap(u.tokens))*int64(unsafe.Sizeof(usageTokenRow{})) +
		int64(cap(u.results))*int64(unsafe.Sizeof(usageTokenSurvivor{})) +
		int64(len(u.pricing))*int64(unsafe.Sizeof(usagePricingTally{})+unsafe.Sizeof(usagePricingKey{}))
	// A total row's strings point into its rows' dictionary and arena.
	for _, totals := range u.totals {
		n += int64(unsafe.Sizeof(totals)) +
			int64(cap(totals.rows))*int64(unsafe.Sizeof(usageTotalRow{})) +
			int64(cap(totals.pricing))*int64(unsafe.Sizeof(usagePricingCount{}))
	}
	return n
}
