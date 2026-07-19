package duckdb

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

func (s *Sync) syncModelPricing(ctx context.Context) error {
	prices, err := s.local.ListModelPricing(ctx)
	if err != nil {
		return err
	}
	if len(prices) == 0 {
		prices = duckFallbackPricingRows()
	}
	if len(prices) == 0 {
		return nil
	}

	existing, err := s.listDuckModelPricing(ctx)
	if err != nil {
		return err
	}
	_, prices = db.FilterChangedModelPricing(existing, prices)
	if len(prices) == 0 {
		return nil
	}

	tx, err := s.duck.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning duckdb pricing sync: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	for i := 0; i < len(prices); i += duckPricingUpsertBatch {
		end := min(i+duckPricingUpsertBatch, len(prices))
		batch := prices[i:end]
		query, args := duckPricingUpsertStatement(batch)
		if err := s.execMutation(ctx, tx, query, args...); err != nil {
			return fmt.Errorf(
				"syncing duckdb pricing batch starting at %d: %w",
				i, err,
			)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing duckdb pricing sync: %w", err)
	}
	return nil
}

const duckPricingUpsertBatch = 100

func duckPricingUpsertStatement(prices []db.ModelPricing) (string, []any) {
	var b strings.Builder
	b.WriteString(`INSERT INTO model_pricing (
		model_pattern, input_per_mtok, output_per_mtok,
		cache_creation_per_mtok, cache_read_per_mtok, updated_at
	) VALUES `)
	args := make([]any, 0, len(prices)*6)
	for i, p := range prices {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(?, ?, ?, ?, ?, ?)")
		args = append(args,
			p.ModelPattern,
			p.InputPerMTok,
			p.OutputPerMTok,
			p.CacheCreationPerMTok,
			p.CacheReadPerMTok,
			p.UpdatedAt,
		)
	}
	b.WriteString(`
	ON CONFLICT(model_pattern) DO UPDATE SET
		input_per_mtok = excluded.input_per_mtok,
		output_per_mtok = excluded.output_per_mtok,
		cache_creation_per_mtok = excluded.cache_creation_per_mtok,
		cache_read_per_mtok = excluded.cache_read_per_mtok,
		updated_at = excluded.updated_at`)
	return b.String(), args
}

func (s *Sync) listDuckModelPricing(ctx context.Context) ([]db.ModelPricing, error) {
	rows, err := s.duck.QueryContext(
		ctx,
		`SELECT model_pattern, input_per_mtok,
			output_per_mtok, cache_creation_per_mtok,
			cache_read_per_mtok, updated_at
		 FROM model_pricing`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing duckdb pricing: %w", err)
	}
	defer rows.Close()

	var out []db.ModelPricing
	for rows.Next() {
		var p db.ModelPricing
		if err := rows.Scan(
			&p.ModelPattern,
			&p.InputPerMTok,
			&p.OutputPerMTok,
			&p.CacheCreationPerMTok,
			&p.CacheReadPerMTok,
			&p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb pricing: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb pricing: %w", err)
	}
	return out, nil
}

func (s *Sync) syncCursorUsageEvents(ctx context.Context) error {
	// Cursor admin rows are global and unattributed, so project-filtered pushes
	// cannot sync them honestly.
	if s.isFiltered() {
		return nil
	}

	events, err := s.local.GetCursorUsageEvents(ctx)
	if err != nil {
		return fmt.Errorf("loading local cursor usage events: %w", err)
	}
	if len(events) == 0 {
		return nil
	}

	tx, err := s.duck.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning duckdb cursor usage sync: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if err := s.bulkInsertCursorUsageEvents(ctx, tx, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing duckdb cursor usage sync: %w", err)
	}
	return nil
}

// syncProjectIdentityObservations publishes project identity observations
// and per-session snapshots up through the local archive's current
// revision, using priorRevision (normally probe.IdentityRevision) as the
// cursor instead of local pg_sync_state. It performs a full publication
// when force is set, priorRevision predates any real publication, or the
// mirror's source_archives scope looks stale; otherwise it applies the
// compact (priorRevision, revision] delta. It returns the revision just
// published so the caller can persist it as mirror metadata's
// IdentityRevision.
func (s *Sync) syncProjectIdentityObservations(
	ctx context.Context, priorRevision int64, force bool,
) (int64, error) {
	revision, err := s.local.ProjectIdentityPublicationRevision(ctx)
	if err != nil {
		return 0, err
	}
	databaseGeneration, err := s.local.GetDatabaseID(ctx)
	if err != nil {
		return 0, fmt.Errorf("loading source database generation: %w", err)
	}
	archiveID, err := s.local.GetArchiveID(ctx)
	if err != nil {
		return 0, fmt.Errorf("loading source archive id: %w", err)
	}

	fullPublication := force || priorRevision <= 0 || priorRevision > revision
	if !fullPublication && priorRevision == revision {
		present, err := s.identityArchivePresent(ctx, archiveID)
		if err != nil {
			return 0, err
		}
		if present {
			return revision, nil
		}
		fullPublication = true
	}

	observations, snapshots, delta, err := s.loadIdentityPublicationScope(
		ctx, fullPublication, priorRevision, revision,
	)
	if err != nil {
		return 0, err
	}
	archiveSalt, err := s.local.GetArchiveSalt(ctx)
	if err != nil {
		return 0, fmt.Errorf("loading source archive salt: %w", err)
	}
	if err := s.writeIdentityPublication(
		ctx, archiveID, archiveSalt, databaseGeneration,
		fullPublication, delta, observations, snapshots,
	); err != nil {
		return 0, err
	}
	return revision, nil
}

func (s *Sync) identityArchivePresent(
	ctx context.Context, archiveID string,
) (bool, error) {
	var present bool
	if err := s.duck.QueryRowContext(ctx, `
		SELECT count(*) > 0 FROM source_archives
		WHERE source_archive_id = ?`, archiveID).Scan(&present); err != nil {
		return false, fmt.Errorf("checking duckdb project identity publication: %w", err)
	}
	return present, nil
}

// loadIdentityPublicationScope loads either the full in-scope identity
// publication (observations plus session snapshots) or the compact delta
// for (priorRevision, revision], depending on fullPublication.
func (s *Sync) loadIdentityPublicationScope(
	ctx context.Context, fullPublication bool, priorRevision, revision int64,
) (
	observations, snapshots []export.ProjectIdentityObservation,
	delta db.ProjectIdentityPublicationDelta, err error,
) {
	if !fullPublication {
		delta, err = s.local.LoadProjectIdentityPublicationDelta(
			ctx, priorRevision, revision, s.projects, s.excludeProjects,
		)
		return delta.Observations, delta.Snapshots, delta, err
	}
	observations, err = s.local.ListProjectIdentityObservations(ctx, nil)
	if err != nil {
		return nil, nil, delta, fmt.Errorf("loading project identity observations: %w", err)
	}
	observations = filterIdentityScope(observations, s.projects, s.excludeProjects)
	snapshots, err = s.local.ListSessionProjectIdentitySnapshots(ctx)
	if err != nil {
		return nil, nil, delta, fmt.Errorf("loading session project identity snapshots: %w", err)
	}
	snapshots = filterIdentityScope(snapshots, s.projects, s.excludeProjects)
	return observations, snapshots, delta, nil
}

// filterIdentityScope restricts a full-publication listing to the push
// scope. The delta path does not need this: LoadProjectIdentityPublicationDelta
// applies projects/excludeProjects in SQL.
func filterIdentityScope(
	items []export.ProjectIdentityObservation, projects, excludeProjects []string,
) []export.ProjectIdentityObservation {
	if len(projects) == 0 && len(excludeProjects) == 0 {
		return items
	}
	out := items[:0]
	for _, item := range items {
		if projectMatchesPushScope(item.Project, projects, excludeProjects) {
			out = append(out, item)
		}
	}
	return out
}

func projectMatchesPushScope(project string, projects, excludeProjects []string) bool {
	if len(projects) > 0 && !slices.Contains(projects, project) {
		return false
	}
	return !slices.Contains(excludeProjects, project)
}

func (s *Sync) writeIdentityPublication(
	ctx context.Context,
	archiveID, archiveSalt, databaseGeneration string,
	fullPublication bool,
	delta db.ProjectIdentityPublicationDelta,
	observations, snapshots []export.ProjectIdentityObservation,
) error {
	tx, err := s.duck.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning duckdb project identity sync: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if err := upsertSourceArchiveScope(
		func(stmt string, args ...any) error {
			return s.execMutation(ctx, tx, stmt, args...)
		},
		func(stmt string, args ...any) *sql.Row {
			return tx.QueryRowContext(ctx, stmt, args...)
		},
		archiveID, archiveSalt,
	); err != nil {
		return err
	}
	execDelta := func(stmt string, args ...any) error {
		return s.execMutation(ctx, tx, stmt, args...)
	}
	if fullPublication {
		if err := deleteProjectIdentityScope(
			execDelta, archiveID, s.projects, s.excludeProjects,
		); err != nil {
			return err
		}
	} else if err := deleteProjectIdentityDelta(
		execDelta, archiveID, databaseGeneration,
		delta.ObservationDeletes, delta.SnapshotDeletes,
	); err != nil {
		return err
	}
	for _, obs := range observations {
		obs.SourceArchiveID = archiveID
		obs.SourceArchiveSalt = archiveSalt
		obs = export.SanitizeStoredProjectIdentityObservation(obs)
		if err := upsertProjectIdentityObservation(
			func(stmt string, args ...any) error {
				return s.execMutation(ctx, tx, stmt, args...)
			},
			func(stmt string, args ...any) *sql.Row {
				return tx.QueryRowContext(ctx, stmt, args...)
			},
			obs, "",
		); err != nil {
			return fmt.Errorf(
				"syncing duckdb project identity observation %s/%s/%s: %w",
				obs.Project, obs.Machine, obs.RootPath, err,
			)
		}
	}
	for i := range snapshots {
		snapshots[i] = export.SanitizeStoredProjectIdentityObservation(snapshots[i])
	}
	if err := upsertSessionProjectIdentitySnapshots(
		func(stmt string, args ...any) error {
			return s.execMutation(ctx, tx, stmt, args...)
		},
		archiveID, databaseGeneration, snapshots,
	); err != nil {
		return fmt.Errorf("syncing duckdb session project identity snapshots: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing duckdb project identity sync: %w", err)
	}
	return nil
}

func duckFallbackPricingRows() []db.ModelPricing {
	src := pricingpkg.FallbackPricing()
	out := make([]db.ModelPricing, len(src))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i, p := range src {
		out[i] = db.ModelPricing{
			ModelPattern:         p.ModelPattern,
			InputPerMTok:         p.InputPerMTok,
			OutputPerMTok:        p.OutputPerMTok,
			CacheCreationPerMTok: p.CacheCreationPerMTok,
			CacheReadPerMTok:     p.CacheReadPerMTok,
			UpdatedAt:            now,
		}
	}
	return out
}

// applyDeletionDelta removes every mirror session tombstoned in the local
// deletion journal within (after, through]. This is the mirror-resident
// replacement for the old local-scan hard-delete reconciliation: the
// journal already tells us exactly which sessions to remove, so there is
// no need to diff the full local/mirror session ID sets on every push.
//
// Tombstones are loaded with NO project filter: a tombstone's recorded
// project is the session's LAST project, so a session that moved out of
// the push scope and was then hard-deleted would be invisible to a
// scope-filtered delta, leaving its mirror row (pushed while it was still
// in scope) behind forever. In-scope tombstones are applied and counted
// unconditionally exactly as before (deleting an already-absent session is
// a cheap no-op); out-of-scope tombstones — the transition case — only
// cost, and only count, when the session is actually still mirror-resident,
// so a never-mirrored out-of-scope deletion stays invisible to filtered
// diagnostics. Work stays bounded by the delta either way.
func (s *Sync) applyDeletionDelta(
	ctx context.Context, after, through int64, result *PushResult,
) error {
	tombstones, err := s.local.LoadSessionDeletionDelta(
		ctx, after, through, nil, nil,
	)
	if err != nil {
		return fmt.Errorf("loading duckdb session deletion delta: %w", err)
	}
	apply, err := s.selectTombstonesToApply(ctx, tombstones)
	if err != nil {
		return err
	}
	if len(apply) == 0 {
		return nil
	}
	if err := s.withDuckTx(ctx, "apply session deletion delta", func(tx *sql.Tx) error {
		for _, sessionID := range apply {
			if err := s.deleteMirrorSession(ctx, tx, sessionID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	result.Diagnostics.DeletedStaleSessions += len(apply)
	return nil
}

// selectTombstonesToApply dedupes the delta and picks the session IDs
// applyDeletionDelta acts on: every in-scope tombstone, plus out-of-scope
// tombstones whose sessions are still mirror-resident. The residency probe
// only runs when out-of-scope tombstones exist, so unfiltered pushes never
// pay for it.
func (s *Sync) selectTombstonesToApply(
	ctx context.Context, tombstones []db.SessionDeletionTombstone,
) ([]string, error) {
	seen := make(map[string]bool, len(tombstones))
	var apply, outOfScope []string
	for _, tombstone := range tombstones {
		if tombstone.SessionID == "" || seen[tombstone.SessionID] {
			continue
		}
		seen[tombstone.SessionID] = true
		if projectMatchesPushScope(tombstone.Project, s.projects, s.excludeProjects) {
			apply = append(apply, tombstone.SessionID)
		} else {
			outOfScope = append(outOfScope, tombstone.SessionID)
		}
	}
	if len(outOfScope) == 0 {
		return apply, nil
	}
	resident, err := s.mirrorResidentSessionIDs(ctx, outOfScope)
	if err != nil {
		return nil, err
	}
	for _, sessionID := range outOfScope {
		if resident[sessionID] {
			apply = append(apply, sessionID)
		}
	}
	return apply, nil
}

// curationSnapshot is one point-in-time read of the local in-scope
// curation state. Its fingerprint is computed from this in-memory data and
// replaceCuration writes this same data, so the fingerprint recorded in
// mirror sync_metadata always describes exactly what the mirror holds.
// Splitting those into separate SQLite reads (the previous shape) allowed
// an ABA race: a star or pin-note toggled and reverted between the
// fingerprint read and the copy read left the mirror holding the
// intermediate state behind a fingerprint that matched the reverted local
// state, so refreshCurationIfChanged skipped forever.
type curationSnapshot struct {
	starred       []string
	pinsBySession map[string][]db.PinnedMessage
}

// loadCurationSnapshot reads the scoped starred session IDs and every
// scoped pin row. Both queries are bounded by curation size, not mirror or
// archive size. Pins are loaded unfiltered by mirror residency: residency
// is a mirror-side property applied at write time (replaceCuration), while
// the fingerprint must track the LOCAL state so a pin for a
// not-yet-mirrored session still forces a refresh once its session lands.
func (s *Sync) loadCurationSnapshot(ctx context.Context) (curationSnapshot, error) {
	starred, err := s.local.ListStarredSessionIDsForScope(
		ctx, s.projects, s.excludeProjects,
	)
	if err != nil {
		return curationSnapshot{}, fmt.Errorf(
			"loading starred sessions for curation snapshot: %w", err)
	}
	pinnedSessions, err := s.local.ListPinnedSessionIDsForScope(
		ctx, s.projects, s.excludeProjects,
	)
	if err != nil {
		return curationSnapshot{}, fmt.Errorf(
			"loading pinned session ids for curation snapshot: %w", err)
	}
	pinsBySession, err := s.local.PinnedMessagesBySession(ctx, pinnedSessions)
	if err != nil {
		return curationSnapshot{}, fmt.Errorf(
			"loading pinned messages for curation snapshot: %w", err)
	}
	return curationSnapshot{starred: starred, pinsBySession: pinsBySession}, nil
}

// fingerprint hashes the snapshot's starred session ids and pinned
// message id/note state. Pin notes are included, not just membership:
// PinMessage on an already-pinned message updates its note in place
// without changing the pinned message id set, so a fingerprint over ids
// alone would miss a note-only edit. HasNote keeps a NULL note distinct
// from an explicit empty note. The payload reproduces the shape the
// previous separate-read fingerprint hashed ({ID, MessageID, CreatedAt,
// Note, HasNote} sorted by message id), so upgrading does not force a
// spurious refresh.
func (snap curationSnapshot) fingerprint() (string, error) {
	pinned := make([]db.PinCurationEntry, 0, len(snap.pinsBySession))
	for _, pins := range snap.pinsBySession {
		for _, p := range pins {
			entry := db.PinCurationEntry{
				ID:        p.ID,
				MessageID: p.MessageID,
				CreatedAt: p.CreatedAt,
			}
			if p.Note != nil {
				entry.Note = *p.Note
				entry.HasNote = true
			}
			pinned = append(pinned, entry)
		}
	}
	slices.SortFunc(pinned, func(a, b db.PinCurationEntry) int {
		if c := cmp.Compare(a.MessageID, b.MessageID); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	payload := struct {
		Starred []string
		Pinned  []db.PinCurationEntry
	}{Starred: snap.starred, Pinned: pinned}
	// A loaded snapshot holds nil for "no stars" while a restricted one
	// (see replaceCuration) holds an empty slice; both must hash the same
	// or an empty curation state would refresh on every push.
	if len(payload.Starred) == 0 {
		payload.Starred = nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encoding curation fingerprint: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// replaceCuration rewrites starred_sessions and pinned_messages from the
// given snapshot — never from a fresh local read — and returns the
// fingerprint of the resident-restricted snapshot it actually wrote. The
// caller records THAT fingerprint, not the full snapshot's: a star or pin
// for an in-scope session absent from the mirror (push failed, or the
// session was created after this push enumerated its candidates) is
// skipped by the residency check, and fingerprinting the full snapshot
// anyway would make the skipped rows look delivered — once the session
// lands, the matching fingerprint would suppress the refresh and the
// star/pin would stay missing until an unrelated curation edit. With the
// written fingerprint, the local state keeps mismatching until every
// curated session is resident, so each push retries the (curation-bounded)
// refresh and converges as soon as the session is mirrored.
//
// All work is bounded by curation size, not mirror size: mirror membership
// is validated for exactly the snapshot's session IDs (one batched lookup,
// see mirrorResidentSessionIDs), pin notes are preserved, and the delete
// side stays the machine-scoped clear of both tables, so removed
// stars/pins disappear without enumerating them.
func (s *Sync) replaceCuration(
	ctx context.Context, snap curationSnapshot,
) (string, error) {
	pinnedSessions := make([]string, 0, len(snap.pinsBySession))
	for id := range snap.pinsBySession {
		pinnedSessions = append(pinnedSessions, id)
	}
	slices.Sort(pinnedSessions)
	resident, err := s.mirrorResidentSessionIDs(
		ctx, append(append([]string(nil), snap.starred...), pinnedSessions...),
	)
	if err != nil {
		return "", err
	}
	written := curationSnapshot{
		pinsBySession: make(map[string][]db.PinnedMessage, len(snap.pinsBySession)),
	}
	for _, id := range snap.starred {
		if resident[id] {
			written.starred = append(written.starred, id)
		}
	}
	residentPinned := make([]string, 0, len(pinnedSessions))
	for _, id := range pinnedSessions {
		if resident[id] {
			residentPinned = append(residentPinned, id)
			written.pinsBySession[id] = snap.pinsBySession[id]
		}
	}
	fingerprint, err := written.fingerprint()
	if err != nil {
		return "", err
	}

	err = s.withDuckTx(ctx, "replace curation rows", func(tx *sql.Tx) error {
		for _, table := range []string{"pinned_messages", "starred_sessions"} {
			if err := s.execMutation(ctx, tx, `
				DELETE FROM `+table+`
				WHERE session_id IN (
					SELECT id FROM sessions WHERE machine = ?
				)`, s.machine); err != nil {
				return fmt.Errorf("clearing duckdb %s: %w", table, err)
			}
		}
		for _, id := range residentPinned {
			if err := insertPinnedMessages(ctx, tx, written.pinsBySession[id]); err != nil {
				return err
			}
		}
		for _, id := range written.starred {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO starred_sessions (session_id, created_at)
				 VALUES (?, current_timestamp)`,
				id,
			); err != nil {
				return fmt.Errorf("syncing starred session %s: %w", id, err)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return fingerprint, nil
}

// refreshCurationIfChanged skips replaceCuration's delete+reinsert when
// the LOCAL in-scope curation state (starred session ids, pinned message
// ids) has not changed since the last push that actually refreshed it,
// tracked via a fingerprint stored in mirror sync_metadata
// (curationFingerprintMetadataKey). This runs on every incremental push
// regardless of whether any session content changed, so curation edits
// (star/pin/unstar/unpin with no session content change) still propagate
// on the very next push exactly as before; the cost of a push that changed
// nothing at all is two small local queries bounded by curation size, and
// even a refresh that does run is curation-bounded (see replaceCuration).
// The stored fingerprint covers what the last refresh actually WROTE, so
// while a curated session is missing from the mirror the local state keeps
// mismatching and every push retries the refresh until it converges (see
// replaceCuration). It reports whether it actually refreshed.
func (s *Sync) refreshCurationIfChanged(ctx context.Context) (bool, error) {
	snap, err := s.loadCurationSnapshot(ctx)
	if err != nil {
		return false, err
	}
	fingerprint, err := snap.fingerprint()
	if err != nil {
		return false, err
	}
	stored, err := readMetadataKey(ctx, s.duck, curationFingerprintMetadataKey)
	if err != nil {
		return false, err
	}
	if fingerprint == stored {
		return false, nil
	}
	written, err := s.replaceCuration(ctx, snap)
	if err != nil {
		return false, err
	}
	if err := recordMetadataKey(
		ctx, s.duck, curationFingerprintMetadataKey, written,
	); err != nil {
		return false, err
	}
	return true, nil
}

// pushSession fully replaces one mirror session row and all of its
// dependent rows (messages, tool calls/results, usage events, secret
// findings, pins) with the current local content, then writes the
// fingerprint used to detect future changes. rebuildMirror and
// incrementalPush share this single write path: on a freshly created
// rebuild file the pre-delete below is simply a no-op.
func (s *Sync) pushSession(
	ctx context.Context, exec duckMutationExecutor, sess db.Session, fingerprint string,
) (int, error) {
	if err := s.upsertSession(ctx, exec, sess, fingerprint); err != nil {
		return 0, err
	}
	msgs, err := s.local.GetAllMessages(ctx, sess.ID)
	if err != nil {
		return 0, fmt.Errorf("reading local messages for %s: %w", sess.ID, err)
	}

	if err := s.replaceSessionDependents(ctx, exec, sess.ID); err != nil {
		return 0, err
	}
	if err := s.replaceUsageEvents(ctx, exec, sess.ID); err != nil {
		return 0, err
	}
	if err := insertMessages(ctx, exec, msgs); err != nil {
		return 0, err
	}
	if err := s.replaceToolRows(ctx, exec, sess.ID, msgs); err != nil {
		return 0, err
	}
	if err := s.replaceSecretFindings(ctx, exec, sess.ID); err != nil {
		return 0, err
	}
	if err := s.replacePinnedMessages(ctx, exec, sess.ID); err != nil {
		return 0, err
	}
	return len(msgs), nil
}

func (s *Sync) replaceSessionDependents(
	ctx context.Context, exec duckMutationExecutor, sessionID string,
) error {
	// usage_events is deliberately absent here: replaceUsageEvents owns that
	// delete+reinsert so the row is cleared exactly once per push.
	for _, stmt := range []string{
		`DELETE FROM pinned_messages WHERE session_id = ?`,
		`DELETE FROM secret_findings WHERE session_id = ?`,
		`DELETE FROM messages WHERE session_id = ?`,
	} {
		if err := s.execMutation(ctx, exec, stmt, sessionID); err != nil {
			return fmt.Errorf("clearing duckdb session dependents: %w", err)
		}
	}
	return nil
}

func (s *Sync) deleteMirrorSession(
	ctx context.Context, tx *sql.Tx, sessionID string,
) error {
	for _, stmt := range []string{
		`DELETE FROM pinned_messages WHERE session_id = ?`,
		`DELETE FROM starred_sessions WHERE session_id = ?`,
		`DELETE FROM secret_findings WHERE session_id = ?`,
		`DELETE FROM tool_result_events WHERE session_id = ?`,
		`DELETE FROM tool_calls WHERE session_id = ?`,
		`DELETE FROM usage_events WHERE session_id = ?`,
		`DELETE FROM messages WHERE session_id = ?`,
		`DELETE FROM sessions WHERE id = ?`,
	} {
		if err := s.execMutation(ctx, tx, stmt, sessionID); err != nil {
			return fmt.Errorf("deleting hard-deleted duckdb session %s: %w", sessionID, err)
		}
	}
	return nil
}

func (s *Sync) execMutation(
	ctx context.Context, exec duckMutationExecutor, stmt string, args ...any,
) error {
	_, err := exec.ExecContext(ctx, stmt, args...)
	return err
}

type duckMutationExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Sync) upsertSession(
	ctx context.Context, exec duckMutationExecutor, sess db.Session, fingerprint string,
) error {
	query := `
		INSERT INTO sessions (
			id, project, machine, agent,
			agent_label, entrypoint,
			first_message, display_name, session_name, started_at, ended_at,
			message_count, user_message_count,
			file_path, file_size, file_mtime, file_inode, file_device,
			file_hash, local_modified_at, transcript_revision,
			parent_session_id,
			relationship_type, total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens, is_automated,
			tool_failure_signal_count, tool_retry_count, edit_churn_count,
			consecutive_failure_max, outcome, outcome_confidence,
			ended_with_role, final_failure_streak, signals_pending_since,
			compaction_count, mid_task_compaction_count,
			context_pressure_max, health_score, health_grade,
			has_tool_calls, has_context_data,
			quality_signal_version, short_prompt_count, unstructured_start,
			missing_success_criteria_count, missing_verification_count,
			duplicate_prompt_count, no_code_context_count,
			runaway_tool_loop_count, data_version,
			cwd, git_branch, source_session_id, source_version, transcript_fidelity,
			parser_malformed_lines, is_truncated, deleted_at, created_at,
			termination_status, secret_leak_count, secrets_rules_version,
			agentsview_push_fingerprint
		) VALUES (
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?
		)`
	query += `
		ON CONFLICT(id) DO UPDATE SET
			project = excluded.project,
			machine = excluded.machine,
			agent = excluded.agent,
			agent_label = excluded.agent_label,
			entrypoint = excluded.entrypoint,
			first_message = excluded.first_message,
			display_name = excluded.display_name,
			session_name = excluded.session_name,
			started_at = excluded.started_at,
			ended_at = excluded.ended_at,
			message_count = excluded.message_count,
			user_message_count = excluded.user_message_count,
			file_path = excluded.file_path,
			file_size = excluded.file_size,
			file_mtime = excluded.file_mtime,
			file_inode = excluded.file_inode,
			file_device = excluded.file_device,
			file_hash = excluded.file_hash,
			local_modified_at = excluded.local_modified_at,
			transcript_revision = excluded.transcript_revision,
			parent_session_id = excluded.parent_session_id,
			relationship_type = excluded.relationship_type,
			total_output_tokens = excluded.total_output_tokens,
			peak_context_tokens = excluded.peak_context_tokens,
			has_total_output_tokens = excluded.has_total_output_tokens,
			has_peak_context_tokens = excluded.has_peak_context_tokens,
			is_automated = excluded.is_automated,
			tool_failure_signal_count = excluded.tool_failure_signal_count,
			tool_retry_count = excluded.tool_retry_count,
			edit_churn_count = excluded.edit_churn_count,
			consecutive_failure_max = excluded.consecutive_failure_max,
			outcome = excluded.outcome,
			outcome_confidence = excluded.outcome_confidence,
			ended_with_role = excluded.ended_with_role,
			final_failure_streak = excluded.final_failure_streak,
			signals_pending_since = excluded.signals_pending_since,
			compaction_count = excluded.compaction_count,
			mid_task_compaction_count = excluded.mid_task_compaction_count,
			context_pressure_max = excluded.context_pressure_max,
			health_score = excluded.health_score,
			health_grade = excluded.health_grade,
			has_tool_calls = excluded.has_tool_calls,
			has_context_data = excluded.has_context_data,
			quality_signal_version = excluded.quality_signal_version,
			short_prompt_count = excluded.short_prompt_count,
			unstructured_start = excluded.unstructured_start,
			missing_success_criteria_count = excluded.missing_success_criteria_count,
			missing_verification_count = excluded.missing_verification_count,
			duplicate_prompt_count = excluded.duplicate_prompt_count,
			no_code_context_count = excluded.no_code_context_count,
			runaway_tool_loop_count = excluded.runaway_tool_loop_count,
			data_version = excluded.data_version,
			cwd = excluded.cwd,
			git_branch = excluded.git_branch,
			source_session_id = excluded.source_session_id,
			source_version = excluded.source_version,
			transcript_fidelity = excluded.transcript_fidelity,
			parser_malformed_lines = excluded.parser_malformed_lines,
			is_truncated = excluded.is_truncated,
			deleted_at = excluded.deleted_at,
			created_at = excluded.created_at,
			termination_status = excluded.termination_status,
			secret_leak_count = excluded.secret_leak_count,
			secrets_rules_version = excluded.secrets_rules_version,
			agentsview_push_fingerprint = excluded.agentsview_push_fingerprint`

	args := sessionInsertArgs(sess, s.machine, fingerprint)
	if err := s.execMutation(ctx, exec, query, args...); err != nil {
		return fmt.Errorf("writing duckdb session %s: %w", sess.ID, err)
	}
	return nil
}

func sessionInsertArgs(sess db.Session, machine, fingerprint string) []any {
	return []any{
		sess.ID, sess.Project, machine, sess.Agent,
		sess.AgentLabel, sess.Entrypoint,
		nilString(sess.FirstMessage), nilString(sess.DisplayName),
		nilString(sess.SessionName),
		nilTime(sess.StartedAt), nilTime(sess.EndedAt),
		sess.MessageCount, sess.UserMessageCount,
		nilString(sess.FilePath), sess.FileSize, sess.FileMtime,
		sess.FileInode, sess.FileDevice, nilString(sess.FileHash),
		nilTime(sess.LocalModifiedAt), transcriptRevisionValue(sess.TranscriptRevision),
		nilString(sess.ParentSessionID),
		sess.RelationshipType, sess.TotalOutputTokens,
		sess.PeakContextTokens, sess.HasTotalOutputTokens,
		sess.HasPeakContextTokens, sess.IsAutomated,
		sess.ToolFailureSignalCount, sess.ToolRetryCount,
		sess.EditChurnCount, sess.ConsecutiveFailureMax,
		sess.Outcome, sess.OutcomeConfidence,
		sess.EndedWithRole, sess.FinalFailureStreak,
		nilString(sess.SignalsPendingSince),
		sess.CompactionCount, sess.MidTaskCompactionCount,
		sess.ContextPressureMax, sess.HealthScore,
		nilString(sess.HealthGrade), sess.HasToolCalls,
		sess.HasContextData,
		sess.QualitySignalVersion, sess.ShortPromptCount,
		sess.UnstructuredStart, sess.MissingSuccessCriteriaCount,
		sess.MissingVerificationCount, sess.DuplicatePromptCount,
		sess.NoCodeContextCount, sess.RunawayToolLoopCount,
		sess.DataVersion,
		sess.Cwd, sess.GitBranch, sess.SourceSessionID,
		sess.SourceVersion, sess.TranscriptFidelity, sess.ParserMalformedLines,
		sess.IsTruncated, nilTime(sess.DeletedAt),
		timeValue(sess.CreatedAt), nilString(sess.TerminationStatus),
		sess.SecretLeakCount, sess.SecretsRulesVersion,
		nilEmpty(fingerprint),
	}
}

func insertMessages(
	ctx context.Context, exec duckMutationExecutor, msgs []db.Message,
) error {
	for _, m := range msgs {
		if _, err := exec.ExecContext(ctx, `
			INSERT INTO messages (
				id, session_id, ordinal, role, content, thinking_text,
				timestamp, has_thinking, has_tool_use, content_length,
				is_system, model, token_usage, context_tokens, output_tokens,
				has_context_tokens, has_output_tokens, claude_message_id,
				claude_request_id, source_type, source_subtype, source_uuid,
				source_parent_uuid, is_sidechain, is_compact_boundary
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.SessionID, m.Ordinal, m.Role, m.Content,
			m.ThinkingText, timeValue(m.Timestamp),
			m.HasThinking, m.HasToolUse, m.ContentLength,
			m.IsSystem, m.Model, string(m.TokenUsage),
			m.ContextTokens, m.OutputTokens,
			m.HasContextTokens, m.HasOutputTokens,
			m.ClaudeMessageID, m.ClaudeRequestID,
			m.SourceType, m.SourceSubtype, m.SourceUUID,
			m.SourceParentUUID, m.IsSidechain, m.IsCompactBoundary,
		); err != nil {
			return fmt.Errorf("inserting duckdb message %s/%d: %w", m.SessionID, m.Ordinal, err)
		}
	}
	return nil
}

func (s *Sync) replaceToolRows(
	ctx context.Context, exec duckMutationExecutor, sessionID string, msgs []db.Message,
) error {
	if err := s.execMutation(ctx, exec,
		`DELETE FROM tool_result_events WHERE session_id = ?`,
		sessionID,
	); err != nil {
		return fmt.Errorf("clearing duckdb tool_result_events for %s: %w", sessionID, err)
	}
	if err := s.execMutation(ctx, exec,
		`DELETE FROM tool_calls WHERE session_id = ?`,
		sessionID,
	); err != nil {
		return fmt.Errorf("clearing duckdb tool_calls for %s: %w", sessionID, err)
	}
	if err := insertToolCalls(ctx, exec, msgs); err != nil {
		return err
	}
	if err := insertToolResultEvents(ctx, exec, msgs); err != nil {
		return err
	}
	return nil
}

func insertToolCalls(
	ctx context.Context, exec duckMutationExecutor, msgs []db.Message,
) error {
	for _, m := range msgs {
		for i, tc := range m.ToolCalls {
			if _, err := exec.ExecContext(ctx, `
				INSERT INTO tool_calls (
					message_id, session_id, tool_name, category,
					call_index, tool_use_id, input_json, skill_name,
					result_content_length, result_content,
					subagent_session_id, file_path
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				m.ID, m.SessionID, tc.ToolName, tc.Category,
				i, tc.ToolUseID, nilEmpty(tc.InputJSON),
				nilEmpty(tc.SkillName), nilZero(tc.ResultContentLength),
				nilEmpty(tc.ResultContent), nilEmpty(tc.SubagentSessionID),
				nilEmpty(tc.FilePath),
			); err != nil {
				return fmt.Errorf("inserting duckdb tool_call %s/%d/%d: %w",
					m.SessionID, m.Ordinal, i, err)
			}
		}
	}
	return nil
}

func insertToolResultEvents(
	ctx context.Context, exec duckMutationExecutor, msgs []db.Message,
) error {
	for _, m := range msgs {
		for i, tc := range m.ToolCalls {
			for _, ev := range tc.ResultEvents {
				if _, err := exec.ExecContext(ctx, `
					INSERT INTO tool_result_events (
						session_id, tool_call_message_ordinal, call_index,
						tool_use_id, agent_id, subagent_session_id,
						source, status, content, content_length,
						timestamp, event_index
					) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					m.SessionID, m.Ordinal, i,
					nilEmpty(ev.ToolUseID), nilEmpty(ev.AgentID),
					nilEmpty(ev.SubagentSessionID), ev.Source, ev.Status,
					ev.Content, ev.ContentLength, timeValue(ev.Timestamp),
					ev.EventIndex,
				); err != nil {
					return fmt.Errorf("inserting duckdb tool_result_event %s/%d/%d: %w",
						m.SessionID, m.Ordinal, i, err)
				}
			}
		}
	}
	return nil
}

func (s *Sync) replaceUsageEvents(
	ctx context.Context, exec duckMutationExecutor, sessionID string,
) error {
	events, err := s.local.GetUsageEvents(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := s.execMutation(ctx, exec,
		`DELETE FROM usage_events WHERE session_id = ?`,
		sessionID,
	); err != nil {
		return fmt.Errorf("clearing duckdb usage_events for %s: %w", sessionID, err)
	}
	for _, ev := range events {
		if err := insertUsageEvent(ctx, exec, ev); err != nil {
			return fmt.Errorf("inserting duckdb usage_event %s: %w", sessionID, err)
		}
	}
	return nil
}

func insertUsageEvent(
	ctx context.Context, exec duckMutationExecutor, ev db.UsageEvent,
) error {
	ordinal, cost, occurredAt := usageEventNullableValues(ev)
	if _, err := exec.ExecContext(ctx, `
		INSERT INTO usage_events (
			id, session_id, message_ordinal, source, model,
			input_tokens, output_tokens,
			cache_creation_input_tokens, cache_read_input_tokens,
			reasoning_tokens, cost_usd, cost_status, cost_source,
			occurred_at, dedup_key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.SessionID, ordinal, ev.Source, ev.Model,
		ev.InputTokens, ev.OutputTokens,
		ev.CacheCreationInputTokens, ev.CacheReadInputTokens,
		ev.ReasoningTokens, cost, ev.CostStatus,
		ev.CostSource, occurredAt, ev.DedupKey,
	); err != nil {
		return err
	}
	return nil
}

func (s *Sync) bulkInsertCursorUsageEvents(
	ctx context.Context, tx *sql.Tx, events []db.CursorUsageEvent,
) error {
	if len(events) == 0 {
		return nil
	}
	const cursorBatch = 100
	for i := 0; i < len(events); i += cursorBatch {
		end := min(i+cursorBatch, len(events))
		batch := events[i:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO cursor_usage_events (
			occurred_at, model, kind,
			input_tokens, output_tokens,
			cache_write_tokens, cache_read_tokens,
			charged_cents, cursor_token_fee,
			user_id, user_email, is_headless, dedup_key
		) VALUES `)
		args := make([]any, 0, len(batch)*13)
		for j, ev := range batch {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
			occurredAt, ok := parseTimestamp(ev.OccurredAt)
			if !ok {
				return fmt.Errorf("parsing cursor usage occurred_at %q", ev.OccurredAt)
			}
			args = append(args,
				occurredAt,
				db.SanitizeUTF8(ev.Model),
				db.SanitizeUTF8(ev.Kind),
				ev.InputTokens,
				ev.OutputTokens,
				ev.CacheWriteTokens,
				ev.CacheReadTokens,
				ev.ChargedCents,
				ev.CursorTokenFee,
				db.SanitizeUTF8(ev.UserID),
				db.SanitizeUTF8(ev.UserEmail),
				ev.IsHeadless,
				db.SanitizeUTF8(ev.DedupKey),
			)
		}
		b.WriteString(` ON CONFLICT DO NOTHING`)
		if err := s.execMutation(ctx, tx, b.String(), args...); err != nil {
			return fmt.Errorf("bulk inserting duckdb cursor_usage_events: %w", err)
		}
	}
	return nil
}

func usageEventNullableValues(ev db.UsageEvent) (any, any, any) {
	var ordinal any
	if ev.MessageOrdinal != nil {
		ordinal = *ev.MessageOrdinal
	}
	var cost any
	if ev.CostUSD != nil {
		cost = *ev.CostUSD
	}
	var occurredAt any
	if ev.OccurredAt != "" {
		occurredAt = timeValue(ev.OccurredAt)
	}
	return ordinal, cost, occurredAt
}

func (s *Sync) replaceSecretFindings(
	ctx context.Context, exec duckMutationExecutor, sessionID string,
) error {
	findings, err := s.local.SessionSecretFindings(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, f := range findings {
		if _, err := exec.ExecContext(ctx, `
			INSERT INTO secret_findings (
				session_id, rule_name, confidence, location_kind,
				message_ordinal, call_index, event_index,
				match_start, match_end, match_index,
				redacted_match, rules_version, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, current_timestamp)`,
			f.SessionID, f.RuleName, f.Confidence, f.LocationKind,
			f.MessageOrdinal, f.CallIndex, f.EventIndex,
			f.MatchStart, f.MatchEnd, f.MatchIndex,
			f.RedactedMatch, f.RulesVersion,
		); err != nil {
			return fmt.Errorf("inserting duckdb secret_finding %s: %w", sessionID, err)
		}
	}
	return nil
}

func (s *Sync) replacePinnedMessages(
	ctx context.Context, exec duckMutationExecutor, sessionID string,
) error {
	pins, err := s.local.ListPinnedMessages(ctx, sessionID, "")
	if err != nil {
		return err
	}
	return insertPinnedMessages(ctx, exec, pins)
}

// insertPinnedMessages inserts pins already loaded from the local archive.
// It is the shared write side for both the single-session push path
// (replacePinnedMessages, one local query per pushed session) and the
// curation refresh (replaceCuration, one batched local query for the
// curation-sized pinned session set).
func insertPinnedMessages(
	ctx context.Context, exec duckMutationExecutor, pins []db.PinnedMessage,
) error {
	for _, p := range pins {
		if _, err := exec.ExecContext(ctx, `
			INSERT INTO pinned_messages (
				id, session_id, message_id, ordinal, note, created_at
			) VALUES (?, ?, ?, ?, ?, ?)`,
			p.ID, p.SessionID, p.MessageID, p.Ordinal,
			p.Note, timeValue(p.CreatedAt),
		); err != nil {
			return fmt.Errorf("inserting duckdb pinned_message %s/%d: %w",
				p.SessionID, p.MessageID, err)
		}
	}
	return nil
}

func nilString(value *string) any {
	if value == nil || *value == "" {
		return nil
	}
	return *value
}

func transcriptRevisionValue(value *string) string {
	if value == nil || *value == "" {
		return "0"
	}
	return *value
}

func nilEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nilZero(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func nilTime(value *string) any {
	if value == nil || *value == "" {
		return nil
	}
	return timeValue(*value)
}

func timeValue(value string) any {
	if value == "" {
		return nil
	}
	if t, ok := parseTimestamp(value); ok {
		return t
	}
	return value
}

func parseTimestamp(value string) (time.Time, bool) {
	candidates := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	value = strings.TrimSpace(value)
	for _, layout := range candidates {
		t, err := time.Parse(layout, value)
		if err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
