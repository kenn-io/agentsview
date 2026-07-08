package sync

import (
	"runtime/debug"

	"go.kenn.io/agentsview/internal/db"
)

const recomputeHeapReleaseDefaultThreshold = 256 << 20

var (
	recomputeHeapReleaseThreshold = recomputeHeapReleaseDefaultThreshold
	freeRecomputeHeap             = debug.FreeOSMemory
)

type recomputeHeapReleaser struct {
	pendingBytes int
}

func (r *recomputeHeapReleaser) Account(heapBytes int) {
	r.pendingBytes += heapBytes
	if r.pendingBytes < recomputeHeapReleaseThreshold {
		return
	}
	r.pendingBytes = 0
	freeRecomputeHeap()
}

func recomputeHeapBytes(
	msgs []db.Message, findings []db.SecretFinding,
) int {
	total := 0
	for _, msg := range msgs {
		total += len(msg.Content) + len(msg.ThinkingText) +
			len(msg.Timestamp) + len(msg.Model) + len(msg.TokenUsage) +
			len(msg.ClaudeMessageID) + len(msg.ClaudeRequestID) +
			len(msg.SourceType) + len(msg.SourceSubtype) +
			len(msg.SourceUUID) + len(msg.SourceParentUUID)
		for _, tc := range msg.ToolCalls {
			total += len(tc.ToolName) + len(tc.Category) +
				len(tc.ToolUseID) + len(tc.InputJSON) +
				len(tc.FilePath) + len(tc.SkillName) +
				len(tc.ResultContent) + len(tc.SubagentSessionID)
			for _, ev := range tc.ResultEvents {
				total += len(ev.ToolUseID) + len(ev.AgentID) +
					len(ev.SubagentSessionID) + len(ev.Source) +
					len(ev.Status) + len(ev.Content) +
					len(ev.Timestamp)
			}
		}
	}
	for _, finding := range findings {
		total += len(finding.SessionID) + len(finding.RuleName) +
			len(finding.Confidence) + len(finding.LocationKind) +
			len(finding.RedactedMatch) + len(finding.RulesVersion)
	}
	return total
}
