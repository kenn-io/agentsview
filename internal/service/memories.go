package service

import (
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	corememory "go.kenn.io/agentsview/internal/memory"
)

const (
	defaultMemoryContextMaxBytes = 4000
	minMemoryContextEntryBytes   = 450
)

func NormalizeMemoryContextMaxBytes(maxBytes int) (int, error) {
	if maxBytes < 0 {
		return 0, fmt.Errorf("context_max_bytes must be non-negative")
	}
	if maxBytes == 0 {
		return defaultMemoryContextMaxBytes, nil
	}
	return maxBytes, nil
}

func ValidateMemoryLimit(limit int) error {
	if limit < 0 {
		return fmt.Errorf("limit must be non-negative")
	}
	return nil
}

func BuildMemoryContext(
	results []db.MemoryResult, maxBytes int, focusText string,
) (string, *MemoryContextMeta, error) {
	normalizedMaxBytes, err := NormalizeMemoryContextMaxBytes(maxBytes)
	if err != nil {
		return "", nil, err
	}
	block := corememory.BuildContext(
		toCoreMemoryResults(results),
		corememory.ContextOptions{
			MaxBytes:      normalizedMaxBytes,
			MaxEntryBytes: memoryContextMaxEntryBytes(normalizedMaxBytes, len(results)),
			FocusText:     focusText,
		},
	)
	meta := MemoryContextMetaFromBlock(block)
	enrichMemoryContextMeta(meta, results)
	return block.Text, meta, nil
}

func BuildMemoryQuerySummary(results []db.MemoryResult) *MemoryQuerySummary {
	summary := &MemoryQuerySummary{
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
		countMemoryQuerySummaryField(summary.ByType, result.Type)
		countMemoryQuerySummaryField(summary.ByScope, result.Scope)
		countMemoryQuerySummaryField(summary.ByStatus, result.Status)
		countMemoryQuerySummaryField(summary.ByProject, result.Project)
		countMemoryQuerySummaryField(summary.ByAgent, result.Agent)
		countMemoryQuerySummaryField(summary.ByCWD, result.CWD)
		countMemoryQuerySummaryField(summary.ByGitBranch, result.GitBranch)
		if len(result.MatchReasons) == 0 {
			countMemoryQuerySummaryField(summary.ByMatchReason, "")
		}
		for _, reason := range result.MatchReasons {
			countMemoryQuerySummaryField(summary.ByMatchReason, reason)
		}
		countMemoryQuerySummaryField(
			summary.ByExtractorMethod, result.ExtractorMethod,
		)
		countMemoryQuerySummaryField(summary.ByModel, result.Model)
		countMemoryQuerySummaryField(summary.BySourceRun, result.SourceRunID)
		countMemoryQuerySummaryField(
			summary.BySourceSession, result.SourceSessionID,
		)
		countMemoryQuerySummaryField(
			summary.BySourceEpisode, result.SourceEpisodeID,
		)
		countMemoryQuerySummaryField(
			summary.ByTransferability,
			memorySummaryBoolLabel(
				result.Transferable, "transferable", "not_transferable",
			),
		)
		countMemoryQuerySummaryField(
			summary.ByProvenanceAudit,
			memorySummaryBoolLabel(
				result.ProvenanceOK,
				"provenance_ok",
				"provenance_unverified",
			),
		)
		countMemoryQuerySummaryField(
			summary.ByEvidence,
			memorySummaryBoolLabel(
				len(result.Evidence) > 0,
				"with_evidence",
				"without_evidence",
			),
		)
		countMemoryQuerySummaryField(
			summary.ByLifecycle,
			result.LifecycleBucket(),
		)
	}
	return summary
}

// BuildMemoryContextSummary summarizes the ranked results that were actually
// included in an assembled memory context.
func BuildMemoryContextSummary(
	results []db.MemoryResult,
	meta *MemoryContextMeta,
) *MemoryQuerySummary {
	if meta == nil {
		return nil
	}
	return BuildMemoryQuerySummary(MemoryContextResults(results, meta))
}

