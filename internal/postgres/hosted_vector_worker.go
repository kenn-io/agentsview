package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/vector"
	"go.kenn.io/kit/embedclient"
	kitvec "go.kenn.io/kit/vector"
)

func embeddingFailureCause(err error) string {
	if apiErr, ok := errors.AsType[*embedclient.APIError](err); ok {
		return fmt.Sprintf("embedding endpoint returned HTTP %d (%s)", apiErr.StatusCode, apiErr.Reason.String())
	}
	if vectorErr, ok := errors.AsType[*embedclient.VectorError](err); ok {
		return fmt.Sprintf("embedding endpoint returned an invalid vector: %v", vectorErr.Err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "embedding request timed out"
	}
	return "embedding request failed; check endpoint connectivity and configuration"
}

// EmbedBatch drains one pass of pending hosted sessions.
func EmbedBatch(ctx context.Context, pg *sql.DB, generation storage.VectorGenerationInfo, encode kitvec.EncodeFunc, maxInputChars, concurrency int, includeAutomated bool, batch ...kitvec.BatchOption) error {
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
	var rejectedSessions, failedSessions int
	var failures []string
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
			cursor = item.id
			var units []db.EmbeddableUnit
			exists, err := scanHostedSession(ctx, pg, item.id, includeAutomated, vector.RunMaxRunes(maxInputChars), func(u db.EmbeddableUnit) error { units = append(units, u); return nil })
			if err != nil {
				return fmt.Errorf("scanning session %s: %w", item.id, err)
			}
			if !exists {
				continue
			}
			docs, err := vector.UnitDocuments(units)
			if err != nil {
				return err
			}
			covered, pending, err := coveredHostedDocuments(ctx, pg, scope.gen.id, item.id, item.revision, includeAutomated, docs, maxInputChars)
			if err != nil {
				return err
			}
			spool, err := vector.NewVectorSpool()
			if err != nil {
				return err
			}
			accepted, rejected, err := vector.FillUnitDocuments(ctx, pending, spool, maxInputChars, concurrency, encode, batch...)
			if err != nil {
				if closeErr := spool.Close(); closeErr != nil {
					return closeErr
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				failedSessions++
				if len(failures) < 3 {
					failures = append(failures, item.id+": "+embeddingFailureCause(err))
				}
				continue
			}
			if len(rejected) > 0 {
				rejectedSessions++
			}
			err = writeHostedVectors(ctx, pg, scope, item.id, item.revision, includeAutomated, accepted, covered, len(rejected) > 0, spool.Chunks)
			if err = errors.Join(err, spool.Close()); err != nil {
				return err
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
			cursor = sessionID
			if err := clearHostedOrphan(ctx, pg, sessionID); err != nil {
				return err
			}
		}
	}
	if failedSessions > 0 {
		return fmt.Errorf("%d hosted sessions failed embedding (%s); %d hosted sessions have rejected embedding inputs", failedSessions, strings.Join(failures, "; "), rejectedSessions)
	}
	if rejectedSessions > 0 {
		return fmt.Errorf("%d hosted sessions have rejected embedding inputs", rejectedSessions)
	}
	return nil
}

func clearHostedOrphan(ctx context.Context, pg *sql.DB, sessionID string) error {
	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := clearSessionVectorsTx(ctx, tx, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func writeHostedVectors(ctx context.Context, pg *sql.DB, scope vectorPushScope, id, revision string, automated bool, docs, covered []storage.VectorPushDoc, partial bool, loadChunks func(string) ([]storage.VectorPushChunk, error)) error {
	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var current string
	err = tx.QueryRowContext(ctx, `SELECT raw_content_revision FROM sessions WHERE id=$1 AND provenance_kind='raw' FOR UPDATE`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if current != revision {
		return nil
	}
	changed, err := hostedDocumentMetadataChanged(ctx, tx, id, append(slices.Clone(docs), covered...))
	if err != nil {
		return err
	}
	stamp := revision + "|" + strconv.FormatBool(automated)
	if partial {
		stamp = "partial|" + stamp
	}
	outcome, err := writeSessionVectorsKeeping(ctx, tx, scope, id, stamp, docs, covered, loadChunks)
	if err != nil {
		return err
	}
	if outcome.deleted > 0 || changed {
		_, err = tx.ExecContext(ctx, `DELETE FROM vector_push_state WHERE session_id=$1 AND generation_id<>$2`, id, scope.gen.id)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// coveredHostedDocuments trusts partial progress only for this session, revision,
// scope, and generation, and only when every expected chunk is present.
func coveredHostedDocuments(ctx context.Context, pg *sql.DB, generation int64, sessionID, revision string, automated bool, docs []storage.VectorPushDoc, maxInputChars int) ([]storage.VectorPushDoc, []storage.VectorPushDoc, error) {
	var stamp string
	err := pg.QueryRowContext(ctx, `SELECT doc_agg_hash FROM vector_push_state WHERE generation_id=$1 AND session_id=$2`, generation, sessionID).Scan(&stamp)
	if errors.Is(err, sql.ErrNoRows) || err == nil && stamp != "partial|"+revision+"|"+strconv.FormatBool(automated) {
		return nil, docs, nil
	}
	if err != nil {
		return nil, nil, err
	}
	rows, err := pg.QueryContext(ctx, fmt.Sprintf(`SELECT d.doc_key,d.content_hash,c.chunk_index FROM vector_documents d LEFT JOIN %s c ON c.doc_key=d.doc_key WHERE d.session_id=$1 AND d.ordinal>=0 ORDER BY d.doc_key,c.chunk_index`, vectorChunkTable(generation)), sessionID)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	type coverage struct {
		hash    string
		indexes []int
	}
	found := make(map[string]*coverage)
	for rows.Next() {
		var key, hash string
		var idx sql.NullInt64
		if err := rows.Scan(&key, &hash, &idx); err != nil {
			return nil, nil, err
		}
		if found[key] == nil {
			found[key] = &coverage{hash: hash}
		}
		if idx.Valid {
			found[key].indexes = append(found[key].indexes, int(idx.Int64))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var covered, pending []storage.VectorPushDoc
	for _, doc := range docs {
		var expected []int
		for _, chunk := range kitvec.Split(doc.Content, kitvec.SplitOptions{MaxRunes: maxInputChars, Overlap: vector.ChunkOverlap(maxInputChars)}) {
			expected = append(expected, chunk.Index)
		}
		entry := found[doc.DocKey]
		if entry != nil && entry.hash == doc.ContentHash && slices.Equal(entry.indexes, expected) {
			covered = append(covered, doc)
		} else {
			pending = append(pending, doc)
		}
	}
	return covered, pending, nil
}

// Shared document metadata is read by every generation's searcher.
func hostedDocumentMetadataChanged(ctx context.Context, tx *sql.Tx, sessionID string, docs []storage.VectorPushDoc) (bool, error) {
	slices.SortFunc(docs, func(a, b storage.VectorPushDoc) int { return strings.Compare(a.DocKey, b.DocKey) })
	rows, err := tx.QueryContext(ctx, `SELECT doc_key,session_id,source_uuid,ordinal,ordinal_end,subordinate,offsets,content,content_hash FROM vector_documents WHERE session_id=$1 AND ordinal>=0 ORDER BY doc_key COLLATE "C"`, sessionID)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	i := 0
	for rows.Next() {
		var existing storage.VectorPushDoc
		if err := rows.Scan(&existing.DocKey, &existing.SessionID, &existing.SourceUUID, &existing.Ordinal, &existing.OrdinalEnd, &existing.Subordinate, &existing.OffsetsJSON, &existing.Content, &existing.ContentHash); err != nil {
			return false, err
		}
		if i == len(docs) {
			return true, nil
		}
		doc := docs[i]
		offsets := doc.OffsetsJSON
		if offsets == "" {
			offsets = "[]"
		}
		if existing.DocKey != doc.DocKey || existing.SessionID != doc.SessionID || existing.SourceUUID != sanitizePG(doc.SourceUUID) || existing.Ordinal != doc.Ordinal || existing.OrdinalEnd != doc.OrdinalEnd || existing.Subordinate != doc.Subordinate || existing.OffsetsJSON != sanitizePG(offsets) || existing.Content != sanitizePG(doc.Content) || existing.ContentHash != doc.ContentHash {
			return true, nil
		}
		i++
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return i != len(docs), nil
}
