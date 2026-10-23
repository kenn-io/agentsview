package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
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
		{&rawActivationUncertain{&pgconn.PgError{Code: "40001"}}, 1},
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

func TestRawActivationRetryReturnsFreshAttempt(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		failures := hegel.Draw(ht, hegel.Integers(0, 4))
		code := []string{"40001", "40P01", "55P03"}[hegel.Draw(ht, hegel.Integers(0, 2))]
		calls := 0
		audit, err := retryRawActivation(ht.Context(), func() (RawActivationAudit, error) {
			calls++
			if calls <= failures {
				return RawActivationAudit{}, rawActivationNowait(&pgconn.PgError{Code: code})
			}
			return RawActivationAudit{CheckedCorpusRevision: int64(calls)}, nil
		})
		require.NoError(ht, err)
		require.EqualValues(ht, failures+1, audit.CheckedCorpusRevision)
		require.Equal(ht, failures+1, calls)
	}, hegel.WithTestCases(20))
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
