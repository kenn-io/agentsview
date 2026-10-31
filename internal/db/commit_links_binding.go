package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type commitBindingQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// resolveCommitSession accepts a namespaced copy only when the archive's
// provider/native-ID ownership records identify one owner. Ownership survives
// trash and permanent deletion independently of session availability.
func resolveCommitSession(ctx context.Context, q commitBindingQuerier, link *CommitLink) (commitSessionInput, error) {
	s, err := readCommitSession(ctx, q, link.SessionID)
	if !errors.Is(err, sql.ErrNoRows) || link.Source != "trailer" {
		return s, err
	}
	agent := "claude"
	if prefix, _, ok := strings.Cut(link.SessionID, ":"); ok {
		agent = prefix
	}
	rows, err := q.QueryContext(ctx, `SELECT session_id FROM raw_archive_sessions
 WHERE parser_id=? AND provider=? ORDER BY session_id LIMIT 2`, link.SessionID, agent)
	if err != nil {
		return s, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return s, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return s, err
	}
	if len(ids) != 1 {
		if len(ids) > 1 {
			link.Ambiguous = true
		}
		return s, sql.ErrNoRows
	}
	link.SessionID = ids[0]
	return readCommitSession(ctx, q, ids[0])
}

// A binding caches only identity resolution. SHA ambiguity belongs to each card.
type commitSessionBinding struct {
	session            commitSessionInput
	missing, ambiguous bool
}

func readCommitSessionBinding(ctx context.Context, q commitBindingQuerier, link CommitLink) (commitSessionBinding, error) {
	identity := CommitLink{Source: link.Source, SessionID: link.SessionID}
	session, err := resolveCommitSession(ctx, q, &identity)
	missing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !missing {
		return commitSessionBinding{}, err
	}
	return commitSessionBinding{session: session, missing: missing, ambiguous: identity.Ambiguous}, nil
}
