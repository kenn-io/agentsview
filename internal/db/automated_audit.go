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

const parentlessRelationshipSQL = `IIF(sessions.session_kind = 'non-interactive', 'subagent', '')`

// parserRelationshipSQL restores parser provenance, falling back to kind for legacy rows.
const parserRelationshipSQL = `COALESCE(sessions.parser_relationship_type, ` + parentlessRelationshipSQL + `)`

// legacyRelationshipInferredSQL backfills relationship_inferred for rows
// written before the column existed, mirroring relationshipInferred. Stored
// rows do not keep the parser's relationship from before parentless
// promotion, so a Grok child that its summary marked as a subagent cannot be
// told apart from a promoted headless run. Grok rows count as explicit
// children: re-deriving them would need a parser data version bump to reparse
// every Grok source, and a legacy Grok worker stays a subagent as it did
// before automated headless runs were cleared.
const legacyRelationshipInferredSQL = `COALESCE(parser_parent_session_id, '') = ''
 AND substr(id, 1, 6) <> 'agent-'
 AND COALESCE(parser_relationship_type,
	IIF(COALESCE(relationship_type, '') = '' AND session_kind = 'non-interactive', 'subagent', relationship_type)) = 'subagent'
 AND (entrypoint = 'sdk-cli' OR (session_kind = 'non-interactive' AND agent <> 'grok'))`

// legacySpawnLinkedSQL backfills spawn_linked for rows that a spawn edge or an
// applied launcher link already gives a parent.
var legacySpawnLinkedSQL = spawnEdgeExistsSQL("sessions") + `
 OR (COALESCE(parser_parent_session_id, '') = ''
	AND EXISTS (SELECT 1 FROM session_external_parents ep
		WHERE ep.session_id = sessions.id AND ep.parent_session_id = sessions.parent_session_id))`

// clearableHeadlessWorkerSQL reports whether a parentless session counts as
// Automated instead of as a subagent: automated is an SQL boolean for the
// automation verdict, the parser marked it subagent only from its headless
// shape, and no spawn edge or launcher link has ever given it a parent.
func clearableHeadlessWorkerSQL(automated string) string {
	return `(` + automated + `)
	 AND sessions.relationship_inferred = 1
	 AND COALESCE(sessions.parent_session_id, '') = ''
	 AND sessions.spawn_linked = 0
	 AND NOT ` + spawnEdgeExistsSQL("sessions")
}

// unlinkedRelationshipSQL is the relationship a session takes when a launcher
// link stops applying to it and spawnLinked is its spawn record afterwards.
// Callers guarantee there is no spawn edge.
func unlinkedRelationshipSQL(spawnLinked string) string {
	return `IIF(sessions.is_automated = 1 AND sessions.relationship_inferred = 1 AND (` + spawnLinked + `) = 0, '', ` + parserRelationshipSQL + `)`
}

// effectiveParentlessRelationshipSQL is unlinkedRelationshipSQL for a session
// whose spawn record stays as stored.
var effectiveParentlessRelationshipSQL = unlinkedRelationshipSQL("sessions.spawn_linked")

func reconcileParentlessWorkers(ctx context.Context, w *writerHandle, onlyAudited bool) error {
	clearable := clearableHeadlessWorkerSQL("sessions.is_automated = 1")
	if _, err := w.Exec(ctx, `UPDATE sessions
		SET relationship_type = 'subagent', local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE `+ParentlessWorkerSQL+` AND NOT (`+clearable+`)`); err != nil {
		return fmt.Errorf("promoting parentless workers: %w", err)
	}
	auditFilter := ""
	if onlyAudited {
		auditFilter = " AND automation_audit = 1"
	}
	if _, err := w.Exec(ctx, `UPDATE sessions
		SET relationship_type = '',
			parser_relationship_type = COALESCE(parser_relationship_type, 'subagent'),
			local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE relationship_type = 'subagent'
		  AND `+clearable+auditFilter); err != nil {
		return fmt.Errorf("clearing automated parentless workers: %w", err)
	}
	if _, err := w.Exec(ctx, `UPDATE sessions
		SET relationship_type = 'subagent', local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE COALESCE(relationship_type, '') = ''
		  AND COALESCE(parent_session_id, '') = ''
		  AND parser_relationship_type = 'subagent'
		  AND NOT (`+clearable+`)`+auditFilter); err != nil {
		return fmt.Errorf("restoring parentless workers: %w", err)
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
			s.entrypoint,
			s.first_message,
			s.user_message_count,
			s.is_automated,
			(
				SELECT m.content
				FROM messages m
				WHERE m.session_id = s.id
				  AND m.role = 'user'
				  AND m.source_subtype <> 'tool_result'
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

const auditAutomatedMatchingHashSQL = `SELECT
			s.id,
			s.session_kind,
			s.entrypoint,
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
				  AND m.source_subtype <> 'tool_result'
				  AND m.is_system = 0
				  AND TRIM(m.content) <> ''
				ORDER BY m.ordinal
				LIMIT 1
			) END
		 WHERE s.automation_audit = 1`

func auditAutomatedMatchingHash(ctx context.Context,
	w *writerHandle,
	patterns automationPatternSnapshot,
) (setIDs, clearIDs []string, err error) {
	rows, err := w.Query(ctx,
		auditAutomatedMatchingHashSQL,
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
			entrypoint       string
			userMessageCount int
			rowAutomated     bool
			firstUser        boundedAutomationText
			firstMessage     boundedAutomationText
		)
		if err := rows.Scan(
			&id,
			&sessionKind,
			&entrypoint,
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
		if IsAutomatedSessionMetadata(sessionKind, entrypoint) {
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
		placeholders, args := InPlaceholders(ids)
		fullRows, err := w.Query(ctx,
			`SELECT
				s.id,
				s.session_kind,
				s.entrypoint,
				s.first_message,
				s.user_message_count,
				s.is_automated,
				(
					SELECT m.content
					FROM messages m
					WHERE m.session_id = s.id
					  AND m.role = 'user'
					  AND m.source_subtype <> 'tool_result'
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
			entrypoint   string
			firstMessage sql.NullString
			firstUser    sql.NullString
			userCount    int
			rowAutomated bool
		)
		if err := rows.Scan(
			&id, &sessionKind, &entrypoint,
			&firstMessage, &userCount, &rowAutomated, &firstUser,
		); err != nil {
			return nil, nil, fmt.Errorf(
				"scanning automated audit candidate: %w", err,
			)
		}
		want := IsAutomatedSessionMetadata(sessionKind, entrypoint) ||
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
