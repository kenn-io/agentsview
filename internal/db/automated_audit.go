package db

import (
	"context"
	"database/sql"
	"fmt"
)

const automationAuditPrefixBytes = AutomationEvidencePrefixBytes

// ParentlessWorkerSQL is the stored-row equivalent of parser.PromoteParentlessWorker.
const ParentlessWorkerSQL = `session_kind = 'non-interactive'
 AND COALESCE(parent_session_id, '') = '' AND COALESCE(relationship_type, '') = ''`

func repairParentlessWorkers(ctx context.Context, w *writerHandle) error {
	if _, err := w.Exec(ctx, `UPDATE sessions
		SET relationship_type = 'subagent', local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE `+ParentlessWorkerSQL); err != nil {
		return fmt.Errorf("repairing parentless workers: %w", err)
	}
	return nil
}

type boundedAutomationText struct {
	prefix         sql.RawBytes
	fullByteLength sql.NullInt64
}

func (text boundedAutomationText) evidence() AutomationTextEvidence {
	return AutomationTextEvidence{
		Prefix:         text.prefix,
		FullByteLength: text.fullByteLength.Int64,
		Valid:          text.fullByteLength.Valid,
	}
}

func auditAutomatedFull(ctx context.Context,
	w *writerHandle,
	patterns automationPatternSnapshot,
) (setIDs, clearIDs []string, err error) {
	rows, err := w.Query(ctx,
		`SELECT
			s.id,
			s.session_kind,
			s.first_message,
			s.user_message_count,
			s.is_automated,
			(
				SELECT m.content
				FROM messages m
				WHERE m.session_id = s.id
				  AND m.role = 'user'
				  AND COALESCE(m.source_subtype, '') <> 'tool_result'
				  AND m.is_system = 0
				  AND TRIM(m.content) <> ''
				ORDER BY m.ordinal
				LIMIT 1
			) AS first_user_message
		 FROM sessions s`,
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"querying automated backfill candidates: %w", err,
		)
	}
	defer rows.Close()

	setIDs, clearIDs, err = scanFullAutomationCandidates(rows, patterns)
	if closeErr := rows.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	return setIDs, clearIDs, err
}

func auditAutomatedMatchingHash(ctx context.Context,
	w *writerHandle,
	patterns automationPatternSnapshot,
) (setIDs, clearIDs []string, err error) {
	rows, err := w.Query(ctx,
		`SELECT
			s.id,
			s.session_kind,
			s.user_message_count,
			s.is_automated,
			substr(CAST(first_user.content AS BLOB), 1, ?)
				AS first_user_prefix,
			octet_length(first_user.content) AS first_user_length,
			CASE
				WHEN s.user_message_count <= 1
				 AND s.first_message IS NOT NULL
				THEN substr(CAST(s.first_message AS BLOB), 1, ?)
			END AS first_message_prefix,
			CASE
				WHEN s.user_message_count <= 1
				 AND s.first_message IS NOT NULL
				THEN octet_length(s.first_message)
			END AS first_message_length
		 FROM sessions s
		 LEFT JOIN messages first_user ON first_user.id =
			CASE WHEN s.user_message_count <= 1 THEN (
				SELECT m.id
				FROM messages m
				WHERE m.session_id = s.id
				  AND m.role = 'user'
				  AND COALESCE(m.source_subtype, '') <> 'tool_result'
				  AND m.is_system = 0
				  AND TRIM(m.content) <> ''
				ORDER BY m.ordinal
				LIMIT 1
			) END`,
		automationAuditPrefixBytes,
		automationAuditPrefixBytes,
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"querying bounded automated audit candidates: %w", err,
		)
	}
	defer rows.Close()

	var unresolved []string
	for rows.Next() {
		var (
			id               string
			sessionKind      string
			userMessageCount int
			rowAutomated     bool
			firstUser        boundedAutomationText
			firstMessage     boundedAutomationText
		)
		if err := rows.Scan(
			&id,
			&sessionKind,
			&userMessageCount,
			&rowAutomated,
			&firstUser.prefix,
			&firstUser.fullByteLength,
			&firstMessage.prefix,
			&firstMessage.fullByteLength,
		); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf(
				"scanning bounded automated audit candidate: %w", err,
			)
		}
		if IsAutomatedSessionMetadata(sessionKind) {
			setIDs, clearIDs = AppendAutomationFlagChange(
				setIDs, clearIDs, id, rowAutomated, true,
			)
			continue
		}

		want, conclusive := patterns.verdictFromEvidence(
			userMessageCount, firstUser.evidence(), firstMessage.evidence(),
		)
		if !conclusive {
			unresolved = append(unresolved, id)
			continue
		}
		setIDs, clearIDs = AppendAutomationFlagChange(
			setIDs, clearIDs, id, rowAutomated, want,
		)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}

	err = queryChunked(unresolved, func(ids []string) error {
		placeholders, args := inPlaceholders(ids)
		fullRows, err := w.Query(ctx,
			`SELECT
				s.id,
				s.session_kind,
				s.first_message,
				s.user_message_count,
				s.is_automated,
				(
					SELECT m.content
					FROM messages m
					WHERE m.session_id = s.id
					  AND m.role = 'user'
					  AND COALESCE(m.source_subtype, '') <> 'tool_result'
					  AND m.is_system = 0
					  AND TRIM(m.content) <> ''
					ORDER BY m.ordinal
					LIMIT 1
				) AS first_user_message
			 FROM sessions s
			 WHERE s.id IN `+placeholders,
			args...,
		)
		if err != nil {
			return fmt.Errorf(
				"querying unresolved automated audit candidates: %w", err,
			)
		}
		defer fullRows.Close()
		batchSet, batchClear, scanErr := scanFullAutomationCandidates(
			fullRows, patterns,
		)
		if closeErr := fullRows.Close(); scanErr == nil && closeErr != nil {
			scanErr = closeErr
		}
		if scanErr != nil {
			return scanErr
		}
		setIDs = append(setIDs, batchSet...)
		clearIDs = append(clearIDs, batchClear...)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return setIDs, clearIDs, nil
}

func scanFullAutomationCandidates(
	rows *sql.Rows,
	patterns automationPatternSnapshot,
) (setIDs, clearIDs []string, err error) {
	for rows.Next() {
		var (
			id           string
			sessionKind  string
			firstMessage sql.NullString
			firstUser    sql.NullString
			userCount    int
			rowAutomated bool
		)
		if err := rows.Scan(
			&id, &sessionKind,
			&firstMessage, &userCount, &rowAutomated, &firstUser,
		); err != nil {
			return nil, nil, fmt.Errorf(
				"scanning automated audit candidate: %w", err,
			)
		}
		want := IsAutomatedSessionMetadata(sessionKind) ||
			patterns.matchesTextCandidates(
				userCount, firstUser, firstMessage,
			)
		setIDs, clearIDs = AppendAutomationFlagChange(
			setIDs, clearIDs, id, rowAutomated, want,
		)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return setIDs, clearIDs, nil
}

// AppendAutomationFlagChange adds id to setIDs or clearIDs when the stored
// automated flag differs from want.
func AppendAutomationFlagChange(
	setIDs, clearIDs []string,
	id string,
	rowAutomated, want bool,
) ([]string, []string) {
	if want && !rowAutomated {
		setIDs = append(setIDs, id)
	} else if !want && rowAutomated {
		clearIDs = append(clearIDs, id)
	}
	return setIDs, clearIDs
}
