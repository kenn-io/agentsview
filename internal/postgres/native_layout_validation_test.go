package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestBulkInsertMessagesRejectsUnsupportedNativeLayout(t *testing.T) {
	pg := newPushSessionProbeDB(t, &pushSessionProbeState{})
	tx, err := pg.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tx.Rollback()) })
	message := db.Message{Ordinal: 7, Content: "retained-body", ContentLayout: &parser.ContentLayout{Version: 2}}
	err = bulkInsertMessages(t.Context(), tx, "native-mirror", []db.Message{message})
	require.ErrorContains(t, err, "unsupported content layout version 2")
	assert.Equal(t, "retained-body", message.Content)
	assert.Equal(t, 2, message.ContentLayout.Version)
}
