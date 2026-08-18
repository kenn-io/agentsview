package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

func TestRawIngestAppendOnlyUnsupportedClassifiesFeatureErrors(t *testing.T) {
	t.Parallel()

	unsupported := &pgconn.PgError{Code: "0A000"}
	assert.True(t, rawIngestAppendOnlyUnsupported(unsupported))
	assert.True(t, rawIngestAppendOnlyUnsupported(
		fmt.Errorf("installing trigger: %w", unsupported),
	))
	assert.True(t, rawIngestAppendOnlyUnsupported(errors.New(
		"ERROR: unimplemented PL/pgSQL (SQLSTATE 0A000)",
	)))
	assert.False(t, rawIngestAppendOnlyUnsupported(&pgconn.PgError{Code: "42501"}))
	assert.False(t, rawIngestAppendOnlyUnsupported(errors.New("connection closed")))
}
