//go:build windows && arm64

package duckdb

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenReportsUnsupportedPlatform(t *testing.T) {
	db, err := Open("sessions.duckdb")
	require.Error(t, err)
	assert.Nil(t, db)
	assert.ErrorIs(t, err, errUnsupportedPlatform)
}

func TestNewQuackStoreReportsUnsupportedPlatform(t *testing.T) {
	store, err := NewQuackStore("quack:localhost:8765", "token", false)
	require.Error(t, err)
	assert.Nil(t, store)
	assert.ErrorIs(t, err, errUnsupportedPlatform)
}
