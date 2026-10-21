package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/fileevidence"
)

// FileEvidenceBinding derives the opaque binding for one archive generation,
// session and transcript revision. Every store that serves file evidence
// must use it so continuation tokens mean the same thing everywhere.
func FileEvidenceBinding(generation, sessionID, revision string) string {
	var tuple strings.Builder
	for _, v := range []string{generation, sessionID, revision} {
		tuple.WriteString(strconv.Itoa(len(v)) + ":" + v)
	}
	sum := sha256.Sum256([]byte(tuple.String()))
	return "v1." + base64.RawURLEncoding.EncodeToString(sum[:])
}

// ParseFileEvidenceSlot validates a stored canonical slot.
func ParseFileEvidenceSlot(slot string) (fileevidence.Position, error) {
	return parseEvidenceSlot(slot)
}

// EncodeFileEvidenceOccurrence returns the opaque selector for one
// occurrence under a binding; DecodeFileEvidenceOccurrence reverses it.
func EncodeFileEvidenceOccurrence(ordinal int, slot, binding string) string {
	raw, _ := json.Marshal([]any{ordinal, slot, binding})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeFileEvidenceOccurrence validates an opaque occurrence selector
// against the current binding and returns its message ordinal and slot.
func DecodeFileEvidenceOccurrence(id, binding string) (int, string, error) {
	if len(id) > 4096 {
		return 0, "", ErrFileEvidenceStale
	}
	raw, err := base64.RawURLEncoding.DecodeString(id)
	var selector []jsontext.Value
	if err != nil || json.Unmarshal(raw, &selector) != nil || len(selector) != 3 {
		return 0, "", ErrFileEvidenceStale
	}
	var ordinal int
	var slot, got string
	if json.Unmarshal(selector[0], &ordinal) != nil || json.Unmarshal(selector[1], &slot) != nil ||
		json.Unmarshal(selector[2], &got) != nil || got != binding || len(slot) > 1024 || ordinal < 0 {
		return 0, "", ErrFileEvidenceStale
	}
	if _, err := parseEvidenceSlot(slot); err != nil {
		return 0, "", ErrFileEvidenceStale
	}
	return ordinal, slot, nil
}

// FileEvidenceExport is one session's committed evidence, read in a single
// snapshot, for transport to a serving replica.
type FileEvidenceExport struct {
	Coverage fileevidence.Coverage
	// Digest covers coverage and every row, so a replica can skip unchanged
	// sessions without comparing rows.
	Digest string
	Rows   []fileevidence.Occurrence
}

// ExportFileEvidence returns a session's evidence rows, coverage and digest.
// Archives without the evidence table export unavailable coverage.
func (db *DB) ExportFileEvidence(ctx context.Context, id string) (FileEvidenceExport, error) {
	out := FileEvidenceExport{Coverage: fileevidence.Coverage{State: "unavailable"}, Rows: []fileevidence.Occurrence{}}
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	h := sha256.New()
	has, err := hasFileEvidence(ctx, tx)
	if err != nil {
		return out, err
	}
	if !has {
		writeStagedDigestInt(h, 0)
		writeStagedDigestString(h, "unavailable")
		out.Digest = hex.EncodeToString(h.Sum(nil))
		return out, nil
	}
	if err := digestFileEvidenceTx(ctx, tx, id, h); err != nil {
		return out, err
	}
	out.Digest = hex.EncodeToString(h.Sum(nil))
	if err := tx.QueryRowContext(ctx, "SELECT file_evidence_version,file_evidence_coverage FROM sessions WHERE id=?", id).Scan(&out.Coverage.Version, &out.Coverage.State); err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT m.ordinal,e."+strings.ReplaceAll(evidenceCols, ",", ",e.")+" FROM messages m JOIN message_file_evidence e ON e.message_id=m.id WHERE m.session_id=? ORDER BY m.ordinal,e.slot", id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int
		r, _, err := scanEvidence(rows, &ordinal)
		if err != nil {
			return out, err
		}
		out.Rows = append(out.Rows, fileevidence.Occurrence{MessageOrdinal: ordinal, Record: r})
	}
	return out, rows.Err()
}
