// ABOUTME: Bounded OpenCode event-journal change feed. Journal rows in;
// ABOUTME: ready/pending identities, continuation state, and an audit reason out.
// ABOUTME: No SourceRef, no filesystem walk, no engine dependency.
package parser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// OpenCode journal drain limits. All are fixed so a degraded pass cannot
// become archive-scale work. Pin them as exported constants so tests can assert
// the exact boundaries.
const (
	// OpenCodeCoverageMaxRows is the maximum number of event rows read in one
	// drain call, before the +1 sentinel that detects continuation.
	OpenCodeCoverageMaxRows = 256

	// OpenCodeCoverageMaxPayloadBytes is the aggregate payload budget for
	// stage-2 fetches in one drain. message.part.updated.1 never enters this
	// budget; the three other measured types are all under 700 bytes max.
	OpenCodeCoverageMaxPayloadBytes = 1 << 20

	// OpenCodeCoverageMaxIDs is the maximum number of distinct session IDs
	// tracked across ReadyIDs and PendingIDs in one checkpoint.
	OpenCodeCoverageMaxIDs = 256

	// OpenCodeCoverageMaxDuration is the wall-clock budget for one drain call.
	OpenCodeCoverageMaxDuration = 2 * time.Second

	// openCodeMaxAnchors is the maximum number of committed anchors kept for
	// cursor continuity. Spanning at least openCodeMinAnchorAggregates ensures
	// a single session deletion cannot erase every witness.
	openCodeMaxAnchors = 8

	// openCodeMinAnchorAggregates is the minimum number of distinct aggregate
	// IDs that committed anchors must span.
	openCodeMinAnchorAggregates = 2
)

// ErrOpenCodeCoverageDatabaseMissing identifies a coverage unit that
// disappeared between a wake and its drain. The coordinator retires that unit
// so normal provider reconciliation can prove the deletion.
var ErrOpenCodeCoverageDatabaseMissing = errors.New(
	"opencode coverage database missing",
)

// OpenCodeFeedOutcome keeps parser classification explicit at the scheduler
// boundary. Operational failures are retryable; structural audit is the only
// outcome that may enter repair.
type OpenCodeFeedOutcome uint8

const (
	OpenCodeFeedOutcomeNone OpenCodeFeedOutcome = iota
	OpenCodeFeedOutcomeReady
	OpenCodeFeedOutcomeContinuation
	OpenCodeFeedOutcomeOperationalError
	OpenCodeFeedOutcomeStructuralAudit
)

type OpenCodeFeedError struct {
	Kind OpenCodeFeedOutcome
	Err  error
}

func (e *OpenCodeFeedError) Error() string { return e.Err.Error() }
func (e *OpenCodeFeedError) Unwrap() error { return e.Err }

// OpenCodeJournalAnchor is one verified (rowid, eventID, aggregateID) triple
// committed into a checkpoint. Multiple anchors spanning multiple aggregates
// mean a single session deletion cannot erase every continuity witness.
type OpenCodeJournalAnchor struct {
	RowID       int64
	EventID     string
	AggregateID string
}

// OpenCodeCoverageCheckpoint is the committed state of one coverage unit's
// journal reader. It is immutable from the adapter's perspective: the worker
// passes it in as read-only, the adapter returns a proposed next checkpoint.
// The worker commits the proposed checkpoint only after a successful archive
// write for all ready identities.
type OpenCodeCoverageCheckpoint struct {
	// Anchors are committed (rowid, eventID, aggregateID) triples. The
	// maximum anchor rowid is the position cursor; the set spans multiple
	// aggregates so a single deletion cannot void every witness.
	Anchors []OpenCodeJournalAnchor

	// SchemaVersion is PRAGMA schema_version at the last capability probe or
	// drain. A change triggers an audit and rebaseline.
	SchemaVersion int64
	// SchemaFingerprint is a deterministic digest of sqlite_master's schema
	// definitions. It catches compatible schema changes that leave the pragma
	// version unchanged across a copied or forked journal.
	SchemaFingerprint string

	// PendingIDs accumulates sessions that appeared in the current logical
	// drain but have not yet settled. The worker carries them across pages
	// and emits them with ReadyIDs when the drain reaches its high-water.
	PendingIDs []string

	// ReadyIDs accumulates sessions that settled in the current logical
	// drain. Emitted to the worker when the drain reaches its high-water.
	ReadyIDs []string

	// AuditLatched means a durable anomaly was detected. The worker will
	// request a full audit and rebaseline before draining again. Latched
	// forever; no hot retry.
	AuditLatched bool

	// Initialized means the baseline was captured on the first drain call.
	// Before initialization DrainOpenCodeJournal captures the pre-startup
	// baseline and returns an empty result.
	Initialized bool

	// HighWaterRowID and HighWaterEventID are the fixed upper boundary for
	// the current logical drain. Captured once per logical drain; all
	// continuation pages use the same boundary so a write between pages
	// does not shift the window.
	HighWaterRowID       int64
	HighWaterEventID     string
	HighWaterAggregateID string
	// HighWaterKnown is true while a logical drain is active. False means
	// the next call is the first page of a new drain.
	HighWaterKnown bool
}

