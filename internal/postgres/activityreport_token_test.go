package postgres

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestActivityReportTokenRequiresConfiguredSecret(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := &Store{}
	_, err := store.EncodeActivityReportToken([]byte(`{"query":"month"}`))
	assert.ErrorIs(err, db.ErrInvalidActivityReportToken)

	store.SetCursorSecret(bytes.Repeat([]byte{1}, 32))
	token, err := store.EncodeActivityReportToken([]byte(`{"query":"month"}`))
	require.NoError(err)
	payload, err := store.DecodeActivityReportToken(token)
	require.NoError(err)
	assert.JSONEq(`{"query":"month"}`, string(payload))
}
