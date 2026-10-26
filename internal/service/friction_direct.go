package service

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/timeutil"
)

func (b *directBackend) SupportsFriction() bool {
	_, ok := b.db.(db.FrictionReader)
	return ok
}

func (b *directBackend) frictionReader() (db.FrictionReader, error) {
	fr, ok := b.db.(db.FrictionReader)
	if !ok {
		return nil, errors.New("this archive does not store friction digests")
	}
	return fr, nil
}

func (b *directBackend) FrictionDigest(
	ctx context.Context, date string,
) (*FrictionDigestView, error) {
	fr, err := b.frictionReader()
	if err != nil {
		return nil, err
	}
	if date == "" {
		latest, err := fr.LatestFrictionDigestDate(ctx)
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
	d, err := fr.GetFrictionDigest(ctx, date)
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

func (b *directBackend) FrictionDigests(
	ctx context.Context, from, to string,
) ([]FrictionDigestListItem, error) {
	fr, err := b.frictionReader()
	if err != nil {
		return nil, err
	}
	rows, err := fr.ListFrictionDigests(ctx, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]FrictionDigestListItem, 0, len(rows))
	for _, d := range rows {
		out = append(out, FrictionDigestListItem{
			Date: d.Date, Timezone: d.Timezone, RulesVersion: d.RulesVersion,
			BuiltAt: d.BuiltAt.UTC(), Revision: d.Revision, SessionsScanned: d.SessionsScanned,
		})
	}
	return out, nil
}

func (b *directBackend) FrictionFindings(
	ctx context.Context, f FrictionFindingFilter,
) (*FrictionFindingPage, error) {
	fr, err := b.frictionReader()
	if err != nil {
		return nil, err
	}
	rows, next, err := fr.ListFrictionFindings(ctx, db.FrictionFindingFilter{
		Date: f.Date, Kind: f.Kind, SessionID: f.SessionID,
		Fingerprint: f.Fingerprint, Limit: f.Limit, Cursor: f.Cursor,
	})
	if err != nil {
		return nil, err
	}
	page := &FrictionFindingPage{Findings: make([]FrictionFindingView, 0, len(rows)), NextCursor: next}
	for _, r := range rows {
		page.Findings = append(page.Findings, FrictionFindingView{
			SessionID: r.SessionID, Kind: r.Kind, Detector: r.Detector,
			MessageOrdinal: r.MessageOrdinal, CallIndex: r.CallIndex, ToolName: r.ToolName,
			Label: r.Label, Text: r.Text, Evidence: r.Evidence, Title: r.Title,
			Fingerprint: r.Fingerprint, OccurredAt: r.OccurredAt, Seq: r.Seq,
			RulesVersion: r.RulesVersion,
		})
	}
	return page, nil
}

func (b *directBackend) FrictionPatterns(
	ctx context.Context, f FrictionPatternFilter,
) (*FrictionPatternPage, error) {
	fr, err := b.frictionReader()
	if err != nil {
		return nil, err
	}
	rows, next, err := fr.ListFrictionPatterns(ctx, db.FrictionPatternFilter{
		Kind: f.Kind, Since: f.Since, Limit: f.Limit, Cursor: f.Cursor,
	})
	if err != nil {
		return nil, err
	}
	page := &FrictionPatternPage{Patterns: make([]FrictionPatternView, 0, len(rows)), NextCursor: next}
	for _, p := range rows {
		page.Patterns = append(page.Patterns, FrictionPatternView{
			Fingerprint: p.Fingerprint, Kind: p.Kind, Title: p.Title,
			FirstSeenDate: p.FirstSeenDate, LastSeenDate: p.LastSeenDate,
			OccurrenceCount: p.OccurrenceCount, SessionCount: p.SessionCount,
			LastSubjectID: p.LastSubjectID, LastOrdinal: p.LastOrdinal,
		})
	}
	return page, nil
}
