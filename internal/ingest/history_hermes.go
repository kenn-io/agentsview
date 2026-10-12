package ingest

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// ErrArchiveContentLoss reports a replacement that would discard protected history.
var ErrArchiveContentLoss = errors.New("source fallback cannot replace archived content")

func reconcileHermesHistory(ctx context.Context, result HistoryResult, prior PriorSession, options ContentOptions) (HistoryResult, error) {
	candidate := result.Candidate
	policy := candidate.Parsed.Replacement
	if policy.Content == parser.ContentReplaceSameSource && !sameHermesContentSource(candidate.Session, prior.Session) {
		comparison := candidate
		if !options.ArchiveContent.OmitsToolContent() {
			comparison.Messages, _ = db.ProjectToolResultImagesWithoutWriting(candidate.Messages, options.ToolResultImages, options.ToolResultImageAssetsDir)
			prior.Messages, _ = db.ProjectToolResultImagesWithoutWriting(prior.Messages, options.ToolResultImages, options.ToolResultImageAssetsDir)
		}
		incoming, err := FinalizeRows(ctx, comparison, options)
		if err != nil {
			return HistoryResult{}, err
		}
		_, archived := db.ProjectSessionForStoragePolicy(prior.Session, prior.Messages, options.ArchiveContent)
		if !hermesReplacementIncludesArchived(incoming.Messages, archived) {
			return HistoryResult{}, fmt.Errorf("%w for session %s", ErrArchiveContentLoss, candidate.Session.ID)
		}
	}
	if policy.Usage != parser.UsagePreserveArchived {
		return result, ctx.Err()
	}
	candidate.Parsed.UsageEvents = make([]parser.ParsedUsageEvent, len(prior.UsageEvents))
	for i, event := range prior.UsageEvents {
		candidate.Parsed.UsageEvents[i] = parser.ParsedUsageEvent{
			SessionID: candidate.Parsed.Session.ID, MessageOrdinal: event.MessageOrdinal,
			Source: event.Source, Model: event.Model, ProviderID: event.ProviderID,
			InputTokens: event.InputTokens, OutputTokens: event.OutputTokens,
			CacheCreationInputTokens: event.CacheCreationInputTokens, CacheReadInputTokens: event.CacheReadInputTokens,
			ReasoningTokens: event.ReasoningTokens, Cost: event.Cost,
			CostStatus: event.CostStatus, CostSource: event.CostSource,
			OccurredAt: event.OccurredAt, DedupKey: event.DedupKey,
		}
	}
	candidate.Session.SourceSessionID = prior.Session.SourceSessionID
	candidate.Session.TotalOutputTokens = prior.Session.TotalOutputTokens
	candidate.Session.HasTotalOutputTokens = prior.Session.HasTotalOutputTokens
	candidate.Session.PeakContextTokens = prior.Session.PeakContextTokens
	candidate.Session.HasPeakContextTokens = prior.Session.HasPeakContextTokens
	result.Candidate = candidate
	result.PriorContributed = true
	return result, ctx.Err()
}

type hermesContentSource struct {
	format string
	path   string
}

func sameHermesContentSource(incoming, archived db.Session) bool {
	current, currentKnown := hermesContentSourceForSession(incoming)
	previous, previousKnown := hermesContentSourceForSession(archived)
	return currentKnown && previousKnown && current == previous
}

