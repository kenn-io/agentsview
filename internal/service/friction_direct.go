package service

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/timeutil"
)

func (b *directBackend) SupportsFriction() bool { return true }

func (b *directBackend) FrictionDigest(
	ctx context.Context, date string,
) (*FrictionDigestView, error) {
	if date == "" {
		latest, err := b.db.LatestFrictionDigestDate(ctx)
		if err != nil {
			return nil, err
		}
		if latest == "" {
			return nil, ErrFrictionDigestNotFound
		}
		date = latest
	}
	if !timeutil.IsValidDate(date) {
		return nil, fmt.Errorf("invalid date %q: use YYYY-MM-DD", date)
	}
	d, err := b.db.GetFrictionDigest(ctx, date)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, ErrFrictionDigestNotFound
	}
	return &FrictionDigestView{
		Date: d.Date, Timezone: d.Timezone, BuiltAt: d.BuiltAt.UTC(),
		Revision: d.Revision, Markdown: string(d.Markdown), Summary: d.SummaryJSON,
	}, nil
}

func (b *directBackend) FrictionPatterns(
	ctx context.Context, f FrictionPatternFilter,
) ([]FrictionPatternView, error) {
	rows, _, err := b.db.ListFrictionPatterns(ctx, db.FrictionPatternFilter{
		Kind: f.Kind, Since: f.Since, Limit: f.Limit,
		LinkState: frictionLinkState(f.Linked),
	})
	if err != nil {
		return nil, err
	}
	return frictionPatternViews(rows, func(string, *int) string { return "" }), nil
}

func frictionPatternViews(
	rows []db.FrictionPattern, sessionURL func(id string, ordinal *int) string,
) []FrictionPatternView {
	out := make([]FrictionPatternView, 0, len(rows))
	for _, p := range rows {
		out = append(out, FrictionPatternView{
			Fingerprint: p.Fingerprint, Kind: p.Kind, Title: p.Title,
			FirstSeenDate: p.FirstSeenDate, LastSeenDate: p.LastSeenDate,
			OccurrenceCount: p.OccurrenceCount, SessionCount: p.SessionCount,
			LastSubjectID: p.LastSubjectID, LastOrdinal: p.LastOrdinal,
			LastSessionURL: sessionURL(p.LastSubjectID, p.LastOrdinal),
		})
	}
	return out
}

// FrictionPatternViews is exported for the HTTP backend, which decodes the
// same row shape from the API.
func FrictionPatternViews(
	rows []db.FrictionPattern, sessionURL func(id string, ordinal *int) string,
) []FrictionPatternView {
	return frictionPatternViews(rows, sessionURL)
}
