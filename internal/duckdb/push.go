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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/uptrace/bun"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
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

	rows, bands, err := db.CanonicalModelPricingRows(prices)
	if err != nil {
		return fmt.Errorf("converting duckdb pricing rows: %w", err)
	}
	if err := db.UpsertModelPricingRows(ctx, s.bun, rows, bands); err != nil {
		return fmt.Errorf("syncing duckdb pricing rows: %w", err)
	}
	return nil
}

func (s *Sync) listDuckModelPricing(ctx context.Context) ([]db.ModelPricing, error) {
	rows, err := s.bun.QueryContext(
		ctx,
		`SELECT p.model_pattern, p.input_microdollars_per_mtok,
			p.output_microdollars_per_mtok, p.cache_creation_microdollars_per_mtok,
			p.cache_read_microdollars_per_mtok, p.updated_at,
			b.above_input_tokens, b.input_microdollars_per_mtok,
			b.output_microdollars_per_mtok,
			b.cache_creation_microdollars_per_mtok,
			b.cache_read_microdollars_per_mtok, b.updated_at
		 FROM model_pricing p
		 LEFT JOIN model_pricing_bands b ON b.model_pattern = p.model_pattern
		 ORDER BY p.model_pattern, b.above_input_tokens`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing duckdb pricing: %w", err)
	}
	defer rows.Close()

	out := make([]db.ModelPricing, 0)
	byPattern := make(map[string]int)
	for rows.Next() {
		var p db.ModelPricing
		var threshold, input, output, cacheCreation, cacheRead sql.NullInt64
		var bandUpdatedAt sql.NullString
		if err := rows.Scan(
			&p.ModelPattern,
			&p.InputPerMTok,
			&p.OutputPerMTok,
			&p.CacheCreationPerMTok,
			&p.CacheReadPerMTok,
			&p.UpdatedAt,
			&threshold,
			&input,
			&output,
			&cacheCreation,
			&cacheRead,
			&bandUpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb pricing: %w", err)
		}
		i, exists := byPattern[p.ModelPattern]
		if !exists {
			i = len(out)
			byPattern[p.ModelPattern] = i
			out = append(out, p)
		}
		if threshold.Valid {
			aboveInputTokens, err := safecast.Convert[int](threshold.Int64)
			if err != nil {
				return nil, fmt.Errorf(
					"converting duckdb pricing threshold for %q: %w",
					p.ModelPattern, err,
				)
			}
			out[i].Bands = append(out[i].Bands, db.PricingBand{
				AboveInputTokens:     aboveInputTokens,
				InputPerMTok:         money.Money{Microdollars: input.Int64},
				OutputPerMTok:        money.Money{Microdollars: output.Int64},
				CacheCreationPerMTok: money.Money{Microdollars: cacheCreation.Int64},
				CacheReadPerMTok:     money.Money{Microdollars: cacheRead.Int64},
				UpdatedAt:            bandUpdatedAt.String,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb pricing: %w", err)
	}
	return out, nil
}

// syncCursorUsageEvents appends the cursor admin usage rows the mirror has
// not consumed yet, tracked by a high-water id in mirror sync_metadata
// (cursorUsageMaxIDMetadataKey). The local table is append-only with a
// monotonic id, so each push's work is bounded by the appended batch, not
// total history — automatic watcher pushes run this on every filesystem
// batch. A fresh rebuild file has no metadata, so the first sync after a
// rebuild loads the full history; if a crash lands between the insert
// commit and the high-water record, the overlap is re-read and deduped by
// the mirror's unique dedup_key index (ON CONFLICT DO NOTHING).
func (s *Sync) syncCursorUsageEvents(ctx context.Context) error {
	// Cursor admin rows are global and unattributed, so project-filtered pushes
	// cannot sync them honestly.
	if s.isFiltered() {
		return nil
	}

	stored, err := readMetadataKey(ctx, s.bun, cursorUsageMaxIDMetadataKey)
	if err != nil {
		return err
	}
	var sinceID int64
	if stored != "" {
		sinceID, err = strconv.ParseInt(stored, 10, 64)
		if err != nil {
			return fmt.Errorf(
				"parsing cursor usage high-water id %q: %w", stored, err)
		}
	}

	events, err := s.local.GetCursorUsageEvents(ctx, sinceID)
	if err != nil {
		return fmt.Errorf("loading local cursor usage events: %w", err)
	}
	if len(events) == 0 {
		return nil
	}

	rows, err := db.CanonicalCursorUsageEventRows(events)
	if err != nil {
		return fmt.Errorf("converting duckdb cursor usage rows: %w", err)
	}
	tx, err := s.bun.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning duckdb cursor usage sync: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := db.AppendCursorUsageEventRows(ctx, tx, rows); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing duckdb cursor usage sync: %w", err)
	}

	maxID := sinceID
	for _, ev := range events {
		if ev.ID > maxID {
			maxID = ev.ID
		}
	}
	return recordMetadataKey(
		ctx, s.bun, cursorUsageMaxIDMetadataKey,
		strconv.FormatInt(maxID, 10),
	)
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
	refreshSessionIDs []string,
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
		if present && len(refreshSessionIDs) == 0 {
			return revision, nil
		}
		if !present {
			fullPublication = true
		}
	}

	observations, snapshots, delta, err := s.loadIdentityPublicationScope(
		ctx, fullPublication, priorRevision, revision, refreshSessionIDs,
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
		fullPublication, delta, observations, snapshots, refreshSessionIDs,
	); err != nil {
		return 0, err
	}
	return revision, nil
}

func (s *Sync) identityArchivePresent(
	ctx context.Context, archiveID string,
) (bool, error) {
	var present bool
	if err := s.bun.QueryRowContext(ctx, `
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
	refreshSessionIDs []string,
) (
	observations, snapshots []export.ProjectIdentityObservation,
	delta db.ProjectIdentityPublicationDelta, err error,
) {
	if !fullPublication {
		delta, err = s.local.LoadProjectIdentityPublicationDelta(
			ctx, priorRevision, revision, s.projects, s.excludeProjects,
		)
		if err != nil {
			return nil, nil, delta, err
		}
		observations = delta.Observations
		snapshots = delta.Snapshots
	} else {
		observations, err = s.local.ListProjectIdentityObservations(ctx, nil)
		if err != nil {
			return nil, nil, delta, fmt.Errorf(
				"loading project identity observations: %w", err,
			)
		}
		observations = filterIdentityScope(
			observations, s.projects, s.excludeProjects,
		)
		snapshots, err =
			s.local.ListPublishableSessionProjectIdentitySnapshots(
				ctx, nil, s.projects, s.excludeProjects,
			)
		if err != nil {
			return nil, nil, delta, fmt.Errorf(
				"loading session project identity snapshots: %w", err,
			)
		}
	}
	if len(refreshSessionIDs) > 0 {
		refreshSnapshots, loadErr :=
			s.local.ListPublishableSessionProjectIdentitySnapshots(
				ctx, refreshSessionIDs, s.projects, s.excludeProjects,
			)
		if loadErr != nil {
			return nil, nil, delta, fmt.Errorf(
				"loading refreshed session project identity snapshots: %w",
				loadErr,
			)
		}
		snapshots = mergeProjectIdentitySnapshots(snapshots, refreshSnapshots)
	}
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
	refreshSessionIDs []string,
) error {
	tx, err := s.bun.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning duckdb project identity sync: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if err := upsertSourceArchiveScope(
		ctx, tx, archiveID, archiveSalt,
	); err != nil {
		return err
	}
	execDelta := func(stmt string, args ...any) error {
		return s.execMutation(ctx, tx, stmt, args...)
	}
	if fullPublication {
		// Rebuild the archive from the mirror's own rows so a filtered
		// publication removes stale out-of-scope identity without carrying
		// excluded-project tombstones.
		if err := deleteProjectIdentityArchive(
			execDelta, archiveID,
		); err != nil {
			return err
		}
	} else if err := deleteProjectIdentityDelta(
		execDelta, archiveID, databaseGeneration,
		delta.ObservationDeletes, delta.SnapshotDeletes,
	); err != nil {
		return err
	}
	if err := deleteSessionProjectIdentitySnapshotsBySessionID(
		execDelta, archiveID, refreshSessionIDs,
	); err != nil {
		return err
	}
	for _, obs := range observations {
		obs.SourceArchiveID = archiveID
		obs.SourceArchiveSalt = archiveSalt
		obs = export.SanitizeStoredProjectIdentityObservation(obs)
		if err := upsertProjectIdentityObservation(
			ctx, tx,
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
		ctx, tx, archiveID, databaseGeneration, snapshots,
	); err != nil {
		return fmt.Errorf("syncing duckdb session project identity snapshots: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing duckdb project identity sync: %w", err)
	}
	return nil
}

func mergeProjectIdentitySnapshots(
	base, refresh []export.ProjectIdentityObservation,
) []export.ProjectIdentityObservation {
	merged := make(map[string]export.ProjectIdentityObservation, len(base)+len(refresh))
	for _, snapshot := range base {
		merged[snapshot.SessionID] = snapshot
	}
	for _, snapshot := range refresh {
		merged[snapshot.SessionID] = snapshot
	}
	out := make([]export.ProjectIdentityObservation, 0, len(merged))
	for _, snapshot := range merged {
		out = append(out, snapshot)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SessionID < out[j].SessionID
	})
	return out
}

func duckFallbackPricingRows() []db.ModelPricing {
	src := pricingpkg.FallbackPricing()
	out := make([]db.ModelPricing, len(src))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i, p := range src {
		bands := make([]db.PricingBand, len(p.Bands))
		for j, band := range p.Bands {
			bands[j] = db.PricingBand{
				AboveInputTokens:     band.AboveInputTokens,
				InputPerMTok:         band.InputPerMTok,
				OutputPerMTok:        band.OutputPerMTok,
				CacheCreationPerMTok: band.CacheCreationPerMTok,
				CacheReadPerMTok:     band.CacheReadPerMTok,
				UpdatedAt:            now,
			}
		}
		out[i] = db.ModelPricing{
			ModelPattern:         p.ModelPattern,
			InputPerMTok:         p.InputPerMTok,
			OutputPerMTok:        p.OutputPerMTok,
			CacheCreationPerMTok: p.CacheCreationPerMTok,
			CacheReadPerMTok:     p.CacheReadPerMTok,
			UpdatedAt:            now,
			Bands:                bands,
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
	if err := s.withDuckTx(ctx, "apply session deletion delta", func(tx bun.Tx) error {
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
// side clears both tables for sessions in this source archive, so removed
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

	err = s.withDuckTx(ctx, "replace curation rows", func(tx bun.Tx) error {
		for _, id := range residentPinned {
			if err := insertPinnedMessages(ctx, tx, written.pinsBySession[id]); err != nil {
				return err
			}
		}
		for _, id := range written.starred {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO starred_sessions (session_id, created_at)
				 VALUES (?, current_timestamp)
				 ON CONFLICT (session_id) DO NOTHING`,
				id,
			); err != nil {
				return fmt.Errorf("syncing starred session %s: %w", id, err)
			}
		}
		pinDelete, pinArgs := stalePinnedMessageDelete(written.pinsBySession)
		if err := s.execMutation(ctx, tx, pinDelete, pinArgs...); err != nil {
			return fmt.Errorf("clearing stale duckdb pinned_messages: %w", err)
		}
		starDelete, starArgs := staleStarredSessionDelete(written.starred)
		if err := s.execMutation(ctx, tx, starDelete, starArgs...); err != nil {
			return fmt.Errorf("clearing stale duckdb starred_sessions: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return fingerprint, nil
}

func stalePinnedMessageDelete(pinsBySession map[string][]db.PinnedMessage) (string, []any) {
	var values []string
	var args []any
	for _, pins := range pinsBySession {
		for _, pin := range pins {
			values = append(values, "(?, ?)")
			args = append(args, pin.SessionID, pin.Ordinal)
		}
	}
	query := `DELETE FROM pinned_messages AS pin
		WHERE pin.session_id IN (SELECT id FROM sessions)`
	if len(values) > 0 {
		query += ` AND NOT EXISTS (
			SELECT 1 FROM (VALUES ` + strings.Join(values, ", ") + `)
				AS current_pin(session_id, ordinal)
			WHERE current_pin.session_id = pin.session_id
				AND current_pin.ordinal = pin.ordinal
		)`
	}
	return query, args
}

func staleStarredSessionDelete(sessionIDs []string) (string, []any) {
	query := `DELETE FROM starred_sessions
		WHERE session_id IN (SELECT id FROM sessions)`
	if len(sessionIDs) == 0 {
		return query, nil
	}
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, sessionID := range sessionIDs {
		placeholders[i] = "?"
		args[i] = sessionID
	}
	return query + ` AND session_id NOT IN (` +
		strings.Join(placeholders, ", ") + `)`, args
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
	stored, err := readMetadataKey(ctx, s.bun, curationFingerprintMetadataKey)
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
		ctx, s.bun, curationFingerprintMetadataKey, written,
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
	ctx context.Context, tx bun.IDB, sess db.Session,
) (int, error) {
	snapshot, err := s.local.ReadSessionReplicationSnapshot(ctx, sess.ID)
	if err != nil {
		return 0, fmt.Errorf("reading local replication snapshot for %s: %w", sess.ID, err)
	}
	s.stampReplicationSnapshot(&snapshot)
	fingerprint, err := db.CanonicalSessionReplicationFingerprint(snapshot)
	if err != nil {
		return 0, fmt.Errorf("fingerprinting duckdb session %s: %w", sess.ID, err)
	}
	if err := s.upsertSession(ctx, tx, snapshot.Session, fingerprint); err != nil {
		return 0, err
	}
	messageRows, callRows, resultRows, err := db.CanonicalMessageRows(snapshot.Messages)
	if err != nil {
		return 0, err
	}
	for i := range messageRows {
		if snapshot.Messages[i].ID <= 0 {
			continue
		}
		id := snapshot.Messages[i].ID
		messageRows[i].ID = &id
	}
	usageRows, err := db.CanonicalUsageEventRows(snapshot.UsageEvents)
	if err != nil {
		return 0, err
	}
	// DuckDB's unique indexes require parent messages to be removed before
	// reinserting a tool call with the same portable key in one transaction.
	// Clear the mirror-only dependents in that order, then let the shared Bun
	// writers own every replacement row.
	if err := s.replaceSessionDependents(ctx, tx, sess.ID); err != nil {
		return 0, err
	}
	if err := db.AppendMessageRows(ctx, tx, sess.ID, messageRows); err != nil {
		return 0, err
	}
	if err := db.ReplaceToolRows(ctx, tx, sess.ID, callRows, resultRows); err != nil {
		return 0, err
	}
	if err := db.ReplaceUsageEventRows(ctx, tx, sess.ID, usageRows); err != nil {
		return 0, err
	}
	if err := db.ReplaceSecretFindingRows(
		ctx, tx, sess.ID, db.CanonicalSecretFindingRows(snapshot.SecretFindings),
	); err != nil {
		return 0, err
	}
	if err := s.execMutation(ctx, tx,
		`DELETE FROM pinned_messages WHERE session_id = ?`, sess.ID,
	); err != nil {
		return 0, fmt.Errorf("clearing duckdb pinned messages for %s: %w", sess.ID, err)
	}
	if err := insertPinnedMessages(ctx, tx, snapshot.PinnedMessages); err != nil {
		return 0, err
	}
	return len(snapshot.Messages), nil
}

func (s *Sync) stampReplicationSnapshot(snapshot *db.SessionReplicationSnapshot) {
	snapshot.Session.Machine = mirroredSessionMachine(snapshot.Session, s.machine)
	snapshot.Session.SourceArchiveID = s.archiveID
	snapshot.Session.SourceDatabaseGeneration = s.databaseGeneration
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
	ctx context.Context, tx duckMutationExecutor, sessionID string,
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
	ctx context.Context, store bun.IDB, sess db.Session, fingerprint string,
) error {
	sess.Machine = mirroredSessionMachine(sess, s.machine)
	sess.SourceArchiveID = s.archiveID
	sess.SourceDatabaseGeneration = s.databaseGeneration
	row, err := db.CanonicalSessionRow(sess)
	if err != nil {
		return fmt.Errorf("converting duckdb session %s: %w", sess.ID, err)
	}
	if err := db.UpsertSessionRow(ctx, store, row); err != nil {
		return fmt.Errorf("writing duckdb session %s: %w", sess.ID, err)
	}
	if _, err := store.NewUpdate().Model((*duckSessionFingerprintRow)(nil)).
		Set("agentsview_push_fingerprint = ?", nilEmpty(fingerprint)).
		Where("id = ?", sess.ID).Exec(ctx); err != nil {
		return fmt.Errorf("writing duckdb session fingerprint %s: %w", sess.ID, err)
	}
	return nil
}

type duckSessionFingerprintRow struct {
	bun.BaseModel `bun:"table:sessions"`
	ID            string `bun:"id,pk"`
}

// mirroredSessionMachine preserves the source archive's machine identity.
// "local" and empty are local-only sentinels, so only those use the machine
// configured for this mirror push.
func mirroredSessionMachine(sess db.Session, fallbackMachine string) string {
	if sess.Machine != "" && sess.Machine != "local" {
		return sess.Machine
	}
	return fallbackMachine
}

// insertPinnedMessages inserts pins already loaded from the local archive.
// It is the shared write side for both a session replication snapshot and the
// batched curation refresh.
func insertPinnedMessages(
	ctx context.Context, exec duckMutationExecutor, pins []db.PinnedMessage,
) error {
	for _, p := range pins {
		if _, err := exec.ExecContext(ctx, `
			UPDATE pinned_messages SET id = -id
			WHERE id = ? AND id > 0
				AND (session_id != ? OR ordinal != ?)`,
			p.ID, p.SessionID, p.Ordinal,
		); err != nil {
			return fmt.Errorf("reconciling duckdb pinned_message id %d: %w", p.ID, err)
		}
		if _, err := exec.ExecContext(ctx, `
			INSERT INTO pinned_messages (
				id, session_id, message_id, ordinal, note, created_at
			) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (session_id, ordinal) DO UPDATE SET
				id = excluded.id,
				message_id = excluded.message_id,
				note = excluded.note,
				created_at = excluded.created_at`,
			p.ID, p.SessionID, p.MessageID, p.Ordinal,
			p.Note, timeValue(p.CreatedAt),
		); err != nil {
			return fmt.Errorf("inserting duckdb pinned_message %s/%d: %w",
				p.SessionID, p.MessageID, err)
		}
	}
	return nil
}

func nilEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
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
