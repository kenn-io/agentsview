package service

import (
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	corerecall "go.kenn.io/agentsview/internal/recall"
)

const (
	defaultRecallContextMaxBytes = 4000
	minRecallContextEntryBytes   = 450
)

func NormalizeRecallContextMaxBytes(maxBytes int) (int, error) {
	if maxBytes < 0 {
		return 0, fmt.Errorf("context_max_bytes must be non-negative")
	}
	if maxBytes == 0 {
		return defaultRecallContextMaxBytes, nil
	}
	return maxBytes, nil
}

func ValidateRecallEntryLimit(limit int) error {
	if limit < 0 {
		return fmt.Errorf("limit must be non-negative")
	}
	return nil
}

func BuildRecallContext(
	results []db.RecallResult, maxBytes int, focusText string,
) (string, *RecallContextMeta, error) {
	normalizedMaxBytes, err := NormalizeRecallContextMaxBytes(maxBytes)
	if err != nil {
		return "", nil, err
	}
	block := corerecall.BuildContext(
		toCoreRecallResults(results),
		corerecall.ContextOptions{
			MaxBytes:      normalizedMaxBytes,
			MaxEntryBytes: recallContextMaxEntryBytes(normalizedMaxBytes, len(results)),
			FocusText:     focusText,
		},
	)
	meta := RecallContextMetaFromBlock(block)
	enrichRecallContextMeta(meta, results)
	return block.Text, meta, nil
}

func BuildRecallQuerySummary(results []db.RecallResult) *RecallQuerySummary {
	summary := &RecallQuerySummary{
		Count:             len(results),
		ByType:            map[string]int{},
		ByScope:           map[string]int{},
		ByStatus:          map[string]int{},
		ByProject:         map[string]int{},
		ByAgent:           map[string]int{},
		ByCWD:             map[string]int{},
		ByGitBranch:       map[string]int{},
		ByMatchReason:     map[string]int{},
		ByExtractorMethod: map[string]int{},
		ByModel:           map[string]int{},
		BySourceRun:       map[string]int{},
		BySourceSession:   map[string]int{},
		BySourceEpisode:   map[string]int{},
		ByTransferability: map[string]int{},
		ByProvenanceAudit: map[string]int{},
		ByEvidence:        map[string]int{},
		ByLifecycle:       map[string]int{},
	}
	for _, result := range results {
		countRecallQuerySummaryField(summary.ByType, result.Type)
		countRecallQuerySummaryField(summary.ByScope, result.Scope)
		countRecallQuerySummaryField(summary.ByStatus, result.Status)
		countRecallQuerySummaryField(summary.ByProject, result.Project)
		countRecallQuerySummaryField(summary.ByAgent, result.Agent)
		countRecallQuerySummaryField(summary.ByCWD, result.CWD)
		countRecallQuerySummaryField(summary.ByGitBranch, result.GitBranch)
		if len(result.MatchReasons) == 0 {
			countRecallQuerySummaryField(summary.ByMatchReason, "")
		}
		for _, reason := range result.MatchReasons {
			countRecallQuerySummaryField(summary.ByMatchReason, reason)
		}
		countRecallQuerySummaryField(
			summary.ByExtractorMethod, result.ExtractorMethod,
		)
		countRecallQuerySummaryField(summary.ByModel, result.Model)
		countRecallQuerySummaryField(summary.BySourceRun, result.SourceRunID)
		countRecallQuerySummaryField(
			summary.BySourceSession, result.SourceSessionID,
		)
		countRecallQuerySummaryField(
			summary.BySourceEpisode, result.SourceEpisodeID,
		)
		countRecallQuerySummaryField(
			summary.ByTransferability,
			recallSummaryBoolLabel(
				result.Transferable, "transferable", "not_transferable",
			),
		)
		countRecallQuerySummaryField(
			summary.ByProvenanceAudit,
			recallSummaryBoolLabel(
				result.ProvenanceOK,
				"provenance_ok",
				"provenance_unverified",
			),
		)
		countRecallQuerySummaryField(
			summary.ByEvidence,
			recallSummaryBoolLabel(
				len(result.Evidence) > 0,
				"with_evidence",
				"without_evidence",
			),
		)
		countRecallQuerySummaryField(
			summary.ByLifecycle,
			result.LifecycleBucket(),
		)
	}
	return summary
}

// BuildRecallContextSummary summarizes the ranked results that were actually
// included in an assembled recall context.
func BuildRecallContextSummary(
	results []db.RecallResult,
	meta *RecallContextMeta,
) *RecallQuerySummary {
	if meta == nil {
		return nil
	}
	return BuildRecallQuerySummary(RecallContextResults(results, meta))
}

func RecallContextResults(
	results []db.RecallResult,
	meta *RecallContextMeta,
) []db.RecallResult {
	if meta == nil || len(meta.IncludedIDs) == 0 {
		return nil
	}
	byID := make(map[string]db.RecallResult, len(results))
	for _, result := range results {
		if result.ID != "" {
			byID[result.ID] = result
		}
	}
	included := make([]db.RecallResult, 0, len(meta.IncludedIDs))
	for _, id := range meta.IncludedIDs {
		if result, ok := byID[id]; ok {
			included = append(included, result)
		}
	}
	return included
}

