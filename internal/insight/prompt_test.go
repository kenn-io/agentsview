package insight

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestBuildPrompt(t *testing.T) {
	tests := []struct {
		name         string
		req          GenerateRequest
		seed         func(t *testing.T, d *db.DB)
		wantContains []string
		wantNot      []string
		checkPrompt  func(t *testing.T, prompt string)
	}{
		{
			name: "with sessions",
			req: GenerateRequest{
				Type:     "daily_activity",
				DateFrom: "2025-01-15",
				DateTo:   "2025-01-15",
			},
			seed: func(t *testing.T, d *db.DB) {
				dbtest.SeedSession(t, d, "s1", "my-app", func(s *db.Session) {
					s.MessageCount = 5
					s.StartedAt = new("2025-01-15T10:00:00Z")
					s.EndedAt = new("2025-01-15T11:00:00Z")
					s.FirstMessage = new("Fix the login bug")
				})
				dbtest.SeedSession(t, d, "s2", "other-app", func(s *db.Session) {
					s.MessageCount = 3
					s.StartedAt = new("2025-01-15T14:00:00Z")
					s.EndedAt = new("2025-01-15T15:00:00Z")
					s.FirstMessage = new("Add tests")
				})
			},
			wantContains: []string{
				"summarizing a day",
				"Date: 2025-01-15",
				"s1",
				"my-app",
				"Fix the login bug",
				"s2",
				"other-app",
				"Add tests",
			},
		},
		{
			name: "project filter",
			req: GenerateRequest{
				Type:     "daily_activity",
				DateFrom: "2025-01-15",
				DateTo:   "2025-01-15",
				Project:  "my-app",
			},
			seed: func(t *testing.T, d *db.DB) {
				dbtest.SeedSession(t, d, "s1", "my-app", func(s *db.Session) {
					s.MessageCount = 5
					s.StartedAt = new("2025-01-15T10:00:00Z")
					s.EndedAt = new("2025-01-15T11:00:00Z")
				})
				dbtest.SeedSession(t, d, "s2", "other-app", func(s *db.Session) {
					s.MessageCount = 3
					s.StartedAt = new("2025-01-15T14:00:00Z")
					s.EndedAt = new("2025-01-15T15:00:00Z")
				})
			},
			wantContains: []string{"Project: my-app"},
			wantNot:      []string{"other-app"},
		},
		{
			name: "user prompt",
			req: GenerateRequest{
				Type:     "daily_activity",
				DateFrom: "2025-01-15",
				DateTo:   "2025-01-15",
				Prompt:   "Focus on security improvements",
			},
			wantContains: []string{
				"User Query",
				"Prioritize addressing",
				"Focus on security improvements",
			},
		},
		{
			name: "agent analysis",
			req: GenerateRequest{
				Type:     "agent_analysis",
				DateFrom: "2025-01-15",
				DateTo:   "2025-01-15",
			},
			wantContains: []string{"analyzing AI agent"},
		},
		{
			name: "truncation",
			req: GenerateRequest{
				Type:     "daily_activity",
				DateFrom: "2025-01-15",
				DateTo:   "2025-01-15",
			},
			seed: func(t *testing.T, d *db.DB) {
				for i := range 55 {
					dbtest.SeedSession(
						t, d,
						fmt.Sprintf("s%d", i), "my-app",
						func(s *db.Session) {
							s.MessageCount = 1
							s.StartedAt = new("2025-01-15T10:00:00Z")
							s.EndedAt = new(fmt.Sprintf("2025-01-15T11:%02d:00Z", i))
						},
					)
				}
			},
			wantContains: []string{"omitted"},
			checkPrompt: func(t *testing.T, prompt string) {
				count := strings.Count(prompt, "### Session")
				assert.Equal(t, 50, count, "got %d sessions in prompt, want 50", count)
			},
		},
		{
			name: "date range",
			req: GenerateRequest{
				Type:     "daily_activity",
				DateFrom: "2025-01-13",
				DateTo:   "2025-01-17",
			},
			seed: func(t *testing.T, d *db.DB) {
				dbtest.SeedSession(t, d, "s1", "my-app", func(s *db.Session) {
					s.MessageCount = 3
					s.StartedAt = new("2025-01-13T10:00:00Z")
				})
				dbtest.SeedSession(t, d, "s2", "my-app", func(s *db.Session) {
					s.MessageCount = 2
					s.StartedAt = new("2025-01-17T14:00:00Z")
				})
			},
			wantContains: []string{"Date Range: 2025-01-13 to 2025-01-17"},
			wantNot:      []string{"Date: 2025"},
		},
		{
			name: "multi-day summary before sessions",
			req: GenerateRequest{
				Type:     "daily_activity",
				DateFrom: "2025-01-13",
				DateTo:   "2025-01-17",
				Summary: &RangeSummary{
					Sessions:    4,
					PeakAgents:  2,
					TopProjects: []KeyMinutes{{Key: "my-app", AgentMinutes: 90}},
				},
			},
			wantContains: []string{"## Range Summary", "Peak concurrency: 2"},
			checkPrompt: func(t *testing.T, prompt string) {
				header := strings.Index(prompt, "## Date Range")
				summary := strings.Index(prompt, "## Range Summary")
				sessions := strings.Index(prompt, "## Sessions")
				require.Positive(t, summary, "summary block present")
				assert.Less(t, header, summary, "summary after date-range header")
				assert.Less(t, summary, sessions, "summary before sessions block")
			},
		},
		{
			name: "single day omits summary block",
			req: GenerateRequest{
				Type:     "daily_activity",
				DateFrom: "2025-01-15",
				DateTo:   "2025-01-15",
			},
			wantNot: []string{"## Range Summary"},
		},
		{
			name: "date range no sessions",
			req: GenerateRequest{
				Type:     "daily_activity",
				DateFrom: "2025-01-13",
				DateTo:   "2025-01-17",
			},
			wantContains: []string{"date range"},
		},
		{
			name: "no sessions",
			req: GenerateRequest{
				Type:     "daily_activity",
				DateFrom: "2025-01-15",
				DateTo:   "2025-01-15",
			},
			wantContains: []string{"No sessions found"},
		},
		{
			name: "excludes automated sessions",
			req: GenerateRequest{
				Type:     "daily_activity",
				DateFrom: "2025-01-15",
				DateTo:   "2025-01-15",
			},
			seed: func(t *testing.T, d *db.DB) {
				// A normal user session.
				dbtest.SeedSession(t, d, "user-session", "my-app", func(s *db.Session) {
					s.MessageCount = 5
					s.UserMessageCount = 2
					s.StartedAt = new("2025-01-15T10:00:00Z")
					s.EndedAt = new("2025-01-15T11:00:00Z")
					s.FirstMessage = new("Fix the login bug")
				})
				// An automated session: roborev review, single-turn,
				// is_automated must be true.
				dbtest.SeedSession(t, d, "auto-session", "my-app", func(s *db.Session) {
					s.MessageCount = 2
					s.UserMessageCount = 1
					s.StartedAt = new("2025-01-15T12:00:00Z")
					s.EndedAt = new("2025-01-15T12:05:00Z")
					s.FirstMessage = new(
						"You are a code reviewer. Review the diff.",
					)
					s.IsAutomated = true
				})
			},
			wantContains: []string{"user-session", "Fix the login bug"},
			wantNot:      []string{"auto-session", "code reviewer"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := dbtest.OpenTestDB(t)
			ctx := context.Background()

			if tt.seed != nil {
				tt.seed(t, d)
			}

			prompt, err := BuildPrompt(ctx, d, tt.req)
			require.NoError(t, err)

			for _, want := range tt.wantContains {
				assert.Contains(t, prompt, want)
			}
			for _, notWant := range tt.wantNot {
				assert.NotContains(t, prompt, notWant)
			}
			if tt.checkPrompt != nil {
				tt.checkPrompt(t, prompt)
			}
		})
	}
}
