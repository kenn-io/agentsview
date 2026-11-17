package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/cenkalti/backoff/v7"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/vector"
	"go.kenn.io/kit/embedclient"
	kitvec "go.kenn.io/kit/vector"
)

// HostedEmbeddingError reports a pass that could not drain every session.
type HostedEmbeddingError struct {
	Processed, Failed int
	Cause             string
}

func (e *HostedEmbeddingError) Error() string {
	return fmt.Sprintf("%d of %d hosted embedding sessions failed", e.Failed, e.Processed)
}

type hostedEmbeddingRetry struct {
	revision string
	next     time.Time
	delay    *backoff.ExponentialBackOff
	cause    string
}

// HostedEmbeddingBackoff keeps failed sessions from monopolizing the encoder.
// A changed revision is eligible immediately, and successful sessions are removed.
type HostedEmbeddingBackoff struct {
	pending map[string]*hostedEmbeddingRetry
}

func NewHostedEmbeddingBackoff() *HostedEmbeddingBackoff {
	return &HostedEmbeddingBackoff{pending: make(map[string]*hostedEmbeddingRetry)}
}

func embeddingFailureCause(err error) string {
	if apiErr, ok := errors.AsType[*embedclient.APIError](err); ok {
		return fmt.Sprintf("embedding endpoint returned HTTP %d", apiErr.StatusCode)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "embedding request timed out"
	}
	return "embedding request failed; check endpoint connectivity and configuration"
}

func (b *HostedEmbeddingBackoff) record(id, revision, cause string, now time.Time) {
	entry := b.pending[id]
	if entry == nil || entry.revision != revision {
		delay := backoff.NewExponentialBackOff()
		delay.InitialInterval = 30 * time.Second
		delay.MaxInterval = 5 * time.Minute
		delay.Reset()
		entry = &hostedEmbeddingRetry{revision: revision, delay: delay}
		b.pending[id] = entry
	}
	entry.cause = cause
	entry.next = now.Add(entry.delay.NextBackOff())
}

// EmbedBatch drains one pass of pending hosted sessions. Failures do not stop
// later sessions; the caller retries the pass until all sessions succeed.
func EmbedBatch(ctx context.Context, pg *sql.DB, generation storage.VectorGenerationInfo, encode kitvec.EncodeFunc, maxInputChars int, includeAutomated bool, batch ...kitvec.BatchOption) error {
	return embedBatch(ctx, pg, generation, encode, maxInputChars, includeAutomated, nil, batch...)
}

func (b *HostedEmbeddingBackoff) EmbedBatch(ctx context.Context, pg *sql.DB, generation storage.VectorGenerationInfo, encode kitvec.EncodeFunc, maxInputChars int, includeAutomated bool, batch ...kitvec.BatchOption) error {
	return embedBatch(ctx, pg, generation, encode, maxInputChars, includeAutomated, b, batch...)
}

