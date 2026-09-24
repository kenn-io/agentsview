//go:build pgtest

package postgres

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
)

type frictionReviewStore interface {
	FrictionSubjectsForDate(ctx context.Context, date string, loc *time.Location, includeDigested bool) ([]db.FrictionSubject, error)
	FrictionFindingsForSubjects(ctx context.Context, ids []string) ([]db.FrictionFinding, error)
	SaveFrictionDigest(ctx context.Context, d db.FrictionDigest, s []db.FrictionDigestSubject, p []db.FrictionPatternUpdate) error
	GetFrictionDigest(ctx context.Context, date string) (*db.FrictionDigest, error)
	LatestFrictionDigestDate(ctx context.Context) (string, error)
	EarliestSessionDate(ctx context.Context, loc *time.Location) (string, error)
	UpdateFrictionDigestRender(ctx context.Context, date string, md, summary []byte, revision int) error
}

func openFrictionParityStores(t *testing.T) (*db.DB, *Store) {
	t.Helper()
	pgURL := testPGURL(t)
	const schema = "agentsview_friction_digest_test"
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	ctx := t.Context()
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Close() })
	require.NoError(t, EnsureSchema(ctx, pg, schema))

	local, err := db.Open(ctx, filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = local.Close() })
	seed := func(id, ended string, parent *string) {
		s := db.Session{
			ID: id, Project: "p", Machine: "machine", Agent: "claude",
			EndedAt: &ended, MessageCount: 1, UserMessageCount: 1, ParentSessionID: parent,
		}
		if parent != nil {
			s.RelationshipType = "subagent"
		}
		require.NoError(t, local.UpsertSession(ctx, s))
		require.NoError(t, local.InsertMessages(ctx, []db.Message{{
			SessionID: id, Ordinal: 0,
			Role: "user", Content: "hi", ContentLength: 2, Timestamp: ended,
		}}))
		occurred := time.Date(2026, 9, 15, 1, 0, 0, 0, time.UTC)
		require.NoError(t, local.ReplaceSessionFriction(ctx, id, []db.FrictionFinding{{
			SessionID: id, Kind: "deferral", Detector: "deferral", MessageOrdinal: new(0),
			Label: "next session", Title: "[friction/deferral] " + id + ": next session",
			Fingerprint: "fl1:" + id, OccurredAt: &occurred, Seq: 0, RulesVersion: "friction-v1",
		}}, &db.FrictionSessionDims{SessionID: id, Seat: "seat-02"}, "friction-v1", "h-"+id))
	}
	seed("a", "2026-09-15T01:00:00Z", nil)
	root := "a"
	seed("b", "2026-09-15T23:30:00Z", &root)
	seed("c", "2026-09-16T00:30:00Z", nil)
	seed("excluded", "2026-09-15T02:00:00Z", nil)
	require.NoError(t, local.ReplaceSessionFriction(ctx, "excluded", nil,
		&db.FrictionSessionDims{SessionID: "excluded", ReviewExcluded: true}, "friction-v1", "h-excluded"))

	syncer := &Sync{pg: pg, local: local, machine: "machine", schema: schema, schemaDone: true}
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return local, store
}

func TestFrictionReviewStoreParity(t *testing.T) {
	local, pgStore := openFrictionParityStores(t)
	for name, s := range map[string]frictionReviewStore{"sqlite": local, "postgres": pgStore} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			subjects, err := s.FrictionSubjectsForDate(ctx, "2026-09-15", time.UTC, false)
			require.NoError(t, err)
			ids := []string{}
			for _, sub := range subjects {
				ids = append(ids, sub.SubjectID)
				if sub.SubjectID == "b" {
					assert.True(t, sub.IsSubAgent)
				}
				assert.Equal(t, "seat-02", sub.Dims.Seat)
				assert.Equal(t, "friction-v1", sub.RulesVersion)
			}
			assert.ElementsMatch(t, []string{"a", "b"}, ids)

			findings, err := s.FrictionFindingsForSubjects(ctx, []string{"b", "a"})
			require.NoError(t, err)
			require.Len(t, findings, 2)
			assert.Equal(t, "a", findings[0].SessionID)
			assert.Equal(t, "next session", findings[0].Label)

			earliest, err := s.EarliestSessionDate(ctx, time.UTC)
			require.NoError(t, err)
			assert.Equal(t, "2026-09-15", earliest)

			d := db.FrictionDigest{
				Date: "2026-09-15", Timezone: "UTC", RulesVersion: "friction-v1",
				BuiltAt: time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC), Revision: 1, SessionsScanned: 2,
				SnapshotJSON: []byte(`{"date":"2026-09-15"}`), SummaryJSON: []byte("{}\n"),
				Markdown: []byte("md\n"), MarkdownSHA256: "abc", RunID: "run-1",
			}
			pats := []db.FrictionPatternUpdate{
				{Fingerprint: "fl1:a", Kind: "deferral", Title: "t", Date: "2026-09-15", SubjectID: "a", Ordinal: new(0), Occurrences: 1},
			}
			require.NoError(t, s.SaveFrictionDigest(ctx, d,
				[]db.FrictionDigestSubject{{SubjectID: "a", Date: "2026-09-15", SubjectKind: friction.SubjectSession}}, pats))
			require.ErrorIs(t, s.SaveFrictionDigest(ctx, d, nil, pats), db.ErrFrictionDigestConflict)
			got, err := s.GetFrictionDigest(ctx, "2026-09-15")
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, d, *got)

			after, err := s.FrictionSubjectsForDate(ctx, "2026-09-15", time.UTC, false)
			require.NoError(t, err)
			require.Len(t, after, 1)
			assert.Equal(t, "b", after[0].SubjectID)

			latest, err := s.LatestFrictionDigestDate(ctx)
			require.NoError(t, err)
			assert.Equal(t, "2026-09-15", latest)
			require.NoError(t, s.UpdateFrictionDigestRender(ctx, "2026-09-15", []byte("md2\n"), []byte("{}\n"), 2))
			require.ErrorIs(t, s.UpdateFrictionDigestRender(ctx, "2026-09-15", []byte("x"), []byte("{}"), 2), db.ErrFrictionDigestConflict)
			missing, err := s.GetFrictionDigest(ctx, "2020-01-01")
			require.NoError(t, err)
			assert.Nil(t, missing)
		})
	}
}

func TestFrictionAvailabilityProbe(t *testing.T) {
	_, store := openFrictionParityStores(t)
	store.DetectFrictionAvailability(t.Context())
	assert.True(t, store.FrictionAvailable())
	assert.True(t, pushSchemaCurrent(t.Context(), store.DB()))
}
