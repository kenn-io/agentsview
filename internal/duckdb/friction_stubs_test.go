package duckdb

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestFrictionReviewStubsAreReadOnly(t *testing.T) {
	s := &Store{}
	ctx := t.Context()
	calls := map[string]func() error{
		"subjects": func() error { _, err := s.FrictionSubjectsForDate(ctx, "2026-09-15", time.UTC, false); return err },
		"findings": func() error { _, err := s.FrictionFindingsForSubjects(ctx, []string{"a"}); return err },
		"usage":    func() error { _, err := s.FrictionUsageForSessions(ctx, []string{"a"}); return err },
		"archive":  func() error { _, err := s.FrictionArchiveSpend(ctx, "2026-09-08", "2026-09-14", time.UTC); return err },
		"save":     func() error { return s.SaveFrictionDigest(ctx, db.FrictionDigest{}, nil, nil) },
		"get":      func() error { _, err := s.GetFrictionDigest(ctx, "2026-09-15"); return err },
		"latest":   func() error { _, err := s.LatestFrictionDigestDate(ctx); return err },
		"earliest": func() error { _, err := s.EarliestSessionDate(ctx, time.UTC); return err },
		"render":   func() error { return s.UpdateFrictionDigestRender(ctx, "2026-09-15", nil, nil, 2) },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) { require.ErrorIs(t, call(), db.ErrReadOnly) })
	}
}
