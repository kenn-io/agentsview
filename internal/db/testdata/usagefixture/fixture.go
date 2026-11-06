// Package usagefixture supplies inputs for database and server usage tests.
package usagefixture

import (
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

const (
	ArchiveID        = "usage-page-fixture"
	ArchiveKey       = "0000000000000000000000000000000000000000000000000000000000000001"
	PricingTimestamp = "2026-08-01T00:00:00Z"
)

var Models = []string{"journal-model", "journal-other"}

// Journal keeps 40% of sessions in the last seven days and spreads the rest
// over preceding months. About 3% of one-row sessions span midnight.
func Journal(i, dailyRows int) db.SessionBatchWrite {
	day := time.Date(2026, 8, 4+i%5, 12, 0, 0, 0, time.UTC)
	if i%10 >= 4 {
		day = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC).AddDate(0, 0, -4-i/10%80)
	}
	s := db.SessionBatchWrite{Session: db.Session{ID: fmt.Sprintf("journal-%05d", i), Project: fmt.Sprintf("project-%d", i%4), Machine: fmt.Sprintf("machine-%d", i/4%2), Agent: []string{"codex", "claude"}[i/8%2], GitBranch: fmt.Sprintf("branch-%d", i%2), StartedAt: new(day.Format(time.RFC3339)), UserMessageCount: 1 + i%3, IsAutomated: i%7 == 0}}
	for j := range 3 {
		offset := 0
		if dailyRows == 3 {
			offset = j
		} else if i%33 == 0 && j == 2 {
			offset = 1
		}
		s.Messages = append(s.Messages, db.Message{SessionID: s.Session.ID, Role: "assistant", Ordinal: j, Timestamp: day.AddDate(0, 0, offset).Add(time.Duration(j) * time.Minute).Format(time.RFC3339), Model: Models[i/16%2], TokenUsage: []byte(`{"input_tokens":100,"output_tokens":20}`)})
	}
	return s
}

// PageExceptions include prior-period, authoritative-cost, and shared-identity
// inputs so page comparisons and exception ownership exercise real data.
func PageExceptions() []db.SessionBatchWrite {
	started := "2026-08-03T23:30:00Z"
	return []db.SessionBatchWrite{
		{Session: db.Session{ID: "reported", Project: "project-0", Machine: "machine-0", Agent: "copilot", StartedAt: &started, UserMessageCount: 2}, Messages: []db.Message{
			{SessionID: "reported", Role: "assistant", Ordinal: 0, Timestamp: started, Model: Models[0], TokenUsage: []byte(`{"input_tokens":100,"output_tokens":20}`)},
			{SessionID: "reported", Role: "assistant", Ordinal: 1, Timestamp: "2026-08-04T00:30:00Z", Model: Models[0], TokenUsage: []byte(`{"input_tokens":200,"output_tokens":30}`)},
		}},
		{Session: db.Session{ID: "duplicate-a", Project: "project-0", Agent: "claude", StartedAt: &started, UserMessageCount: 1}, Messages: []db.Message{{SessionID: "duplicate-a", Role: "assistant", Timestamp: started, Model: Models[0], TokenUsage: []byte(`{"output_tokens":10}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"}, {SessionID: "duplicate-a", Role: "assistant", Ordinal: 1, Timestamp: started, Model: Models[0], TokenUsage: []byte(`{"input_tokens":17}`)}}},
		{Session: db.Session{ID: "duplicate-b", Project: "project-1", Agent: "claude", StartedAt: &started, UserMessageCount: 2}, Messages: []db.Message{{SessionID: "duplicate-b", Role: "assistant", Timestamp: started, Model: Models[0], TokenUsage: []byte(`{"output_tokens":20}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"}}},
	}
}

const (
	ReportedCostMicrodollars = 1234567
	ReportedInputTokens      = 10
	ReportedTimestamp        = "2026-08-04T01:00:00Z"
)
