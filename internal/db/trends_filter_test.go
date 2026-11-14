package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTrendsSessionFilter(t *testing.T) {
	monday, hour := 0, 12
	f := AnalyticsFilter{
		From: "2025-03-10", To: "2025-03-11", Timezone: "Pacific/Kiritimati",
		Model: "model-a", DayOfWeek: &monday, Hour: &hour,
		Project: "project-a", Agent: "agent-a", Machine: "machine-a", GitBranch: "branch-a",
		MinUserMessages: 2, ExcludeOneShot: true, ExcludeAutomated: true,
		ExcludeInteractive: true, AutomatedScope: "human", IncludeSubagents: true, IncludeForks: true,
		ActiveSince: "2025-03-01T00:00:00Z", Termination: "clean",
		ToolCategory: "tool-category", ToolName: "tool-name", ToolMetric: "tool-metric", EvidenceOffset: 3,
	}
	assert.Equal(t, AnalyticsFilter{
		Timezone: "Pacific/Kiritimati", Project: "project-a", Agent: "agent-a", Machine: "machine-a", GitBranch: "branch-a",
		MinUserMessages: 2, ExcludeOneShot: true, ExcludeAutomated: true,
		ExcludeInteractive: true, AutomatedScope: "human", IncludeSubagents: true, IncludeForks: true,
		ActiveSince: "2025-03-01T00:00:00Z", Termination: "clean",
		ToolCategory: "tool-category", ToolName: "tool-name", ToolMetric: "tool-metric", EvidenceOffset: 3,
	}, f.TrendsSessionFilter())
}
