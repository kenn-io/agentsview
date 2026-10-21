package db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/fileevidence"
)

var ErrFileEvidenceStale = errors.New("file evidence binding is stale")

type FileEvidenceReader interface {
	ReadFileEvidenceSnapshot(context.Context, string, fileevidence.Window) (fileevidence.Snapshot, error)
	ValidateFileEvidenceBinding(context.Context, string, string) error
	ReadMessagesWithFileEvidence(context.Context, string, MessageWindow) (FileEvidenceMessagePage, error)
}
type FileEvidenceMessagePage struct {
	Messages          []Message
	Evidence          map[int][]fileevidence.Record
	Binding, Revision string
	Coverage          fileevidence.Coverage
}

const evidenceCols = "slot,kind,media_type,display_name,display_path,size,sha256,proof_method,retention,asset_ref,ineligible_reason,support_kind,support_digest,call_index,result_index"

func scanEvidence(rows *sql.Rows, prefix ...any) (fileevidence.Record, evidenceSupport, error) {
	var r fileevidence.Record
	var s evidenceSupport
	var slot string
	prefix = append(prefix, &slot, &r.Kind, &r.MediaType, &r.DisplayName, &r.DisplayPath, &r.Size, &r.SHA256, &r.ProofMethod, &r.Retention, &r.AssetRef, &r.IneligibleReason, &s.Kind, &s.Digest, &s.Call, &s.Result)
	if err := rows.Scan(prefix...); err != nil {
		return r, s, err
	}
	var err error
	r.ResultEventIndex = s.resultEventIndex()
	r.Position, err = parseEvidenceSlot(slot)
	if err != nil {
		return r, s, err
	}
	return r, s, nil
}

func parseEvidenceSlot(slot string) (fileevidence.Position, error) {
	var p fileevidence.Position
	var tuple []jsontext.Value
	var version int
	if len(slot) > 1024 || json.Unmarshal([]byte(slot), &tuple) != nil || len(tuple) != 6 {
		return p, ErrFileEvidenceStale
	}
	for i, dst := range []any{&version, &p.Entry, &p.Call, &p.ResultEntry, &p.Result, &p.Block} {
		if json.Unmarshal(tuple[i], dst) != nil {
			return p, ErrFileEvidenceStale
		}
	}
	canonical, err := p.Slot()
	if err != nil || version != 1 || canonical != slot {
		return p, ErrFileEvidenceStale
	}
	return p, nil
}

