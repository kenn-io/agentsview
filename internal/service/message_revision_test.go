package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

type changingRevisionStore struct {
	db.Store
	reads int
}

func (s *changingRevisionStore) GetSession(
	_ context.Context, id string,
) (*db.Session, error) {
	s.reads++
	revision := "1"
	if s.reads > 1 {
		revision = "2"
	}
	return &db.Session{ID: id, TranscriptRevision: &revision}, nil
}

func (s *changingRevisionStore) GetMessagesWindow(
	_ context.Context, id string, _ db.MessageWindow,
) ([]db.Message, error) {
	return []db.Message{dbtest.UserMsg(id, 0, "observed page")}, nil
}

func TestMessagesRevisionBoundReadRejectsChangedTranscript(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	dbtest.SeedSessionWithMessages(t, database, "revisioned", "proj", []db.Message{
		dbtest.UserMsg("revisioned", 0, "original evidence"),
		dbtest.AsstMsg("revisioned", 1, "original answer"),
	}, dbtest.WithMessageCounts(2, 1))
	backend := service.NewDirectBackend(database, nil)

	first, err := backend.Messages(t.Context(), "revisioned", service.MessageFilter{Limit: 20})
	require.NoError(t, err)
	require.NotEmpty(t, first.TranscriptRevision)
	require.NotEmpty(t, first.EvidenceSource)
	require.Len(t, first.Messages, 2)

	same, err := backend.Messages(t.Context(), "revisioned", service.MessageFilter{
		Limit: 20, ExpectedRevision: first.TranscriptRevision,
		EvidenceSource: first.EvidenceSource,
	})
	require.NoError(t, err)
	assert.Equal(t, first.TranscriptRevision, same.TranscriptRevision)

	require.NoError(t, database.ReplaceSessionMessages(t.Context(), "revisioned", []db.Message{
		dbtest.UserMsg("revisioned", 0, "changed evidence"),
		dbtest.AsstMsg("revisioned", 1, "changed answer"),
	}))

	_, err = backend.Messages(t.Context(), "revisioned", service.MessageFilter{
		Limit: 20, ExpectedRevision: first.TranscriptRevision,
	})
	require.ErrorIs(t, err, service.ErrSourceChanged)

	_, err = backend.Messages(t.Context(), "revisioned", service.MessageFilter{
		Limit: 20, EvidenceSource: "different-archive",
	})
	require.ErrorIs(t, err, service.ErrSourceChanged)
}

func TestMessagesUnboundReadOmitsRevisionWhenTranscriptChangesDuringRead(t *testing.T) {
	store := &changingRevisionStore{}
	backend := service.NewReadOnlyBackend(store)

	result, err := backend.Messages(t.Context(), "changing", service.MessageFilter{Limit: 20})
	require.NoError(t, err)
	require.Len(t, result.Messages, 1)
	assert.Empty(t, result.TranscriptRevision)
	assert.Empty(t, result.EvidenceSource)

	store.reads = 0
	_, err = backend.Messages(t.Context(), "changing", service.MessageFilter{
		Limit: 20, ExpectedRevision: "1",
	})
	require.ErrorIs(t, err, service.ErrSourceChanged)
}

func TestSearchContentIncludesTranscriptRevision(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	dbtest.SeedSessionWithMessages(t, database, "revisioned", "proj", []db.Message{
		dbtest.UserMsg("revisioned", 0, "bounded evidence marker"),
		dbtest.AsstMsg("revisioned", 1, "answer"),
	}, dbtest.WithMessageCounts(2, 1))
	backend := service.NewDirectBackend(database, nil)

	session, err := database.GetSession(t.Context(), "revisioned")
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.TranscriptRevision)

	for _, mode := range []string{"substring", "regex", "terms"} {
		t.Run(mode, func(t *testing.T) {
			result, err := backend.SearchContent(t.Context(), service.ContentSearchRequest{
				Pattern: "bounded evidence marker", Mode: mode,
				Limit: 10, IncludeOneShot: true,
			})
			require.NoError(t, err)
			require.Len(t, result.Matches, 1)
			assert.True(t, result.RevisionBound)
			assert.Equal(t, *session.TranscriptRevision,
				result.Matches[0].TranscriptRevision)
		})
	}
}
