package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/fileevidence"
)

// Rows are keyed by message ordinal rather than referencing messages: the
// message push replaces rows wholesale, and a cascade would silently drop
// evidence whose digest still matches the local archive.
const fileEvidenceDDL = `
CREATE TABLE IF NOT EXISTS message_file_evidence (
    session_id        TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    ordinal           INTEGER NOT NULL,
    slot              TEXT NOT NULL,
    kind              TEXT NOT NULL,
    media_type        TEXT NOT NULL,
    display_name      TEXT NOT NULL,
    display_path      TEXT NOT NULL,
    size              BIGINT,
    sha256            TEXT NOT NULL,
    proof_method      TEXT NOT NULL,
    retention         TEXT NOT NULL,
    asset_ref         TEXT NOT NULL,
    ineligible_reason TEXT NOT NULL,
    PRIMARY KEY (session_id, ordinal, slot)
);
`

// fileEvidenceGenerationKey names this PG archive's evidence generation.
// A recreated archive gets a new value, so old bindings become stale even
// when session revisions repeat.
const fileEvidenceGenerationKey = "file_evidence_generation"

const pgEvidenceCols = "slot, kind, media_type, display_name, display_path, size, sha256, proof_method, retention, asset_ref, ineligible_reason"

func ensureFileEvidenceSchema(ctx context.Context, pg *sql.DB) error {
	if _, err := pg.ExecContext(ctx, fileEvidenceDDL); err != nil {
		return fmt.Errorf("creating pg file evidence table: %w", err)
	}
	return nil
}

var hostedFileEvidenceTables = []HostedTable{
	{Name: "message_file_evidence", Key: []string{"session_id", "ordinal", "slot"}, ForeignKeys: sessionHostedFK()},
}

// ensureFileEvidenceGeneration records this archive's generation the first
// time evidence is written; readers cannot mint bindings without it.
func ensureFileEvidenceGeneration(ctx context.Context, tx *sql.Tx) error {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("creating file evidence generation: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sync_metadata (key, value) VALUES ($1, $2)
		 ON CONFLICT (key) DO NOTHING`,
		fileEvidenceGenerationKey, hex.EncodeToString(b[:]),
	); err != nil {
		return fmt.Errorf("recording file evidence generation: %w", err)
	}
	return nil
}

// pushFileEvidence replaces a session's PG evidence when the local digest
// differs. Evidence has its own digest because message fingerprints
// deliberately exclude it.
func (s *Sync) pushFileEvidence(
	ctx context.Context, tx *sql.Tx, sessionID string, full bool,
) (bool, error) {
	export, err := s.local.ExportFileEvidence(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("reading local file evidence for %s: %w", sessionID, err)
	}
	var stored string
	err = tx.QueryRowContext(ctx,
		`SELECT file_evidence_digest FROM sessions WHERE id = $1`, sessionID,
	).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("reading pg file evidence digest for %s: %w", sessionID, err)
	}
	if !full && stored == export.Digest {
		return false, nil
	}
	if err := ensureFileEvidenceGeneration(ctx, tx); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM message_file_evidence WHERE session_id = $1`, sessionID,
	); err != nil {
		return false, fmt.Errorf("deleting pg file evidence for %s: %w", sessionID, err)
	}
	const perRow = 13
	const batch = 400
	for start := 0; start < len(export.Rows); start += batch {
		end := min(start+batch, len(export.Rows))
		values := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*perRow)
		for _, row := range export.Rows[start:end] {
			slot, err := row.Record.Position.Slot()
			if err != nil {
				return false, fmt.Errorf("encoding file evidence slot for %s: %w", sessionID, err)
			}
			r := row.Record
			n := len(args)
			ph := make([]string, perRow)
			for i := range ph {
				ph[i] = fmt.Sprintf("$%d", n+i+1)
			}
			values = append(values, "("+strings.Join(ph, ",")+")")
			args = append(args, sessionID, row.MessageOrdinal, slot, r.Kind, r.MediaType,
				r.DisplayName, r.DisplayPath, r.Size, r.SHA256, r.ProofMethod,
				r.Retention, r.AssetRef, r.IneligibleReason)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO message_file_evidence (session_id, ordinal, "+pgEvidenceCols+") VALUES "+
				strings.Join(values, ","),
			args...,
		); err != nil {
			return false, fmt.Errorf("inserting pg file evidence for %s: %w", sessionID, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions
		 SET file_evidence_version = $2, file_evidence_coverage = $3, file_evidence_digest = $4
		 WHERE id = $1`,
		sessionID, export.Coverage.Version, export.Coverage.State, export.Digest,
	); err != nil {
		return false, fmt.Errorf("updating pg file evidence coverage for %s: %w", sessionID, err)
	}
	return stored != export.Digest, nil
}

// evidenceTx opens a read-only snapshot so binding, coverage, totals and
// rows describe one state.
func (s *Store) evidenceTx(ctx context.Context) (*sql.Tx, error) {
	return s.pg.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
}

// pgEvidenceHeader mirrors the SQLite header: an archive without the
// generation cannot mint a safe binding, and one without the table reports
// unavailable coverage.
func pgEvidenceHeader(ctx context.Context, tx *sql.Tx, id string) (fileevidence.Snapshot, bool, error) {
	var s fileevidence.Snapshot
	var generation string
	err := tx.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = $1`, fileEvidenceGenerationKey,
	).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) || isUndefinedTable(err) {
		return s, false, db.ErrDatabaseIDMissing
	}
	if err != nil {
		return s, false, fmt.Errorf("reading file evidence generation: %w", err)
	}
	if strings.TrimSpace(generation) == "" {
		return s, false, db.ErrDatabaseIDMissing
	}
	s.Coverage.State = "unavailable"
	err = tx.QueryRowContext(ctx,
		`SELECT transcript_revision, file_evidence_version, file_evidence_coverage
		 FROM sessions WHERE id = $1`, id,
	).Scan(&s.Revision, &s.Coverage.Version, &s.Coverage.State)
	if isUndefinedColumn(err) {
		if err = tx.QueryRowContext(ctx,
			`SELECT transcript_revision FROM sessions WHERE id = $1`, id,
		).Scan(&s.Revision); err != nil {
			return s, false, err
		}
		s.Binding = db.FileEvidenceBinding(generation, id, s.Revision)
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	s.Binding = db.FileEvidenceBinding(generation, id, s.Revision)
	if !pgHasTableTx(ctx, tx, "message_file_evidence") {
		// Coverage columns without rows would claim a complete empty result.
		s.Coverage = fileevidence.Coverage{State: "unavailable"}
		return s, false, nil
	}
	return s, true, nil
}

var _ db.FileEvidenceReader = (*Store)(nil)

func pgHasTableTx(ctx context.Context, tx *sql.Tx, table string) bool {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists)
	return err == nil && exists
}

