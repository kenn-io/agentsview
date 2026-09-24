//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestPGFrictionPersonaFilter(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })
	ctx := context.Background()

	local := testDB(t)
	ps, err := New(pgURL, "agentsview", local, "machine-persona-filter", true, storage.PusherOptions{})
	require.NoError(t, err)
	defer ps.Close()
	require.NoError(t, ps.EnsureSchema(ctx))

	var subjects []db.FrictionDigestSubject
	var patterns []db.FrictionPatternUpdate
	for _, s := range []struct{ id, persona string }{{"s-helper", "helper"}, {"s-reviewer", "reviewer"}, {"s-plain", ""}} {
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: s.id, Project: "p", Machine: "local", Agent: "claude"}))
		var dims *db.FrictionSessionDims
		if s.persona != "" {
			dims = &db.FrictionSessionDims{SessionID: s.id, Persona: s.persona, DimsSource: "nanoclaw"}
		}
		ord := 1
		require.NoError(t, local.ReplaceSessionFriction(ctx, s.id, []db.FrictionFinding{{
			SessionID: s.id, Kind: "correction", Detector: "correction.chat",
			MessageOrdinal: &ord, Text: "no, not there", Title: "[friction/correction] " + s.id,
			Fingerprint: "fl1:" + s.id, RulesVersion: "friction-v1",
		}}, dims, "friction-v1", "hash-"+s.id))
		subjects = append(subjects, db.FrictionDigestSubject{SubjectID: s.id, Date: "2026-07-01", SubjectKind: "session"})
		patterns = append(patterns, db.FrictionPatternUpdate{
			Fingerprint: "fl1:" + s.id, Kind: "correction",
			Title: "[friction/correction] " + s.id, Date: "2026-07-01", SubjectID: s.id, Occurrences: 1,
		})
	}
	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err)

	// Read back from the schema the pusher wrote (New(…, "agentsview", …)).
	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.SaveFrictionDigest(ctx, db.FrictionDigest{
		Date: "2026-07-01", Timezone: "UTC", RulesVersion: "friction-v1",
		BuiltAt: time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC), Revision: 1, SessionsScanned: 3,
		SnapshotJSON: []byte("{}"), SummaryJSON: []byte("{}"), Markdown: []byte("# Friction Log — 2026-07-01\n"),
		MarkdownSHA256: "x", RunID: "01J00000000000000000000000",
	}, subjects, patterns))

	for _, tt := range []struct {
		persona string
		want    []string
	}{
		{"helper", []string{"s-helper"}},
		{"nobody", nil},
		{"", []string{"s-helper", "s-plain", "s-reviewer"}},
	} {
		rows, _, err := store.ListFrictionFindings(ctx, db.FrictionFindingFilter{Persona: tt.persona, Limit: 100})
		require.NoError(t, err)
		var got []string
		for _, f := range rows {
			got = append(got, f.SessionID)
		}
		assert.ElementsMatch(t, tt.want, got, "findings %q", tt.persona)

		pats, _, err := store.ListFrictionPatterns(ctx, db.FrictionPatternFilter{Persona: tt.persona, Limit: 100})
		require.NoError(t, err)
		got = nil
		for _, p := range pats {
			got = append(got, p.LastSubjectID)
		}
		assert.ElementsMatch(t, tt.want, got, "patterns %q", tt.persona)
	}
}
