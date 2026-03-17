package insight

import (
	"context"
	"fmt"
	"strings"

	"github.com/wesm/agentsview/internal/db"
)

const maxSessions = 50

// GenerateRequest describes what insight to generate.
type GenerateRequest struct {
	Type     string
	DateFrom string
	DateTo   string
	Project  string
	Prompt   string
}

// BuildPrompt queries sessions for the given date and assembles
// a prompt for the AI agent.
func BuildPrompt(
	ctx context.Context,
	database db.Store,
	req GenerateRequest,
) (string, error) {
	filter := db.SessionFilter{
		DateFrom: req.DateFrom,
		DateTo:   req.DateTo,
		Limit:    maxSessions + 1,
	}
	if req.Project != "" {
		filter.Project = req.Project
	}

	page, err := database.ListSessions(ctx, filter)
	if err != nil {
		return "", fmt.Errorf("querying sessions: %w", err)
	}

	var b strings.Builder
	writeSystemInstruction(&b, req.Type)
	if req.DateFrom == req.DateTo {
		b.WriteString("\n## Date: ")
		b.WriteString(req.DateFrom)
	} else {
		b.WriteString("\n## Date Range: ")
		b.WriteString(req.DateFrom)
		b.WriteString(" to ")
		b.WriteString(req.DateTo)
	}
	b.WriteString("\n\n")

	if req.Project != "" {
		b.WriteString("## Project: ")
		b.WriteString(req.Project)
		b.WriteString("\n\n")
	}

	sessions := page.Sessions
	truncated := len(sessions) > maxSessions
	if truncated {
		sessions = sessions[:maxSessions]
	}

	b.WriteString("## Sessions\n\n")
	if len(sessions) == 0 {
		if req.DateFrom == req.DateTo {
			b.WriteString(
				"No sessions found for this date.\n",
			)
		} else {
			b.WriteString(
				"No sessions found for this date range.\n",
			)
		}
	} else {
		for i, s := range sessions {
			fmt.Fprintf(&b, "### Session %d\n", i+1)
			fmt.Fprintf(&b, "- ID: %s\n", s.ID)
			fmt.Fprintf(&b, "- Project: %s\n", s.Project)
			fmt.Fprintf(&b, "- Agent: %s\n", s.Agent)
			if s.StartedAt != nil {
				fmt.Fprintf(&b, "- Started: %s\n", *s.StartedAt)
			}
			if s.EndedAt != nil {
				fmt.Fprintf(&b, "- Ended: %s\n", *s.EndedAt)
			}
			fmt.Fprintf(
				&b, "- Messages: %d\n", s.MessageCount,
			)
			if s.FirstMessage != nil {
				fmt.Fprintf(
					&b, "- First message: %s\n",
					truncateString(*s.FirstMessage, 200),
				)
			}
			b.WriteString("\n")
		}
		if truncated {
			fmt.Fprintf(
				&b,
				"(Showing %d of %d sessions; "+
					"remaining sessions omitted)\n\n",
				maxSessions, page.Total,
			)
		}
	}

	if req.Prompt != "" {
		b.WriteString("## User Query\n\n")
		b.WriteString(
			"The user has provided the following " +
				"specific request. Prioritize addressing " +
				"this in your response:\n\n",
		)
		b.WriteString(req.Prompt)
		b.WriteString("\n")
	}

	return b.String(), nil
}

func writeSystemInstruction(b *strings.Builder, typ string) {
	switch typ {
	case "agent_analysis":
		b.WriteString(
			"You are analyzing AI agent sessions. " +
				"Provide deeper analysis of patterns, " +
				"effectiveness, and suggestions for " +
				"improving CLAUDE.md or agent workflows. " +
				"Focus on actionable insights.\n",
		)
	default:
		b.WriteString(
			"You are summarizing a day of AI agent " +
				"activity. Provide a concise markdown " +
				"summary of what was accomplished, " +
				"key decisions made, and notable " +
				"patterns. Group by project if multiple " +
				"projects are present.\n",
		)
	}
}

func truncateString(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}
