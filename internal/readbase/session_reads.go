package readbase

import (
	"context"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

func (s Sessions) ListStarredSessionIDs(ctx context.Context) ([]string, error) {
	query, args := s.backend.StarredSessionIDsSQL()
	rows, err := s.backend.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, s.backend.ReadError(ListStarredSessions, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s Sessions) ListPinnedMessages(
	ctx context.Context, sessionID string, project string,
) ([]db.PinnedMessage, error) {
	query, args := s.backend.PinnedMessagesSQL(sessionID, project)
	rows, err := s.backend.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, s.backend.ReadError(ListPinnedMessages, err)
	}
	defer rows.Close()
	var pins []db.PinnedMessage
	withContent := sessionID == ""
	for rows.Next() {
		var p db.PinnedMessage
		var created any
		if withContent {
			err = rows.Scan(&p.ID, &p.SessionID, &p.MessageID, &p.Ordinal,
				&p.Note, &created, &p.Content, &p.Role,
				&p.SessionProject, &p.SessionAgent,
				&p.SessionDisplayName, &p.SessionFirstMessage)
		} else {
			err = rows.Scan(&p.ID, &p.SessionID, &p.MessageID,
				&p.Ordinal, &p.Note, &created)
		}
		if err != nil {
			return nil, err
		}
		p.CreatedAt = s.backend.FormatTime(created)
		pins = append(pins, p)
	}
	return pins, rows.Err()
}

func (s Sessions) getMachineMetadata(ctx context.Context, prefix string) (map[string]string, error) {
	query, args := s.backend.GetMachineMetadataSQL(prefix)
	rows, err := s.backend.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, s.backend.ReadError(ReadMachineMetadata, err)
	}
	defer rows.Close()
	metadata := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		metadata[strings.TrimPrefix(key, prefix)] = value
	}
	return metadata, rows.Err()
}

func (s Sessions) ListSecretFindings(
	ctx context.Context, f db.SecretFindingFilter,
) (db.SecretFindingPage, error) {
	if f.Limit <= 0 || f.Limit > db.MaxContentSearchLimit {
		f.Limit = db.DefaultContentSearchLimit
	}
	query, args := s.backend.ListSecretFindingsSQL(f)
	rows, err := s.backend.QueryContext(ctx, query, args...)
	if err != nil {
		return db.SecretFindingPage{}, s.backend.ReadError(ListFindings, err)
	}
	defer rows.Close()
	var out []db.SecretFindingRow
	for rows.Next() {
		var r db.SecretFindingRow
		if err := rows.Scan(&r.SessionID, &r.RuleName, &r.Confidence,
			&r.LocationKind, &r.MessageOrdinal, &r.CallIndex,
			&r.EventIndex, &r.MatchStart, &r.MatchEnd, &r.MatchIndex,
			&r.RedactedMatch, &r.RulesVersion, &r.Project, &r.Agent); err != nil {
			return db.SecretFindingPage{}, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return db.SecretFindingPage{}, err
	}
	page := db.SecretFindingPage{Findings: out}
	if len(out) > f.Limit {
		page.Findings = out[:f.Limit]
		page.NextCursor = f.Cursor + f.Limit
	}
	return page, nil
}

func (s Sessions) GetMachineLabels(ctx context.Context) (map[string]string, error) {
	return s.getMachineMetadata(ctx, db.MachineLabelKeyPrefix)
}

func (s Sessions) GetMachineAliases(ctx context.Context) (map[string]string, error) {
	return s.getMachineMetadata(ctx, db.MachineAliasKeyPrefix)
}

func (s Sessions) SecretFindingSource(
	ctx context.Context, f db.SecretFinding,
) (string, bool, error) {
	msgs, err := s.GetAllMessages(ctx, f.SessionID)
	if err != nil {
		return "", false, err
	}
	text, ok := db.FindingSourceFromMessages(msgs, f)
	return text, ok, nil
}

func (s Sessions) GetMatchingMachineIDs(ctx context.Context, candidates []string) ([]string, error) {
	ids, err := db.MatchingMachineIDs(ctx, s.backend, s.backend.Dialect(), candidates)
	if err != nil {
		return nil, s.backend.ReadError(ReadMachineMetadata, err)
	}
	return ids, nil
}
