package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// HostedEmbeddingError reports a pass that could not drain every session.
type HostedEmbeddingError struct{ Processed, Failed int }

func (e *HostedEmbeddingError) Error() string {
	return fmt.Sprintf("%d of %d hosted embedding sessions failed", e.Failed, e.Processed)
}

// EmbedBatch drains one pass of pending hosted sessions. Failures do not stop
// later sessions; the caller retries the pass until all sessions succeed.
func EmbedBatch(ctx context.Context, pg *sql.DB, generation storage.VectorGenerationInfo, encode kitvec.EncodeFunc, maxInputChars int, includeAutomated bool, batch ...kitvec.BatchOption) error {
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
	_ = tx.Rollback()
	if err != nil {
		return err
	}
	scope := vectorPushScope{gen: vectorGeneration{id: id, halfvecType: ext + ".halfvec"}, genIDs: ids}
	var processed, failed int
	cursor := ""
	for {
		rows, err := pg.QueryContext(ctx, `SELECT s.id, s.raw_content_revision FROM sessions s LEFT JOIN vector_push_state p ON p.generation_id=$1 AND p.session_id=s.id WHERE s.provenance_kind='raw' AND s.id>$2 AND (p.session_id IS NULL OR p.doc_agg_hash <> s.raw_content_revision || '|' || $3) ORDER BY s.id LIMIT 16`, id, cursor, fmt.Sprint(includeAutomated))
		if err != nil {
			return err
		}
		type pending struct{ id, revision string }
		var page []pending
		for rows.Next() {
			var p pending
			if err = rows.Scan(&p.id, &p.revision); err != nil {
				break
			}
			page = append(page, p)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, item := range page {
			processed++
			cursor = item.id
			var units []db.EmbeddableUnit
			err := scanHostedUnits(ctx, pg, item.id, includeAutomated, vector.RunMaxRunes(maxInputChars), func(u db.EmbeddableUnit) error { units = append(units, u); return nil })
			var docs []storage.VectorPushDoc
			if err == nil {
				docs, err = vector.EncodeUnitDocuments(ctx, units, maxInputChars, encode, batch...)
			}
			if err == nil {
				err = writeHostedVectors(ctx, pg, scope, item.id, item.revision, includeAutomated, docs)
			}
			if err != nil {
				failed++
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	cursor = ""
	for {
		rows, err := pg.QueryContext(ctx, `SELECT DISTINCT p.session_id FROM vector_push_state p LEFT JOIN sessions s ON s.id=p.session_id WHERE p.session_id>$1 AND s.id IS NULL ORDER BY p.session_id LIMIT 16`, cursor)
		if err != nil {
			return err
		}
		var page []string
		for rows.Next() {
			var sessionID string
			if err = rows.Scan(&sessionID); err != nil {
				break
			}
			page = append(page, sessionID)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
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
			}
		}
	}
	if failed > 0 {
		return &HostedEmbeddingError{Processed: processed, Failed: failed}
	}
	return nil
}

func clearHostedOrphan(ctx context.Context, pg *sql.DB, sessionID string) error {
	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
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
	defer tx.Rollback()
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
	if _, err = writeSessionVectors(ctx, tx, scope, id, revision+"|"+fmt.Sprint(automated), docs); err != nil {
		return err
	}
	return tx.Commit()
}
