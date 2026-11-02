package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Only a known NOWAIT boundary can retry 55P03. A permanent refusal, arbitrary
// lock error or unknown commit outcome must not be replayed as rolled back work.
func TestRawActivationRetryClassification(t *testing.T) {
	for _, tc := range []struct {
		err      error
		attempts int
	}{
		{&pgconn.PgError{Code: "40001"}, 5},
		{&pgconn.PgError{Code: "40P01"}, 5},
		{rawActivationNowait(&pgconn.PgError{Code: "55P03"}), 5},
		{&pgconn.PgError{Code: "55P03"}, 1},
		{errors.New("commit outcome unknown"), 1},
		{&rawActivationUncertainError{&pgconn.PgError{Code: "40001"}}, 1},
		{errors.New("parity refused"), 1},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			n := 0
			_, err := retryRawActivation(t.Context(), func() (RawActivationAudit, error) { n++; return RawActivationAudit{}, tc.err })
			require.Error(t, err)
			assert.Equal(t, tc.attempts, n)
		})
	}
}

func TestRawActivationRetryCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	_, err := retryRawActivation(ctx, func() (RawActivationAudit, error) {
		calls++
		cancel()
		return RawActivationAudit{}, &pgconn.PgError{Code: "40001"}
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, calls)
}
