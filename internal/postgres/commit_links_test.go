package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func TestHostedCommitLinksUnavailable(t *testing.T) {
	// The hosted adapter must reject this capability before any database access.
	backend := service.NewReadOnlyBackend(&HostedStore{})
	session, err := backend.SessionCommits(t.Context(), "session-a")
	require.ErrorIs(t, err, db.ErrCommitLinksUnavailable)
	assert.Nil(t, session)
	commits, err := backend.CommitSessions(t.Context(), []string{"abc1234"})
	require.ErrorIs(t, err, db.ErrCommitLinksUnavailable)
	assert.Nil(t, commits)
}
