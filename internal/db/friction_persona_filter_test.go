package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFrictionPersonaFilter(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	var subjects []FrictionDigestSubject
	var patterns []FrictionPatternUpdate
	for _, s := range []struct{ id, persona string }{{"s-helper", "helper"}, {"s-reviewer", "reviewer"}, {"s-plain", ""}} {
		require.NoError(t, d.UpsertSession(ctx, Session{ID: s.id, Project: "p", Machine: "local", Agent: "claude"}))
		var dims *FrictionSessionDims
		if s.persona != "" {
			dims = &FrictionSessionDims{SessionID: s.id, Persona: s.persona, DimsSource: "nanoclaw"}
		}
		ord := 1
		require.NoError(t, d.ReplaceSessionFriction(ctx, s.id, []FrictionFinding{{
			SessionID: s.id, Kind: "correction", Detector: "correction.chat",
			MessageOrdinal: &ord, Text: "no, not there", Title: "[friction/correction] " + s.id,
			Fingerprint: "fl1:" + s.id, RulesVersion: "friction-v1",
		}}, dims, "friction-v1", "hash-"+s.id))
		subjects = append(subjects, FrictionDigestSubject{SubjectID: s.id, Date: "2026-07-01", SubjectKind: "session"})
		patterns = append(patterns, FrictionPatternUpdate{
			Fingerprint: "fl1:" + s.id, Kind: "correction",
			Title: "[friction/correction] " + s.id, Date: "2026-07-01", SubjectID: s.id, Occurrences: 1,
		})
	}
	require.NoError(t, d.SaveFrictionDigest(ctx, FrictionDigest{
		Date: "2026-07-01", Timezone: "UTC", RulesVersion: "friction-v1",
		BuiltAt: time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC), Revision: 1, SessionsScanned: 3,
		SnapshotJSON: []byte("{}"), SummaryJSON: []byte("{}"), Markdown: []byte("# Friction Log — 2026-07-01\n"),
		MarkdownSHA256: "x", RunID: "01J00000000000000000000000",
	}, subjects, patterns))

	tests := []struct {
		persona string
		want    []string
	}{
		{"helper", []string{"s-helper"}},
		{"reviewer", []string{"s-reviewer"}},
		{"nobody", nil},
		{"", []string{"s-helper", "s-plain", "s-reviewer"}},
	}
	for _, tt := range tests {
		t.Run("findings/"+tt.persona, func(t *testing.T) {
			rows, _, err := d.ListFrictionFindings(ctx, FrictionFindingFilter{Persona: tt.persona, Limit: 100})
			require.NoError(t, err)
			var got []string
			for _, f := range rows {
				got = append(got, f.SessionID)
			}
			assert.ElementsMatch(t, tt.want, got)
		})
		t.Run("patterns/"+tt.persona, func(t *testing.T) {
			rows, _, err := d.ListFrictionPatterns(ctx, FrictionPatternFilter{Persona: tt.persona, Limit: 100})
			require.NoError(t, err)
			var got []string
			for _, p := range rows {
				got = append(got, p.LastSubjectID)
			}
			assert.ElementsMatch(t, tt.want, got)
		})
	}
}
