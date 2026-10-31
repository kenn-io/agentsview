package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"time"

	"github.com/cenkalti/backoff/v7"
	"github.com/jackc/pgx/v5/pgconn"
	"go.kenn.io/agentsview/internal/rawparity"
)

// RawEmbeddingMode is evidence supplied by the adapter after loading and
// resolving the exact target's embedding-aware configuration. Zero is unknown;
// an absent field, invalid recipe or missing vector table never means disabled.
type RawEmbeddingMode string

const (
	RawEmbeddingsUnknown    RawEmbeddingMode = ""
	RawEmbeddingsDisabled   RawEmbeddingMode = "not_configured"
	RawEmbeddingsConfigured RawEmbeddingMode = "configured"
)

type RawActivationOptions struct {
	Version           string
	AcceptDifferences bool
	Embeddings        RawEmbeddingMode
}

// RawActivationAudit retains the original gate counts and the explicit waiver,
// never transcripts or per-session report entries. Both revisions are captured
// in the committing transaction; trigger publication may add several revisions.
type RawActivationAudit struct {
	Scope                   rawparity.Scope     `json:"scope"`
	Parity                  RawActivationParity `json:"parity"`
	AcceptDifferences       bool                `json:"accept_differences"`
	Embeddings              string              `json:"embeddings"`
	CheckedCorpusRevision   int64               `json:"checked_corpus_revision"`
	ActivatedCorpusRevision int64               `json:"activated_corpus_revision"`
}
type RawActivationParity struct {
	Pass      bool                  `json:"pass"`
	Counts    rawparity.Counts      `json:"counts"`
	Readiness []rawparity.Readiness `json:"readiness"`
}

func checkRawActivationEmbeddingMode(mode RawEmbeddingMode) error {
	switch mode {
	case RawEmbeddingsDisabled:
		return nil
	case RawEmbeddingsConfigured:
		return errors.New("raw-rollout activation requires the hosted embedding coverage gate; configured embeddings cannot activate yet")
	default:
		return errors.New("raw-rollout embedding configuration is unknown; integrate the embedding-aware selected-target configuration before activation")
	}
}

func checkRawActivationGate(r rawparity.Report, accept bool, mode RawEmbeddingMode) error {
	if err := checkRawActivationEmbeddingMode(mode); err != nil {
		return err
	}
	projected := false
	for _, s := range r.Readiness {
		projected = projected || s.Projected > 0
		if s.Pending != 0 || s.Failed != 0 || s.Unsupported != 0 {
			return errors.New("raw-rollout parity refused: sources are not ready")
		}
	}
	if !projected || r.Counts["ambiguous"] != 0 || r.Counts["unmatchable"] != 0 || (!accept && r.Counts["differs"] != 0) {
		return errors.New("raw-rollout parity refused: empty scope, unresolved identity or content differences")
	}
	return nil
}

type rawActivationLockConflictError struct{ error }

func (e *rawActivationLockConflictError) Unwrap() error { return e.error }

func rawActivationNowait(err error) error {
	if pg, ok := errors.AsType[*pgconn.PgError](err); ok && pg.Code == "55P03" {
		return &rawActivationLockConflictError{err}
	}
	return err
}

// An uncertain successful commit/cleanup must never be replayed.
type rawActivationUncertainError struct{ error }

func (e *rawActivationUncertainError) Unwrap() error { return e.error }

func retryRawActivation(ctx context.Context, operation func() (RawActivationAudit, error)) (RawActivationAudit, error) {
	policy := backoff.NewExponentialBackOff()
	policy.InitialInterval = 10 * time.Millisecond
	policy.MaxInterval = 100 * time.Millisecond
	value, err := backoff.Retry(ctx, func() (RawActivationAudit, error) {
		value, err := operation()
		if err == nil {
			return value, nil
		}
		pg, ok := errors.AsType[*pgconn.PgError](err)
		_, uncertain := errors.AsType[*rawActivationUncertainError](err)
		_, nowait := errors.AsType[*rawActivationLockConflictError](err)
		if uncertain || !ok || (pg.Code != "40001" && pg.Code != "40P01" && (!nowait || pg.Code != "55P03")) {
			return value, backoff.Permanent(err)
		}
		return value, err
	}, backoff.WithBackOff(policy), backoff.WithMaxTries(5), backoff.WithMaxElapsedTime(0))
	if err != nil {
		return value, errors.Join(backoff.AsRetryError(err).LastErr, context.Cause(ctx))
	}
	return value, nil
}

//nolint:unused // used by the later fenced activation engine
func discardRawActivationConnection(conn *sql.Conn) {
	// Returning ErrBadConn through Raw tells database/sql to close the physical
	// driver connection. Conn.Close alone would return a session guard to the pool.
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}
