package parser

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// OpenCodeCoverage limits one journal drain. The limits are deliberately
// fixed so a degraded pass cannot become archive-scale work.
const (
	OpenCodeCoverageMaxRows         = 256
	OpenCodeCoverageMaxPayloadBytes = 1 << 20
	OpenCodeCoverageMaxIDs          = 256
	OpenCodeCoverageMaxDuration     = 2 * time.Second
)

type OpenCodeCoverageState struct {
	Generation       uint64
	LastRowID        int64
	HighWaterRowID   int64
	LastEventID      string
	HighWaterEventID string
	ContainerState   SQLiteContainerState
	ContainerKnown   bool
	HighWaterKnown   bool
	Initialized      bool
	AuditLatched     bool
	PendingIDs       []string
	ReadyIDs         []string
	RemovedIDs       []string
}

type OpenCodeCoverageBatch struct {
	SessionIDs    []string
	RemovedIDs    []string
	More          bool
	AuditRequired bool
	Rows          int
	PayloadBytes  int
	Next          OpenCodeCoverageState
}

type openCodeCoverageJournalSupport uint8

type openCodeCoverageExistence uint8

const (
	openCodeCoverageJournalUnknown openCodeCoverageJournalSupport = iota
	openCodeCoverageJournalSupported
	openCodeCoverageJournalIncompatible
)

const (
	openCodeCoverageExistenceUnchanged openCodeCoverageExistence = iota
	openCodeCoverageExistencePresent
	openCodeCoverageExistenceRemoved
)

func probeOpenCodeCoverageJournal(
	ctx context.Context, dbPath string,
) openCodeCoverageJournalSupport {
	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return openCodeCoverageJournalUnknown
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(event)")
	if err != nil {
		return openCodeCoverageJournalUnknown
	}
	defer rows.Close()
	required := map[string]bool{
		"id": false, "aggregate_id": false, "seq": false,
		"type": false, "data": false,
	}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(
			&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey,
		); err != nil {
			return openCodeCoverageJournalUnknown
		}
		if _, ok := required[name]; ok {
			required[name] = true
		}
	}
	if rows.Err() != nil {
		return openCodeCoverageJournalUnknown
	}
	for _, present := range required {
		if !present {
			return openCodeCoverageJournalIncompatible
		}
	}
	probe, err := db.QueryContext(ctx, "SELECT rowid FROM event LIMIT 0")
	if err != nil {
		return openCodeCoverageJournalIncompatible
	}
	if probe.Close() != nil {
		return openCodeCoverageJournalUnknown
	}
	return openCodeCoverageJournalSupported
}

