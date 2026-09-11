package postgres

import (
	"context"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"slices"
	"time"
)

func hostedMapped[T any](ctx context.Context, h *HostedStore, read func() (T, error), collect func(*T, *hostedRefs)) (T, error) {
	return hostedRead(ctx, h, func(_ hostedRevision) (T, error) {
		v, err := read()
		if err != nil {
			return v, err
		}
		r := hostedRefs{}
		collect(&v, &r)
		err = r.mapIDs(ctx, h)
		return v, err
	})
}
func (h *HostedStore) Search(ctx context.Context, f db.SearchFilter) (db.SearchPage, error) {
	return hostedMapped(ctx, h, func() (db.SearchPage, error) { return h.physical.Search(ctx, f) }, func(p *db.SearchPage, r *hostedRefs) {
		p.Results = slices.Clone(p.Results)
		for i := range p.Results {
			r.add(&p.Results[i].SessionID)
		}
	})
}
func (h *HostedStore) SearchContent(ctx context.Context, f db.ContentSearchFilter) (db.ContentSearchPage, error) {
	return hostedMapped(ctx, h, func() (db.ContentSearchPage, error) {
		q := f
		var err error
		q.ExcludeSessionIDs, err = h.resolveBatch(ctx, f.ExcludeSessionIDs, true, false)
		if err != nil {
			return db.ContentSearchPage{}, err
		}
		return h.physical.SearchContent(ctx, q)
	}, func(p *db.ContentSearchPage, r *hostedRefs) {
		p.Matches = slices.Clone(p.Matches)
		for i := range p.Matches {
			m := &p.Matches[i]
			r.add(&m.SessionID)
			r.link(m.SessionID, &m.ParentSessionID)
			m.ContextBefore = r.messages(m.ContextBefore)
			m.ContextAfter = r.messages(m.ContextAfter)
		}
	})
}
func (h *HostedStore) ListSecretFindings(ctx context.Context, f db.SecretFindingFilter) (db.SecretFindingPage, error) {
	return hostedMapped(ctx, h, func() (db.SecretFindingPage, error) { return h.physical.ListSecretFindings(ctx, f) }, func(p *db.SecretFindingPage, r *hostedRefs) {
		p.Findings = slices.Clone(p.Findings)
		for i := range p.Findings {
			r.add(&p.Findings[i].SessionID)
		}
	})
}
func (h *HostedStore) SecretFindingSource(ctx context.Context, f db.SecretFinding) (string, bool, error) {
	type result struct {
		content string
		ok      bool
	}
	v, err := hostedDetail(ctx, h, f.SessionID, func(id string) (result, error) {
		f.SessionID = id
		c, ok, err := h.physical.SecretFindingSource(ctx, f)
		return result{c, ok}, err
	})
	return v.content, v.ok, err
}
func (h *HostedStore) GetSessionTiming(ctx context.Context, id string) (*db.SessionTiming, error) {
	return hostedMapped(ctx, h, func() (*db.SessionTiming, error) {
		target, err := h.resolve(ctx, id)
		if err != nil || target.SessionID == "" {
			return nil, err
		}
		session, err := h.physical.GetSession(ctx, target.SessionID)
		if err != nil || session == nil {
			return nil, err
		}
		turns, err := h.physical.queryTurnRows(ctx, target.SessionID)
		if err != nil {
			return nil, err
		}
		join := `LEFT JOIN LATERAL (SELECT min(target_session.started_at) AS started_at,max(target_session.ended_at) AS ended_at ` + hostedLinkFromSQL + ` AND owner.session_id=tc.session_id AND e.kind='call' AND e.ordinal=tc.message_ordinal AND e.call_index=tc.call_index HAVING count(DISTINCT target.session_id)=1) s_sub ON true`
		if target.Legacy {
			join = `LEFT JOIN sessions s_sub ON s_sub.id=tc.subagent_session_id`
		}
		calls, err := h.physical.queryCallRowsWithJoin(ctx, target.SessionID, join)
		if err != nil {
			return nil, err
		}
		return db.AssembleTiming(session, turns, calls, time.Now().UTC()), nil
	}, func(p **db.SessionTiming, r *hostedRefs) {
		if *p == nil {
			return
		}
		v := **p
		*p = &v
		r.add(&v.SessionID)
		if v.SlowestCall != nil {
			c := *v.SlowestCall
			v.SlowestCall = &c
			r.parent(v.SessionID, &c.SubagentSessionID, nil)
		}
		v.Turns = slices.Clone(v.Turns)
		for i := range v.Turns {
			v.Turns[i].Calls = slices.Clone(v.Turns[i].Calls)
			for j := range v.Turns[i].Calls {
				r.parent(v.SessionID, &v.Turns[i].Calls[j].SubagentSessionID, nil)
			}
		}
	})
}
func (h *HostedStore) GetSessionUsage(ctx context.Context, id string, breakdown bool) (*db.SessionUsage, error) {
	return hostedMapped(ctx, h, func() (*db.SessionUsage, error) {
		target, err := h.resolve(ctx, id)
		if err != nil || target.SessionID == "" {
			return nil, err
		}
		return h.physical.GetSessionUsage(ctx, target.SessionID, breakdown)
	}, func(p **db.SessionUsage, r *hostedRefs) {
		if *p == nil {
			return
		}
		v := **p
		*p = &v
		r.add(&v.SessionID)
		v.Breakdown = slices.Clone(v.Breakdown)
		for i := range v.Breakdown {
			r.add(&v.Breakdown[i].SubagentSessionID)
		}
	})
}
func (h *HostedStore) GetTopSessionsByCost(ctx context.Context, f db.UsageFilter, limit int) ([]db.TopSessionEntry, error) {
	return hostedMapped(ctx, h, func() ([]db.TopSessionEntry, error) { return h.physical.GetTopSessionsByCost(ctx, f, limit) }, func(p *[]db.TopSessionEntry, r *hostedRefs) {
		*p = slices.Clone(*p)
		for i := range *p {
			r.add(&(*p)[i].SessionID)
		}
	})
}
func (h *HostedStore) GetAnalyticsTopSessions(ctx context.Context, f db.AnalyticsFilter, metric string) (db.TopSessionsResponse, error) {
	return hostedMapped(ctx, h, func() (db.TopSessionsResponse, error) { return h.physical.GetAnalyticsTopSessions(ctx, f, metric) }, func(p *db.TopSessionsResponse, r *hostedRefs) {
		p.Sessions = slices.Clone(p.Sessions)
		for i := range p.Sessions {
			r.add(&p.Sessions[i].ID)
		}
	})
}
func (h *HostedStore) GetAnalyticsSignalSessions(ctx context.Context, f db.AnalyticsFilter, signal string, limit int) (db.SignalSessionsResponse, error) {
	return hostedMapped(ctx, h, func() (db.SignalSessionsResponse, error) {
		return h.physical.GetAnalyticsSignalSessions(ctx, f, signal, limit)
	}, func(p *db.SignalSessionsResponse, r *hostedRefs) {
		p.Sessions = slices.Clone(p.Sessions)
		for i := range p.Sessions {
			r.add(&p.Sessions[i].SessionID)
		}
	})
}
func (h *HostedStore) RecentEdits(ctx context.Context, f db.RecentEditsParams) (db.RecentEditsResult, error) {
	return hostedMapped(ctx, h, func() (db.RecentEditsResult, error) { return h.physical.RecentEdits(ctx, f) }, func(p *db.RecentEditsResult, r *hostedRefs) {
		p.Files = slices.Clone(p.Files)
		for i := range p.Files {
			f := &p.Files[i]
			r.add(&f.LastSessionID)
			f.Edits = slices.Clone(f.Edits)
			for j := range f.Edits {
				r.add(&f.Edits[j].SessionID)
			}
		}
	})
}
func (h *HostedStore) ListArchiveWorktreeCandidates(ctx context.Context, f db.ArchiveWorktreeCandidateRequest) ([]db.WorktreeReclassificationCandidate, error) {
	return hostedMapped(ctx, h, func() ([]db.WorktreeReclassificationCandidate, error) {
		return h.physical.ListArchiveWorktreeCandidates(ctx, f)
	}, func(p *[]db.WorktreeReclassificationCandidate, r *hostedRefs) {
		*p = slices.Clone(*p)
		for i := range *p {
			c := &(*p)[i]
			c.Examples = slices.Clone(c.Examples)
			for j := range c.Examples {
				r.add(&c.Examples[j].SessionID)
			}
		}
	})
}
func (r *hostedRefs) report(p *activity.Report) {
	p.BySession = slices.Clone(p.BySession)
	for i := range p.BySession {
		r.add(&p.BySession[i].SessionID)
	}
	p.Intervals = slices.Clone(p.Intervals)
	for i := range p.Intervals {
		r.add(&p.Intervals[i].SessionID)
	}
}
func (h *HostedStore) GetActivityReport(ctx context.Context, f db.AnalyticsFilter, q activity.Query) (activity.Report, error) {
	return hostedMapped(ctx, h, func() (activity.Report, error) { return h.physical.GetActivityReport(ctx, f, q) }, func(p *activity.Report, r *hostedRefs) { r.report(p) })
}

