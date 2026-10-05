package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestHostedStoreDoesNotExposeUnscopedFrictionTables(t *testing.T) {
	h := &HostedStore{}
	ctx := t.Context()
	checks := []struct {
		name string
		run  func() error
	}{
		{"subjects", func() error { _, err := h.FrictionSubjectsForDate(ctx, "2026-10-01", time.UTC, false); return err }},
		{"findings for subjects", func() error { _, err := h.FrictionFindingsForSubjects(ctx, []string{"session"}); return err }},
		{"usage", func() error { _, err := h.FrictionUsageForSessions(ctx, []string{"session"}); return err }},
		{"archive spend", func() error { _, err := h.FrictionArchiveSpend(ctx, "2026-10-01", "2026-10-01", time.UTC); return err }},
		{"save digest", func() error { return h.SaveFrictionDigest(ctx, db.FrictionDigest{}, nil, nil) }},
		{"get digest", func() error { _, err := h.GetFrictionDigest(ctx, "2026-10-01"); return err }},
		{"latest digest", func() error { _, err := h.LatestFrictionDigestDate(ctx); return err }},
		{"earliest session", func() error { _, err := h.EarliestSessionDate(ctx, time.UTC); return err }},
		{"render digest", func() error { return h.UpdateFrictionDigestRender(ctx, "2026-10-01", nil, nil, 1) }},
		{"list findings", func() error { _, _, err := h.ListFrictionFindings(ctx, db.FrictionFindingFilter{}); return err }},
		{"list digests", func() error { _, err := h.ListFrictionDigests(ctx, "", ""); return err }},
		{"list patterns", func() error { _, _, err := h.ListFrictionPatterns(ctx, db.FrictionPatternFilter{}); return err }},
		{"get filing links", func() error { _, err := h.GetFrictionIssueLinks(ctx, []string{"fl1:test"}); return err }},
		{"upsert filing link", func() error { return h.UpsertFrictionIssueLink(ctx, db.FrictionIssueLink{}) }},
		{"delete filing link", func() error { return h.DeleteFrictionIssueLink(ctx, "fl1:test") }},
		{"due filings", func() error { _, err := h.DueFrictionFilings(ctx, time.Now(), 10); return err }},
		{"digest dates", func() error { _, err := h.DigestDatesForFingerprints(ctx, []string{"fl1:test"}); return err }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			require.ErrorIs(t, check.run(), db.ErrReadOnly)
		})
	}
}