// OpenCodeFeedResult is what DrainOpenCodeJournal returns from one call.
type OpenCodeFeedResult struct {
	Outcome OpenCodeFeedOutcome
	// ReadyIDs lists sessions that settled (info.time.completed) in this
	// logical drain. Non-nil only when More is false. The worker archives
	// these before committing Next.
	ReadyIDs []string

	// PendingIDs lists sessions still streaming when the drain reached its
	// high-water. Non-nil only when More is false. The worker carries these
	// as hints for the next drain.
	PendingIDs []string

	// More is true when the drain hit the row limit before the high-water.
	// The worker should schedule an immediate continuation from Next.
	More bool

	// AuditRequired is true when a durable anomaly was detected. The worker
	// latches an audit job and rebaselines before draining again.
	AuditRequired bool

	// RowsRead is the number of metadata rows examined (stage 1). Bounded
	// by OpenCodeCoverageMaxRows independent of archive size.
	RowsRead int

	// PayloadBytes is the total bytes fetched in stage-2 queries. Bounded
	// by OpenCodeCoverageMaxPayloadBytes.
	PayloadBytes int

	// Next is the proposed next checkpoint. The worker commits it only after
	// all ready identities have been successfully archived.
	Next OpenCodeCoverageCheckpoint
}

// openCodeEventMeta is a metadata-only row from stage-1 SELECT.
type openCodeEventMeta struct {
	RowID       int64
	EventID     string
	AggregateID string
	Type        string
	PayloadSize int
}

// openCodeEventStatus is the classification state of one session identity
// within a drain.
type openCodeEventStatus uint8

const (
	openCodeStatusPending openCodeEventStatus = iota
	openCodeStatusReady
)

// ProbeOpenCodeJournalCapability checks whether a container's event journal is
// compatible with the bounded feed. Eligibility requires:
//   - event table with required columns (id, aggregate_id, seq, type, data)
//   - event_sequence table with owner_id column
//   - PRAGMA schema_version readable
//
// Returns the schema version, whether the container is compatible, and any
// unexpected error. Unknown schema returns (0, false, nil) — incompatible but
// not an error: the capability gate falls back to existing base behavior.
func ProbeOpenCodeJournalCapability(
	ctx context.Context, dbPath string,
) (schemaVersion int64, compatible bool, err error) {
	if _, statErr := os.Stat(dbPath); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, statErr
	}
	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return 0, false, opencodeCoverageDatabaseError(dbPath, err)
	}
	defer db.Close()

	// Check PRAGMA schema_version first; if it's unreadable, the DB is inaccessible.
	if err := db.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&schemaVersion); err != nil {
		return 0, false, openCodeJournalContextError(ctx, err)
	}

	// Required columns in the event table.
	requiredEventCols := map[string]bool{
		"id": false, "aggregate_id": false, "seq": false,
		"type": false, "data": false,
	}
	rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info('event')")
	if err != nil {
		return 0, false, openCodeJournalContextError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return 0, false, openCodeJournalContextError(ctx, err)
		}
		if _, ok := requiredEventCols[name]; ok {
			requiredEventCols[name] = true
		}
	}
	if rows.Err() != nil {
		return 0, false, openCodeJournalContextError(ctx, rows.Err())
	}
	for _, present := range requiredEventCols {
		if !present {
			return 0, false, nil
		}
	}

	// event_sequence must have owner_id column.
	ownerIDPresent := false
	seqRows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info('event_sequence')")
	if err != nil {
		return 0, false, openCodeJournalContextError(ctx, err)
	}
	defer seqRows.Close()
	for seqRows.Next() {
		var name string
		if err := seqRows.Scan(&name); err != nil {
			return 0, false, openCodeJournalContextError(ctx, err)
		}
		if name == "owner_id" {
			ownerIDPresent = true
		}
	}
	if seqRows.Err() != nil {
		return 0, false, openCodeJournalContextError(ctx, seqRows.Err())
	}
	if !ownerIDPresent {
		return 0, false, nil
	}

	return schemaVersion, true, nil
}

