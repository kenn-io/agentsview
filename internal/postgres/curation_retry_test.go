package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetryPGCurationOnlyRetriesAbortedTransactions(t *testing.T) {
	for _, tc := range []struct {
		name         string
		err          error
		wantAttempts int
	}{
		{"deadlock", &pgconn.PgError{Code: "40P01"}, 2},
		{"serialization", &pgconn.PgError{Code: "40001"}, 2},
		{"constraint", &pgconn.PgError{Code: "23505"}, 1},
		{"transport", errors.New("connection lost"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				attempts := 0
				value, err := retryPGCuration(t.Context(), func() (int, error) {
					attempts++
					if attempts == 1 {
						return 0, fmt.Errorf("write failed: %w", tc.err)
					}
					return 42, nil
				})
				assert.Equal(t, tc.wantAttempts, attempts)
				if tc.wantAttempts == 2 {
					require.NoError(t, err)
					assert.Equal(t, 42, value)
				} else {
					require.ErrorIs(t, err, tc.err)
					assert.Zero(t, value)
				}
			})
		})
	}
}

func TestRetryPGCurationBoundsRetriesAndPreservesCancellation(t *testing.T) {
	for _, cancelDuringRetry := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelDuringRetry), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				attempts := 0
				aborted := &pgconn.PgError{Code: "40001"}
				_, err := retryPGCuration(ctx, func() (int, error) {
					attempts++
					if cancelDuringRetry {
						cancel()
					}
					return 0, aborted
				})
				require.ErrorIs(t, err, aborted)
				if cancelDuringRetry {
					require.ErrorIs(t, err, context.Canceled)
					assert.Equal(t, 1, attempts)
				} else {
					assert.Equal(t, 5, attempts)
				}
			})
		})
	}
}