func embedBatch(ctx context.Context, pg *sql.DB, generation storage.VectorGenerationInfo, encode kitvec.EncodeFunc, maxInputChars int, includeAutomated bool, retry *HostedEmbeddingBackoff, batch ...kitvec.BatchOption) error {
	id, _, ok, err := LookupVectorGeneration(ctx, pg, generation.Fingerprint)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("hosted embedding generation is missing; run hosted-provision")
	}
	ext, err := vectorExtensionSchema(ctx, pg)
	if err != nil {
		return fmt.Errorf("hosted embeddings require pgvector and hosted-provision: %w", err)
	}
	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	ids, err := existingChunkGenerationsTx(ctx, tx)
	rollbackErr := tx.Rollback()
	if err != nil {
		return err
	}
	if rollbackErr != nil {
		return rollbackErr
	}
	scope := vectorPushScope{gen: vectorGeneration{id: id, halfvecType: ext + ".halfvec"}, genIDs: ids}
	var processed, failed int
	var cause string
	seen := make(map[string]bool)
	cursor := ""
	for {
		type pending struct{ id, revision string }
		page, err := func() ([]pending, error) {
			rows, err := pg.QueryContext(ctx, `SELECT s.id, s.raw_content_revision FROM sessions s LEFT JOIN vector_push_state p ON p.generation_id=$1 AND p.session_id=s.id WHERE s.provenance_kind='raw' AND s.id>$2 AND (p.session_id IS NULL OR p.doc_agg_hash <> s.raw_content_revision || '|' || $3) ORDER BY s.id LIMIT 16`, id, cursor, strconv.FormatBool(includeAutomated))
			if err != nil {
				return nil, err
			}
			defer func() { _ = rows.Close() }()
			var page []pending
			for rows.Next() {
				var p pending
				if err := rows.Scan(&p.id, &p.revision); err != nil {
					return nil, err
				}
				page = append(page, p)
			}
			return page, rows.Err()
		}()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, item := range page {
			processed++
			cursor = item.id
			seen[item.id] = true
			if retry != nil {
				if entry := retry.pending[item.id]; entry != nil && entry.revision == item.revision && time.Now().Before(entry.next) {
					failed++
					if cause == "" {
						cause = entry.cause
					}
					continue
				}
			}
			var units []db.EmbeddableUnit
			err := scanHostedUnits(ctx, pg, item.id, includeAutomated, vector.RunMaxRunes(maxInputChars), func(u db.EmbeddableUnit) error { units = append(units, u); return nil })
			failureCause := "reading hosted sessions failed; check database connectivity and permissions"
			var docs []storage.VectorPushDoc
			if err == nil {
				docs, err = vector.EncodeUnitDocuments(ctx, units, maxInputChars, encode, batch...)
				if err != nil {
					failureCause = embeddingFailureCause(err)
				}
			}
			if err == nil {
				err = writeHostedVectors(ctx, pg, scope, item.id, item.revision, includeAutomated, docs)
				failureCause = "writing hosted vectors failed; check database permissions and concurrent session changes"
			}
			if err != nil {
				failed++
				if cause == "" {
					cause = failureCause
				}
				if retry != nil {
					retry.record(item.id, item.revision, failureCause, time.Now())
				}
			} else if retry != nil {
				delete(retry.pending, item.id)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	if retry != nil {
		for id := range retry.pending {
			if !seen[id] {
				delete(retry.pending, id)
			}
		}
	}
	cursor = ""
	for {
		page, err := func() ([]string, error) {
			rows, err := pg.QueryContext(ctx, `SELECT DISTINCT p.session_id FROM vector_push_state p LEFT JOIN sessions s ON s.id=p.session_id WHERE p.session_id>$1 AND s.id IS NULL ORDER BY p.session_id LIMIT 16`, cursor)
			if err != nil {
				return nil, err
			}
			defer func() { _ = rows.Close() }()
			var page []string
			for rows.Next() {
				var sessionID string
				if err := rows.Scan(&sessionID); err != nil {
					return nil, err
				}
				page = append(page, sessionID)
			}
			return page, rows.Err()
		}()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, sessionID := range page {
			processed++
			cursor = sessionID
			if err := clearHostedOrphan(ctx, pg, sessionID); err != nil {
				failed++
				if cause == "" {
					cause = "removing stale vectors failed; check database connectivity and permissions"
				}
			}
		}
	}
	if failed > 0 {
		return &HostedEmbeddingError{Processed: processed, Failed: failed, Cause: cause}
	}
	return nil
}

func clearHostedOrphan(ctx context.Context, pg *sql.DB, sessionID string) error {
	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// A missing row cannot be locked. Fence a concurrent restore until cleanup
	// commits so it cannot lose newly written vectors.
	if _, err := tx.ExecContext(ctx, `LOCK TABLE sessions IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM sessions WHERE id=$1 FOR UPDATE`, sessionID).Scan(&id)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := clearSessionVectorsTx(ctx, tx, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func writeHostedVectors(ctx context.Context, pg *sql.DB, scope vectorPushScope, id, revision string, automated bool, docs []storage.VectorPushDoc) error {
	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var current string
	err = tx.QueryRowContext(ctx, `SELECT raw_content_revision FROM sessions WHERE id=$1 AND provenance_kind='raw' FOR UPDATE`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("session removed during encoding")
	}
	if err != nil {
		return err
	}
	if current != revision {
		return errors.New("session changed during encoding")
	}
	if _, err = writeSessionVectors(ctx, tx, scope, id, revision+"|"+strconv.FormatBool(automated), docs); err != nil {
		return err
	}
	return tx.Commit()
}