func hasFileEvidence(ctx context.Context, q messageRowsQuerier) (bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='message_file_evidence'")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}

func hydrateFileEvidence(ctx context.Context, q messageRowsQuerier, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	has, err := hasFileEvidence(ctx, q)
	if err != nil || !has {
		return err
	}
	for start := 0; start < len(messages); start += 400 {
		end := min(start+400, len(messages))
		args := []any{}
		byID := map[int64]int{}
		for i := start; i < end; i++ {
			args = append(args, messages[i].ID)
			byID[messages[i].ID] = i
		}
		rows, err := q.QueryContext(ctx, "SELECT message_id,"+evidenceCols+" FROM message_file_evidence WHERE message_id IN ("+placeholderList(len(args))+") ORDER BY message_id,slot", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			r, s, e := scanEvidence(rows, &id)
			if e != nil {
				rows.Close()
				return e
			}
			i := byID[id]
			messages[i].FileEvidence = append(messages[i].FileEvidence, r)
			messages[i].evidenceSupport = append(messages[i].evidenceSupport, s)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func storeFileEvidenceTx(tx transactionQueries, messages []Message, ids []int64) error {
	for i, m := range messages {
		if _, err := tx.Exec("DELETE FROM message_file_evidence WHERE message_id=?", ids[i]); err != nil {
			return err
		}
		for j, r := range m.FileEvidence {
			slot, err := r.Position.Slot()
			if err != nil {
				return err
			}
			s := m.evidenceSupport[j]
			if _, err := tx.Exec("INSERT INTO message_file_evidence(message_id,"+evidenceCols+") VALUES ("+placeholderList(16)+")", ids[i], slot, r.Kind, r.MediaType, r.DisplayName, r.DisplayPath, r.Size, r.SHA256, r.ProofMethod, r.Retention, r.AssetRef, r.IneligibleReason, s.Kind, s.Digest, s.Call, s.Result); err != nil {
				return err
			}
		}
	}
	return nil
}

func setEvidenceCoverageTx(tx transactionQueries, id string, c fileevidence.Coverage, replace bool) (bool, error) {
	if c.Version == 0 {
		return false, nil
	}
	var version int
	var state string
	if err := tx.QueryRow("SELECT file_evidence_version,file_evidence_coverage FROM sessions WHERE id=?", id).Scan(&version, &state); err != nil {
		return false, err
	}
	if !replace {
		var retained bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM messages m JOIN message_file_evidence e ON e.message_id=m.id WHERE m.session_id=?)", id).Scan(&retained); err != nil {
			return false, err
		}
		if retained {
			c.State = "complete"
		} else if state == "withheld" {
			c.State = state
		}
	}
	if version == c.Version && state == c.State {
		return false, nil
	}
	_, err := tx.Exec("UPDATE sessions SET file_evidence_version=?,file_evidence_coverage=? WHERE id=?", c.Version, c.State, id)
	return true, err
}

func evidenceHeader(ctx context.Context, tx *sql.Tx, id string) (fileevidence.Snapshot, error) {
	var s fileevidence.Snapshot
	var generation string
	err := tx.QueryRowContext(ctx, "SELECT COALESCE(transcript_revision,'') FROM sessions WHERE id=?", id).Scan(&s.Revision)
	if err != nil {
		return s, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT value FROM archive_metadata WHERE key='database_id'").Scan(&generation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return s, ErrDatabaseIDMissing
		}
		return s, fmt.Errorf("reading database id: %w", err)
	}
	if strings.TrimSpace(generation) == "" {
		return s, ErrDatabaseIDMissing
	}
	s.Binding = FileEvidenceBinding(generation, id, s.Revision)
	s.Coverage.State = "unavailable"
	has, err := hasFileEvidence(ctx, tx)
	if err != nil {
		return s, err
	}
	if has {
		err = tx.QueryRowContext(ctx, "SELECT file_evidence_version,file_evidence_coverage FROM sessions WHERE id=?", id).Scan(&s.Coverage.Version, &s.Coverage.State)
	}
	return s, err
}

func (db *DB) ValidateFileEvidenceBinding(ctx context.Context, id, binding string) error {
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	s, err := evidenceHeader(ctx, tx, id)
	if err == nil && s.Binding != binding {
		return ErrFileEvidenceStale
	}
	return err
}

func (db *DB) ReadFileEvidenceSnapshot(ctx context.Context, id string, w fileevidence.Window) (fileevidence.Snapshot, error) {
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fileevidence.Snapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	s, err := evidenceHeader(ctx, tx, id)
	if err != nil {
		return s, err
	}
	if w.Binding != "" && w.Binding != s.Binding {
		return s, ErrFileEvidenceStale
	}
	if w.Binding == "" && (w.Offset > 0 || w.OccurrenceID != "") {
		return s, ErrFileEvidenceStale
	}
	s.Rows = []fileevidence.Occurrence{}
	if s.Coverage.Version == 0 {
		return s, nil
	}
	where := "m.session_id=?"
	args := []any{id}
	if w.FirstOrdinal != nil {
		where += " AND m.ordinal>=?"
		args = append(args, *w.FirstOrdinal)
	}
	if w.LastOrdinal != nil {
		where += " AND m.ordinal<=?"
		args = append(args, *w.LastOrdinal)
	}
	if w.OccurrenceID != "" {
		ordinal, slot, err := DecodeFileEvidenceOccurrence(w.OccurrenceID, s.Binding)
		if err != nil {
			return s, err
		}
		where += " AND m.ordinal=? AND e.slot=?"
		args = append(args, ordinal, slot)
	}
	from := " FROM messages m JOIN message_file_evidence e ON e.message_id=m.id WHERE " + where
	if err = tx.QueryRowContext(ctx, "SELECT count(*)"+from, args...).Scan(&s.Total); err != nil {
		return s, err
	}
	limit := w.Limit
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 50)
	offset := max(w.Offset, 0)
	cols := "e." + strings.ReplaceAll(evidenceCols, ",", ",e.")
	rows, err := tx.QueryContext(ctx, "SELECT m.ordinal,"+cols+from+" ORDER BY m.ordinal,e.slot LIMIT ? OFFSET ?", append(args, limit, offset)...)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int
		r, _, e := scanEvidence(rows, &ordinal)
		if e != nil {
			return s, e
		}
		s.Rows = append(s.Rows, fileevidence.Occurrence{MessageOrdinal: ordinal, Record: r})
	}
	if offset+len(s.Rows) < s.Total {
		s.NextOffset = new(offset + len(s.Rows))
	}
	return s, rows.Err()
}