// InitializeOpenCodeCoverageCheckpoint installs a row-zero checkpoint for a
// newly admitted physical journal. The first bounded drain owns all existing
// and triggering rows, so admission never captures a high-water baseline.
func InitializeOpenCodeCoverageCheckpoint(ctx context.Context, dbPath string) (OpenCodeCoverageCheckpoint, error) {
	schemaVersion, compatible, err := ProbeOpenCodeJournalCapability(ctx, dbPath)
	if err != nil {
		return OpenCodeCoverageCheckpoint{}, err
	}
	if !compatible {
		return OpenCodeCoverageCheckpoint{}, fmt.Errorf("opencode journal is not compatible: %s", dbPath)
	}
	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return OpenCodeCoverageCheckpoint{}, opencodeCoverageDatabaseError(dbPath, err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return OpenCodeCoverageCheckpoint{}, openCodeJournalContextError(ctx, err)
	}
	defer func() { _ = tx.Rollback() }()
	fingerprint, err := openCodeJournalSchemaFingerprint(ctx, tx)
	if err != nil {
		return OpenCodeCoverageCheckpoint{}, openCodeJournalContextError(ctx, err)
	}
	return OpenCodeCoverageCheckpoint{Initialized: true, SchemaVersion: schemaVersion, SchemaFingerprint: fingerprint}, nil
}

