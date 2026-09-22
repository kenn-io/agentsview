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

// syncBetweenSearchAndContext is the archive as the service sees it when
// a sync replaces a session's messages after the search query ran but
// before its context windows were read. The first context read applies
// the replacement, then every read answers from the replaced transcript.
type syncBetweenSearchAndContext struct {
	*db.DB
	replaced bool
}

func (s *syncBetweenSearchAndContext) GetMessagesWindow(
	ctx context.Context, sessionID string, w db.MessageWindow,
) ([]db.Message, error) {
	if !s.replaced {
		s.replaced = true
		err := s.ReplaceSessionMessages(ctx, sessionID, []db.Message{
			dbtest.UserMsg(sessionID, 0, "changed lead-in"),
			dbtest.UserMsg(sessionID, 1, "bounded evidence marker"),
			dbtest.AsstMsg(sessionID, 2, "changed answer"),
		})
		if err != nil {
			return nil, err
		}
	}
	return s.DB.GetMessagesWindow(ctx, sessionID, w)
}

func TestSearchContentContextFromNewerRevisionIsNotBound(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	dbtest.SeedSessionWithMessages(t, database, "revisioned", "proj", []db.Message{
		dbtest.UserMsg("revisioned", 0, "original lead-in"),
		dbtest.UserMsg("revisioned", 1, "bounded evidence marker"),
		dbtest.AsstMsg("revisioned", 2, "original answer"),
	}, dbtest.WithMessageCounts(3, 2))
	request := service.ContentSearchRequest{
		Pattern: "bounded evidence marker", Mode: "substring",
		Limit: 10, Context: 1, IncludeOneShot: true,
	}

	stable, err := service.NewDirectBackend(database, nil).SearchContent(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, stable.Matches, 1)
	assert.True(t, stable.RevisionBound,
		"context read at the match revision keeps the result bound")
	require.Len(t, stable.Matches[0].ContextBefore, 1)
	assert.Equal(t, "original lead-in", stable.Matches[0].ContextBefore[0].Content)

	racy := service.NewReadOnlyBackend(&syncBetweenSearchAndContext{DB: database})
	result, err := racy.SearchContent(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, result.Matches, 1)
	assert.False(t, result.RevisionBound,
		"context from a newer transcript revision must drop the bound claim")
	assert.Equal(t, stable.Matches[0].TranscriptRevision,
		result.Matches[0].TranscriptRevision,
		"the match still cites the revision the search ran at")
	require.Len(t, result.Matches[0].ContextBefore, 1)
	assert.Equal(t, "changed lead-in", result.Matches[0].ContextBefore[0].Content)
}
