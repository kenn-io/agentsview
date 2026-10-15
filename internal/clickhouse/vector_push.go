package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/storage"
)

// vectorProgressStride bounds how many sessions the delta scan examines
// between progress reports; every vector batch flush also reports.
const vectorProgressStride = 2000

// vectorCompleteMarker is the vector_push_state session_id (never a real
// session id) recording this archive's clean generation-wide pass.
const vectorCompleteMarker = ""

// pushVectors replicates the local active embedding generation into the
// mirror for this archive: it pushes sessions whose aggregate document hash
// changed and evicts vector state for sessions the mirror no longer holds
// for this archive. It is a no-op (Skipped) when no source is attached or
// the local index has no active generation.
//
// Ownership is by source archive id, the same rule the session tables use:
// a session is pushed only when this archive's session row is resident in
// the mirror, and evicted when its state row outlives that residency. There
// are no per-row owner markers to adjudicate.
//
// scope, when non-nil, limits the reconciliation to those session ids (a
// change-triggered watch push); eviction and vector-only changes then wait
// for the next generation-wide push. A scoped push promotes itself to
// generation-wide until this archive has recorded one clean generation-wide
// pass over the generation (first push, tables recreated, or an earlier
// pass interrupted or deferred sessions), so a partial generation is always
// completed by the next push rather than only when its sessions change.
// fullSince, when nonzero, marks a full push: every session's vectors are
// re-sent, repairing state rows that wrongly report a session current, except
// sessions whose state row this push already wrote (version fullSince or
// later) with the current hash, so a resumed full push keeps its progress.
// repair re-sends only the sessions it names. failed names sessions whose
// session-phase push failed this run; their vectors are deferred so vector
// rows never run ahead of the session rows.
func (s *Sync) pushVectors(
	ctx context.Context, fullSince uint64, repair []string, scope []string,
	failed map[string]struct{}, onProgress func(storage.PushProgress),
) (storage.VectorPushResult, error) {
	var res storage.VectorPushResult
	if s.local.ArchiveContent().UsageOnly() {
		res.Skipped, res.SkippedReason = true, "usage-only archive keeps no vectors"
		return res, s.clearUsageOnlyVectors(ctx)
	}
	if s.vectorSource == nil {
		res.Skipped, res.SkippedReason = true, "no vector source configured"
		return res, nil
	}
	if scope != nil && len(scope) == 0 {
		log.Printf("clickhouse vector push: no changed sessions; deferring reconciliation to the next generation-wide push")
		return res, nil
	}
	export, err := s.beginVectorExport(ctx, scope, &res)
	if err != nil {
		return res, vectorPhaseError(err)
	}
	defer func() {
		if export != nil {
			_ = export.Close()
		}
	}()
	gen := export.Generation()
	res.GenerationID = vectorGenerationID(gen.Fingerprint)

	if scope != nil {
		completed, err := s.archiveCompletedGeneration(ctx, gen.Fingerprint)
		if err != nil {
			return res, err
		}
		if !completed {
			log.Printf("clickhouse vector push: this archive has not completed a generation-wide pass over %q; promoting scoped push to generation-wide reconciliation",
				gen.Fingerprint)
			_ = export.Close()
			scope = nil
			export, err = s.beginVectorExport(ctx, nil, &res)
			if err != nil {
				return res, vectorPhaseError(err)
			}
			gen = export.Generation()
		}
	}
	if scope == nil {
		if err := s.ensureVectorGeneration(ctx, gen); err != nil {
			return res, err
		}
	}

	local, err := export.SessionDocHashes(ctx, scope)
	if err != nil {
		return res, fmt.Errorf("reading local vector doc hashes: %w", err)
	}
	state, err := s.readVectorPushState(ctx, gen.Fingerprint, scope)
	if err != nil {
		return res, err
	}
	candidates := make([]string, 0, len(local)+len(state))
	for id := range local {
		candidates = append(candidates, id)
	}
	for id := range state {
		if _, inLocal := local[id]; !inLocal {
			candidates = append(candidates, id)
		}
	}
	resident, err := s.archiveResidentSessionIDs(ctx, candidates)
	if err != nil {
		return res, err
	}
	log.Printf("clickhouse vector push: comparing %d local session(s) against %d mirror state row(s) for generation %s",
		len(local), len(state), gen.Fingerprint)

	examined := 0
	report := func() {
		if onProgress == nil {
			return
		}
		onProgress(storage.PushProgress{
			Phase:               "vectors",
			VectorSessionsDone:  examined,
			VectorSessionsTotal: len(local),
			VectorChunksPushed:  res.ChunksPushed,
		})
	}
	batch := s.newVectorBatch(gen.Fingerprint)
	flush := func() error {
		if batch.empty() {
			return nil
		}
		if err := s.flushVectorBatch(ctx, batch); err != nil {
			return err
		}
		res.SessionsPushed += len(batch.sessions)
		res.DocsPushed += batch.docs
		res.ChunksPushed += len(batch.chunkRows)
		batch.reset()
		report()
		return nil
	}
	var evict []string
	for sessionID, agg := range local {
		examined++
		if examined%vectorProgressStride == 0 {
			report()
		}
		prev, hasState := state[sessionID]
		if !resident[sessionID] {
			// The session phase did not push this session (out of the
			// project scope, or never mirrored). Stale vector state for it
			// is evicted below; nothing new is written.
			if hasState {
				evict = append(evict, sessionID)
			}
			continue
		}
		if hasState && prev.hash == agg && !slices.Contains(repair, sessionID) &&
			(fullSince == 0 || prev.version >= fullSince) {
			res.SessionsUnchanged++
			continue
		}
		if _, isFailed := failed[sessionID]; isFailed {
			res.DeferSession(sessionID)
			continue
		}
		docs, exportHash, err := export.SessionDocs(ctx, sessionID)
		if err != nil {
			return res, fmt.Errorf("reading local docs for session %s: %w", sessionID, err)
		}
		// The export hash covers exactly the docs read above, in the same
		// local snapshot. A mismatch with the delta-scan hash means the
		// local index changed between the scan and this export (an
		// embeddings rebuild refilling the generation) and the docs may be
		// a partial view. Defer: the next push re-derives the delta from
		// the settled index.
		if exportHash != agg {
			res.DeferSession(sessionID)
			continue
		}
		batch.add(sessionID, agg, docs)
		if batch.full() {
			if err := flush(); err != nil {
				return res, err
			}
		}
	}
	if err := flush(); err != nil {
		return res, err
	}
	report()

	for sessionID := range state {
		if _, inLocal := local[sessionID]; inLocal {
			continue
		}
		if _, isFailed := failed[sessionID]; isFailed {
			res.DeferSession(sessionID)
			continue
		}
		evict = append(evict, sessionID)
	}
	for _, sessionID := range evict {
		if err := s.evictVectorSession(ctx, gen.Fingerprint, sessionID); err != nil {
			return res, err
		}
		res.SessionsEvicted++
	}
	if scope == nil && res.SessionsDeferred == 0 {
		if err := insertRows(ctx, s.conn, "vector_push_state", [][]any{{
			s.archiveID, gen.Fingerprint, vectorCompleteMarker, "", newPushVersion(),
		}}); err != nil {
			return res, err
		}
	}
	log.Printf("clickhouse vector push: %d session(s) pushed, %d unchanged, %d deferred, %d evicted, %d chunks",
		res.SessionsPushed, res.SessionsUnchanged, res.SessionsDeferred,
		res.SessionsEvicted, res.ChunksPushed)
	return res, nil
}

