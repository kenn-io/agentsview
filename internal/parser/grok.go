package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

type grokSummary struct {
	Summary       string `json:"summary"`
	FirstPrompt   string `json:"firstPrompt"`
	ModelID       string `json:"modelId"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
	LastActiveAt  string `json:"lastActiveAt"`
	Hostname      string `json:"hostname"`
	NumMessages   int    `json:"numMessages"`
	WorktreeLabel string `json:"worktreeLabel"`
}

type grokSignalMetrics struct {
	TotalOutputTokens int
	PeakContextTokens int
}

func ParseGrokSummary(
	path, projectHint, machine string,
) (ParseResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ParseResult{}, fmt.Errorf("stat %s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ParseResult{}, fmt.Errorf("read %s: %w", path, err)
	}
	var summary grokSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		return ParseResult{}, fmt.Errorf("decode %s: %w", path, err)
	}

	rawID := filepath.Base(filepath.Dir(path))
	if !IsValidSessionID(rawID) {
		return ParseResult{}, fmt.Errorf("invalid grok session id for %s", path)
	}
	signals, err := parseGrokSignals(filepath.Join(filepath.Dir(path), "signals.json"))
	if err != nil {
		return ParseResult{}, err
	}

	project := firstNonEmptyJSONLString(
		strings.TrimSpace(summary.WorktreeLabel),
		strings.TrimSpace(projectHint),
	)
	startedAt := grokParseTime(summary.CreatedAt)
	endedAt := grokEndedAt(summary)
	firstPrompt := strings.TrimSpace(summary.FirstPrompt)
	userMessageCount := 0
	if firstPrompt != "" {
		userMessageCount = 1
	}
	result := ParseResult{
		Session: ParsedSession{
			ID:                 "grok:" + rawID,
			Project:            project,
			Machine:            machine,
			Agent:              AgentGrok,
			SourceSessionID:    rawID,
			SourceVersion:      "grok-summary-v1",
			TranscriptFidelity: "summary",
			FirstMessage: truncate(
				strings.ReplaceAll(firstPrompt, "\n", " "),
				300,
			),
			SessionName:         strings.TrimSpace(summary.Summary),
			StartedAt:           startedAt,
			EndedAt:             endedAt,
			MessageCount:        max(summary.NumMessages, 0),
			UserMessageCount:    userMessageCount,
			CountsAuthoritative: true,
			File: FileInfo{
				Path:  path,
				Size:  info.Size(),
				Mtime: info.ModTime().UnixNano(),
			},
		},
	}
	if signals.TotalOutputTokens > 0 {
		result.Session.TotalOutputTokens = signals.TotalOutputTokens
		result.Session.HasTotalOutputTokens = true
	}
	if signals.PeakContextTokens > 0 {
		result.Session.PeakContextTokens = signals.PeakContextTokens
		result.Session.HasPeakContextTokens = true
	}
	result.Session.aggregateTokenPresenceKnown =
		result.Session.HasTotalOutputTokens ||
			result.Session.HasPeakContextTokens
	return result, nil
}

func parseGrokSignals(path string) (grokSignalMetrics, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return grokSignalMetrics{}, nil
	}
	if err != nil {
		return grokSignalMetrics{}, fmt.Errorf("read %s: %w", path, err)
	}
	if !gjson.ValidBytes(data) {
		return grokSignalMetrics{}, fmt.Errorf("decode %s: invalid json", path)
	}
	return grokSignalMetrics{
		TotalOutputTokens: grokFirstPositiveInt(
			data,
			"tokenUsage.totalOutputTokens",
			"usage.totalOutputTokens",
			"outputTokens",
		),
		PeakContextTokens: grokFirstPositiveInt(
			data,
			"tokenUsage.peakContextTokens",
			"usage.peakContextTokens",
			"peakContextTokens",
			"contextTokens",
		),
	}, nil
}

func grokFirstPositiveInt(data []byte, paths ...string) int {
	for _, path := range paths {
		value := gjson.GetBytes(data, path)
		if !value.Exists() {
			continue
		}
		if n := int(value.Int()); n > 0 {
			return n
		}
	}
	return 0
}

func grokParseTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return ts
}

func grokEndedAt(summary grokSummary) time.Time {
	for _, value := range []string{
		summary.LastActiveAt,
		summary.UpdatedAt,
		summary.CreatedAt,
	} {
		if ts := grokParseTime(value); !ts.IsZero() {
			return ts
		}
	}
	return time.Time{}
}