// ReadOpenCodeCoverage reads the append-only OpenCode event journal by keyset
// pages. It validates the schema before materializing payload data.
func ReadOpenCodeCoverage(ctx context.Context, dbPath string, state OpenCodeCoverageState) (OpenCodeCoverageBatch, error) {
	if state.AuditLatched {
		return OpenCodeCoverageBatch{Next: state}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, OpenCodeCoverageMaxDuration)
	defer cancel()
	containerState, containerKnown := StatSQLiteContainerState(dbPath)
	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return OpenCodeCoverageBatch{}, err
	}
	defer db.Close()
	var high int64
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(rowid),0) FROM event").Scan(&high); err != nil {
		state.AuditLatched = true
		return OpenCodeCoverageBatch{AuditRequired: true, Next: state}, nil
	}
	if !state.Initialized {
		state.Initialized = true
		state.LastRowID = high
		state.ContainerState = containerState
		state.ContainerKnown = containerKnown
		if high > 0 {
			if err := db.QueryRowContext(
				ctx, "SELECT id FROM event WHERE rowid = ?", high,
			).Scan(&state.LastEventID); err != nil {
				state.AuditLatched = true
				return OpenCodeCoverageBatch{AuditRequired: true, Next: state}, nil
			}
		}
		return OpenCodeCoverageBatch{Next: state}, nil
	}
	if !containerKnown || !state.ContainerKnown || sqliteContainerReplaced(state.ContainerState, containerState) {
		if containerKnown {
			state.ContainerState = containerState
			state.ContainerKnown = true
		}
		state.AuditLatched = true
		captureOpenCodeCoverageHighWater(ctx, db, high, &state)
		return OpenCodeCoverageBatch{AuditRequired: true, Next: state}, nil
	}
	continuing := state.HighWaterKnown
	if !state.HighWaterKnown {
		if !captureOpenCodeCoverageHighWater(ctx, db, high, &state) {
			state.AuditLatched = true
			return OpenCodeCoverageBatch{AuditRequired: true, Next: state}, nil
		}
	}
	if continuing && state.HighWaterRowID > 0 {
		var highWaterAnchor string
		if err := db.QueryRowContext(
			ctx, "SELECT id FROM event WHERE rowid = ?", state.HighWaterRowID,
		).Scan(&highWaterAnchor); err != nil || highWaterAnchor != state.HighWaterEventID {
			state.AuditLatched = true
			return OpenCodeCoverageBatch{AuditRequired: true, Next: state}, nil
		}
	}
	if state.LastRowID > 0 {
		var anchor string
		if err := db.QueryRowContext(
			ctx, "SELECT id FROM event WHERE rowid = ?", state.LastRowID,
		).Scan(&anchor); err != nil || anchor != state.LastEventID {
			state.AuditLatched = true
			return OpenCodeCoverageBatch{AuditRequired: true, Next: state}, nil
		}
	}
	if high < state.LastRowID {
		state.AuditLatched = true
		return OpenCodeCoverageBatch{AuditRequired: true, Next: state}, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT rowid, id, aggregate_id, seq, type, length(data) FROM event WHERE rowid > ? AND rowid <= ? ORDER BY rowid LIMIT ?`, state.LastRowID, state.HighWaterRowID, OpenCodeCoverageMaxRows+1)
	if err != nil {
		state.AuditLatched = true
		return OpenCodeCoverageBatch{AuditRequired: true, Next: state}, nil
	}
	defer rows.Close()
	batch := OpenCodeCoverageBatch{Next: state}
	tracked := make(map[string]struct{})
	for _, ids := range [][]string{
		batch.Next.PendingIDs, batch.Next.ReadyIDs, batch.Next.RemovedIDs,
	} {
		for _, id := range ids {
			tracked[id] = struct{}{}
		}
	}
	for rows.Next() {
		if batch.Rows >= OpenCodeCoverageMaxRows {
			batch.More = true
			break
		}
		var rowID int64
		var id string
		var seq int64
		var aggregate, typ string
		var payloadBytes int
		if err := rows.Scan(&rowID, &id, &aggregate, &seq, &typ, &payloadBytes); err != nil {
			return OpenCodeCoverageBatch{}, err
		}
		if payloadBytes < 0 || payloadBytes > OpenCodeCoverageMaxPayloadBytes ||
			batch.PayloadBytes+payloadBytes > OpenCodeCoverageMaxPayloadBytes {
			batch.AuditRequired = true
			batch.Next.AuditLatched = true
			break
		}
		var data []byte
		if err := db.QueryRowContext(ctx, "SELECT data FROM event WHERE rowid = ?", rowID).Scan(&data); err != nil {
			return OpenCodeCoverageBatch{}, err
		}
		batch.Rows++
		batch.PayloadBytes += payloadBytes
		batch.Next.LastRowID = rowID
		batch.Next.LastEventID = id
		batch.Next.ContainerState = containerState
		batch.Next.ContainerKnown = true
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			batch.AuditRequired = true
			batch.Next.AuditLatched = true
			break
		}
		sid := aggregate
		if sid == "" {
			continue
		}
		if _, exists := tracked[sid]; !exists && len(tracked) >= OpenCodeCoverageMaxIDs {
			batch.AuditRequired = true
			batch.Next.AuditLatched = true
			break
		}
		tracked[sid] = struct{}{}
		settled, existence, known := classifyOpenCodeCoverageEvent(
			typ, aggregate, payload,
		)
		if !known {
			batch.AuditRequired = true
			batch.Next.AuditLatched = true
			break
		}
		switch existence {
		case openCodeCoverageExistenceRemoved:
			batch.Next.PendingIDs = removeCoverageID(batch.Next.PendingIDs, sid)
			batch.Next.ReadyIDs = removeCoverageID(batch.Next.ReadyIDs, sid)
			batch.Next.RemovedIDs = appendCoverageID(batch.Next.RemovedIDs, sid)
			continue
		case openCodeCoverageExistencePresent:
			batch.Next.RemovedIDs = removeCoverageID(batch.Next.RemovedIDs, sid)
		case openCodeCoverageExistenceUnchanged:
			if slices.Contains(batch.Next.RemovedIDs, sid) {
				continue
			}
		}
		if settled {
			batch.Next.PendingIDs = removeCoverageID(batch.Next.PendingIDs, sid)
			batch.Next.ReadyIDs = appendCoverageID(batch.Next.ReadyIDs, sid)
		} else {
			batch.Next.ReadyIDs = removeCoverageID(batch.Next.ReadyIDs, sid)
			batch.Next.PendingIDs = appendCoverageID(batch.Next.PendingIDs, sid)
		}
	}
	if err := openCodeCoverageIterationError(rows.Err(), ctx.Err()); err != nil {
		return batch, err
	}
	if !batch.More && !batch.AuditRequired {
		batch.SessionIDs = append(batch.SessionIDs, batch.Next.ReadyIDs...)
		batch.SessionIDs = append(batch.SessionIDs, batch.Next.PendingIDs...)
		batch.RemovedIDs = append(batch.RemovedIDs, batch.Next.RemovedIDs...)
		batch.Next.HighWaterRowID = 0
		batch.Next.HighWaterEventID = ""
		batch.Next.HighWaterKnown = false
		batch.Next.PendingIDs = nil
		batch.Next.ReadyIDs = nil
		batch.Next.RemovedIDs = nil
	}
	return batch, nil
}

func openCodeCoverageIterationError(rowsErr, ctxErr error) error {
	if rowsErr != nil {
		return rowsErr
	}
	return ctxErr
}

func captureOpenCodeCoverageHighWater(
	ctx context.Context, db *sql.DB, high int64, state *OpenCodeCoverageState,
) bool {
	state.HighWaterRowID = high
	state.HighWaterEventID = ""
	state.HighWaterKnown = true
	if high == 0 {
		return true
	}
	if err := db.QueryRowContext(
		ctx, "SELECT id FROM event WHERE rowid = ?", high,
	).Scan(&state.HighWaterEventID); err != nil {
		state.HighWaterKnown = false
		return false
	}
	return true
}

func sqliteContainerReplaced(before, after SQLiteContainerState) bool {
	return before.DBInode != 0 && after.DBInode != 0 &&
		(before.DBInode != after.DBInode || before.DBDevice != after.DBDevice)
}

func classifyOpenCodeCoverageEvent(
	typ, aggregate string, payload map[string]any,
) (settled bool, existence openCodeCoverageExistence, known bool) {
	sessionID, ok := payloadString(payload, "sessionID")
	if !ok || sessionID != aggregate {
		return false, openCodeCoverageExistenceUnchanged, false
	}
	switch typ {
	case "session.created.1", "session.updated.1":
		info, ok := payloadMap(payload, "info")
		if !ok || !payloadStringEquals(info, "id", sessionID) {
			return false, openCodeCoverageExistenceUnchanged, false
		}
		if _, ok := payloadString(info, "projectID"); !ok {
			return false, openCodeCoverageExistenceUnchanged, false
		}
		return true, openCodeCoverageExistencePresent, true
	case "session.deleted.1":
		info, ok := payloadMap(payload, "info")
		if !ok || !payloadStringEquals(info, "id", sessionID) {
			return false, openCodeCoverageExistenceUnchanged, false
		}
		return true, openCodeCoverageExistenceRemoved, true
	case "message.removed.1":
		if _, ok := payloadString(payload, "messageID"); !ok {
			return false, openCodeCoverageExistenceUnchanged, false
		}
		return true, openCodeCoverageExistenceUnchanged, true
	case "message.part.updated.1":
		part, ok := payloadMap(payload, "part")
		if !ok || !payloadStringEquals(part, "sessionID", sessionID) {
			return false, openCodeCoverageExistenceUnchanged, false
		}
		if _, ok := payloadString(part, "id"); !ok {
			return false, openCodeCoverageExistenceUnchanged, false
		}
		if _, ok := payloadString(part, "messageID"); !ok {
			return false, openCodeCoverageExistenceUnchanged, false
		}
		if _, ok := payloadNumber(payload, "time"); !ok {
			return false, openCodeCoverageExistenceUnchanged, false
		}
		return false, openCodeCoverageExistenceUnchanged, true
	case "message.part.removed.1":
		if _, ok := payloadString(payload, "messageID"); !ok {
			return false, openCodeCoverageExistenceUnchanged, false
		}
		if _, ok := payloadString(payload, "partID"); !ok {
			return false, openCodeCoverageExistenceUnchanged, false
		}
		return true, openCodeCoverageExistenceUnchanged, true
	case "message.updated.1":
		info, ok := payloadMap(payload, "info")
		if !ok || !payloadStringEquals(info, "sessionID", sessionID) {
			return false, openCodeCoverageExistenceUnchanged, false
		}
		if _, ok := payloadString(info, "id"); !ok {
			return false, openCodeCoverageExistenceUnchanged, false
		}
		role, ok := payloadString(info, "role")
		if !ok {
			return false, openCodeCoverageExistenceUnchanged, false
		}
		if role == "user" {
			return true, openCodeCoverageExistenceUnchanged, true
		}
		if role == "assistant" {
			timing, _ := info["time"].(map[string]any)
			_, completed := payloadNumber(timing, "completed")
			failure, failed := info["error"]
			failed = failed && failure != nil
			return completed || failed, openCodeCoverageExistenceUnchanged, true
		}
		return false, openCodeCoverageExistenceUnchanged, true
	}
	return false, openCodeCoverageExistenceUnchanged, false
}

func payloadString(payload map[string]any, key string) (string, bool) {
	value, ok := payload[key].(string)
	return value, ok && value != ""
}

func payloadStringEquals(payload map[string]any, key, want string) bool {
	value, ok := payloadString(payload, key)
	return ok && value == want
}

func payloadMap(payload map[string]any, key string) (map[string]any, bool) {
	value, ok := payload[key].(map[string]any)
	return value, ok
}

func payloadNumber(payload map[string]any, key string) (float64, bool) {
	value, ok := payload[key].(float64)
	return value, ok
}

func appendCoverageID(ids []string, id string) []string {
	for _, existing := range ids {
		if existing == id {
			return ids
		}
	}
	return append(ids, id)
}

func removeCoverageID(ids []string, id string) []string {
	for i, existing := range ids {
		if existing == id {
			return append(ids[:i], ids[i+1:]...)
		}
	}
	return ids
}

func (b OpenCodeCoverageBatch) Validate() error {
	if b.Rows < 0 || b.Rows > OpenCodeCoverageMaxRows {
		return fmt.Errorf("coverage rows exceed %d", OpenCodeCoverageMaxRows)
	}
	if b.PayloadBytes < 0 || b.PayloadBytes > OpenCodeCoverageMaxPayloadBytes {
		return fmt.Errorf("coverage bytes exceed %d", OpenCodeCoverageMaxPayloadBytes)
	}
	return nil
}

var _ = sql.ErrNoRows