// errVectorPhaseSkipped marks a beginVectorExport miss that is not a
// failure: res is already marked Skipped and the phase returns cleanly.
var errVectorPhaseSkipped = errors.New("vector phase skipped")

// vectorPhaseError maps a skipped export to a clean return and passes any
// other error through.
func vectorPhaseError(err error) error {
	if errors.Is(err, errVectorPhaseSkipped) {
		return nil
	}
	return err
}

// beginVectorExport opens the local export for scope. It returns an error
// wrapping errVectorPhaseSkipped, with res marked Skipped, when the local
// index is not ready or has no active generation; the export is non-nil
// whenever the error is nil.
func (s *Sync) beginVectorExport(
	ctx context.Context, scope []string, res *storage.VectorPushResult,
) (storage.VectorExport, error) {
	export, hasGen, err := s.vectorSource.BeginExport(ctx, scope)
	if errors.Is(err, storage.ErrVectorSourceNotReady) {
		res.Skipped, res.SkippedReason = true, err.Error()
		log.Printf("clickhouse vector push: skipped: %v", err)
		return nil, errVectorPhaseSkipped
	}
	if err != nil {
		return nil, fmt.Errorf("resolving local vector generation: %w", err)
	}
	if !hasGen {
		res.Skipped, res.SkippedReason = true, "no active local generation"
		return nil, errVectorPhaseSkipped
	}
	if export == nil {
		return nil, errors.New("resolving local vector generation: BeginExport returned a nil export")
	}
	return export, nil
}

