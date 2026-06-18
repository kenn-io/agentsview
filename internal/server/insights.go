package server

import (
	"fmt"
	"strings"
)

var validInsightTypes = map[string]bool{
	"daily_activity": true,
	"agent_analysis": true,
}

type generateInsightRequest struct {
	Type     string `json:"type"`
	DateFrom string `json:"date_from"`
	DateTo   string `json:"date_to"`
	Project  string `json:"project,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	Agent    string `json:"agent,omitempty"`
	// Timezone is the IANA zone the caller's date range is expressed in, so
	// the attached activity summary covers the same local-day window as the
	// activity dashboard the dates were derived from. Empty means UTC.
	Timezone string `json:"timezone,omitempty"`
}

func insightGenerateClientMessage(
	agent string, err error,
) string {
	if err == nil {
		return fmt.Sprintf("%s generation failed", agent)
	}
	msg := err.Error()
	// Strip stderr dump after newline for the short client message; full details
	// are in the log stream.
	if idx := strings.Index(msg, "\nstderr:"); idx > 0 {
		msg = msg[:idx]
	}
	if idx := strings.Index(msg, "\nraw:"); idx > 0 {
		msg = msg[:idx]
	}
	return msg
}