func scanPGEvidence(rows *sql.Rows, ordinal *int) (fileevidence.Record, error) {
	var r fileevidence.Record
	var slot string
	if err := rows.Scan(ordinal, &slot, &r.Kind, &r.MediaType, &r.DisplayName,
		&r.DisplayPath, &r.Size, &r.SHA256, &r.ProofMethod, &r.Retention,
		&r.AssetRef, &r.IneligibleReason); err != nil {
		return r, err
	}
	p, err := db.ParseFileEvidenceSlot(slot)
	if err != nil {
		return r, err
	}
	r.Position = p
	return r, nil
}

// ValidateFileEvidenceBinding rejects a binding minted before the current
// archive generation or transcript revision.
func (s *Store) ValidateFileEvidenceBinding(ctx context.Context, id, binding string) error {
	if s.hostedRelations {
		return db.ErrDatabaseIDMissing
	}
	tx, err := s.evidenceTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	h, _, err := pgEvidenceHeader(ctx, tx, id)
	if err == nil && h.Binding != binding {
		return db.ErrFileEvidenceStale
	}
	return err
}

// ReadFileEvidenceSnapshot pages a session's evidence with the same window,
// selector and limit rules as the SQLite archive.
func (s *Store) ReadFileEvidenceSnapshot(ctx context.Context, id string, w fileevidence.Window) (fileevidence.Snapshot, error) {
	if s.hostedRelations {
		return fileevidence.Snapshot{}, db.ErrDatabaseIDMissing
	}
	tx, err := s.evidenceTx(ctx)
	if err != nil {
		return fileevidence.Snapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	snap, has, err := pgEvidenceHeader(ctx, tx, id)
	if err != nil {
		return snap, err
	}
	if w.Binding != "" && w.Binding != snap.Binding {
		return snap, db.ErrFileEvidenceStale
	}
	if w.Binding == "" && (w.Offset > 0 || w.OccurrenceID != "") {
		return snap, db.ErrFileEvidenceStale
	}
	snap.Rows = []fileevidence.Occurrence{}
	if !has || snap.Coverage.Version == 0 {
		return snap, nil
	}
	where := "session_id = $1"
	args := []any{id}
	add := func(clause string, v any) {
		args = append(args, v)
		where += fmt.Sprintf(" AND "+clause, len(args))
	}
	if w.FirstOrdinal != nil {
		add("ordinal >= $%d", *w.FirstOrdinal)
	}
	if w.LastOrdinal != nil {
		add("ordinal <= $%d", *w.LastOrdinal)
	}
	if w.OccurrenceID != "" {
		ordinal, slot, err := db.DecodeFileEvidenceOccurrence(w.OccurrenceID, snap.Binding)
		if err != nil {
			return snap, err
		}
		add("ordinal = $%d", ordinal)
		add("slot = $%d", slot)
	}
	from := " FROM message_file_evidence WHERE " + where
	if err := tx.QueryRowContext(ctx, "SELECT count(*)"+from, args...).Scan(&snap.Total); err != nil {
		return snap, err
	}
	limit := w.Limit
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 50)
	offset := max(w.Offset, 0)
	query := fmt.Sprintf("SELECT ordinal, %s%s ORDER BY ordinal, slot LIMIT $%d OFFSET $%d",
		pgEvidenceCols, from, len(args)+1, len(args)+2)
	rows, err := tx.QueryContext(ctx, query, append(args, limit, offset)...)
	if err != nil {
		return snap, err
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int
		r, err := scanPGEvidence(rows, &ordinal)
		if err != nil {
			return snap, err
		}
		snap.Rows = append(snap.Rows, fileevidence.Occurrence{MessageOrdinal: ordinal, Record: r})
	}
	if offset+len(snap.Rows) < snap.Total {
		snap.NextOffset = new(offset + len(snap.Rows))
	}
	return snap, rows.Err()
}

