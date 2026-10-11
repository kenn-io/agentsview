package insight

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/stringutil"
)

const maxSessions = 50

// GenerateRequest describes what insight to generate.
type GenerateRequest struct {
	Type           string
	DateFrom       string
	DateTo         string
	Project        string
	Prompt         string
	SessionID      string
	AutomatedScope string
	Summary        *RangeSummary // non-nil only for multi-day ranges
	// MaxPromptBytes caps the prompt for agents that take it as one
	// command-line argument; zero means no cap.
	MaxPromptBytes int
}

// BuildPrompt queries sessions for the given date and assembles
// a prompt for the AI agent. It returns ErrPromptTooLarge when the prompt
// exceeds req.MaxPromptBytes, since it has no evidence to trim.
func BuildPrompt(
	ctx context.Context,
	database db.Store,
	req GenerateRequest,
) (string, error) {
	prompt, err := buildPrompt(ctx, database, req)
	if err == nil && req.MaxPromptBytes > 0 && promptArgSize(prompt) > req.MaxPromptBytes {
		return "", ErrPromptTooLarge
	}
	return prompt, err
}

func buildPrompt(
	ctx context.Context,
	database db.Store,
	req GenerateRequest,
) (string, error) {
	if req.SessionID != "" {
		return buildSessionPrompt(ctx, database, req)
	}
	automatedScope := req.AutomatedScope
	if automatedScope == "" {
		automatedScope = "human"
	}
	filter := db.SessionFilter{
		DateFrom:       req.DateFrom,
		DateTo:         req.DateTo,
		Limit:          maxSessions + 1,
		AutomatedScope: automatedScope,
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

	if req.Summary != nil {
		req.Summary.WriteTo(&b)
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
					stringutil.TruncateRunes(*s.FirstMessage, 200, "..."),
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

func buildSessionPrompt(
	ctx context.Context,
	database db.Store,
	req GenerateRequest,
) (string, error) {
	in, err := loadSessionPromptInput(ctx, database, req.SessionID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	writeSystemInstruction(&b, req.Type)
	writeSessionBody(&b, req.SessionID, in)
	writeUserQuery(&b, req.Prompt)
	return b.String(), nil
}

type sessionPromptInput struct {
	sess   *db.Session
	msgs   []db.Message
	timing *db.SessionTiming
	usage  *db.SessionUsage
}

func loadSessionPromptInput(
	ctx context.Context,
	database db.Store,
	sessionID string,
) (sessionPromptInput, error) {
	sess, err := database.GetSession(ctx, sessionID)
	if err != nil {
		return sessionPromptInput{}, fmt.Errorf("getting session: %w", err)
	}
	if sess == nil {
		return sessionPromptInput{}, fmt.Errorf("session not found: %s", sessionID)
	}
	return loadSessionEvidence(ctx, database, sess)
}

// loadSessionEvidence reads the transcript, timing and usage of a session already looked up.
func loadSessionEvidence(
	ctx context.Context,
	database db.Store,
	sess *db.Session,
) (sessionPromptInput, error) {
	sessionID := sess.ID
	msgs, err := database.GetAllMessages(ctx, sessionID)
	if err != nil {
		return sessionPromptInput{}, fmt.Errorf("getting messages: %w", err)
	}
	timing, err := database.GetSessionTiming(ctx, sessionID)
	if err != nil {
		return sessionPromptInput{}, fmt.Errorf("getting timing: %w", err)
	}
	usage, err := database.GetSessionUsage(ctx, sessionID, false)
	if err != nil {
		return sessionPromptInput{}, fmt.Errorf("getting usage: %w", err)
	}
	return sessionPromptInput{sess: sess, msgs: msgs, timing: timing, usage: usage}, nil
}

const sessionPreviewRunes = 800

// writeSessionBody returns how many messages were cut to a preview.
func writeSessionBody(b *strings.Builder, sessionID string, in sessionPromptInput) int {
	sess, msgs, timing, usage := in.sess, in.msgs, in.timing, in.usage
	fmt.Fprintf(b, "\n## Session: %s\n\n", sessionID)
	fmt.Fprintf(b, "- Project: %s\n", sess.Project)
	fmt.Fprintf(b, "- Agent: %s\n", sess.Agent)
	if sess.StartedAt != nil {
		fmt.Fprintf(b, "- Started: %s\n", *sess.StartedAt)
	}
	if sess.EndedAt != nil {
		fmt.Fprintf(b, "- Ended: %s\n", *sess.EndedAt)
	}
	fmt.Fprintf(b, "- Messages: %d\n", sess.MessageCount)
	if usage != nil && usage.HasTokenData {
		fmt.Fprintf(b, "- Output tokens: %d\n", usage.TotalOutputTokens)
		fmt.Fprintf(b, "- Peak context tokens: %d\n", usage.PeakContextTokens)
	}
	if usage != nil && usage.HasCost {
		fmt.Fprintf(b, "- Cost: %s\n", money.FormatUSD(usage.Cost, money.DisplayCents))
	}
	if timing != nil {
		fmt.Fprintf(b, "- Duration: %.1fs\n", float64(timing.TotalDurationMs)/1000)
		fmt.Fprintf(b, "- Tool calls: %d\n", timing.ToolCallCount)
	}

	previews := 0
	b.WriteString("\n## Messages\n\n")
	if len(msgs) == 0 {
		b.WriteString("No messages found for this session.\n")
	} else {
		for _, m := range msgs {
			if m.IsSystem {
				continue
			}
			fmt.Fprintf(b, "### Message %d: %s\n", m.Ordinal, m.Role)
			if m.Timestamp != "" {
				fmt.Fprintf(b, "- Timestamp: %s\n", m.Timestamp)
			}
			if m.Model != "" {
				fmt.Fprintf(b, "- Model: %s\n", m.Model)
			}
			if hasContext, hasOutput := m.TokenPresence(); hasContext || hasOutput {
				fmt.Fprintf(b, "- Context tokens: %d\n", m.ContextTokens)
				fmt.Fprintf(b, "- Output tokens: %d\n", m.OutputTokens)
			}
			if utf8.RuneCountInString(m.Content) > sessionPreviewRunes {
				previews++
			}
			fmt.Fprintf(b, "\n%s\n\n", stringutil.TruncateRunes(m.Content, sessionPreviewRunes, "..."))
		}
	}
	return previews
}

func writeUserQuery(b *strings.Builder, prompt string) {
	if prompt != "" {
		b.WriteString("## User Query\n\n")
		b.WriteString(prompt)
		b.WriteString("\n")
	}
}

func writeSystemInstruction(b *strings.Builder, typ string) {
	switch typ {
	case ToolEffectivenessType:
		b.WriteString(toolEffectivenessInstruction)
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