func openCodeJournalSchemaFingerprint(
	ctx context.Context, tx *sql.Tx,
) (string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT type, name, tbl_name, COALESCE(sql, '')
		  FROM sqlite_master
		 WHERE type IN ('table', 'index', 'trigger', 'view')
		 ORDER BY type, name, tbl_name, sql`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	hash := sha256.New()
	for rows.Next() {
		var typ, name, table, sqlText string
		if err := rows.Scan(&typ, &name, &table, &sqlText); err != nil {
			return "", err
		}
		_, _ = hash.Write([]byte(typ))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(table))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(sqlText))
		_, _ = hash.Write([]byte{0})
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}

// DrainOpenCodeJournal reads one bounded page of the OpenCode event journal
// starting from the checkpoint. It uses two-stage admission: stage 1 reads
// only metadata (rowid, id, aggregate_id, type, octet_length(data)); stage 2
// fetches payload only for settlement-bearing types within the byte budget.
// message.part.updated.1 is always handled from metadata alone.
//
// DrainOpenCodeJournal never performs archive-scale discovery: it reads at
// most OpenCodeCoverageMaxRows rows per call and tracks at most
// OpenCodeCoverageMaxIDs session IDs.
func DrainOpenCodeJournal(
	ctx context.Context,
	dbPath string,
	checkpoint OpenCodeCoverageCheckpoint,
) (OpenCodeFeedResult, error) {
	if checkpoint.AuditLatched {
		return OpenCodeFeedResult{Next: checkpoint}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, OpenCodeCoverageMaxDuration)
	defer cancel()

	if _, err := os.Stat(dbPath); err != nil {
		return OpenCodeFeedResult{Next: checkpoint}, opencodeCoverageDatabaseError(dbPath, err)
	}

	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return OpenCodeFeedResult{Next: checkpoint}, opencodeCoverageDatabaseError(dbPath, err)
	}
	defer db.Close()

	// Use a read transaction so stage-1 and stage-2 queries see a consistent snapshot.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return OpenCodeFeedResult{Next: checkpoint}, fmt.Errorf(
			"opencode coverage begin read transaction %q: %w", dbPath, err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	// Read schema version.
	var schemaVersion int64
	if err := tx.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&schemaVersion); err != nil {
		return OpenCodeFeedResult{Next: checkpoint}, err
	}
	schemaFingerprint, err := openCodeJournalSchemaFingerprint(ctx, tx)
	if err != nil {
		return OpenCodeFeedResult{Next: checkpoint}, err
	}
	if checkpoint.Initialized && checkpoint.SchemaFingerprint != "" &&
		schemaFingerprint != checkpoint.SchemaFingerprint {
		next := checkpoint
		next.AuditLatched = true
		next.SchemaFingerprint = schemaFingerprint
		return OpenCodeFeedResult{AuditRequired: true, Next: next}, nil
	}
	if checkpoint.Initialized && checkpoint.SchemaVersion != 0 &&
		schemaVersion != checkpoint.SchemaVersion {
		next := checkpoint
		next.AuditLatched = true
		next.SchemaVersion = schemaVersion
		return OpenCodeFeedResult{AuditRequired: true, Next: next}, nil
	}

	// Current MAX(rowid) in the event table.
	var maxRowID int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(rowid), 0) FROM event").Scan(&maxRowID); err != nil {
		return OpenCodeFeedResult{Next: checkpoint}, err
	}

	// Initialize on first wake: capture the pre-startup baseline position so
	// the first actual drain reads only events committed after baseline, not
	// the entire journal history. HighWaterKnown is left false so the next
	// drain call captures the high-water from the live max rowid at that time,
	// retaining any events committed between baseline preparation and startup
	// completion.
	if !checkpoint.Initialized {
		next := checkpoint
		next.Initialized = true
		next.SchemaVersion = schemaVersion
		next.SchemaFingerprint = schemaFingerprint
		// Baseline anchors: sample the most-recent events at the current
		// position. positionRowID on the next drain call equals max(anchor.RowID),
		// so stage-1 reads only events committed after this snapshot.
		if maxRowID > 0 {
			anchors, ok, sampleErr := sampleAnchors(
				ctx, tx, 0, maxRowID, openCodeMaxAnchors,
			)
			if sampleErr != nil {
				return OpenCodeFeedResult{Next: checkpoint}, sampleErr
			}
			if !ok {
				// Non-fatal: start with no anchors. The first drain uses
				// positionRowID=0 and reads from the start of the journal,
				// which is correct for a newly empty or unreadable DB.
				anchors = nil
			}
			next.Anchors = anchors
		}
		// HighWaterKnown stays false: the next drain call sets the high-water
		// from the live max rowid so events committed during startup are retained.
		return OpenCodeFeedResult{Next: next}, nil
	}

	// The maximum anchor is the cursor. A lower surviving witness cannot prove
	// that the cursor row was not deleted and reused, so validate the maximum
	// anchor specifically and rewind to the highest verified witness when it is
	// gone. If no witness survives, continuity is unprovable and the audit lane
	// must establish a new boundary.
	if len(checkpoint.Anchors) > 0 {
		maxAnchor := checkpoint.Anchors[0]
		for _, anchor := range checkpoint.Anchors[1:] {
			if anchor.RowID > maxAnchor.RowID {
				maxAnchor = anchor
			}
		}
		verified := make([]OpenCodeJournalAnchor, 0, len(checkpoint.Anchors))
		for _, anchor := range checkpoint.Anchors {
			var id, aggregateID string
			err := tx.QueryRowContext(ctx,
				"SELECT id, aggregate_id FROM event WHERE rowid = ?", anchor.RowID,
			).Scan(&id, &aggregateID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return OpenCodeFeedResult{Next: checkpoint}, openCodeJournalContextError(ctx, err)
			}
			if err == nil && id == anchor.EventID && aggregateID == anchor.AggregateID {
				verified = append(verified, anchor)
			}
		}
		maxVerified := int64(0)
		for _, anchor := range verified {
			if anchor.RowID > maxVerified {
				maxVerified = anchor.RowID
			}
		}
		if maxVerified != maxAnchor.RowID {
			if maxVerified == 0 {
				// The cursor has no verified witness, so continuity cannot be
				// established even when the current max rowid is higher.
				next := checkpoint
				next.AuditLatched = true
				next.SchemaVersion = schemaVersion
				return OpenCodeFeedResult{AuditRequired: true, Next: next}, nil
			} else {
				checkpoint.Anchors = verified
			}
		}
	}

	// Determine position cursor: the maximum anchor rowid, or the stored
	// high-water if we're at the start of a new drain with no session changes.
	positionRowID := anchorsMaxRowID(checkpoint.Anchors)

	// Manage the high-water boundary.
	var hwRowID int64
	var hwEventID, hwAggregateID string
	if !checkpoint.HighWaterKnown {
		// First page of a new drain: capture the fixed high-water.
		hwRowID = maxRowID
		if hwRowID > 0 {
			if err := tx.QueryRowContext(ctx,
				"SELECT id, aggregate_id FROM event WHERE rowid = ?", hwRowID,
			).Scan(&hwEventID, &hwAggregateID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					next := checkpoint
					next.AuditLatched = true
					return OpenCodeFeedResult{AuditRequired: true, Next: next}, nil
				}
				return OpenCodeFeedResult{Next: checkpoint}, err
			}
		}
	} else {
		// Continuation page: verify the high-water anchor is still intact.
		hwRowID = checkpoint.HighWaterRowID
		hwEventID = checkpoint.HighWaterEventID
		hwAggregateID = checkpoint.HighWaterAggregateID
		if hwRowID > 0 {
			var currentHWID, currentHWAggregateID string
			err := tx.QueryRowContext(ctx,
				"SELECT id, aggregate_id FROM event WHERE rowid = ?", hwRowID,
			).Scan(&currentHWID, &currentHWAggregateID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return OpenCodeFeedResult{Next: checkpoint}, openCodeJournalContextError(ctx, err)
			}
			if err != nil || currentHWID != hwEventID || currentHWAggregateID != hwAggregateID {
				next := checkpoint
				next.AuditLatched = true
				return OpenCodeFeedResult{AuditRequired: true, Next: next}, nil
			}
		}
	}

	// If already at or beyond the high-water, the drain is complete.
	if positionRowID >= hwRowID {
		result := OpenCodeFeedResult{
			Next: checkpoint,
		}
		result.ReadyIDs = append([]string(nil), checkpoint.ReadyIDs...)
		result.PendingIDs = append([]string(nil), checkpoint.PendingIDs...)
		result.Next.ReadyIDs = nil
		result.Next.PendingIDs = nil
		result.Next.HighWaterKnown = false
		result.Next.HighWaterRowID = 0
		result.Next.HighWaterEventID = ""
		result.Next.HighWaterAggregateID = ""
		return result, nil
	}

	// Stage 1: read metadata for events (positionRowID, hwRowID].
	metaRows, err := tx.QueryContext(ctx, `
		SELECT rowid, id, aggregate_id, type, octet_length(data)
		  FROM event
		 WHERE rowid > ? AND rowid <= ?
		 ORDER BY rowid
		 LIMIT ?`,
		positionRowID, hwRowID, OpenCodeCoverageMaxRows+1,
	)
	if err != nil {
		return OpenCodeFeedResult{Next: checkpoint}, err
	}
	defer metaRows.Close()

	meta := make([]openCodeEventMeta, 0, OpenCodeCoverageMaxRows+1)
	for metaRows.Next() {
		var m openCodeEventMeta
		if err := metaRows.Scan(&m.RowID, &m.EventID, &m.AggregateID, &m.Type, &m.PayloadSize); err != nil {
			return OpenCodeFeedResult{Next: checkpoint}, err
		}
		meta = append(meta, m)
	}
	if err := metaRows.Err(); err != nil {
		return OpenCodeFeedResult{Next: checkpoint}, err
	}

	// Determine continuation: more than maxRows rows available?
	more := len(meta) > OpenCodeCoverageMaxRows
	if more {
		meta = meta[:OpenCodeCoverageMaxRows]
	}

	// Accumulate identity states from the checkpoint.
	status := make(map[string]openCodeEventStatus)
	for _, id := range checkpoint.ReadyIDs {
		status[id] = openCodeStatusReady
	}
	for _, id := range checkpoint.PendingIDs {
		if _, exists := status[id]; !exists {
			status[id] = openCodeStatusPending
		}
	}

	// Track new anchors from this drain.
	var newAnchors []OpenCodeJournalAnchor

	remainingBudget := OpenCodeCoverageMaxPayloadBytes
	totalPayloadBytes := 0
	rowsRead := 0
	auditRequired := false

	for _, m := range meta {
		if ctx.Err() != nil {
			return OpenCodeFeedResult{Next: checkpoint}, ctx.Err()
		}

		rowsRead++

		switch m.Type {
		case "message.part.updated.1", "message.part.updated":
			// Never read payload for streaming parts: metadata alone suffices.
			// A streaming part always sets the session to pending.
			if len(status) < OpenCodeCoverageMaxIDs || hasID(status, m.AggregateID) {
				status[m.AggregateID] = openCodeStatusPending
			} else {
				auditRequired = true
				break
			}

		case "message.updated.1", "message.updated":
			// Settlement candidate: read payload to check info.time.completed.
			if m.PayloadSize < 0 || m.PayloadSize > remainingBudget {
				auditRequired = true
				break
			}
			var data []byte
			err := tx.QueryRowContext(ctx,
				"SELECT data FROM event WHERE rowid = ? AND octet_length(data) <= ?",
				m.RowID, remainingBudget,
			).Scan(&data)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					auditRequired = true
					break
				}
				return OpenCodeFeedResult{Next: checkpoint}, err
			}
			totalPayloadBytes += len(data)
			remainingBudget -= len(data)

			settled, ok := classifyMessageUpdated(m.AggregateID, data)
			if !ok {
				auditRequired = true
				break
			}
			if len(status) >= OpenCodeCoverageMaxIDs && !hasID(status, m.AggregateID) {
				auditRequired = true
				break
			}
			if settled {
				status[m.AggregateID] = openCodeStatusReady
			} else {
				// Do not downgrade from ready: trailing message.updated.1 events
				// routinely follow settlement and must not reset it.
				if _, exists := status[m.AggregateID]; !exists {
					status[m.AggregateID] = openCodeStatusPending
				}
			}

		case "session.updated.1", "session.updated", "session.created.1", "session.created":
			// Session metadata is durable source state, so apply it at the end
			// of this drain even when no settlement-bearing message exists.
			if len(status) >= OpenCodeCoverageMaxIDs && !hasID(status, m.AggregateID) {
				auditRequired = true
				break
			}
			status[m.AggregateID] = openCodeStatusReady

		default:
			// Unrecognized event type or version: latch audit.
			auditRequired = true
		}

		if auditRequired {
			break
		}

		// Collect anchor candidate.
		newAnchors = append(newAnchors, OpenCodeJournalAnchor{
			RowID:       m.RowID,
			EventID:     m.EventID,
			AggregateID: m.AggregateID,
		})
	}

	// Build next checkpoint.
	next := checkpoint
	next.SchemaVersion = schemaVersion
	next.SchemaFingerprint = schemaFingerprint
	next.HighWaterRowID = hwRowID
	next.HighWaterEventID = hwEventID
	next.HighWaterAggregateID = hwAggregateID
	next.HighWaterKnown = true

	if auditRequired {
		next.AuditLatched = true
		return OpenCodeFeedResult{
			Outcome:       OpenCodeFeedOutcomeStructuralAudit,
			AuditRequired: true,
			RowsRead:      rowsRead,
			PayloadBytes:  totalPayloadBytes,
			Next:          next,
		}, nil
	}

	// Update anchor set: merge old and new, keep up to 8 across ≥2 aggregates.
	next.Anchors = mergeAnchors(checkpoint.Anchors, newAnchors)

	// Convert status map to ReadyIDs/PendingIDs for the next checkpoint.
	var pendingIDs, readyIDs []string
	for id, s := range status {
		if s == openCodeStatusReady {
			readyIDs = append(readyIDs, id)
		} else {
			pendingIDs = append(pendingIDs, id)
		}
	}

	if more {
		// More pages remain in this logical drain.
		next.PendingIDs = pendingIDs
		next.ReadyIDs = readyIDs
		return OpenCodeFeedResult{
			Outcome:      OpenCodeFeedOutcomeContinuation,
			More:         true,
			RowsRead:     rowsRead,
			PayloadBytes: totalPayloadBytes,
			Next:         next,
		}, nil
	}

	// Drain reached the high-water: emit ready/pending and reset.
	next.PendingIDs = nil
	next.ReadyIDs = nil
	next.HighWaterKnown = false
	next.HighWaterRowID = 0
	next.HighWaterEventID = ""
	next.HighWaterAggregateID = ""
	return OpenCodeFeedResult{
		Outcome:      OpenCodeFeedOutcomeReady,
		ReadyIDs:     readyIDs,
		PendingIDs:   pendingIDs,
		RowsRead:     rowsRead,
		PayloadBytes: totalPayloadBytes,
		Next:         next,
	}, nil
}

func opencodeCoverageDatabaseError(dbPath string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrOpenCodeCoverageDatabaseMissing, dbPath)
	}
	return &OpenCodeFeedError{Kind: OpenCodeFeedOutcomeOperationalError,
		Err: fmt.Errorf("opencode coverage database %q: %w", dbPath, err)}
}

// OpenCodeJournalEventInput is one event fed to the reference model.
// Data is nil for types that do not require payload (message.part.updated.1).
type OpenCodeJournalEventInput struct {
	RowID       int64
	EventID     string
	AggregateID string
	Type        string
	Data        []byte
}

// ReduceOpenCodeJournalEvents applies a sequence of journal events to a
// checkpoint using the same reducer logic as DrainOpenCodeJournal. Pure
// function: no database, no filesystem, no clock. Used by tests to verify
// ordering, settlement, downgrade, and bound invariants without I/O.
func ReduceOpenCodeJournalEvents(
	checkpoint OpenCodeCoverageCheckpoint,
	events []OpenCodeJournalEventInput,
) (next OpenCodeCoverageCheckpoint, auditRequired bool) {
	next = checkpoint
	status := make(map[string]openCodeEventStatus)
	for _, id := range checkpoint.ReadyIDs {
		status[id] = openCodeStatusReady
	}
	for _, id := range checkpoint.PendingIDs {
		if _, exists := status[id]; !exists {
			status[id] = openCodeStatusPending
		}
	}

	var newAnchors []OpenCodeJournalAnchor
	for _, e := range events {
		switch e.Type {
		case "message.part.updated.1", "message.part.updated":
			if len(status) >= OpenCodeCoverageMaxIDs && !hasID(status, e.AggregateID) {
				return next, true
			}
			status[e.AggregateID] = openCodeStatusPending

		case "message.updated.1", "message.updated":
			if len(status) >= OpenCodeCoverageMaxIDs && !hasID(status, e.AggregateID) {
				return next, true
			}
			if e.Data == nil {
				// Treat as pending (metadata-only in reference model = no payload = no settlement).
				if _, exists := status[e.AggregateID]; !exists {
					status[e.AggregateID] = openCodeStatusPending
				}
			} else {
				settled, ok := classifyMessageUpdated(e.AggregateID, e.Data)
				if !ok {
					return next, true
				}
				if settled {
					status[e.AggregateID] = openCodeStatusReady
				} else {
					// No downgrade from ready for trailing non-settling events.
					if _, exists := status[e.AggregateID]; !exists {
						status[e.AggregateID] = openCodeStatusPending
					}
				}
			}

		case "session.updated.1", "session.updated", "session.created.1", "session.created":
			if len(status) >= OpenCodeCoverageMaxIDs && !hasID(status, e.AggregateID) {
				return next, true
			}
			status[e.AggregateID] = openCodeStatusReady

		default:
			return next, true
		}

		if e.EventID != "" {
			newAnchors = append(newAnchors, OpenCodeJournalAnchor{
				RowID:       e.RowID,
				EventID:     e.EventID,
				AggregateID: e.AggregateID,
			})
		}
	}

	next.Anchors = mergeAnchors(checkpoint.Anchors, newAnchors)
	var pendingIDs, readyIDs []string
	for id, s := range status {
		if s == openCodeStatusReady {
			readyIDs = append(readyIDs, id)
		} else {
			pendingIDs = append(pendingIDs, id)
		}
	}
	next.ReadyIDs = readyIDs
	next.PendingIDs = pendingIDs
	return next, false
}

// classifyMessageUpdated decodes the narrow envelope of a message.updated.1
// payload and determines whether the event represents settlement (assistant
// turn completed). Returns (settled, ok). ok=false means the payload is
// malformed or the envelope fails the identity cross-check → audit.
//
// Settlement requires:
//   - info.role == "assistant"
//   - info.time.completed is a JSON number (integer milliseconds)
//   - sessionID == aggregate_id
//   - info.sessionID == aggregate_id
//
// A missing info.time.completed is an ordinary deferral (settled=false, ok=true).
func classifyMessageUpdated(aggregateID string, data []byte) (settled, ok bool) {
	var envelope struct {
		SessionID string `json:"sessionID"`
		Info      struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
			Role      string `json:"role"`
			Time      *struct {
				Completed *json.Number `json:"completed"`
			} `json:"time"`
		} `json:"info"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return false, false
	}
	// Identity cross-check.
	if envelope.SessionID != aggregateID {
		return false, false
	}
	if envelope.Info.SessionID != aggregateID {
		return false, false
	}
	if envelope.Info.ID == "" {
		return false, false
	}
	role := envelope.Info.Role
	if role == "" {
		return false, false
	}
	if role != "assistant" {
		// User or other role: ordinary pending event, not an anomaly.
		return false, true
	}
	// Assistant: check for completion timestamp.
	if envelope.Info.Time == nil || envelope.Info.Time.Completed == nil {
		// No completion field: ordinary deferral, not an anomaly.
		return false, true
	}
	// Completed must be a number (integer milliseconds).
	if _, err := envelope.Info.Time.Completed.Int64(); err != nil {
		// Non-integer completion value → malformed → audit.
		return false, false
	}
	return true, true
}

