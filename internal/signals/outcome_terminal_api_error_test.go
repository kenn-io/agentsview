package signals

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestClassifyOutcome_TerminalRawAPIError(t *testing.T) {
	got := ClassifyOutcome(OutcomeInput{
		MessageCount:      4,
		EndedWithRole:     "assistant",
		LastAssistantText: "API Error: Unable to connect to API (ConnectionRefused)",
		LastActivity:      time.Now().Add(-time.Hour),
	})

	assert.Equal(t, OutcomeResult{
		Outcome:    "errored",
		Confidence: "medium",
	}, got)
}

func TestClassifyOutcome_TerminalRawAPIErrorTwoMessageSession(t *testing.T) {
	got := ClassifyOutcome(OutcomeInput{
		MessageCount:      2,
		EndedWithRole:     "assistant",
		LastAssistantText: "API Error: Unable to connect to API (ConnectionRefused)",
		LastActivity:      time.Now().Add(-time.Hour),
	})

	assert.Equal(t, OutcomeResult{
		Outcome:    "errored",
		Confidence: "medium",
	}, got)
}

func TestClassifyOutcome_TerminalRawAPIErrorSingleMessageSession(t *testing.T) {
	got := ClassifyOutcome(OutcomeInput{
		MessageCount:      1,
		EndedWithRole:     "assistant",
		LastAssistantText: "API Error: Unable to connect to API (ConnectionRefused)",
		LastActivity:      time.Now().Add(-time.Hour),
	})

	assert.Equal(t, OutcomeResult{
		Outcome:    "errored",
		Confidence: "medium",
	}, got)
}

func TestClassifyOutcome_TerminalRawAPIErrorFalsePositive(t *testing.T) {
	got := ClassifyOutcome(OutcomeInput{
		MessageCount:      4,
		EndedWithRole:     "assistant",
		LastAssistantText: "The final line was `API Error: Unable to connect to API`, but the retry succeeded and here is the answer.",
		LastActivity:      time.Now().Add(-time.Hour),
	})

	assert.Equal(t, OutcomeResult{
		Outcome:    "completed",
		Confidence: "medium",
	}, got)
}

func TestClassifyOutcome_RecentTwoMessageAssistantSessionStillCompleted(t *testing.T) {
	got := ClassifyOutcome(OutcomeInput{
		MessageCount:      2,
		EndedWithRole:     "assistant",
		LastAssistantText: "Here is the answer.",
		LastActivity:      time.Now(),
	})

	assert.Equal(t, OutcomeResult{
		Outcome:    "completed",
		Confidence: "medium",
	}, got)
}