func ValidateRecallContextEntries(
	contextEntries []db.RecallResult,
	meta *RecallContextMeta,
) error {
	if meta == nil {
		return nil
	}
	if contextEntries == nil {
		if len(meta.IncludedIDs) == 0 {
			return nil
		}
		return fmt.Errorf(
			"context_entries ids must match context_meta.included_ids",
		)
	}
	if len(contextEntries) != len(meta.IncludedIDs) {
		return fmt.Errorf(
			"context_entries ids must match context_meta.included_ids",
		)
	}
	for i, recall := range contextEntries {
		if recall.ID != meta.IncludedIDs[i] {
			return fmt.Errorf(
				"context_entries ids must match context_meta.included_ids",
			)
		}
	}
	return nil
}

func countRecallQuerySummaryField(counts map[string]int, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "(none)"
	}
	counts[value]++
}

func recallSummaryBoolLabel(ok bool, trueLabel, falseLabel string) string {
	if ok {
		return trueLabel
	}
	return falseLabel
}

func recallContextMaxEntryBytes(maxBytes int, resultCount int) int {
	if maxBytes <= 0 || resultCount <= 1 {
		return 0
	}
	maxEntryBytes := maxBytes / resultCount
	if maxEntryBytes < minRecallContextEntryBytes &&
		maxBytes >= minRecallContextEntryBytes {
		maxEntryBytes = minRecallContextEntryBytes
	}
	if maxEntryBytes > maxBytes {
		maxEntryBytes = maxBytes
	}
	return maxEntryBytes
}

func RecallContextMetaFromBlock(block corerecall.ContextBlock) *RecallContextMeta {
	if block.EntryCount == 0 && len(block.IncludedIDs) == 0 &&
		!block.Truncated {
		return nil
	}
	return &RecallContextMeta{
		EntryCount:                        block.EntryCount,
		Truncated:                         block.Truncated,
		IncludedIDs:                       block.IncludedIDs,
		SourceSessionIDs:                  block.SourceSessionIDs,
		SourceEpisodeIDs:                  block.SourceEpisodeIDs,
		SourceRunIDs:                      block.SourceRunIDs,
		TruncatedFrom:                     block.TruncatedFrom,
		OmittedCount:                      block.OmittedCount,
		PromptInjectionContext:            block.PromptInjectionContext,
		PromptInjectionContextIDs:         block.PromptInjectionContextIDs,
		PromptInjectionContextReasons:     block.PromptInjectionContextReasons,
		PromptInjectionContextReasonsByID: block.PromptInjectionContextReasonsByID,
	}
}

func enrichRecallContextMeta(
	meta *RecallContextMeta,
	results []db.RecallResult,
) {
	if meta == nil || len(meta.IncludedIDs) == 0 || len(results) == 0 {
		return
	}
	byID := make(map[string]db.RecallResult, len(results))
	for _, result := range results {
		if result.ID != "" {
			byID[result.ID] = result
		}
	}
	typesByID := map[string]string{}
	reasonsByID := map[string][]string{}
	for _, id := range meta.IncludedIDs {
		result, ok := byID[id]
		if !ok {
			continue
		}
		if result.Type != "" {
			typesByID[id] = result.Type
		}
		if len(result.MatchReasons) > 0 {
			reasonsByID[id] = uniqueTrimmedStrings(result.MatchReasons)
		}
	}
	if len(typesByID) > 0 {
		meta.IncludedTypesByID = typesByID
	}
	if len(reasonsByID) > 0 {
		meta.IncludedMatchReasonsByID = reasonsByID
	}
}

func uniqueTrimmedStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func toCoreRecallResults(results []db.RecallResult) []corerecall.Result {
	out := make([]corerecall.Result, 0, len(results))
	for _, result := range results {
		out = append(out, corerecall.Result{
			Entry: corerecall.Entry{
				ID:                  result.ID,
				Type:                result.Type,
				Scope:               result.Scope,
				Status:              result.Status,
				Title:               result.Title,
				Body:                result.Body,
				Trigger:             result.Trigger,
				Confidence:          result.Confidence,
				Uncertainty:         result.Uncertainty,
				Project:             result.Project,
				CWD:                 result.CWD,
				GitBranch:           result.GitBranch,
				Agent:               result.Agent,
				SourceSessionID:     result.SourceSessionID,
				SourceEpisodeID:     result.SourceEpisodeID,
				SourceRunID:         result.SourceRunID,
				SupersedesEntryID:   result.SupersedesEntryID,
				SupersededByEntryID: result.SupersededByEntryID,
				CreatedAt:           result.CreatedAt,
				UpdatedAt:           result.UpdatedAt,
				Evidence:            toCoreRecallEvidence(result.Evidence),
			},
			Score:     result.Score,
			Breakdown: result.ScoreBreakdown,
		})
	}
	return out
}

func toCoreRecallEvidence(evidence []db.RecallEvidence) []corerecall.Evidence {
	out := make([]corerecall.Evidence, 0, len(evidence))
	for _, item := range evidence {
		out = append(out, corerecall.Evidence{
			SessionID:           item.SessionID,
			MessageStartOrdinal: item.MessageStartOrdinal,
			MessageEndOrdinal:   item.MessageEndOrdinal,
			ToolUseID:           item.ToolUseID,
			Snippet:             item.Snippet,
		})
	}
	return out
}
