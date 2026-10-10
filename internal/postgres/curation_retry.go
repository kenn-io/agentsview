package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/cenkalti/backoff/v7"
	"github.com/jackc/pgx/v5/pgconn"
)

// Retrying is safe only for complete SQL transactions whose effects roll back
// together. Each attempt must release all locks and revalidate session identity.
// Hosted identity fences report serialization failures instead of waiting for
// alias locks after row locks; ordinary restore/purge can also deadlock.
func retryPGCuration[T any](ctx context.Context, operation func() (T, error)) (T, error) {
	policy := backoff.NewExponentialBackOff()
	policy.InitialInterval = 10 * time.Millisecond
	policy.MaxInterval = 100 * time.Millisecond
	value, err := backoff.Retry(ctx, func() (T, error) {
		value, err := operation()
		if err == nil {
			return value, nil
		}
		pgErr, ok := errors.AsType[*pgconn.PgError](err)
		if !ok || (pgErr.Code != "40P01" && pgErr.Code != "40001") {
			return value, backoff.Permanent(err)
		}
		return value, err
	}, backoff.WithBackOff(policy), backoff.WithMaxTries(5), backoff.WithMaxElapsedTime(0))
	if err != nil {
		return value, errors.Join(backoff.AsRetryError(err).LastErr, context.Cause(ctx))
	}
	return value, nil
}
