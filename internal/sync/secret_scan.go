package sync

import (
	"sync/atomic"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/secrets"
)

// secretScanBytes counts the content bytes passed to the secret scanner.
// Tests use deltas of it to gate the incremental path: a maintained delta
// must scan no more than the delta's own content.
var secretScanBytes atomic.Int64

// SecretScanBytes returns the total scanned byte count so far. Monotonic.
func SecretScanBytes() int64 {
	return secretScanBytes.Load()
}

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
	add := func(sessionID, loc string, ord int, call, event *int, content string, matches []secrets.Match) {
		secretScanBytes.Add(int64(len(content)))
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
				// The incremental persist path (applySignalDeltaTx) inserts
				// f.RulesVersion verbatim, unlike replaceSecretFindingsTx
				// which overrides it; stamp it here so inline findings are
				// visible to current-version listings.
				RulesVersion: secrets.DefiniteRulesVersion(),
			})
			if m.Confidence == secrets.ConfidenceDefinite {
				definiteCount++
			}
		}
	}
	for _, msg := range msgs {
		add(msg.SessionID, "message", msg.Ordinal, nil, nil,
			msg.Content, scan(msg.Content))
		for ci := range msg.ToolCalls {
			tc := msg.ToolCalls[ci]
			callIdx := ci
			add(msg.SessionID, "tool_input", msg.Ordinal, &callIdx, nil,
				tc.InputJSON, scan(tc.InputJSON))
			if len(tc.ResultEvents) > 0 {
				for ei := range tc.ResultEvents {
					// Store the slice position, which is what the persistence
					// layer (resolveToolResultEvents) writes as event_index.
					// SecretFindingSource reads findings back through the same
					// normalized value, so --reveal can re-locate the source.
					evIdx := ei
					add(msg.SessionID, "tool_result_event", msg.Ordinal,
						&callIdx, &evIdx, tc.ResultEvents[ei].Content,
						scan(tc.ResultEvents[ei].Content))
				}
			} else {
				add(msg.SessionID, "tool_result", msg.Ordinal, &callIdx, nil,
					tc.ResultContent, scan(tc.ResultContent))
			}
		}
	}
	return findings, definiteCount
}
