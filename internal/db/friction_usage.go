package db

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/friction"
)

// FrictionArchiveWindow is jilog's ArchiveSpend::window: (date−7, date−1).
func FrictionArchiveWindow(date string) (string, string, error) {
	day, err := time.Parse("2006-01-02", date)
	if err != nil {
		return "", "", fmt.Errorf("parsing friction date %q: %w", date, err)
	}
	return day.AddDate(0, 0, -7).Format("2006-01-02"), day.AddDate(0, 0, -1).Format("2006-01-02"), nil
}

func newFrictionPeriod() friction.PeriodSpend {
	return friction.PeriodSpend{
		Total:  friction.USDFromMicros(0),
		Agents: map[string]friction.USD{},
		Models: map[string]friction.USD{},
	}
}

func addFrictionPeriodRow(p *friction.PeriodSpend, row DailyUsageEntry) {
	p.Days++
	p.Total = p.Total.Add(friction.USDFromMicros(row.TotalCost.Microdollars))
	for _, a := range row.AgentBreakdowns {
		p.Agents[a.Agent] = addFrictionUSD(p.Agents, a.Agent, a.Cost.Microdollars)
	}
	for _, m := range row.ModelBreakdowns {
		p.Models[m.ModelName] = addFrictionUSD(p.Models, m.ModelName, m.Cost.Microdollars)
	}
}

func addFrictionUSD(m map[string]friction.USD, key string, micros int64) friction.USD {
	v := friction.USDFromMicros(micros)
	if cur, ok := m[key]; ok {
		return cur.Add(v)
	}
	return v
}

// FrictionArchiveSpendFromDaily ports ArchiveSpend::summarize
// (archive_spend.rs:93-109): rows in [from, to] form the week, the row
// dated `to` is yesterday, and a window with no rows yields nil.
func FrictionArchiveSpendFromDaily(
	res DailyUsageResult, from, to, timezone string,
) *friction.ArchiveSpend {
	week := newFrictionPeriod()
	var yesterday *friction.PeriodSpend
	for _, row := range res.Daily {
		if row.Date < from || row.Date > to {
			continue
		}
		addFrictionPeriodRow(&week, row)
		if row.Date == to {
			y := newFrictionPeriod()
			addFrictionPeriodRow(&y, row)
			yesterday = &y
		}
	}
	if week.Days == 0 {
		return nil
	}
	return &friction.ArchiveSpend{
		Yesterday: yesterday, Week: week,
		WeekFrom: from, WeekTo: to, Timezone: timezone,
	}
}

// FrictionUsageFromSessionUsage converts one session's usage into the
// review's stats form. The bool is false when the session carries no
// token or cost data (jilog load_stats → None).
func FrictionUsageFromSessionUsage(u *SessionUsage) (friction.SessionUsage, bool) {
	if u == nil || (!u.HasTokenData && !u.HasCost) {
		return friction.SessionUsage{}, false
	}
	out := friction.SessionUsage{SubjectID: u.SessionID, ModelCosts: map[string]friction.USD{}}
	for _, row := range u.Breakdown {
		out.InputTokens += uint64(row.InputTokens + row.CacheReadInputTokens + row.CacheCreationInputTokens)
		out.OutputTokens += uint64(row.OutputTokens)
		if row.HasCost {
			out.ModelCosts[row.Model] = addFrictionUSD(out.ModelCosts, row.Model, row.Cost.Microdollars)
		}
	}
	if u.HasCost {
		c := friction.USDFromMicros(u.Cost.Microdollars)
		out.CostUSD = &c
	}
	return out, true
}

// FrictionUsageSource is the part of db.Store both primary backends share
// for per-session usage.
type FrictionUsageSource interface {
	GetSessionUsage(ctx context.Context, sessionID string, includeBreakdown bool) (*SessionUsage, error)
	GetDailyUsage(ctx context.Context, f UsageFilter) (DailyUsageResult, error)
}

// FrictionUsageForSessionsFrom is the shared implementation of
// FrictionUsageForSessions for any backend with GetSessionUsage.
func FrictionUsageForSessionsFrom(
	ctx context.Context, src FrictionUsageSource, sessionIDs []string,
) (map[string]friction.SessionUsage, error) {
	out := make(map[string]friction.SessionUsage, len(sessionIDs))
	for _, id := range sessionIDs {
		u, err := src.GetSessionUsage(ctx, id, true)
		if err != nil {
			return nil, fmt.Errorf("friction usage for %s: %w", id, err)
		}
		if fu, ok := FrictionUsageFromSessionUsage(u); ok {
			fu.SubjectID = id
			out[id] = fu
		}
	}
	return out, nil
}

// FrictionArchiveSpendFrom is the shared implementation of
// FrictionArchiveSpend: native daily usage in the review zone replaces
// jilog's `agentsview usage daily` shell-out (spec §4.1).
func FrictionArchiveSpendFrom(
	ctx context.Context, src FrictionUsageSource, from, to string, loc *time.Location,
) (*friction.ArchiveSpend, error) {
	res, err := src.GetDailyUsage(ctx, UsageFilter{
		From: from, To: to, Timezone: loc.String(),
		Breakdowns: true, SkipSessionCounts: true,
	})
	if err != nil {
		return nil, fmt.Errorf("friction archive spend %s..%s: %w", from, to, err)
	}
	return FrictionArchiveSpendFromDaily(res, from, to, loc.String()), nil
}

// FrictionUsageForSessions implements review.Store.
func (db *DB) FrictionUsageForSessions(
	ctx context.Context, sessionIDs []string,
) (map[string]friction.SessionUsage, error) {
	return FrictionUsageForSessionsFrom(ctx, db, sessionIDs)
}

// FrictionArchiveSpend implements review.Store.
func (db *DB) FrictionArchiveSpend(
	ctx context.Context, from, to string, loc *time.Location,
) (*friction.ArchiveSpend, error) {
	return FrictionArchiveSpendFrom(ctx, db, from, to, loc)
}
