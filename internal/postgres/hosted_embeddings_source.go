package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

func embeddingManifest(snapshot *HostedEmbeddingSnapshot) string {
	// Hash metadata and exact content/input digests without copying the entire
	// retained transcript into a second JSON buffer.
	h := sha256.New()
	encoder := json.NewEncoder(h)
	_ = encoder.Encode(struct {
		Generation      int64
		Recipe, Session string
		Revision        int64
		Eligible        bool
	}{snapshot.Generation.ID, snapshot.Generation.Recipe.Fingerprint, snapshot.Lease.SessionID, snapshot.Lease.Revision, snapshot.Eligible})
	for _, doc := range snapshot.Documents {
		unit := doc.Unit
		unit.Content = ""
		chunks := make([]struct {
			Index              int
			Hash, DeclaredHash string
		}, len(doc.Chunks))
		for i, c := range doc.Chunks {
			chunks[i].Index = c.Index
			chunks[i].Hash = embeddingInputHash(snapshot.Generation.Recipe, c.Text)
			chunks[i].DeclaredHash = c.InputHash
		}
		_ = encoder.Encode(struct {
			Key                       string
			Unit                      db.EmbeddableUnit
			ContentHash, DeclaredHash string
			Chunks                    any
		}{doc.Key, unit, embeddingHash([]byte(doc.Unit.Content)), doc.ContentHash, chunks})
	}
	return hex.EncodeToString(h.Sum(nil))
}
func embeddingInputHash(recipe HostedEmbeddingRecipe, text string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(recipe.DocumentPrefix))
	_, _ = h.Write([]byte(text))
	_, _ = h.Write([]byte(recipe.InputSuffix))
	return hex.EncodeToString(h.Sum(nil))
}
func (s *HostedEmbeddingStore) ReadSession(ctx context.Context, l HostedEmbeddingLease) (*HostedEmbeddingSnapshot, error) {
	if len(l.SessionID) > 65536 {
		return nil, ErrHostedEmbeddingWorkLimit
	}
	ctx, cancel := context.WithTimeout(ctx, s.limits.SnapshotTimeout)
	defer cancel()
	tx, e := s.pg.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if e != nil {
		return nil, e
	}
	defer func() { _ = tx.Rollback() }()
	g, e := loadEmbeddingGeneration(ctx, tx, l.GenerationID)
	if e != nil {
		return nil, e
	}
	var revision int64
	if e = tx.QueryRowContext(ctx, `SELECT revision FROM hosted_embedding_sources WHERE tenant_id=$1 AND session_id=$2`, s.tenant, l.SessionID).Scan(&revision); e != nil {
		return nil, e
	}
	if revision != l.Revision {
		return nil, ErrHostedEmbeddingStale
	}
	eligible, e := embeddingEligible(ctx, tx, l.SessionID, g.Recipe.IncludeAutomated)
	if e != nil {
		return nil, e
	}
	snap := &HostedEmbeddingSnapshot{Generation: g, Lease: l, Eligible: eligible}
	var used int64
	var chunkCount, rowCount int
	counter := newOccurrenceCounter(s.limits.MaxOccurrenceBytes, s.limits.MaxSpillBytes)
	defer func() { _ = counter.close() }()
	charge := func(n int64) error {
		used += n
		if used > s.limits.MaxSnapshotBytes {
			return ErrHostedEmbeddingWorkLimit
		}
		return nil
	}
	if eligible {
		var subordinate bool
		if e = tx.QueryRowContext(ctx, `SELECT relationship_type IN ('subagent','fork') OR (COALESCE(parent_session_id,'')<>'' AND relationship_type<>'continuation') FROM sessions WHERE tenant_id=$1 AND id=$2`, s.tenant, l.SessionID).Scan(&subordinate); e != nil {
			return nil, e
		}
		reducer := db.NewEmbeddingUnitReducer(s.limits.MaxUnitBytes, func(u db.EmbeddableUnit) error {
			if len(snap.Documents) >= s.limits.MaxDocuments {
				return ErrHostedEmbeddingWorkLimit
			}
			occ, e := counter.next(ctx, u.SourceUUID)
			if e != nil {
				return e
			}
			doc := HostedEmbeddingDocument{Key: vector.DocKey(u.Kind, u.SessionID, u.SourceUUID, u.Ordinal, occ), Unit: u, ContentHash: embeddingHash([]byte(u.Content))}
			if e = charge(int64(len(doc.Key) + len(u.SourceUUID) + len(u.SessionID) + len(u.Offsets)*24 + 128)); e != nil {
				return e
			}
			chunks, e := boundedEmbeddingChunks(u.Content, g.Recipe, s.limits.MaxChunks-chunkCount, s.limits.MaxSnapshotBytes-used)
			if e != nil {
				return e
			}
			for _, chunk := range chunks {
				chunkCount++
				// Budget the complete output, including cache hits, before any encoder
				// can retain vectors. Recipe dimensions and chunk count are bounded.
				if chunkCount > s.limits.MaxChunks || int64(chunkCount)*int64(g.Recipe.Dimensions)*4 > s.limits.MaxVectorBytes {
					return ErrHostedEmbeddingWorkLimit
				}
				if e = charge(int64(len(chunk.Text) + 128)); e != nil {
					return e
				}
				doc.Chunks = append(doc.Chunks, HostedEmbeddingChunk{Index: chunk.Index, Text: chunk.Text, InputHash: embeddingInputHash(g.Recipe, chunk.Text)})
			}
			snap.Documents = append(snap.Documents, doc)
			return nil
		})
		after := -1
		for {
			predicate := `role IN ('user','assistant') AND NOT is_system AND ` + db.PostgresSystemPrefixSQL("m.content", "m.role")
			rows, e := tx.QueryContext(ctx, `SELECT ordinal,left(role,32),octet_length(source_uuid),CASE WHEN octet_length(source_uuid)<=65536 AND (`+predicate+`) THEN source_uuid ELSE '' END,octet_length(content),CASE WHEN octet_length(content)<=$4 AND (`+predicate+`) THEN content ELSE '' END,is_sidechain,(`+predicate+`) FROM messages m WHERE tenant_id=$1 AND session_id=$2 AND ordinal>$3 ORDER BY ordinal LIMIT $5`, s.tenant, l.SessionID, after, s.limits.MaxUnitBytes, s.limits.RowPageSize)
			if e != nil {
				return nil, e
			}
			n := 0
			for rows.Next() {
				var row db.EmbeddingUnitRow
				var uuidBytes, contentBytes int64
				var embeddable bool
				row.SessionID = l.SessionID
				row.SubordinateSession = subordinate
				e = rows.Scan(&row.Ordinal, &row.Role, &uuidBytes, &row.SourceUUID, &contentBytes, &row.Content, &row.Sidechain, &embeddable)
				if e != nil {
					rows.Close()
					return nil, e
				}
				rowCount++
				after = row.Ordinal
				n++
				if rowCount > s.limits.MaxRows {
					rows.Close()
					return nil, ErrHostedEmbeddingWorkLimit
				}
				if !embeddable {
					continue
				}
				if uuidBytes > 65536 || contentBytes > s.limits.MaxUnitBytes {
					rows.Close()
					return nil, ErrHostedEmbeddingWorkLimit
				}
				if e = charge(contentBytes + uuidBytes + 64); e != nil {
					rows.Close()
					return nil, e
				}
				if e = reducer.Push(row); e != nil {
					rows.Close()
					if errors.Is(e, db.ErrEmbeddingUnitTooLarge) {
						return nil, ErrHostedEmbeddingWorkLimit
					}
					return nil, e
				}
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return nil, e
			}
			if n == 0 {
				break
			}
		}
		if e = reducer.Finish(); e != nil {
			if errors.Is(e, db.ErrEmbeddingUnitTooLarge) {
				return nil, ErrHostedEmbeddingWorkLimit
			}
			return nil, e
		}
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	snap.ManifestHash = embeddingManifest(snap)
	// This second transaction binds only a complete, closed source snapshot.
	bind, e := s.fence(ctx)
	if e != nil {
		return nil, e
	}
	defer func() { _ = bind.Rollback() }()
	if _, e = s.lockLease(ctx, bind, l, true); e != nil {
		return nil, e
	}
	if _, e = bind.ExecContext(ctx, `UPDATE hosted_embedding_requirements SET snapshot_manifest_hash=$4,expected_documents=$5,expected_chunks=$6 WHERE tenant_id=$1 AND generation_id=$2 AND session_id=$3`, s.tenant, l.GenerationID, l.SessionID, snap.ManifestHash, len(snap.Documents), chunkCount); e != nil {
		return nil, e
	}
	if e = bind.Commit(); e != nil {
		return nil, e
	}
	return snap, nil
}

// Reject pathological overlap before Split allocates its rune array and all
// windows. The conservative preflight counts blank windows too; limits fail
// explicitly instead of silently changing the splitter's document boundaries.
func boundedEmbeddingChunks(content string, r HostedEmbeddingRecipe, maxChunks int, maxBytes int64) ([]kitvec.Chunk, error) {
	runes := utf8.RuneCountInString(content)
	windows := 1
	if runes > r.MaxInputChars {
		stride := r.MaxInputChars - r.ChunkOverlapChars
		windows = 1 + (runes-r.MaxInputChars+stride-1)/stride
	}
	if windows > maxChunks || int64(runes)*4+int64(windows)*(int64(min(len(content), r.MaxInputChars*4))+64) > maxBytes {
		return nil, ErrHostedEmbeddingWorkLimit
	}
	return kitvec.Split(content, kitvec.SplitOptions{MaxRunes: r.MaxInputChars, Overlap: r.ChunkOverlapChars}), nil
}
