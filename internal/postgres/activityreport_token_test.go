package postgres

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestActivityReportTokenRequiresConfiguredSecret(t *testing.T) {
	store := &Store{}
	_, err := store.EncodeActivityReportToken([]byte(`{"query":"month"}`))
	assert.ErrorIs(t, err, db.ErrInvalidActivityReportToken)

	store.SetCursorSecret(bytes.Repeat([]byte{1}, 32))
	token, err := store.EncodeActivityReportToken([]byte(`{"query":"month"}`))
	require.NoError(t, err)
	payload, err := store.DecodeActivityReportToken(token)
	require.NoError(t, err)
	assert.JSONEq(t, `{"query":"month"}`, string(payload))
}
