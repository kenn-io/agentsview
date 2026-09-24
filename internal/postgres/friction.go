package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// pushFrictionFindings replaces one session's findings in the push
// transaction. PostgreSQL created_at records the push time.
func (s *Sync) pushFrictionFindings(
	ctx context.Context, tx *sql.Tx, sessionID string,
) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM friction_findings WHERE session_id = $1`, sessionID)
	if err != nil {
		return false, fmt.Errorf("deleting pg friction_findings for %s: %w", sessionID, err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("counting deleted friction_findings for %s: %w", sessionID, err)
	}
	findings, err := s.local.SessionFrictionFindings(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("reading local friction_findings for %s: %w", sessionID, err)
	}
	if len(findings) == 0 {
		return deleted > 0, nil
	}
	const batch = 50
	const cols = 14
	for i := 0; i < len(findings); i += batch {
		chunk := findings[i:min(i+batch, len(findings))]
		var b strings.Builder
		b.WriteString(`INSERT INTO friction_findings (
			session_id, kind, detector, message_ordinal, call_index,
			tool_name, label, text, evidence, title, fingerprint,
			occurred_at, seq, rules_version) VALUES `)
		args := make([]any, 0, len(chunk)*cols)
		for j, f := range chunk {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('(')
			for k := range cols {
				if k > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, "$%d", j*cols+k+1)
			}
			b.WriteByte(')')
			var occurred any
			if f.OccurredAt != nil {
				occurred = f.OccurredAt.UTC()
			}
			args = append(args,
				f.SessionID, f.Kind, f.Detector, f.MessageOrdinal, f.CallIndex,
				sanitizePG(f.ToolName), sanitizePG(f.Label), sanitizePG(f.Text),
				sanitizePG(f.Evidence), sanitizePG(f.Title), f.Fingerprint,
				occurred, f.Seq, f.RulesVersion,
			)
		}
		if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
			return false, fmt.Errorf("bulk inserting friction_findings for %s: %w", sessionID, err)
		}
	}
	return true, nil
}

// pushFrictionDims replaces one session's dimensions row in the push
// transaction and reports whether a row was inserted or deleted.
func (s *Sync) pushFrictionDims(
	ctx context.Context, tx *sql.Tx, sessionID string,
) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM friction_session_dims WHERE session_id = $1`, sessionID)
	if err != nil {
		return false, fmt.Errorf("deleting pg friction_session_dims for %s: %w", sessionID, err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("counting deleted friction_session_dims for %s: %w", sessionID, err)
	}
	dims, err := s.local.SessionFrictionDims(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("reading local friction_session_dims for %s: %w", sessionID, err)
	}
	if dims == nil {
		return deleted > 0, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO friction_session_dims (
			session_id, seat, persona, channel, dims_source, review_excluded
		) VALUES ($1, $2, $3, $4, $5, $6)`,
		sessionID, sanitizePG(dims.Seat), sanitizePG(dims.Persona),
		sanitizePG(dims.Channel), sanitizePG(dims.DimsSource), dims.ReviewExcluded,
	); err != nil {
		return false, fmt.Errorf("inserting friction_session_dims for %s: %w", sessionID, err)
	}
	return true, nil
}