// Map keys are copied into addressable entries for the same single batch lookup.
func hostedMapKeys[T any](m map[string]T, r *hostedRefs) func() map[string]T {
	type entry struct {
		key   string
		value T
	}
	entries := make([]entry, 0, len(m))
	for k, v := range m {
		entries = append(entries, entry{k, v})
	}
	for i := range entries {
		r.add(&entries[i].key)
	}
	return func() map[string]T {
		if m == nil {
			return nil
		}
		out := make(map[string]T, len(m))
		for _, e := range entries {
			out[e.key] = e.value
		}
		return out
	}
}
func (h *HostedStore) BuildActivityReportArtifacts(ctx context.Context, f db.AnalyticsFilter, q activity.Query, progress activity.ProgressFunc) (activity.CandidateArtifacts, error) {
	return hostedRead(ctx, h, func(_ hostedRevision) (activity.CandidateArtifacts, error) {
		p, err := h.physical.BuildActivityReportArtifacts(ctx, f, q, progress)
		if err != nil {
			return p, err
		}
		r := hostedRefs{}
		r.report(&p.Report)
		p.Sessions = slices.Clone(p.Sessions)
		for i := range p.Sessions {
			r.add(&p.Sessions[i].SessionID)
		}
		membership := hostedMapKeys(p.Membership, &r)
		if err = r.mapIDs(ctx, h); err != nil {
			return p, err
		}
		p.Membership = membership()
		return p, nil
	})
}
func (h *HostedStore) ActivityReportSourceProbe(ctx context.Context) (activity.SourceProbe, error) {
	return hostedRead(ctx, h, func(rev hostedRevision) (activity.SourceProbe, error) {
		p, err := h.physical.ActivityReportSourceProbe(ctx)
		p.HostedIdentityRevision = rev.Identity
		p.HostedSelectionRevision = rev.Selection
		p.HostedCorpusRevision = rev.Corpus
		return p, err
	})
}
func (h *HostedStore) GetSessionUsageRows(ctx context.Context, aliases []string) (*activity.SessionUsageRows, error) {
	return hostedRead(ctx, h, func(_ hostedRevision) (*activity.SessionUsageRows, error) {
		ids, err := h.resolveBatch(ctx, aliases, false, false)
		if err != nil {
			return nil, err
		}
		p, err := h.physical.GetSessionUsageRows(ctx, ids)
		if err != nil || p == nil {
			return p, err
		}
		v := *p
		r := hostedRefs{}
		v.Rows = slices.Clone(v.Rows)
		for i := range v.Rows {
			r.add(&v.Rows[i].SessionID)
			r.add(&v.Rows[i].SourceSessionID)
		}
		raw := hostedMapKeys(v.RawOutputTokensBySession, &r)
		dedup := hostedMapKeys(v.DeduplicatedOutputTokens, &r)
		discarded := hostedMapKeys(v.DiscardedContributingSessions, &r)
		coverage := hostedMapKeys(v.CanonicalTokenCoverageBySession, &r)
		if err = r.mapIDs(ctx, h); err != nil {
			return nil, err
		}
		v.RawOutputTokensBySession = raw()
		v.DeduplicatedOutputTokens = dedup()
		v.DiscardedContributingSessions = discarded()
		v.CanonicalTokenCoverageBySession = coverage()
		return &v, nil
	})
}

var _ db.ActivityReportArtifactStore = (*HostedStore)(nil)
var _ db.ActivityReportProbeStore = (*HostedStore)(nil)
var _ db.ActivityReportTokenStore = (*HostedStore)(nil)
