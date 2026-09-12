package db

import (
	"context"
	"fmt"
	"github.com/uptrace/bun"

	"go.kenn.io/agentsview/internal/secrets"
)

// scanStoredSecretFindingsTx scans canonical stored content using its original
// coordinates, including result events without a surviving tool call.
func scanStoredSecretFindingsTx(
	ctx context.Context, tx bun.Tx, sessionID string,
) ([]SecretFinding, int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT 'message', ordinal, NULL, NULL, content
		FROM messages WHERE session_id = ?
		UNION ALL
		SELECT 'tool_input', m.ordinal, COALESCE(tc.call_index, 0), NULL,
		       COALESCE(tc.input_json, '')
		FROM tool_calls tc JOIN messages m ON m.id = tc.message_id
		WHERE tc.session_id = ?
		UNION ALL
		SELECT 'tool_result', m.ordinal, COALESCE(tc.call_index, 0), NULL,
		       COALESCE(tc.result_content, '')
		FROM tool_calls tc JOIN messages m ON m.id = tc.message_id
		WHERE tc.session_id = ? AND NOT EXISTS (
		 SELECT 1 FROM tool_result_events ev
		 WHERE ev.session_id = tc.session_id
		   AND ev.tool_call_message_ordinal = m.ordinal
		   AND ev.call_index = COALESCE(tc.call_index, 0))
		UNION ALL
		SELECT 'tool_result_event', tool_call_message_ordinal, call_index,
		       event_index, content
		FROM tool_result_events WHERE session_id = ?
	`, sessionID, sessionID, sessionID, sessionID)
	if err != nil {
		return nil, 0, fmt.Errorf("querying stripped session for secret scan: %w", err)
	}
	defer rows.Close()
	findings := make([]SecretFinding, 0)
	definiteCount := 0
	for rows.Next() {
		var source SecretFinding
		var content string
		if err := rows.Scan(&source.LocationKind, &source.MessageOrdinal,
			&source.CallIndex, &source.EventIndex, &content); err != nil {
			return nil, 0, err
		}
		for _, match := range secrets.Scan(content) {
			finding := source
			finding.SessionID = sessionID
			finding.RuleName = match.Rule
			finding.Confidence = match.Confidence
			finding.MatchStart = match.Start
			finding.MatchEnd = match.End
			finding.MatchIndex = match.Index
			finding.RedactedMatch = match.Redacted
			finding.RulesVersion = secrets.RulesVersion()
			findings = append(findings, finding)
			if match.Confidence == secrets.ConfidenceDefinite {
				definiteCount++
			}
		}
	}
	return findings, definiteCount, rows.Err()
}
