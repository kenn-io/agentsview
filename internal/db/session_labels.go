package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrSessionLabelsInvalid identifies invalid label input.
var ErrSessionLabelsInvalid = errors.New("invalid session labels")

const (
	// MaxSessionLabels bounds the labels stored for one session.
	MaxSessionLabels = 64
	// MaxSessionLabelBytes bounds one label's UTF-8 length.
	MaxSessionLabelBytes = 200
)

// SessionLabels is the stored label set for one session.
type SessionLabels struct {
	SessionID string   `json:"session_id"`
	Labels    []string `json:"labels"`
	// SessionFound reports whether the session is already archived. Labels
	// for a session that has not synced yet are kept and appear once it
	// does.
	SessionFound bool `json:"session_found"`
	// Changed reports whether a write altered the stored labels.
	Changed bool `json:"-"`
}

// NormalizeSessionLabel trims a label and validates it. Labels are free
// text; "key=value" is a convention, so a label may not start with "=".
func NormalizeSessionLabel(label string) (string, error) {
	label = strings.TrimSpace(label)
	switch {
	case label == "":
		return "", fmt.Errorf("%w: label is empty", ErrSessionLabelsInvalid)
	case !utf8.ValidString(label):
		return "", fmt.Errorf("%w: label is not valid UTF-8", ErrSessionLabelsInvalid)
	case len(label) > MaxSessionLabelBytes:
		return "", fmt.Errorf(
			"%w: label longer than %d bytes", ErrSessionLabelsInvalid,
			MaxSessionLabelBytes,
		)
	case strings.HasPrefix(label, "="):
		return "", fmt.Errorf(
			"%w: label %q has an empty key", ErrSessionLabelsInvalid, label,
		)
	case strings.IndexFunc(label, unicode.IsControl) >= 0:
		return "", fmt.Errorf(
			"%w: label contains a control character", ErrSessionLabelsInvalid,
		)
	}
	return label, nil
}

// NormalizeSessionLabels normalizes, deduplicates, and sorts labels.
func NormalizeSessionLabels(labels []string) ([]string, error) {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		normalized, err := NormalizeSessionLabel(label)
		if err != nil {
			return nil, err
		}
		out = append(out, normalized)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > MaxSessionLabels {
		return nil, fmt.Errorf(
			"%w: more than %d labels", ErrSessionLabelsInvalid, MaxSessionLabels,
		)
	}
	return out, nil
}

// LabelFilterValues trims label filter values and drops empty ones.
func LabelFilterValues(labels []string) []string {
	var out []string
	for _, label := range labels {
		if label = strings.TrimSpace(label); label != "" {
			out = append(out, label)
		}
	}
	return out
}

func normalizeLabelSessionID(sessionID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", fmt.Errorf("%w: session_id is required", ErrSessionLabelsInvalid)
	}
	return sessionID, nil
}

// GetSessionLabels returns a session's labels, including labels stored for
// a session that has not synced yet.
func (db *DB) GetSessionLabels(
	ctx context.Context, sessionID string,
) (SessionLabels, error) {
	sessionID, err := normalizeLabelSessionID(sessionID)
	if err != nil {
		return SessionLabels{}, err
	}
	labels, err := querySessionLabels(ctx, db.getReader(), sessionID)
	if err != nil {
		return SessionLabels{}, err
	}
	var found int
	if err := db.getReader().QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM sessions WHERE id = ?)", sessionID,
	).Scan(&found); err != nil {
		return SessionLabels{}, fmt.Errorf("checking session %s: %w", sessionID, err)
	}
	return SessionLabels{
		SessionID: sessionID, Labels: labels, SessionFound: found != 0,
	}, nil
}