// hasID reports whether id is a key in the status map.
func hasID(status map[string]openCodeEventStatus, id string) bool {
	_, ok := status[id]
	return ok
}

// anchorsMaxRowID returns the maximum RowID in the anchor set, or 0.
func anchorsMaxRowID(anchors []OpenCodeJournalAnchor) int64 {
	var max int64
	for _, a := range anchors {
		if a.RowID > max {
			max = a.RowID
		}
	}
	return max
}

// mergeAnchors produces the next committed anchor set from old (verified at
// drain start) and new (collected during this drain). Keeps up to
// openCodeMaxAnchors, preferring recent entries, spanning at least
// openCodeMinAnchorAggregates distinct aggregates where possible.
func mergeAnchors(old, newAnchors []OpenCodeJournalAnchor) []OpenCodeJournalAnchor {
	// Index by rowid to deduplicate.
	byRowID := make(map[int64]OpenCodeJournalAnchor, len(old)+len(newAnchors))
	for _, a := range old {
		byRowID[a.RowID] = a
	}
	for _, a := range newAnchors {
		byRowID[a.RowID] = a
	}

	// Collect all and sort descending by rowid (most recent first).
	all := make([]OpenCodeJournalAnchor, 0, len(byRowID))
	for _, a := range byRowID {
		all = append(all, a)
	}
	sortAnchorsDesc(all)

	// Greedily select up to openCodeMaxAnchors, ensuring at least
	// openCodeMinAnchorAggregates distinct aggregates.
	selected := make([]OpenCodeJournalAnchor, 0, openCodeMaxAnchors)
	aggSeen := make(map[string]bool)
	for _, a := range all {
		if len(selected) >= openCodeMaxAnchors {
			break
		}
		selected = append(selected, a)
		aggSeen[a.AggregateID] = true
	}
	// If we don't span enough aggregates, try again prioritizing coverage.
	if len(aggSeen) < openCodeMinAnchorAggregates && len(all) > len(selected) {
		selected = selected[:0]
		aggSeen = make(map[string]bool)
		// First pass: one from each aggregate (most recent per aggregate).
		aggBest := make(map[string]OpenCodeJournalAnchor)
		for _, a := range all { // all is descending; first per aggregate is best
			if _, exists := aggBest[a.AggregateID]; !exists {
				aggBest[a.AggregateID] = a
			}
		}
		for _, a := range all {
			if best, ok := aggBest[a.AggregateID]; ok && best.RowID == a.RowID {
				selected = append(selected, a)
				aggSeen[a.AggregateID] = true
				if len(selected) >= openCodeMaxAnchors {
					break
				}
			}
		}
		// Fill remaining slots from any anchor.
		for _, a := range all {
			if len(selected) >= openCodeMaxAnchors {
				break
			}
			alreadyIn := false
			for _, s := range selected {
				if s.RowID == a.RowID {
					alreadyIn = true
					break
				}
			}
			if !alreadyIn {
				selected = append(selected, a)
			}
		}
		sortAnchorsDesc(selected)
	}
	return selected
}

