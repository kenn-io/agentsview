package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

func TestRawIngestAppendOnlyUnsupportedClassifiesFeatureErrors(t *testing.T) {
	assert := assert.New(t)

	t.Parallel()

	unsupported := &pgconn.PgError{Code: "0A000"}
	assert.True(rawIngestAppendOnlyUnsupported(unsupported))
	assert.True(rawIngestAppendOnlyUnsupported(
		fmt.Errorf("installing trigger: %w", unsupported),
	))
	assert.True(rawIngestAppendOnlyUnsupported(errors.New(
		"ERROR: unimplemented PL/pgSQL (SQLSTATE 0A000)",
	)))
	assert.False(rawIngestAppendOnlyUnsupported(&pgconn.PgError{Code: "42501"}))
	assert.False(rawIngestAppendOnlyUnsupported(errors.New("connection closed")))
}