// vectorGenerationRegistered reports whether the mirror holds a
// vector_generations row for fingerprint.
func (s *Sync) vectorGenerationRegistered(ctx context.Context, fingerprint string) (bool, error) {
	return s.rowExists(ctx, "vector generation",
		`SELECT count() FROM vector_generations WHERE fingerprint = ?`, fingerprint)
}

// vectorGenerationID folds a generation fingerprint into the nonzero int64
// the watch orchestrator memoizes to decide whether a scoped push may stay
// scoped. ClickHouse keys generations by fingerprint, so the id is derived
// from it rather than allocated.
func vectorGenerationID(fingerprint string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(fingerprint))
	return int64(h.Sum64()>>1) | 1
}

// rowExists runs a count() query and reports whether it found any row.
func (s *Sync) rowExists(ctx context.Context, what, query string, args ...any) (bool, error) {
	var n int
	if err := s.conn.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return false, fmt.Errorf("reading clickhouse %s: %w", what, err)
	}
	return n > 0, nil
}

// archiveCompletedGeneration reports whether this archive has recorded a
// clean generation-wide pass over the generation.
func (s *Sync) archiveCompletedGeneration(ctx context.Context, fingerprint string) (bool, error) {
	return s.rowExists(ctx, "vector completion marker", `
		SELECT count() FROM vector_push_state
		WHERE source_archive_id = ? AND generation_fingerprint = ? AND session_id = ?`,
		s.archiveID, fingerprint, vectorCompleteMarker)
}

// ensureVectorGeneration registers gen when the mirror does not hold it.
// Machines with matching embedding configs share one generation.
func (s *Sync) ensureVectorGeneration(ctx context.Context, gen storage.VectorGenerationInfo) error {
	registered, err := s.vectorGenerationRegistered(ctx, gen.Fingerprint)
	if err != nil || registered {
		return err
	}
	now := time.Now().UTC()
	return insertRows(ctx, s.conn, "vector_generations", [][]any{{
		gen.Fingerprint, gen.Model, int64(gen.Dimension), &now, newPushVersion(),
	}})
}

// clearUsageOnlyVectors removes every vector row this archive recorded, in
// every generation, once the local archive keeps usage only: the mirror
// must not keep serving transcript text the archive no longer holds. Rows
// another archive still records for the same session survive, as in any
// eviction, and this archive's completion markers go so a later push with
// vectors re-enabled starts generation-wide.
func (s *Sync) clearUsageOnlyVectors(ctx context.Context) error {
	evict, err := s.archiveVectorSessions(ctx)
	if err != nil {
		return err
	}
	for _, o := range evict {
		if err := s.evictVectorSession(ctx, o.fingerprint, o.sessionID); err != nil {
			return err
		}
	}
	// Documents outlive chunks when a push stopped after inserting them or
	// an eviction stopped between its two deletes; neither state nor chunks
	// lead back to them, so sweep them by the session's archive directly.
	if _, err := s.conn.ExecContext(ctx, `
		DELETE FROM vector_documents
		WHERE session_id IN (SELECT id FROM sessions WHERE source_archive_id = ?)
		  AND session_id NOT IN (SELECT session_id FROM vector_chunks)
		  AND session_id NOT IN (SELECT session_id FROM vector_push_state WHERE source_archive_id <> ?)`,
		s.archiveID, s.archiveID); err != nil {
		return fmt.Errorf("clearing clickhouse orphaned vector documents: %w", err)
	}
	if _, err := s.conn.ExecContext(ctx, `
		DELETE FROM vector_push_state WHERE source_archive_id = ? AND session_id = ?`,
		s.archiveID, vectorCompleteMarker); err != nil {
		return fmt.Errorf("clearing clickhouse vector completion markers: %w", err)
	}
	if len(evict) > 0 {
		log.Printf("clickhouse vector push: usage-only archive; evicted vectors for %d session(s)", len(evict))
	}
	return nil
}

