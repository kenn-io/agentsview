package review

import (
	"context"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
)

type savedDigest struct {
	digest   db.FrictionDigest
	subjects []db.FrictionDigestSubject
	patterns []db.FrictionPatternUpdate
}

type fakeReviewStore struct {
	mu       sync.Mutex
	sessions map[string][]db.FrictionSubject // by date
	findings map[string][]db.FrictionFinding // by subject
	digests  map[string]db.FrictionDigest
	digested map[string]string // subject -> date
	patterns map[string]db.FrictionPattern
	saves    []savedDigest
}

func newFakeReviewStore() *fakeReviewStore {
	return &fakeReviewStore{
		sessions: map[string][]db.FrictionSubject{}, findings: map[string][]db.FrictionFinding{},
		digests: map[string]db.FrictionDigest{}, digested: map[string]string{}, patterns: map[string]db.FrictionPattern{},
	}
}

func (f *fakeReviewStore) FrictionSubjectsForDate(_ context.Context, date string, _ *time.Location, includeDigested bool) ([]db.FrictionSubject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.FrictionSubject
	for _, s := range f.sessions[date] {
		if d, ok := f.digested[s.SubjectID]; ok && (!includeDigested || d != date) {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeReviewStore) FrictionFindingsForSubjects(_ context.Context, ids []string) ([]db.FrictionFinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.FrictionFinding
	for _, id := range ids {
		out = append(out, f.findings[id]...)
	}
	return out, nil
}

func (f *fakeReviewStore) FrictionUsageForSessions(context.Context, []string) (map[string]friction.SessionUsage, error) {
	return map[string]friction.SessionUsage{}, nil
}

func (f *fakeReviewStore) FrictionArchiveSpend(context.Context, string, string, *time.Location) (*friction.ArchiveSpend, error) {
	return nil, nil
}

func (f *fakeReviewStore) SaveFrictionDigest(_ context.Context, d db.FrictionDigest, subjects []db.FrictionDigestSubject, patterns []db.FrictionPatternUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.digests[d.Date] = d
	for _, s := range subjects {
		f.digested[s.SubjectID] = s.Date
	}
	for _, u := range patterns {
		p, ok := f.patterns[u.Fingerprint]
		if !ok {
			p = db.FrictionPattern{Fingerprint: u.Fingerprint, Kind: u.Kind, Title: u.Title, FirstSeenDate: u.Date}
		}
		p.LastSeenDate, p.LastSubjectID, p.LastOrdinal = u.Date, u.SubjectID, u.Ordinal
		p.OccurrenceCount += u.Occurrences
		p.SessionCount++
		f.patterns[u.Fingerprint] = p
	}
	f.saves = append(f.saves, savedDigest{digest: d, subjects: subjects, patterns: patterns})
	return nil
}

func (f *fakeReviewStore) GetFrictionDigest(_ context.Context, date string) (*db.FrictionDigest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.digests[date]; ok {
		return &d, nil
	}
	return nil, nil
}

func (f *fakeReviewStore) LatestFrictionDigestDate(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	latest := ""
	for date := range f.digests {
		latest = max(latest, date)
	}
	return latest, nil
}

func (f *fakeReviewStore) EarliestSessionDate(context.Context, *time.Location) (string, error) {
	return "2026-09-01", nil
}

func (f *fakeReviewStore) UpdateFrictionDigestRender(_ context.Context, date string, md, summary []byte, revision int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.digests[date]
	d.Markdown, d.SummaryJSON, d.Revision = md, summary, revision
	f.digests[date] = d
	return nil
}

func (f *fakeReviewStore) FrictionDigestedSubjects(_ context.Context, ids []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for _, id := range ids {
		if d, ok := f.digested[id]; ok {
			out[id] = d
		}
	}
	return out, nil
}

func (f *fakeReviewStore) FrictionPatternsByFingerprint(_ context.Context, fps []string) (map[string]db.FrictionPattern, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]db.FrictionPattern{}
	for _, fp := range fps {
		if p, ok := f.patterns[fp]; ok {
			out[fp] = p
		}
	}
	return out, nil
}

var _ Store = (*fakeReviewStore)(nil)
