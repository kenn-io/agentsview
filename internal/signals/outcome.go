package signals

import (
	"strings"
	"time"
)

// RecencyWindow is the duration within which a session is
// considered still active.
const RecencyWindow = 10 * time.Minute

// OutcomeInput holds the data needed to classify a session's
// outcome. Populated by the caller from DB queries.
type OutcomeInput struct {
	IsAutomated        bool
	MessageCount       int
	EndedWithRole      string // "user" or "assistant"
	FinalFailureStreak int
	LastAssistantText  string
	LastActivity       time.Time
}

// OutcomeResult is the classification result for a session.
type OutcomeResult struct {
	Outcome    string // "completed", "abandoned", "errored", "unknown"
	Confidence string // "high", "medium", "low"
	IsRecent   bool
}

var giveUpPatterns = []string{
	"i'm unable to",
	"i can't proceed",
	"i don't have access",
	"i cannot proceed",
	"i am unable to",
}

// ClassifyOutcome classifies a session's outcome based on its
// metadata. Pure computation, no DB access.
func ClassifyOutcome(in OutcomeInput) OutcomeResult {
	if in.IsAutomated {
		return OutcomeResult{"unknown", "low", false}
	}

	if in.EndedWithRole == "assistant" && hasTerminalAPIErrorText(in.LastAssistantText) {
		if isRecent(in.LastActivity) {
			return OutcomeResult{"unknown", "low", true}
		}
		return OutcomeResult{"errored", "medium", false}
	}

	if in.MessageCount == 2 && in.EndedWithRole == "assistant" {
		return OutcomeResult{"completed", "medium", false}
	}

	if in.MessageCount < 3 {
		return OutcomeResult{"unknown", "low", false}
	}

	if isRecent(in.LastActivity) {
		return OutcomeResult{"unknown", "low", true}
	}

	if in.EndedWithRole == "user" {
		conf := "medium"
		if in.MessageCount >= 10 {
			conf = "high"
		}
		return OutcomeResult{"abandoned", conf, false}
	}

	if in.FinalFailureStreak >= 3 {
		return OutcomeResult{"errored", "medium", false}
	}

	if in.EndedWithRole == "assistant" {
		conf := "medium"
		if hasGiveUpPattern(in.LastAssistantText) {
			conf = "low"
		}
		return OutcomeResult{"completed", conf, false}
	}

	return OutcomeResult{"unknown", "low", false}
}

func isRecent(t time.Time) bool {
	if t.IsZero() {
		return false
	}
	return time.Since(t) < RecencyWindow
}

func hasGiveUpPattern(text string) bool {
	lower := strings.ToLower(text)
	for _, p := range giveUpPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func hasTerminalAPIErrorText(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.HasPrefix(lower, "api error:")
}