func hermesContentSourceForSession(session db.Session) (hermesContentSource, bool) {
	if session.FilePath == nil {
		return hermesContentSource{}, false
	}
	filePath := path.Clean(strings.ReplaceAll(*session.FilePath, `\`, "/"))
	physical := parser.ResolveSourceFilePath(filePath)
	format := session.SourceVersion
	if format == "" {
		switch path.Ext(physical) {
		case ".jsonl":
			format = "hermes-jsonl-v1"
		case ".json":
			format = "hermes-json-v1"
		}
	}
	switch format {
	case parser.HermesStateSourceVersion:
		if path.Base(physical) != "state.db" {
			return hermesContentSource{}, false
		}
	case "hermes-jsonl-v1", "hermes-json-v1":
		if path.Base(physical) == "state.db" {
			id := session.ID[strings.LastIndex(session.ID, ":")+1:]
			fileName := id + ".jsonl"
			if format == "hermes-json-v1" {
				fileName = "session_" + id + ".json"
			}
			physical = path.Join(path.Dir(physical), "sessions", fileName)
		}
	default:
		return hermesContentSource{}, false
	}
	return hermesContentSource{format: format, path: physical}, true
}

func hermesReplacementIncludesArchived(incoming, archived []db.Message) bool {
	next := 0
	for _, message := range archived {
		previous := normalizedHermesMessage(message)
		for next < len(incoming) && !normalizedHermesMessage(incoming[next]).includes(previous) {
			next++
		}
		if next == len(incoming) {
			return false
		}
		next++
	}
	return true
}

type hermesArchivedMessage struct {
	Role         string
	Content      string
	ThinkingText string
	IsSystem     bool
	ToolCalls    []db.ToolCall
}

func (message hermesArchivedMessage) includes(archived hermesArchivedMessage) bool {
	incomingCalls, archivedCalls := message.ToolCalls, archived.ToolCalls
	if archived.ThinkingText != "" && message.ThinkingText != archived.ThinkingText {
		return false
	}
	message.ThinkingText, archived.ThinkingText = "", ""
	message.ToolCalls, archived.ToolCalls = nil, nil
	if !reflect.DeepEqual(message, archived) || len(incomingCalls) < len(archivedCalls) {
		return false
	}
	for i, call := range archivedCalls {
		if !hermesToolCallIncludesArchived(incomingCalls[i], call) {
			return false
		}
	}
	return true
}

func hermesToolCallIncludesArchived(incoming, archived db.ToolCall) bool {
	if (archived.ResultContent != "" || archived.ResultContentLength != 0) &&
		(incoming.ResultContent != archived.ResultContent || incoming.ResultContentLength != archived.ResultContentLength) {
		return false
	}
	if len(incoming.ResultEvents) < len(archived.ResultEvents) {
		return false
	}
	for i, event := range archived.ResultEvents {
		if !reflect.DeepEqual(incoming.ResultEvents[i], event) {
			return false
		}
	}
	incoming.ResultContent, archived.ResultContent = "", ""
	incoming.ResultContentLength, archived.ResultContentLength = 0, 0
	incoming.ResultEvents, archived.ResultEvents = nil, nil
	return reflect.DeepEqual(incoming, archived)
}

func normalizedHermesMessage(message db.Message) hermesArchivedMessage {
	if message.Role == "assistant" && message.HasThinking && strings.HasPrefix(message.Content, "[Thinking]\n") {
		reasoning, answer, ok := strings.Cut(strings.TrimPrefix(message.Content, "[Thinking]\n"), "\n[/Thinking]\n")
		if ok {
			message.Content = answer
			if message.ThinkingText == "" {
				message.ThinkingText = reasoning
			}
		}
	}
	message.ToolCalls = append([]db.ToolCall(nil), message.ToolCalls...)
	for i := range message.ToolCalls {
		call := &message.ToolCalls[i]
		call.MessageID = 0
		call.SessionID = ""
		call.Rendering = ""
		value := jsontext.Value(call.InputJSON)
		if err := value.Canonicalize(jsontext.CanonicalizeRawInts(false), jsontext.CanonicalizeRawFloats(false)); err == nil {
			call.InputJSON = string(value)
		}
	}
	return hermesArchivedMessage{
		Role: message.Role, Content: message.Content,
		ThinkingText: message.ThinkingText, IsSystem: message.IsSystem,
		ToolCalls: message.ToolCalls,
	}
}