func MemoryContextResults(
	results []db.MemoryResult,
	meta *MemoryContextMeta,
) []db.MemoryResult {
	if meta == nil || len(meta.IncludedIDs) == 0 {
		return nil
	}
	byID := make(map[string]db.MemoryResult, len(results))
	for _, result := range results {
		if result.ID != "" {
			byID[result.ID] = result
		}
	}
	included := make([]db.MemoryResult, 0, len(meta.IncludedIDs))
	for _, id := range meta.IncludedIDs {
		if result, ok := byID[id]; ok {
			included = append(included, result)
		}
	}
	return included
}

func ValidateMemoryContextMemories(
	contextMemories []db.MemoryResult,
	meta *MemoryContextMeta,
) error {
	if meta == nil {
		return nil
	}
	if contextMemories == nil {
		if len(meta.IncludedIDs) == 0 {
			return nil
		}
		return fmt.Errorf(
			"context_memories ids must match context_meta.included_ids",
		)
	}
	if len(contextMemories) != len(meta.IncludedIDs) {
		return fmt.Errorf(
			"context_memories ids must match context_meta.included_ids",
		)
	}
	for i, memory := range contextMemories {
		if memory.ID != meta.IncludedIDs[i] {
			return fmt.Errorf(
				"context_memories ids must match context_meta.included_ids",
			)
		}
	}
	return nil
}

func countMemoryQuerySummaryField(counts map[string]int, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "(none)"
	}
	counts[value]++
}

func memorySummaryBoolLabel(ok bool, trueLabel, falseLabel string) string {
	if ok {
		return trueLabel
	}
	return falseLabel
}

func memoryContextMaxEntryBytes(maxBytes int, resultCount int) int {
	if maxBytes <= 0 || resultCount <= 1 {
		return 0
	}
	maxEntryBytes := maxBytes / resultCount
	if maxEntryBytes < minMemoryContextEntryBytes &&
		maxBytes >= minMemoryContextEntryBytes {
		maxEntryBytes = minMemoryContextEntryBytes
	}
	if maxEntryBytes > maxBytes {
		maxEntryBytes = maxBytes
	}
	return maxEntryBytes
}

func MemoryContextMetaFromBlock(block corememory.ContextBlock) *MemoryContextMeta {
	if block.MemoryCount == 0 && len(block.IncludedIDs) == 0 &&
		!block.Truncated {
		return nil
	}
	return &MemoryContextMeta{
		MemoryCount:                       block.MemoryCount,
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

func enrichMemoryContextMeta(
	meta *MemoryContextMeta,
	results []db.MemoryResult,
) {
	if meta == nil || len(meta.IncludedIDs) == 0 || len(results) == 0 {
		return
	}
	byID := make(map[string]db.MemoryResult, len(results))
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

func toCoreMemoryResults(results []db.MemoryResult) []corememory.Result {
	out := make([]corememory.Result, 0, len(results))
	for _, result := range results {
		out = append(out, corememory.Result{
			Memory: corememory.Memory{
				ID:                   result.ID,
				Type:                 result.Type,
				Scope:                result.Scope,
				Status:               result.Status,
				Title:                result.Title,
				Body:                 result.Body,
				Trigger:              result.Trigger,
				Confidence:           result.Confidence,
				Uncertainty:          result.Uncertainty,
				Project:              result.Project,
				CWD:                  result.CWD,
				GitBranch:            result.GitBranch,
				Agent:                result.Agent,
				SourceSessionID:      result.SourceSessionID,
				SourceEpisodeID:      result.SourceEpisodeID,
				SourceRunID:          result.SourceRunID,
				SupersedesMemoryID:   result.SupersedesMemoryID,
				SupersededByMemoryID: result.SupersededByMemoryID,
				CreatedAt:            result.CreatedAt,
				UpdatedAt:            result.UpdatedAt,
				Evidence:             toCoreMemoryEvidence(result.Evidence),
			},
			Score:     result.Score,
			Breakdown: result.ScoreBreakdown,
		})
	}
	return out
}

func toCoreMemoryEvidence(evidence []db.MemoryEvidence) []corememory.Evidence {
	out := make([]corememory.Evidence, 0, len(evidence))
	for _, item := range evidence {
		out = append(out, corememory.Evidence{
			SessionID:           item.SessionID,
			MessageStartOrdinal: item.MessageStartOrdinal,
			MessageEndOrdinal:   item.MessageEndOrdinal,
			ToolUseID:           item.ToolUseID,
			Snippet:             item.Snippet,
		})
	}
	return out
}
