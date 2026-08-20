package postgres

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

func TestRawDeviceAuthUniqueViolationRejectsTypedNil(t *testing.T) {
	t.Parallel()

	var pgErr *pgconn.PgError
	var err error = pgErr
	assert.NotPanics(t, func() {
		assert.False(t, rawDeviceAuthUniqueViolation(err))
	})
}
