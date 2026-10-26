package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/vector"
)

// HubVectorGeneration serves only the generation selected by pg serve. Mixed
// pusher compatibility is unavailable rather than arbitrarily picking a host.
func (s *Store) HubVectorGeneration(ctx context.Context) (vector.HubGeneration, error) {
	unavailable := vector.HubGeneration{Unavailable: "selected vector generation unavailable"}
	selected, ok := s.getVectorSearcher().(*vectorSearcher)
	if !ok || selected.fingerprint == "" || selected.createdAt.IsZero() {
		return unavailable, nil
	}
	return readHubVectorGeneration(ctx, s.pg, selected)
}

func readHubVectorGeneration(ctx context.Context, reader pgSessionQueryer, selected *vectorSearcher) (vector.HubGeneration, error) {
	unavailable := vector.HubGeneration{Unavailable: "selected vector generation unavailable"}
	rows, err := reader.QueryContext(ctx, `SELECT g.fingerprint,g.model,g.dimension,to_jsonb(g)->>'params',
 (to_jsonb(m)->>'data_version')::integer,(to_jsonb(m)->>'include_automated')::boolean,
 (to_jsonb(m)->>'chunk_hash_version')::integer
 FROM vector_generations g LEFT JOIN vector_generation_machines m ON m.generation_id=g.id
 WHERE g.id=$1 AND g.fingerprint=$2 AND g.created_at=$3 AND g.dimension=$4
 AND to_jsonb(m) ? 'chunk_hash_version'
 AND EXISTS (SELECT 1 FROM pg_attribute
     WHERE attrelid=to_regclass($5) AND attname='source_hash' AND NOT attisdropped)`,
		selected.genID, selected.fingerprint, selected.createdAt, selected.dimension, selected.chunkTable)
	if isUndefinedTable(err) {
		return unavailable, nil
	}
	if err != nil {
		return unavailable, err
	}
	defer rows.Close()
	var result vector.HubGeneration
	found := false
	for rows.Next() {
		var gen vector.HubGeneration
		var params sql.NullString
		var version, hashVersion sql.NullInt64
		var automated sql.NullBool
		if err := rows.Scan(&gen.Fingerprint, &gen.Model, &gen.Dimension, &params, &version, &automated, &hashVersion); err != nil {
			return unavailable, err
		}
		if !params.Valid || json.Unmarshal([]byte(params.String), &gen.Params) != nil || gen.Params == nil || !version.Valid || version.Int64 <= 0 || !automated.Valid {
			return unavailable, nil
		}
		// Legacy witnesses predate chunk-hash certification. Their null version
		// remains valid when the stored chunks carry the required provenance.
		// A newer explicit contract cannot be interpreted by this reader.
		if hashVersion.Valid && hashVersion.Int64 != 1 {
			return unavailable, nil
		}
		gen.DataVersion = int(version.Int64)
		gen.IncludeAutomated = automated.Bool
		if found && (gen.DataVersion != result.DataVersion || gen.IncludeAutomated != result.IncludeAutomated) {
			return unavailable, nil
		}
		result = gen
		found = true
	}
	if err := rows.Err(); err != nil {
		return unavailable, err
	}
	if !found {
		return unavailable, nil
	}
	return result, nil
}

func (s *Store) LookupHubVectors(ctx context.Context, req vector.HubLookupRequest) (vector.HubLookupReply, error) {
	return s.lookupHubVectors(ctx, req, vector.MaxHubLookupBytes)
}