// ReadMessagesWithFileEvidence reads the ordinary message window, then
// attaches evidence from a snapshot whose transcript revision matches the
// one observed with the messages. A revision race retries rather than
// pairing messages with another revision's evidence.
func (s *Store) ReadMessagesWithFileEvidence(ctx context.Context, id string, w db.MessageWindow) (db.FileEvidenceMessagePage, error) {
	if s.hostedRelations {
		return db.FileEvidenceMessagePage{}, db.ErrDatabaseIDMissing
	}
	caller := w.ObservedRevision
	for range 3 {
		observed := ""
		w.ObservedRevision = &observed
		msgs, err := s.GetMessagesWindow(ctx, id, w)
		if err != nil {
			return db.FileEvidenceMessagePage{}, err
		}
		page, retry, err := s.attachFileEvidence(ctx, id, msgs, observed)
		if err != nil || retry {
			if err != nil {
				return db.FileEvidenceMessagePage{}, err
			}
			continue
		}
		if caller != nil {
			*caller = page.Revision
		}
		return page, nil
	}
	return db.FileEvidenceMessagePage{}, db.ErrFileEvidenceStale
}

func (s *Store) attachFileEvidence(ctx context.Context, id string, msgs []db.Message, observed string) (db.FileEvidenceMessagePage, bool, error) {
	tx, err := s.evidenceTx(ctx)
	if err != nil {
		return db.FileEvidenceMessagePage{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	h, has, err := pgEvidenceHeader(ctx, tx, id)
	if err != nil {
		return db.FileEvidenceMessagePage{}, false, err
	}
	if len(msgs) > 0 && observed != h.Revision {
		return db.FileEvidenceMessagePage{}, true, nil
	}
	page := db.FileEvidenceMessagePage{
		Messages: msgs, Binding: h.Binding, Revision: h.Revision,
		Coverage: h.Coverage, Evidence: map[int][]fileevidence.Record{},
	}
	if !has || len(msgs) == 0 {
		return page, false, nil
	}
	first, last := msgs[0].Ordinal, msgs[0].Ordinal
	index := make(map[int]int, len(msgs))
	for i, m := range msgs {
		first, last = min(first, m.Ordinal), max(last, m.Ordinal)
		index[m.Ordinal] = i
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT ordinal, "+pgEvidenceCols+` FROM message_file_evidence
		 WHERE session_id = $1 AND ordinal BETWEEN $2 AND $3
		 ORDER BY ordinal, slot`,
		id, first, last,
	)
	if err != nil {
		return page, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int
		r, err := scanPGEvidence(rows, &ordinal)
		if err != nil {
			return page, false, err
		}
		i, ok := index[ordinal]
		if !ok {
			continue // a role-filtered window skips this message
		}
		msgs[i].FileEvidence = append(msgs[i].FileEvidence, r)
		page.Evidence[ordinal] = append(page.Evidence[ordinal], r)
	}
	return page, false, rows.Err()
}