func (db *DB) ReadMessagesWithFileEvidence(ctx context.Context, id string, w MessageWindow) (FileEvidenceMessagePage, error) {
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return FileEvidenceMessagePage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	s, err := evidenceHeader(ctx, tx, id)
	if err != nil {
		return FileEvidenceMessagePage{}, err
	}
	p := FileEvidenceMessagePage{Binding: s.Binding, Revision: s.Revision, Coverage: s.Coverage, Evidence: map[int][]fileevidence.Record{}}
	clause, roleArgs := roleFilterClause(w.Roles)
	args := []any{id}
	var query string
	if w.Around != nil {
		query = fmt.Sprintf("SELECT * FROM (SELECT * FROM (SELECT %s FROM messages WHERE session_id=? AND ordinal<?%s ORDER BY ordinal DESC LIMIT ?) UNION ALL SELECT %s FROM messages WHERE session_id=? AND ordinal=? UNION ALL SELECT * FROM (SELECT %s FROM messages WHERE session_id=? AND ordinal>?%s ORDER BY ordinal LIMIT ?)) ORDER BY ordinal", selectMessageCols, clause, selectMessageCols, selectMessageCols, clause)
		args = append(args, *w.Around)
		args = append(args, roleArgs...)
		args = append(args, max(w.Before, 0), id, *w.Around, id, *w.Around)
		args = append(args, roleArgs...)
		args = append(args, max(w.After, 0))
	} else {
		from := 0
		if w.From != nil {
			from = *w.From
		}
		dir, op := "ASC", ">="
		if !w.Asc {
			dir, op = "DESC", "<="
		}
		limit := w.Limit
		if limit <= 0 || limit > MaxMessageLimit {
			limit = DefaultMessageLimit
		}
		query = fmt.Sprintf("SELECT %s FROM messages WHERE session_id=? AND ordinal%s?%s ORDER BY ordinal %s LIMIT ?", selectMessageCols, op, clause, dir)
		args = append(args, from)
		args = append(args, roleArgs...)
		args = append(args, limit)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	p.Messages, err = scanMessages(rows)
	rows.Close()
	if err != nil {
		return p, err
	}
	if err = attachToolCallsWithQuerier(ctx, tx, p.Messages); err != nil {
		return p, err
	}
	for _, m := range p.Messages {
		p.Evidence[m.Ordinal] = m.FileEvidence
	}
	if w.ObservedRevision != nil {
		*w.ObservedRevision = s.Revision
	}
	return p, nil
}

func digestFileEvidenceTx(ctx context.Context, tx *sql.Tx, id string, h interface{ Write([]byte) (int, error) }) error {
	var version int
	var state string
	if err := tx.QueryRowContext(ctx, "SELECT file_evidence_version,file_evidence_coverage FROM sessions WHERE id=?", id).Scan(&version, &state); err != nil {
		return err
	}
	writeStagedDigestInt(h, int64(version))
	writeStagedDigestString(h, state)
	rows, err := tx.QueryContext(ctx, "SELECT m.ordinal,e."+strings.ReplaceAll(evidenceCols, ",", ",e.")+" FROM messages m JOIN message_file_evidence e ON e.message_id=m.id WHERE m.session_id=? ORDER BY m.ordinal,e.slot", id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int
		r, s, e := scanEvidence(rows, &ordinal)
		if e != nil {
			return e
		}
		b, e := json.Marshal([]any{ordinal, r, s})
		if e != nil {
			return e
		}
		writeStagedDigestString(h, string(b))
	}
	return rows.Err()
}

// FileEvidenceDiffers compares the local evidence stream independently of
// portable message fingerprints, which deliberately exclude this metadata.
func (db *DB) FileEvidenceDiffers(ctx context.Context, id string, messages []Message, coverage fileevidence.Coverage) (bool, error) {
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	has, err := hasFileEvidence(ctx, tx)
	if err != nil {
		return false, err
	}
	stored := sha256.New()
	if has {
		if err := digestFileEvidenceTx(ctx, tx, id, stored); err != nil {
			return false, err
		}
	} else {
		writeStagedDigestInt(stored, 0)
		writeStagedDigestString(stored, "unavailable")
	}
	proposed := sha256.New()
	c := evidenceCoverage(messages, coverage)
	writeStagedDigestInt(proposed, int64(c.Version))
	state := c.State
	if c.Version == 0 {
		state = "unavailable"
	}
	writeStagedDigestString(proposed, state)
	for _, m := range messages {
		for i, r := range m.FileEvidence {
			if i >= len(m.evidenceSupport) {
				return true, nil
			}
			b, err := json.Marshal([]any{m.Ordinal, r, m.evidenceSupport[i]})
			if err != nil {
				return false, err
			}
			writeStagedDigestString(proposed, string(b))
		}
	}
	return !bytes.Equal(stored.Sum(nil), proposed.Sum(nil)), nil
}