func (s *Store) lookupHubVectors(ctx context.Context, req vector.HubLookupRequest, limit int) (vector.HubLookupReply, error) {
	reply := vector.HubLookupReply{Version: vector.HubVectorVersion, Fingerprint: req.Fingerprint, Chunks: []vector.HubChunk{}, Absent: []int{}}
	if err := req.Validate(); err != nil {
		return reply, err
	}
	selected, ok := s.getVectorSearcher().(*vectorSearcher)
	if !ok || selected.fingerprint == "" || selected.createdAt.IsZero() || req.Fingerprint != selected.fingerprint {
		return reply, vector.ErrHubGenerationChanged
	}
	start, chunkStart, err := hubLookupCursor(req)
	if err != nil {
		return reply, err
	}
	tx, err := s.pg.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return reply, err
	}
	defer func() { _ = tx.Rollback() }()
	// Compatibility and chunks must come from the same pinned snapshot. Direct
	// lookups and continuation pages cannot rely on a prior discovery response.
	generation, err := readHubVectorGeneration(ctx, tx, selected)
	if err != nil {
		return reply, err
	}
	if generation.Unavailable != "" {
		return reply, vector.ErrHubGenerationChanged
	}
	// Reserve bounded framing, all absent indices and a continuation cursor.
	size := 2048
	for i := start; i < len(req.Identities); i++ {
		d := req.Identities[i]
		key := vector.DocKey(d.UnitKind, d.SessionID, d.SourceUUID, d.Ordinal, d.Occurrence)
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT c.chunk_index,c.embedding::text,count(*) OVER()
 FROM vector_documents d JOIN vector_push_state ps ON ps.session_id=d.session_id AND ps.generation_id=$1
 JOIN %s c ON c.doc_key=d.doc_key WHERE d.doc_key=$2 AND d.content_hash=$3
 AND c.source_hash=$3 AND d.ordinal>=0
 AND NOT EXISTS (SELECT 1 FROM %s sibling WHERE sibling.doc_key=c.doc_key
     AND sibling.source_hash IS DISTINCT FROM $3)
 ORDER BY c.chunk_index`, selected.chunkTable, selected.chunkTable), selected.genID, key, d.ContentHash)
		if isUndefinedTable(err) || isUndefinedColumn(err) {
			return reply, vector.ErrHubGenerationChanged
		}
		if err != nil {
			return reply, err
		}
		defer rows.Close()
		count := 0
		before := len(reply.Chunks)
		for rows.Next() {
			var chunk vector.HubChunk
			var literal string
			chunk.Index = i
			if err := rows.Scan(&chunk.ChunkIndex, &literal, &chunk.ChunkCount); err != nil {
				rows.Close()
				return reply, err
			}
			count++
			if chunk.ChunkIndex < chunkStart {
				continue
			}
			var values []float32
			if err := json.Unmarshal([]byte(literal), &values); err != nil || len(values) != selected.dimension {
				rows.Close()
				return reply, errors.New("invalid stored vector")
			}
			chunk.Vector, err = vector.EncodeHubVector(values)
			if err != nil {
				rows.Close()
				return reply, err
			}
			encoded, _ := json.Marshal(chunk)
			if size+len(encoded)+1 > limit {
				rows.Close()
				if len(reply.Chunks) == 0 {
					return reply, errors.New("vector chunk exceeds response limit")
				}
				reply.NextCursor = makeHubLookupCursor(req, i, chunk.ChunkIndex)
				return reply, nil
			}
			size += len(encoded) + 1
			reply.Chunks = append(reply.Chunks, chunk)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return reply, err
		}
		if count > 0 && len(reply.Chunks) == before {
			return reply, fmt.Errorf("%w: cursor beyond stored chunks", vector.ErrHubLookupInvalid)
		}
		if count == 0 {
			reply.Absent = append(reply.Absent, i)
		}
		chunkStart = 0
	}
	return reply, tx.Commit()
}

func hubLookupDigest(req vector.HubLookupRequest) string {
	req.Cursor = ""
	raw, _ := json.Marshal(req)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:12])
}

func makeHubLookupCursor(req vector.HubLookupRequest, index, chunk int) string {
	return base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "%d:%d:%s", index, chunk, hubLookupDigest(req)))
}

func hubLookupCursor(req vector.HubLookupRequest) (int, int, error) {
	if req.Cursor == "" {
		return 0, 0, nil
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(req.Cursor)
	parts := strings.Split(string(raw), ":")
	if err == nil && len(parts) == 3 && parts[2] == hubLookupDigest(req) {
		index, e1 := strconv.Atoi(parts[0])
		chunk, e2 := strconv.Atoi(parts[1])
		if e1 == nil && e2 == nil && index >= 0 && index < len(req.Identities) && chunk >= 0 {
			return index, chunk, nil
		}
	}
	return 0, 0, fmt.Errorf("%w: invalid cursor", vector.ErrHubLookupInvalid)
}