// ReadSessionLabels returns a session's labels from any store. The local
// archive also reports labels stored for a session that has not synced;
// mirrors carry labels on the session row.
func ReadSessionLabels(
	ctx context.Context, store Store, sessionID string,
) (SessionLabels, error) {
	if local, ok := store.(*DB); ok && local != nil {
		return local.GetSessionLabels(ctx, sessionID)
	}
	sessionID, err := normalizeLabelSessionID(sessionID)
	if err != nil {
		return SessionLabels{}, err
	}
	sess, err := store.GetSession(ctx, sessionID)
	if err != nil {
		return SessionLabels{}, err
	}
	result := SessionLabels{SessionID: sessionID, Labels: []string{}}
	if sess != nil {
		result.SessionFound = true
		result.Labels = append(result.Labels, sess.Labels...)
	}
	return result, nil
}

// SetSessionLabels replaces a session's labels.
func (db *DB) SetSessionLabels(
	ctx context.Context, sessionID string, labels []string,
) (SessionLabels, error) {
	normalized, err := NormalizeSessionLabels(labels)
	if err != nil {
		return SessionLabels{}, err
	}
	return db.writeSessionLabels(ctx, sessionID, func([]string) []string {
		return normalized
	})
}

// UpdateSessionLabels adds and removes labels in one transaction. Removing
// a label the session does not carry is not an error.
func (db *DB) UpdateSessionLabels(
	ctx context.Context, sessionID string, add, remove []string,
) (SessionLabels, error) {
	add, err := NormalizeSessionLabels(add)
	if err != nil {
		return SessionLabels{}, err
	}
	var drop []string
	for _, label := range remove {
		if label = strings.TrimSpace(label); label != "" {
			drop = append(drop, label)
		}
	}
	return db.writeSessionLabels(ctx, sessionID, func(current []string) []string {
		next := slices.DeleteFunc(slices.Clone(current), func(label string) bool {
			return slices.Contains(drop, label)
		})
		next = append(next, add...)
		slices.Sort(next)
		return slices.Compact(next)
	})
}

func (db *DB) writeSessionLabels(
	ctx context.Context, sessionID string, next func([]string) []string,
) (SessionLabels, error) {
	if err := db.requireWritable(); err != nil {
		return SessionLabels{}, err
	}
	sessionID, err := normalizeLabelSessionID(sessionID)
	if err != nil {
		return SessionLabels{}, err
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return SessionLabels{}, fmt.Errorf("beginning session label write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := querySessionLabels(ctx, tx, sessionID)
	if err != nil {
		return SessionLabels{}, err
	}
	labels := next(current)
	if len(labels) > MaxSessionLabels {
		return SessionLabels{}, fmt.Errorf(
			"%w: more than %d labels", ErrSessionLabelsInvalid, MaxSessionLabels,
		)
	}
	result := SessionLabels{SessionID: sessionID, Labels: labels}
	if labels == nil {
		result.Labels = []string{}
	}
	var found int
	if err := tx.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM sessions WHERE id = ?)", sessionID,
	).Scan(&found); err != nil {
		return SessionLabels{}, fmt.Errorf("checking session %s: %w", sessionID, err)
	}
	result.SessionFound = found != 0
	if slices.Equal(current, labels) {
		return result, nil
	}
	result.Changed = true

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM session_labels WHERE session_id = ?", sessionID,
	); err != nil {
		return SessionLabels{}, fmt.Errorf("clearing session labels: %w", err)
	}
	for _, label := range labels {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO session_labels (session_id, label) VALUES (?, ?)",
			sessionID, label,
		); err != nil {
			return SessionLabels{}, fmt.Errorf("saving session label: %w", err)
		}
	}
	// Mirrors select changed sessions by local_modified_at, so a label-only
	// change must bump it to be pushed.
	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ?`, sessionID,
	); err != nil {
		return SessionLabels{}, fmt.Errorf("marking labeled session modified: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SessionLabels{}, fmt.Errorf("committing session labels: %w", err)
	}
	return result, nil
}

func querySessionLabels(
	ctx context.Context, q messageRowsQuerier, sessionID string,
) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT label FROM session_labels WHERE session_id = ? ORDER BY label",
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing session labels: %w", err)
	}
	defer rows.Close()
	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, fmt.Errorf("scanning session label: %w", err)
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}
