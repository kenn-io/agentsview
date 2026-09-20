package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// sessionPushBatchSize bounds how many sessions share one set of insert
// blocks and delete statements.
const sessionPushBatchSize = 100

// idBatchSize bounds IN (...) lists sent to ClickHouse.
const idBatchSize = 500

// dependentTables are the per-session tables replaced on every push, in
// insert order. Deletes run in the same order.
var dependentTables = []string{
	"messages", "tool_calls", "tool_result_events", "usage_events",
	"secret_findings", "pinned_messages",
}

// newPushVersion returns the version stamped on every row a push writes.
// ReplacingMergeTree keeps the highest version per ordering key.
func newPushVersion() uint64 {
	return uint64(time.Now().UnixNano())
}

// PushWithOptions runs one push: it fingerprints the changed window,
// rewrites changed sessions (dependents first, then a version-bounded
// delete, then the session rows), applies local hard deletes, refreshes
// stars and pins, and advances this archive's cursor only when every
// session succeeded.
func (s *Sync) PushWithOptions(
	ctx context.Context, opts storage.PushOptions, onProgress func(storage.PushProgress),
) (storage.PushResult, error) {
	start := time.Now()
	var result storage.PushResult
	if err := s.EnsureSchema(ctx); err != nil {
		return result, err
	}
	conn := s.conn
	if err := CheckDataVersionCompat(ctx, conn); err != nil {
		return result, err
	}
	if onProgress != nil {
		onProgress(storage.PushProgress{Phase: "preparing"})
	}
	version := newPushVersion()
	cutoff := time.Now().UTC().Format(localSyncTimestampLayout)

	meta, err := readMetadata(ctx, conn,
		s.archiveKey(lastPushCutoffKeyBase),
		s.archiveKey(pushScopeKeyBase),
		s.archiveKey(deletionRevisionKeyBase),
		s.archiveKey(identityRevisionKeyBase),
		s.archiveKey(mappingRevisionKeyBase),
	)
	if err != nil {
		return result, err
	}
	storedCutoff := meta[s.archiveKey(lastPushCutoffKeyBase)]
	storedScope := meta[s.archiveKey(pushScopeKeyBase)]
	storedDeletion, _ := strconv.ParseInt(meta[s.archiveKey(deletionRevisionKeyBase)], 10, 64)
	storedIdentity, _ := strconv.ParseInt(meta[s.archiveKey(identityRevisionKeyBase)], 10, 64)
	storedMapping, _ := strconv.ParseInt(meta[s.archiveKey(mappingRevisionKeyBase)], 10, 64)

	full, reason := s.decideFull(opts, storedCutoff, storedScope)
	localDeletion, err := s.local.SessionDeletionPublicationRevision(ctx)
	if err != nil {
		return result, fmt.Errorf("reading local deletion revision: %w", err)
	}
	if !full && storedDeletion > localDeletion {
		full, reason = true, "local deletion journal was reset"
	}
	result.Full, result.FullReason = full, reason
	if full {
		log.Printf("clickhouse push: full push (%s)", reason)
	}

	if err := s.syncMachineMetadata(ctx, version); err != nil {
		return result, err
	}
	if err := s.syncModelPricing(ctx); err != nil {
		return result, err
	}
	if err := s.syncCursorUsageEvents(ctx); err != nil {
		return result, err
	}
	if s.pricer, err = s.syncUsagePrices(ctx, full); err != nil {
		return result, err
	}
	if !full {
		if err := s.applyDeletionDelta(ctx, storedDeletion, localDeletion, &result); err != nil {
			return result, err
		}
	}

	since := storedCutoff
	if full {
		since = ""
	}
	candidates, err := s.local.ListSessionsForMirrorWindow(ctx, since, nil, nil)
	if err != nil {
		return result, fmt.Errorf("listing sessions for clickhouse push: %w", err)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	inScope, outOfScope := s.partitionPushScope(candidates)
	if err := s.deleteResidentSessions(ctx, sessionIDs(outOfScope), &result); err != nil {
		return result, err
	}

	fingerprints, err := s.sessionFingerprints(ctx, inScope, onProgress)
	if err != nil {
		return result, err
	}
	changed := inScope
	if !full {
		changed, err = s.selectChangedSessions(ctx, inScope, fingerprints, &result)
		if err != nil {
			return result, err
		}
	}

	failed := map[string]struct{}{}
	for offset := 0; offset < len(changed); offset += sessionPushBatchSize {
		end := min(offset+sessionPushBatchSize, len(changed))
		if err := s.pushBatchWithRetry(
			ctx, changed[offset:end], fingerprints, version,
			offset, len(changed), &result, onProgress,
			failed,
		); err != nil {
			return result, err
		}
	}

	if full {
		if err := s.deleteSessionsMissingLocally(ctx, sessionIDs(inScope), &result); err != nil {
			return result, err
		}
	}
	if err := s.refreshCurationIfChanged(ctx, version); err != nil {
		return result, err
	}

	var vectorScope []string
	if opts.ScopeVectorsToChangedSessions {
		vectorScope = sessionIDs(changed)
	}
	result.Vectors, err = s.pushVectors(ctx, full, vectorScope, failed, onProgress)
	if err != nil {
		return result, fmt.Errorf("vector push: %w", err)
	}

	identityRevision := storedIdentity
	mappingRevision := storedMapping
	if result.Errors == 0 {
		identityRevision, err = s.syncProjectIdentityObservations(
			ctx, storedIdentity, full, sessionIDs(changed),
		)
		if err != nil {
			return result, err
		}
		mappingRevision, err = s.syncWorktreeMappings(ctx, storedMapping, full)
		if err != nil {
			return result, err
		}
	} else {
		log.Printf(
			"clickhouse push: skipping identity and mapping refresh after %d session push errors",
			result.Errors,
		)
	}

	if result.Errors == 0 {
		if err := writeMetadata(ctx, conn, map[string]string{
			s.archiveKey(lastPushCutoffKeyBase):   cutoff,
			s.archiveKey(lastPushAtKeyBase):       time.Now().UTC().Format(time.RFC3339),
			s.archiveKey(lastPushMachineKeyBase):  s.machine,
			s.archiveKey(pushScopeKeyBase):        s.scopeString(),
			s.archiveKey(deletionRevisionKeyBase): strconv.FormatInt(localDeletion, 10),
			s.archiveKey(identityRevisionKeyBase): strconv.FormatInt(identityRevision, 10),
			s.archiveKey(mappingRevisionKeyBase):  strconv.FormatInt(mappingRevision, 10),
			schemaVersionKey:                      strconv.Itoa(SchemaVersion),
			sourceDataVersionKey:                  strconv.Itoa(db.CurrentDataVersion()),
		}); err != nil {
			return result, err
		}
	}
	result.Duration = time.Since(start)
	return result, nil
}

func (s *Sync) archiveKey(base string) string {
	return archiveMetadataKey(base, s.archiveID)
}

func (s *Sync) decideFull(opts storage.PushOptions, storedCutoff, storedScope string) (bool, string) {
	switch {
	case opts.Full:
		return true, "requested"
	case storedCutoff == "":
		return true, "first push from this archive"
	case storedScope != s.scopeString():
		return true, "push scope changed"
	}
	return false, ""
}

// partitionPushScope splits window candidates by this Sync's project scope
// in Go. The window is listed without project filters so a session whose
// project moved out of scope is still seen and its mirror rows removed.
func (s *Sync) partitionPushScope(candidates []db.Session) (inScope, outOfScope []db.Session) {
	if !s.isFiltered() {
		return candidates, nil
	}
	for _, sess := range candidates {
		if projectMatchesPushScope(sess.Project, s.projects, s.excludeProjects) {
			inScope = append(inScope, sess)
		} else {
			outOfScope = append(outOfScope, sess)
		}
	}
	return inScope, outOfScope
}

func projectMatchesPushScope(project string, projects, excludeProjects []string) bool {
	if len(projects) > 0 && !slices.Contains(projects, project) {
		return false
	}
	return !slices.Contains(excludeProjects, project)
}

func sessionIDs(sessions []db.Session) []string {
	ids := make([]string, len(sessions))
	for i, sess := range sessions {
		ids[i] = sess.ID
	}
	return ids
}

// selectChangedSessions keeps the candidates whose local fingerprint
// differs from the mirror's. A session missing from the mirror reads back
// as "" and is therefore always pushed, which repairs rows lost to a
// failed earlier push without a separate repair pass.
func (s *Sync) selectChangedSessions(
	ctx context.Context, candidates []db.Session, fingerprints map[string]string,
	result *storage.PushResult,
) ([]db.Session, error) {
	stored, err := s.readMirrorFingerprints(ctx, sessionIDs(candidates))
	if err != nil {
		return nil, err
	}
	changed := make([]db.Session, 0, len(candidates))
	for _, sess := range candidates {
		if fingerprints[sess.ID] != stored[sess.ID] {
			changed = append(changed, sess)
		} else {
			result.SkippedUnchanged++
		}
	}
	return changed, nil
}

func (s *Sync) readMirrorFingerprints(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	for batch := range idBatches(ids) {
		placeholders, args := inArgs(batch)
		if err := func() error {
			rows, err := s.conn.QueryContext(ctx,
				"SELECT id, agentsview_push_fingerprint FROM sessions WHERE id IN ("+placeholders+")",
				args...)
			if err != nil {
				return fmt.Errorf("reading clickhouse fingerprints: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var id, fp string
				if err := rows.Scan(&id, &fp); err != nil {
					return fmt.Errorf("scanning clickhouse fingerprint: %w", err)
				}
				out[id] = fp
			}
			if err := rows.Err(); err != nil {
				return err
			}
			return nil
		}(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// residentSessionIDs reports which ids currently have a session row.
func (s *Sync) residentSessionIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	resident := make(map[string]bool, len(ids))
	for batch := range idBatches(uniqueIDs(ids)) {
		placeholders, args := inArgs(batch)
		if err := func() error {
			rows, err := s.conn.QueryContext(ctx,
				"SELECT id FROM sessions WHERE id IN ("+placeholders+")", args...)
			if err != nil {
				return fmt.Errorf("reading clickhouse resident sessions: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return fmt.Errorf("scanning clickhouse resident session: %w", err)
				}
				resident[id] = true
			}
			if err := rows.Err(); err != nil {
				return err
			}
			return nil
		}(); err != nil {
			return nil, err
		}
	}
	return resident, nil
}

func uniqueIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func idBatches(ids []string) func(func([]string) bool) {
	return func(yield func([]string) bool) {
		for start := 0; start < len(ids); start += idBatchSize {
			end := min(start+idBatchSize, len(ids))
			if !yield(ids[start:end]) {
				return
			}
		}
	}
}

func inArgs(values []string) (string, []any) {
	args := make([]any, len(values))
	for i, v := range values {
		args[i] = v
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(values)), ","), args
}

// deleteResidentSessions removes the given sessions from every table when
// they are currently mirrored, and counts them as stale deletions.
func (s *Sync) deleteResidentSessions(ctx context.Context, ids []string, result *storage.PushResult) error {
	if len(ids) == 0 {
		return nil
	}
	resident, err := s.residentSessionIDs(ctx, ids)
	if err != nil {
		return err
	}
	var remove []string
	for _, id := range uniqueIDs(ids) {
		if resident[id] {
			remove = append(remove, id)
		}
	}
	if err := s.deleteMirrorSessions(ctx, remove); err != nil {
		return err
	}
	result.DeletedStale += len(remove)
	return nil
}

// derivedSessionTables are filled by materialized views, which see inserts but
// not deletes, so removing a session has to clear them explicitly.
var derivedSessionTables = []string{"usage_messages", "terminal_event_snapshots"}

// deleteMirrorSessions removes every row for the given sessions.
func (s *Sync) deleteMirrorSessions(ctx context.Context, ids []string) error {
	for batch := range idBatches(ids) {
		placeholders, args := inArgs(batch)
		for _, table := range slices.Concat(dependentTables, derivedSessionTables, []string{"starred_sessions"}) {
			if _, err := s.conn.ExecContext(ctx,
				"DELETE FROM "+table+" WHERE session_id IN ("+placeholders+")", args...); err != nil {
				return fmt.Errorf("deleting clickhouse %s rows: %w", table, err)
			}
		}
		if _, err := s.conn.ExecContext(ctx,
			"DELETE FROM sessions WHERE id IN ("+placeholders+")", args...); err != nil {
			return fmt.Errorf("deleting clickhouse sessions: %w", err)
		}
	}
	return nil
}

// applyDeletionDelta removes sessions the local archive hard-deleted in
// (after, through]. Tombstones are loaded without project filters because
// a tombstone records the session's last project, which may already be
// out of scope; out-of-scope tombstones are applied only when the session
// is still mirrored.
func (s *Sync) applyDeletionDelta(ctx context.Context, after, through int64, result *storage.PushResult) error {
	if after >= through {
		return nil
	}
	tombstones, err := s.local.LoadSessionDeletionDelta(ctx, after, through, nil, nil)
	if err != nil {
		return fmt.Errorf("loading session deletion delta: %w", err)
	}
	var inScope, outOfScope []string
	for _, t := range tombstones {
		if t.SessionID == "" {
			continue
		}
		if projectMatchesPushScope(t.Project, s.projects, s.excludeProjects) {
			inScope = append(inScope, t.SessionID)
		} else {
			outOfScope = append(outOfScope, t.SessionID)
		}
	}
	inScope = uniqueIDs(inScope)
	if err := s.deleteMirrorSessions(ctx, inScope); err != nil {
		return err
	}
	result.DeletedStale += len(inScope)
	return s.deleteResidentSessions(ctx, outOfScope, result)
}

// deleteSessionsMissingLocally removes this archive's mirror sessions that
// a full push did not see locally.
func (s *Sync) deleteSessionsMissingLocally(ctx context.Context, keep []string, result *storage.PushResult) error {
	rows, err := s.conn.QueryContext(ctx,
		"SELECT id FROM sessions WHERE source_archive_id = ?", s.archiveID)
	if err != nil {
		return fmt.Errorf("listing clickhouse sessions for archive: %w", err)
	}
	defer rows.Close()
	keepSet := make(map[string]bool, len(keep))
	for _, id := range keep {
		keepSet[id] = true
	}
	var remove []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scanning clickhouse archive session: %w", err)
		}
		if !keepSet[id] {
			remove = append(remove, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	remove = uniqueIDs(remove)
	if err := s.deleteMirrorSessions(ctx, remove); err != nil {
		return err
	}
	result.DeletedStale += len(remove)
	return nil
}

// pushBatchWithRetry writes one batch; when the batch fails for a reason
// other than cancellation it retries each session alone and counts the
// ones that still fail.
func (s *Sync) pushBatchWithRetry(
	ctx context.Context, batch []db.Session, fingerprints map[string]string,
	version uint64, offset, total int, result *storage.PushResult,
	onProgress func(storage.PushProgress),
	failed map[string]struct{},
) error {
	counts, err := s.pushSessionBatch(ctx, batch, fingerprints, version)
	if err == nil {
		for i := range batch {
			result.SessionsPushed++
			result.MessagesPushed += counts[i]
			reportProgress(offset+i+1, total, result, onProgress)
		}
		return nil
	}
	if fatal := fatalPushError(ctx, err); fatal != nil {
		return fatal
	}
	log.Printf("clickhouse push: batch at %d failed; retrying sessions individually: %v", offset, err)
	for i, sess := range batch {
		if err := ctx.Err(); err != nil {
			result.Errors += len(batch) - i
			return err
		}
		counts, err := s.pushSessionBatch(ctx, batch[i:i+1], fingerprints, version)
		switch {
		case err == nil:
			result.SessionsPushed++
			result.MessagesPushed += counts[0]
		case fatalPushError(ctx, err) != nil:
			result.Errors += len(batch) - i
			return err
		default:
			result.Errors++
			failed[sess.ID] = struct{}{}
			log.Printf("clickhouse push: skipping session %s after error: %v", sess.ID, err)
		}
		reportProgress(offset+i+1, total, result, onProgress)
	}
	return nil
}

func fatalPushError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func reportProgress(done, total int, result *storage.PushResult, onProgress func(storage.PushProgress)) {
	if onProgress == nil {
		return
	}
	onProgress(storage.PushProgress{
		SessionsDone: done, SessionsTotal: total,
		MessagesDone: result.MessagesPushed, Errors: result.Errors,
	})
}

// sessionPayload is everything one session contributes to the mirror.
type sessionPayload struct {
	session  db.Session
	messages []db.Message
	usage    []db.UsageEvent
	findings []db.SecretFinding
	pins     []db.PinnedMessage
}

func (s *Sync) loadPayload(ctx context.Context, sess db.Session) (sessionPayload, error) {
	p := sessionPayload{session: sess}
	var err error
	if p.messages, err = s.local.GetAllMessages(ctx, sess.ID); err != nil {
		return p, fmt.Errorf("reading local messages for %s: %w", sess.ID, err)
	}
	if p.usage, err = s.local.GetUsageEvents(ctx, sess.ID); err != nil {
		return p, fmt.Errorf("reading local usage events for %s: %w", sess.ID, err)
	}
	if p.findings, err = s.local.SessionSecretFindings(ctx, sess.ID); err != nil {
		return p, fmt.Errorf("reading local secret findings for %s: %w", sess.ID, err)
	}
	if p.pins, err = s.local.ListPinnedMessages(ctx, sess.ID, ""); err != nil {
		return p, fmt.Errorf("reading local pins for %s: %w", sess.ID, err)
	}
	return p, nil
}

// pushSessionBatch writes one batch in the order the consistency design
// requires: dependents, version-bounded delete of older dependents, then
// session rows carrying the fingerprint. Returns the message count per
// session.
func (s *Sync) pushSessionBatch(
	ctx context.Context, batch []db.Session, fingerprints map[string]string,
	version uint64,
) ([]int, error) {
	payloads := make([]sessionPayload, 0, len(batch))
	counts := make([]int, 0, len(batch))
	for _, sess := range batch {
		p, err := s.loadPayload(ctx, sess)
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, p)
		counts = append(counts, len(p.messages))
	}
	if err := s.insertDependents(ctx, payloads, version); err != nil {
		return nil, err
	}
	ids := sessionIDs(batch)
	// Price the new usage rows right after they become readable. Until this
	// returns, a usage read prices the updated session's events per request.
	if s.pricer == nil {
		pricer, err := s.newUsagePricer(ctx)
		if err != nil {
			return nil, err
		}
		s.pricer = pricer
	}
	for batchIDs := range idBatches(ids) {
		if err := s.priceUsage(ctx, s.pricer, usagePriceScope{sessionIDs: batchIDs}); err != nil {
			return nil, err
		}
	}
	for batchIDs := range idBatches(ids) {
		placeholders, args := inArgs(batchIDs)
		// The usage_messages view never sees deletes, so a shorter republished
		// session would leave its removed ordinals behind at the old version.
		for _, table := range slices.Concat(dependentTables, []string{"usage_messages"}) {
			if _, err := s.conn.ExecContext(ctx,
				"DELETE FROM "+table+" WHERE session_id IN ("+placeholders+") AND push_version < ?",
				append(args, version)...); err != nil {
				return nil, fmt.Errorf("deleting older clickhouse %s rows: %w", table, err)
			}
		}
	}
	if s.hooks != nil && s.hooks.beforeSessionRows != nil {
		if err := s.hooks.beforeSessionRows(batch); err != nil {
			return nil, err
		}
	}
	if err := s.insertSessions(ctx, payloads, fingerprints, version); err != nil {
		return nil, err
	}
	return counts, nil
}

func (s *Sync) insertDependents(ctx context.Context, payloads []sessionPayload, version uint64) error {
	var messages, toolCalls, toolResults, usage, findings, pins [][]any
	for _, p := range payloads {
		for _, m := range p.messages {
			messages = append(messages, messageRow(m, version))
			for i, tc := range m.ToolCalls {
				toolCalls = append(toolCalls, toolCallRow(m, tc, i, version))
				for _, ev := range tc.ResultEvents {
					toolResults = append(toolResults, toolResultEventRow(m, i, ev, version))
				}
			}
		}
		for _, ev := range p.usage {
			usage = append(usage, usageEventRow(ev, version))
		}
		for i, f := range p.findings {
			findings = append(findings, secretFindingRow(f, i, version))
		}
		for _, pin := range p.pins {
			pins = append(pins, pinnedMessageRow(pin, version))
		}
	}
	for _, block := range []struct {
		table string
		rows  [][]any
	}{
		{"messages", messages},
		{"tool_calls", toolCalls},
		{"tool_result_events", toolResults},
		{"usage_events", usage},
		{"secret_findings", findings},
		{"pinned_messages", pins},
	} {
		if err := insertRows(ctx, s.conn, block.table, block.rows); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sync) insertSessions(
	ctx context.Context, payloads []sessionPayload, fingerprints map[string]string, version uint64,
) error {
	rows := make([][]any, 0, len(payloads))
	for _, p := range payloads {
		rows = append(rows, s.sessionRow(p, fingerprints[p.session.ID], version))
	}
	return insertRows(ctx, s.conn, "sessions", rows)
}

// insertRows sends one native block insert per table. The driver's
// database/sql interface batches Exec calls between Begin and Commit.
func insertRows(ctx context.Context, conn *sql.DB, table string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	spec, ok := tableByName(table)
	if !ok {
		return fmt.Errorf("clickhouse table %s has no spec", table)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting clickhouse %s insert: %w", table, err)
	}
	stmt, err := tx.PrepareContext(ctx, insertSQL(spec))
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("preparing clickhouse %s insert: %w", table, err)
	}
	defer stmt.Close()
	for _, row := range rows {
		if len(row) != len(spec.columns)+1 {
			_ = tx.Rollback()
			return fmt.Errorf("clickhouse %s row has %d values for %d columns",
				table, len(row), len(spec.columns)+1)
		}
		if _, err := stmt.ExecContext(ctx, row...); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("appending clickhouse %s row: %w", table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("writing clickhouse %s rows: %w", table, err)
	}
	return nil
}

func insertSQL(spec tableSpec) string {
	names := make([]string, 0, len(spec.columns)+1)
	for _, c := range spec.columns {
		names = append(names, c.name)
	}
	names = append(names, pushVersionCol)
	return "INSERT INTO " + spec.name + " (" + strings.Join(names, ", ") + ")"
}

// mirroredSessionMachine keeps the recorded machine key except for the
// local-only sentinels, which take the machine configured for this push.
func mirroredSessionMachine(sess db.Session, fallback string) string {
	if sess.Machine != "" && sess.Machine != "local" {
		return sess.Machine
	}
	return fallback
}

func (s *Sync) sessionRow(p sessionPayload, fingerprint string, version uint64) []any {
	sess := p.session
	return []any{
		sess.ID, sess.Project, sess.ProjectAssigned,
		mirroredSessionMachine(sess, s.machine), sess.Agent,
		sess.AgentLabel, sess.Entrypoint, sess.SessionKind,
		nullString(sess.FirstMessage), nullString(sess.DisplayName), nullString(sess.SessionName),
		nullTime(sess.StartedAt), nullTime(sess.EndedAt),
		int64(sess.MessageCount), int64(sess.UserMessageCount),
		nullString(sess.FilePath), nullInt64(sess.FileSize), nullInt64(sess.FileMtime),
		nullInt64(sess.FileInode), nullInt64(sess.FileDevice), nullString(sess.FileHash),
		nullTime(sess.LocalModifiedAt), transcriptRevisionValue(sess.TranscriptRevision),
		nullString(sess.ParentSessionID), sess.RelationshipType,
		int64(sess.TotalOutputTokens), int64(sess.PeakContextTokens),
		sess.HasTotalOutputTokens, sess.HasPeakContextTokens, sess.IsAutomated,
		int64(sess.ToolFailureSignalCount), int64(sess.ToolRetryCount),
		int64(sess.EditChurnCount), int64(sess.ConsecutiveFailureMax),
		sess.Outcome, sess.OutcomeConfidence, sess.EndedWithRole, int64(sess.FinalFailureStreak),
		nullString(sess.SignalsPendingSince),
		int64(sess.CompactionCount), int64(sess.MidTaskCompactionCount),
		sess.ContextPressureMax, nullIntPtr(sess.HealthScore), nullString(sess.HealthGrade),
		sess.HasToolCalls, sess.HasContextData,
		int64(sess.QualitySignalVersion), int64(sess.ShortPromptCount), sess.UnstructuredStart,
		int64(sess.MissingSuccessCriteriaCount), int64(sess.MissingVerificationCount),
		int64(sess.DuplicatePromptCount), int64(sess.NoCodeContextCount),
		int64(sess.RunawayToolLoopCount), int64(sess.DataVersion),
		sess.Cwd, sess.GitBranch, sess.SourceSessionID, sess.SourceVersion, sess.TranscriptFidelity,
		int64(sess.ParserMalformedLines), sess.IsTruncated,
		nullTime(sess.DeletedAt), nullString(sess.DeletionCause), timeValue(sess.CreatedAt),
		nullString(sess.TerminationStatus),
		int64(sess.SecretLeakCount), sess.SecretsRulesVersion,
		lastMessageAt(p.messages), fingerprint, s.archiveID,
		version,
	}
}

func lastMessageAt(msgs []db.Message) *time.Time {
	var latest *time.Time
	for _, m := range msgs {
		t, ok := parseTimestamp(m.Timestamp)
		if !ok {
			continue
		}
		if latest == nil || t.After(*latest) {
			tt := t
			latest = &tt
		}
	}
	return latest
}

func messageRow(m db.Message, version uint64) []any {
	return []any{
		m.ID, m.SessionID, int64(m.Ordinal), m.Role, m.Content, m.ThinkingText,
		timeValue(m.Timestamp), m.HasThinking, m.HasToolUse, int64(m.ContentLength),
		m.IsSystem, m.Model, m.ReasoningEffort, string(m.TokenUsage),
		int64(m.ContextTokens), int64(m.OutputTokens), m.ProviderID,
		m.HasContextTokens, m.HasOutputTokens, m.ClaudeMessageID, m.ClaudeRequestID,
		m.SourceType, m.SourceSubtype, m.PromptSource, m.SourceUUID, m.SourceParentUUID,
		m.IsSidechain, m.IsCompactBoundary,
		version,
	}
}

func toolCallRow(m db.Message, tc db.ToolCall, callIndex int, version uint64) []any {
	stored := db.DedupToolCallResultSummary(tc.ResultContent, tc.ResultEvents)
	length := tc.ResultContentLength
	if length == 0 {
		if stored != "" {
			length = len(stored)
		} else if len(tc.ResultEvents) == 1 {
			length = len(tc.ResultEvents[0].Content)
		}
	}
	return []any{
		m.ID, int64(m.Ordinal), m.SessionID, tc.ToolName, tc.Category, int64(callIndex),
		tc.ToolUseID, tc.InputJSON, tc.SkillName, int64(length),
		stored,
		tc.SubagentSessionID, tc.FilePath,
		version,
	}
}

func toolResultEventRow(m db.Message, callIndex int, ev db.ToolResultEvent, version uint64) []any {
	return []any{
		m.SessionID, int64(m.Ordinal), int64(callIndex), ev.ToolUseID, ev.AgentID,
		ev.SubagentSessionID, ev.Source, ev.Status, ev.Content, int64(ev.ContentLength),
		timeValue(ev.Timestamp), int64(ev.EventIndex),
		version,
	}
}

func usageEventRow(ev db.UsageEvent, version uint64) []any {
	var cost *int64
	if ev.Cost != nil {
		c := ev.Cost.Microdollars
		cost = &c
	}
	return []any{
		ev.ID, ev.SessionID, nullIntPtr(ev.MessageOrdinal), ev.Source, ev.Model, ev.ProviderID,
		int64(ev.InputTokens), int64(ev.OutputTokens), int64(ev.CacheCreationInputTokens),
		int64(ev.CacheReadInputTokens), int64(ev.ReasoningTokens), cost,
		ev.CostStatus, ev.CostSource, timeValue(ev.OccurredAt), ev.DedupKey,
		version,
	}
}

func secretFindingRow(f db.SecretFinding, index int, version uint64) []any {
	return []any{
		f.SessionID, int64(index), f.RuleName, f.Confidence, f.LocationKind,
		int64(f.MessageOrdinal), nullIntPtr(f.CallIndex), nullIntPtr(f.EventIndex),
		int64(f.MatchStart), int64(f.MatchEnd), int64(f.MatchIndex),
		f.RedactedMatch, f.RulesVersion, nil,
		version,
	}
}

func pinnedMessageRow(p db.PinnedMessage, version uint64) []any {
	return []any{
		p.ID, p.SessionID, p.MessageID, int64(p.Ordinal), "", p.Note,
		timeValue(p.CreatedAt),
		version,
	}
}

func nullString(v *string) *string {
	if v == nil || *v == "" {
		return nil
	}
	return v
}

func nullInt64(v *int64) *int64 { return v }

func nullIntPtr(v *int) *int64 {
	if v == nil {
		return nil
	}
	n := int64(*v)
	return &n
}

func transcriptRevisionValue(v *string) string {
	if v == nil || *v == "" {
		return "0"
	}
	return *v
}

func nullTime(v *string) *time.Time {
	if v == nil {
		return nil
	}
	return timeValue(*v)
}

// timeValue parses an archive timestamp into UTC. Empty or unparseable
// values become NULL.
func timeValue(v string) *time.Time {
	t, ok := parseTimestamp(v)
	if !ok {
		return nil
	}
	return &t
}

func parseTimestamp(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05.999999",
	} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// syncMachineMetadata mirrors machine display labels and proven aliases
// into sync_metadata under the shared key prefixes.
func (s *Sync) syncMachineMetadata(ctx context.Context, version uint64) error {
	labels, err := s.local.GetMachineLabels(ctx)
	if err != nil {
		return fmt.Errorf("reading local machine labels: %w", err)
	}
	aliases, err := s.local.GetMachineAliases(ctx)
	if err != nil {
		return fmt.Errorf("reading local machine aliases: %w", err)
	}
	values := make(map[string]string, len(labels)+len(aliases))
	for machine, label := range labels {
		values[db.MachineLabelKeyPrefix+machine] = label
	}
	for machine, alias := range aliases {
		values[db.MachineAliasKeyPrefix+machine] = alias
	}
	return writeMetadataVersion(ctx, s.conn, values, version)
}
