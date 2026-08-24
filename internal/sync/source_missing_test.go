package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func assertSourceMissingState(t *testing.T, session *db.Session) {
	t.Helper()
	require.NotNil(t, session)
	assert.Nil(t, session.DeletedAt,
		"missing source material must not put the session in user trash")
	assert.Nil(t, session.DeletionCause,
		"legacy deletion cause must not represent source availability")
	assert.NotNil(t, session.SourceMissingAt)
}