// sortAnchorsDesc sorts anchors by RowID descending in place.
func sortAnchorsDesc(anchors []OpenCodeJournalAnchor) {
	for i := 1; i < len(anchors); i++ {
		for j := i; j > 0 && anchors[j].RowID > anchors[j-1].RowID; j-- {
			anchors[j], anchors[j-1] = anchors[j-1], anchors[j]
		}
	}
}

// sampleAnchors samples up to n (rowid, eventID, aggregateID) triples from
// [afterRowID, upToRowID], preferring the most recent rows.
func sampleAnchors(
	ctx context.Context, tx *sql.Tx,
	afterRowID, upToRowID int64, n int,
) ([]OpenCodeJournalAnchor, bool, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT rowid, id, aggregate_id
		  FROM event
		 WHERE rowid > ? AND rowid <= ?
		 ORDER BY rowid DESC
		 LIMIT ?`,
		afterRowID, upToRowID, n,
	)
	if err != nil {
		return nil, false, openCodeJournalContextError(ctx, err)
	}
	defer rows.Close()
	anchors := make([]OpenCodeJournalAnchor, 0, openCodeMaxAnchors)
	for rows.Next() {
		var a OpenCodeJournalAnchor
		if err := rows.Scan(&a.RowID, &a.EventID, &a.AggregateID); err != nil {
			return nil, false, openCodeJournalContextError(ctx, err)
		}
		anchors = append(anchors, a)
	}
	if rows.Err() != nil {
		err := rows.Err()
		return nil, false, openCodeJournalContextError(ctx, err)
	}
	// Return in ascending order.
	for i, j := 0, len(anchors)-1; i < j; i, j = i+1, j-1 {
		anchors[i], anchors[j] = anchors[j], anchors[i]
	}
	return anchors, true, nil
}

func openCodeJournalContextError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &OpenCodeFeedError{Kind: OpenCodeFeedOutcomeOperationalError, Err: err}
}
