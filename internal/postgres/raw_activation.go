package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json/v2"
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

// ActivateRawRollout owns every attempt, including the physical connection and
// session guard. No repeatable-read snapshot exists while waiting for rollout.
// Callers must supply authoritative embedding configuration, not a default bool.
func ActivateRawRollout(ctx context.Context, database *sql.DB, schema, tenant, device, provider string, options RawActivationOptions) (RawActivationAudit, error) {
	if err := validateHostedBinding(schema, tenant); err != nil {
		return RawActivationAudit{}, err
	}
	if err := validateRawIngestProcessingVersion(options.Version); err != nil {
		return RawActivationAudit{}, err
	}
	return retryRawActivation(ctx, func() (RawActivationAudit, error) {
		return activateRawRolloutOnce(ctx, database, schema, tenant, device, provider, options)
	})
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

func discardRawActivationConnection(conn *sql.Conn) {
	// Returning ErrBadConn through Raw tells database/sql to close the physical
	// driver connection. Conn.Close alone would return a session guard to the pool.
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

func activateRawRolloutOnce(ctx context.Context, database *sql.DB, schema, tenant, device, provider string, options RawActivationOptions) (audit RawActivationAudit, resultErr error) {
	conn, err := database.Conn(ctx)
	if err != nil {
		return audit, err
	}
	guarded := false
	var tx *sql.Tx
	committed := false
	commitUncertain := false
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
		if !guarded || commitUncertain {
			discardRawActivationConnection(conn)
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var released bool
		err := conn.QueryRowContext(cleanup, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, "agentsview-rollout:"+schema).Scan(&released)
		if err != nil || !released {
			discardRawActivationConnection(conn)
			cleanupErr := errors.New("raw-rollout session guard cleanup failed; physical connection discarded")
			if committed {
				resultErr = &rawActivationUncertainError{cleanupErr}
			} else {
				resultErr = errors.Join(resultErr, cleanupErr)
			}
			return
		}
		resultErr = errors.Join(resultErr, conn.Close())
	}()
	// Even a failed acquisition may have succeeded on the server. Until success
	// is known, the defer disposes of the physical connection without pool reuse.
	if _, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, "agentsview-rollout:"+schema); err != nil {
		return audit, err
	}
	guarded = true
	tx, err = conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return audit, err
	}
	if err = validateRawRolloutTransaction(ctx, tx, schema, tenant); err != nil {
		return audit, err
	}
	if err = checkRawActivationEmbeddingMode(options.Embeddings); err != nil {
		return audit, err
	}
	prepared, err := prepareRawActivation(ctx, tx, device, provider)
	if err != nil {
		return audit, err
	}
	// Corpus is the final publication fence. NOWAIT has an explicitly classified
	// retry boundary; every retry reacquires rollout and starts a fresh snapshot.
	if err = tx.QueryRowContext(ctx, `SELECT corpus_revision FROM raw_corpus_state WHERE singleton=1 FOR UPDATE NOWAIT`).Scan(&audit.CheckedCorpusRevision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return audit, errors.New("raw-rollout publication state is not initialized; project sources before activation")
		}
		return audit, rawActivationNowait(err)
	}
	// D6 inserts the exact active-generation FOR SHARE NOWAIT/work_epoch fence
	// here, before final parity/coverage, using the sibling's actual schema.
	report, err := rawparity.Run(ctx, rawActivationReader{RawParityReader{Tx: tx, Version: options.Version}}, rawparity.Scope{Device: device, Provider: provider})
	if err != nil {
		return audit, err
	}
	if err = checkRawActivationGate(report, options.AcceptDifferences, options.Embeddings); err != nil {
		return audit, err
	}
	audit.Scope = report.Scope
	audit.Parity = RawActivationParity{Pass: report.Pass, Counts: report.Counts, Readiness: report.Readiness}
	audit.AcceptDifferences = options.AcceptDifferences
	audit.Embeddings = string(options.Embeddings)
	// All writes below preserve the accepted physical content. Their relation,
	// sequence, unique-key, parent, child and trigger resources were prepared.
	if _, err = tx.ExecContext(ctx, `INSERT INTO raw_rollout_scopes(tenant_id,device_id,provider,state) VALUES($1,$2,$3,'active') ON CONFLICT(tenant_id,device_id,provider) DO UPDATE SET state='active',updated_at=clock_timestamp(),updated_by=current_user`, tenant, device, provider); err != nil {
		return audit, err
	}
	if err = prepared.apply(ctx, tx); err != nil {
		return audit, err
	}
	if err = publishRawRevision(ctx, tx, true, nil); err != nil {
		return audit, err
	}
	// Legacy visibility triggers have already published here. Capture the final
	// revision while still owning corpus; an unrelated commit cannot alter audit.
	if err = tx.QueryRowContext(ctx, `SELECT corpus_revision FROM raw_corpus_state WHERE singleton=1`).Scan(&audit.ActivatedCorpusRevision); err != nil {
		return audit, err
	}
	encoded, err := json.Marshal(audit)
	if err != nil {
		return audit, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE raw_rollout_scopes SET parity=$3::jsonb,activated_corpus_revision=$4 WHERE device_id=$1 AND provider=$2`, device, provider, string(encoded), audit.ActivatedCorpusRevision); err != nil {
		return audit, err
	}
	if err = tx.Commit(); err != nil {
		// A server SQL error is a known failed commit. A transport/cancellation
		// error can hide an accepted commit: never replay it or reuse its session.
		if _, known := errors.AsType[*pgconn.PgError](err); !known {
			commitUncertain = true
			return audit, &rawActivationUncertainError{err}
		}
		return audit, err
	}
	committed = true
	return audit, nil
}