// archiveVectorSession is one (generation, session) pair this archive has
// recorded in vector_push_state.
type archiveVectorSession struct{ fingerprint, sessionID string }

// archiveVectorSessions lists every (generation, session) pair this archive
// is answerable for: pairs it recorded in vector_push_state, plus pairs that
// hold chunks for a session whose row this archive pushed. The second set
// catches a session push interrupted after its chunks landed but before its
// state row, which no later push would otherwise revisit once the archive
// keeps usage only.
func (s *Sync) archiveVectorSessions(ctx context.Context) ([]archiveVectorSession, error) {
	rows, err := s.conn.QueryContext(ctx, `
		SELECT generation_fingerprint, session_id FROM vector_push_state
		WHERE source_archive_id = ? AND session_id <> ?
		UNION DISTINCT
		SELECT c.generation_fingerprint, c.session_id FROM vector_chunks c
		WHERE c.session_id IN (SELECT id FROM sessions WHERE source_archive_id = ?)`,
		s.archiveID, vectorCompleteMarker, s.archiveID)
	if err != nil {
		return nil, fmt.Errorf("listing clickhouse archive vector sessions: %w", err)
	}
	defer rows.Close()
	var out []archiveVectorSession
	for rows.Next() {
		var o archiveVectorSession
		if err := rows.Scan(&o.fingerprint, &o.sessionID); err != nil {
			return nil, fmt.Errorf("scanning clickhouse archive vector session: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// vectorSessionState is one session's vector_push_state row.
type vectorSessionState struct {
	hash    string
	version uint64
}

// readVectorPushState returns this archive's per-session aggregate hashes
// and their push versions for the generation, limited to scope when non-nil.
// The completion marker row is not a session and is excluded.
func (s *Sync) readVectorPushState(
	ctx context.Context, fingerprint string, scope []string,
) (map[string]vectorSessionState, error) {
	state := map[string]vectorSessionState{}
	read := func(where string, args []any) error {
		rows, err := s.conn.QueryContext(ctx, `
			SELECT session_id, doc_agg_hash, push_version FROM vector_push_state
			WHERE source_archive_id = ? AND generation_fingerprint = ? AND session_id <> ?`+where,
			append([]any{s.archiveID, fingerprint, vectorCompleteMarker}, args...)...)
		if err != nil {
			return fmt.Errorf("reading clickhouse vector push state: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var st vectorSessionState
			if err := rows.Scan(&id, &st.hash, &st.version); err != nil {
				return fmt.Errorf("scanning clickhouse vector push state: %w", err)
			}
			state[id] = st
		}
		return rows.Err()
	}
	if scope == nil {
		return state, read("", nil)
	}
	for batch := range idBatches(uniqueIDs(scope)) {
		placeholders, args := inArgs(batch)
		if err := read(" AND session_id IN ("+placeholders+")", args); err != nil {
			return nil, err
		}
	}
	return state, nil
}

// archiveResidentSessionIDs reports which of ids have a session row this
// archive pushed to the mirror.
func (s *Sync) archiveResidentSessionIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	resident := make(map[string]bool, len(ids))
	for batch := range idBatches(uniqueIDs(ids)) {
		placeholders, args := inArgs(batch)
		if err := func() error {
			rows, err := s.conn.QueryContext(ctx,
				"SELECT id FROM sessions WHERE source_archive_id = ? AND id IN ("+placeholders+")",
				append([]any{s.archiveID}, args...)...)
			if err != nil {
				return fmt.Errorf("reading clickhouse vector resident sessions: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return fmt.Errorf("scanning clickhouse vector resident session: %w", err)
				}
				resident[id] = true
			}
			return rows.Err()
		}(); err != nil {
			return nil, err
		}
	}
	return resident, nil
}

// vectorBatchSessions bounds the sessions one vector flush covers. Their ids
// appear in the flush's IN lists, so the bound keeps those statements under
// max_query_size.
const vectorBatchSessions = idBatchSize

// vectorBatchBytes bounds the estimated document text and embedding payload
// one flush holds in memory. A single session larger than the bound flushes
// alone.
const vectorBatchBytes = 32 << 20

// vectorBatch accumulates the rows of several sessions so one flush writes
// them with a fixed number of statements.
type vectorBatch struct {
	fingerprint string
	maxSessions int
	sessions    []string
	stateHashes []string
	docRows     [][]any
	chunkRows   [][]any
	docs        int
	bytes       int
}

func (s *Sync) newVectorBatch(fingerprint string) *vectorBatch {
	maxSessions := vectorBatchSessions
	if s.vectorBatchSessions > 0 {
		maxSessions = s.vectorBatchSessions
	}
	return &vectorBatch{fingerprint: fingerprint, maxSessions: maxSessions}
}

// add queues one session's documents and chunks. Rows carry no push version
// yet: the flush stamps one version on the whole batch.
func (b *vectorBatch) add(sessionID, aggHash string, docs []storage.VectorPushDoc) {
	b.sessions = append(b.sessions, sessionID)
	b.stateHashes = append(b.stateHashes, aggHash)
	b.docs += len(docs)
	for _, doc := range docs {
		offsets := doc.OffsetsJSON
		if offsets == "" {
			offsets = "[]"
		}
		b.docRows = append(b.docRows, []any{
			doc.DocKey, doc.SessionID, doc.SourceUUID,
			int64(doc.Ordinal), int64(doc.OrdinalEnd), doc.Subordinate,
			offsets, doc.Content, doc.ContentHash, nil,
		})
		b.bytes += len(doc.DocKey) + len(doc.SessionID) + len(doc.SourceUUID) +
			len(offsets) + len(doc.Content) + len(doc.ContentHash)
		for _, chunk := range doc.Chunks {
			b.chunkRows = append(b.chunkRows, []any{
				b.fingerprint, doc.DocKey, int64(chunk.ChunkIndex), sessionID,
				chunk.Embedding, nil,
			})
			b.bytes += len(doc.DocKey) + len(sessionID) + 4*len(chunk.Embedding)
		}
	}
}

func (b *vectorBatch) empty() bool { return len(b.sessions) == 0 }

func (b *vectorBatch) full() bool {
	return len(b.sessions) >= b.maxSessions || b.bytes >= vectorBatchBytes
}

func (b *vectorBatch) reset() {
	b.sessions, b.stateHashes = b.sessions[:0], b.stateHashes[:0]
	b.docRows, b.chunkRows = nil, nil
	b.docs, b.bytes = 0, 0
}

// flushVectorBatch rewrites the batch's documents, chunks, and push state for
// the generation under one push version. Rows are inserted first, then older
// versions of the batch sessions' rows are deleted, then the state rows land,
// so an interrupted flush leaves every batch session's state row behind and
// the next push repeats those sessions instead of trusting half-written ones.
func (s *Sync) flushVectorBatch(ctx context.Context, b *vectorBatch) error {
	version := newPushVersion()
	for _, row := range b.docRows {
		row[len(row)-1] = version
	}
	for _, row := range b.chunkRows {
		row[len(row)-1] = version
	}
	if err := insertRows(ctx, s.conn, "vector_documents", b.docRows); err != nil {
		return err
	}
	if err := insertRows(ctx, s.conn, "vector_chunks", b.chunkRows); err != nil {
		return err
	}
	placeholders, ids := inArgs(b.sessions)
	// Older versions of these sessions' rows are now superseded: chunks for
	// this generation, and every generation's chunks for documents that
	// vanished locally (a document is shared across generations, so a
	// vanished doc's rows are stale everywhere). The vanished-doc check
	// pairs doc_key with session_id, so a doc that moved to another session
	// still counts as vanished from its old one, and the subquery needs no
	// second copy of the session list: rows at or above this version belong
	// to this flush or to a later push.
	if _, err := s.conn.ExecContext(ctx, `
		DELETE FROM vector_chunks
		WHERE session_id IN (`+placeholders+`) AND generation_fingerprint = ? AND push_version < ?`,
		slices.Concat(ids, []any{b.fingerprint, version})...); err != nil {
		return fmt.Errorf("deleting older clickhouse vector chunks for %d session(s): %w", len(ids), err)
	}
	if _, err := s.conn.ExecContext(ctx, `
		DELETE FROM vector_chunks
		WHERE session_id IN (`+placeholders+`) AND (session_id, doc_key) NOT IN (
			SELECT session_id, doc_key FROM vector_documents WHERE push_version >= ?)`,
		slices.Concat(ids, []any{version})...); err != nil {
		return fmt.Errorf("deleting clickhouse vector chunks of vanished docs for %d session(s): %w", len(ids), err)
	}
	if _, err := s.conn.ExecContext(ctx, `
		DELETE FROM vector_documents WHERE session_id IN (`+placeholders+`) AND push_version < ?`,
		slices.Concat(ids, []any{version})...); err != nil {
		return fmt.Errorf("deleting older clickhouse vector documents for %d session(s): %w", len(ids), err)
	}
	if s.hooks != nil && s.hooks.beforeVectorState != nil {
		if err := s.hooks.beforeVectorState(b.sessions); err != nil {
			return err
		}
	}
	stateRows := make([][]any, len(b.sessions))
	for i, sessionID := range b.sessions {
		stateRows[i] = []any{s.archiveID, b.fingerprint, sessionID, b.stateHashes[i], version}
	}
	return insertRows(ctx, s.conn, "vector_push_state", stateRows)
}

// evictVectorSession drops this archive's push state for one session and
// generation. The session's chunks, and its document rows once no
// generation references them, go only when no other archive still records
// the session for this generation: after an ownership handoff the new owner's
// rows must survive the former owner's reconciliation. The state row goes
// last so a failure midway leaves the session discoverable for the next
// push to finish.
func (s *Sync) evictVectorSession(ctx context.Context, fingerprint, sessionID string) error {
	otherOwners, newest, err := s.vectorEvictionBound(ctx, fingerprint, sessionID)
	if err != nil {
		return err
	}
	if !otherOwners {
		if err := s.deleteVectorChunksThrough(ctx, fingerprint, sessionID, newest); err != nil {
			return err
		}
		if _, err := s.conn.ExecContext(ctx, `
			DELETE FROM vector_documents
			WHERE session_id = ? AND session_id NOT IN (
				SELECT session_id FROM vector_chunks WHERE session_id = ?)`,
			sessionID, sessionID); err != nil {
			return fmt.Errorf("evicting clickhouse vector documents for %s: %w", sessionID, err)
		}
	}
	if _, err := s.conn.ExecContext(ctx, `
		DELETE FROM vector_push_state
		WHERE source_archive_id = ? AND generation_fingerprint = ? AND session_id = ?`,
		s.archiveID, fingerprint, sessionID); err != nil {
		return fmt.Errorf("evicting clickhouse vector push state for %s: %w", sessionID, err)
	}
	return nil
}

// vectorEvictionBound reads whether another archive owns the pair and the
// newest chunk version it holds. Another archive owns the pair through its
// state row or through the session now being mirrored under it (a push
// mirrors sessions before chunks, and chunks before its state row). The
// caller deletes only through the version seen here so later inserts
// survive.
func (s *Sync) vectorEvictionBound(ctx context.Context, fingerprint, sessionID string) (otherOwners bool, newest uint64, err error) {
	var owners uint64
	if err := s.conn.QueryRowContext(ctx, `
		SELECT
			(SELECT count() FROM vector_push_state
			  WHERE generation_fingerprint = ? AND session_id = ? AND source_archive_id <> ?)
			+ (SELECT count() FROM sessions WHERE id = ? AND source_archive_id <> ?),
			(SELECT coalesce(max(push_version), 0) FROM vector_chunks
			  WHERE session_id = ? AND generation_fingerprint = ?)`,
		fingerprint, sessionID, s.archiveID, sessionID, s.archiveID, sessionID, fingerprint,
	).Scan(&owners, &newest); err != nil {
		return false, 0, fmt.Errorf("reading clickhouse vector owners for %s: %w", sessionID, err)
	}
	return owners > 0, newest, nil
}

// deleteVectorChunksThrough removes the pair's chunks at or below newest.
func (s *Sync) deleteVectorChunksThrough(ctx context.Context, fingerprint, sessionID string, newest uint64) error {
	if _, err := s.conn.ExecContext(ctx, `
		DELETE FROM vector_chunks
		WHERE session_id = ? AND generation_fingerprint = ? AND push_version <= ?`,
		sessionID, fingerprint, newest); err != nil {
		return fmt.Errorf("evicting clickhouse vector chunks for %s: %w", sessionID, err)
	}
	return nil
}
