package sync

import (
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/secrets"
)

// computeSignalsAndSecrets computes a session's signal update and its secret
// findings from the same message slice, returning the update with the
// secret-leak count and rules version already populated. Every sync write
// path uses this so no site can forget to stamp the rules version.
//
// The inline path scans definite rules only (secrets.ScanDefinite) and stamps
// the definite rules version. This keeps the FP-prone, CPU-heavy candidate
// regexes out of the sync hot path; an explicit secrets scan runs the full
// ruleset to add candidate findings. The two versions differ on purpose so
// backfill re-scans inline-only sessions (see secrets.DefiniteRulesVersion).
func computeSignalsAndSecrets(
	s db.Session, msgs []db.Message,
) (db.SessionSignalUpdate, []db.SecretFinding) {
	update := computeSignalsFromMessages(s, msgs)
	findings, leak := scanSecretsFromMessages(s, msgs, secrets.ScanDefinite)
	update.SecretLeakCount = leak
	update.SecretsRulesVersion = secrets.DefiniteRulesVersion()
	return update, findings
}

// scanSecretsFromMessages detects secrets across a session's message content,
// tool inputs, and canonical tool output (result events when present, else
// result_content) using scan: secrets.ScanDefinite for the fast inline path,
// or secrets.Scan for the full explicit scan. Returns the findings and the
// count of definite findings (the secret_leak_count signal). Pure: no DB
// access.
func scanSecretsFromMessages(
	_ db.Session, msgs []db.Message, scan func(string) []secrets.Match,
) (findings []db.SecretFinding, definiteCount int) {
	findings = make([]db.SecretFinding, 0)
	add := func(sessionID, loc string, ord int, call, event *int, matches []secrets.Match) {
		for _, m := range matches {
			findings = append(findings, db.SecretFinding{
				SessionID:      sessionID,
				RuleName:       m.Rule,
				Confidence:     m.Confidence,
				LocationKind:   loc,
				MessageOrdinal: ord,
				CallIndex:      call,
				EventIndex:     event,
				MatchStart:     m.Start,
				MatchEnd:       m.End,
				MatchIndex:     m.Index,
				RedactedMatch:  m.Redacted,
			})
			if m.Confidence == secrets.ConfidenceDefinite {
				definiteCount++
			}
		}
	}
	for _, msg := range msgs {
		add(msg.SessionID, "message", msg.Ordinal, nil, nil,
			scan(msg.Content))
		for ci := range msg.ToolCalls {
			tc := msg.ToolCalls[ci]
			callIdx := ci
			add(msg.SessionID, "tool_input", msg.Ordinal, &callIdx, nil,
				scan(tc.InputJSON))
			if len(tc.ResultEvents) > 0 {
				eventIndexes := db.CanonicalToolResultEventIndexes(tc.ResultEvents)
				for ei := range tc.ResultEvents {
					evIdx := eventIndexes[ei]
					add(msg.SessionID, "tool_result_event", msg.Ordinal,
						&callIdx, &evIdx, scan(tc.ResultEvents[ei].Content))
				}
			} else {
				add(msg.SessionID, "tool_result", msg.Ordinal, &callIdx, nil,
					scan(tc.ResultContent))
			}
		}
	}
	return findings, definiteCount
}
