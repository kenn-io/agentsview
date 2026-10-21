package ingest

import (
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/fileevidence"
	"go.kenn.io/agentsview/internal/parser"
)

func nativeEvidenceCoverage(agent parser.AgentType) fileevidence.Coverage {
	if agent == parser.AgentClaude || agent == parser.AgentCodex || agent == parser.AgentPi {
		return fileevidence.Coverage{Version: 1, State: "complete"}
	}
	return fileevidence.Coverage{}
}

func nativeEvidenceCandidates(agent parser.AgentType, candidates []fileevidence.Candidate) []fileevidence.Candidate {
	if nativeEvidenceCoverage(agent).Version == 0 {
		return nil
	}
	return append([]fileevidence.Candidate(nil), candidates...)
}

// Move result attachments only to an unambiguous preceding call, before empty
// display carriers are removed. Pi text proofs already identify their call.
func pairFileEvidence(messages []db.Message, blocked map[string]bool) {
	type owner struct{ message, call int }
	owners := map[string][]owner{}
	for i := range messages {
		for j, c := range messages[i].ToolCalls {
			owners[c.ToolUseID] = append(owners[c.ToolUseID], owner{i, j})
		}
	}
	for i := range messages {
		pending := messages[i].FileEvidenceCandidates
		messages[i].FileEvidenceCandidates = nil
		for _, c := range pending {
			dest := i
			if c.ToolUseID != "" {
				matches := owners[c.ToolUseID]
				if len(matches) != 1 || matches[0].message >= i {
					messages[i].FileEvidenceCoverage.State = "withheld"
					continue
				}
				dest = matches[0].message
				c.Call = matches[0].call
				c.Record.Position.Call = matches[0].call
			}
			if (c.Support == "result" || c.Record.Kind == "read") && (c.Call < 0 || c.Call >= len(messages[dest].ToolCalls) || blocked[messages[dest].ToolCalls[c.Call].Category]) {
				messages[dest].FileEvidenceCoverage.State = "withheld"
				continue
			}
			messages[dest].FileEvidenceCandidates = fileevidence.AppendCandidate(messages[dest].FileEvidenceCandidates, c)
		}
	}
}
