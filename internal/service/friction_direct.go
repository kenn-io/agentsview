package service

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/timeutil"
)

func (b *directBackend) SupportsFriction() bool {
	_, ok := b.db.(db.FrictionReader)
	return ok
}

var frictionKinds = []friction.Kind{
	friction.KindCorrection, friction.KindError, friction.KindWorkaround, friction.KindDeferral,
	friction.KindPattern, friction.KindFrustration, friction.KindInterruption,
}

// validateFrictionFilter applies the HTTP routes' rules to direct reads, so a
// malformed date is an error rather than a silent lexical comparison.
func validateFrictionFilter(dates map[string]string, kind string, limit int) error {
	for field, date := range dates {
		if date != "" && !timeutil.IsValidDate(date) {
			return fmt.Errorf("invalid %s %q: use YYYY-MM-DD", field, date)
		}
	}
	if kind != "" && !slices.Contains(frictionKinds, friction.Kind(kind)) {
		return fmt.Errorf("invalid kind %q", kind)
	}
	if limit < 0 || limit > db.MaxFrictionListLimit {
		return fmt.Errorf("limit must be between 1 and %d", db.MaxFrictionListLimit)
	}
	return nil
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
	if err := validateFrictionFilter(map[string]string{"from": from, "to": to}, "", 0); err != nil {
		return nil, err
	}
	if from != "" && to != "" && from > to {
		return nil, fmt.Errorf("from %s is after to %s", from, to)
	}
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
	if err := validateFrictionFilter(map[string]string{"date": f.Date}, f.Kind, f.Limit); err != nil {
		return nil, err
	}
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
	if err := validateFrictionFilter(map[string]string{"since": f.Since}, f.Kind, f.Limit); err != nil {
		return nil, err
	}
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
