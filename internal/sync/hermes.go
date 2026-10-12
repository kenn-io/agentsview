package sync

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

func (e *Engine) validateHermesReplacement(ctx context.Context, result parser.ParseResult) error {
	session := result.Session
	if session.Agent != parser.AgentHermes && session.Agent != parser.AgentAugureDesktop {
		return nil
	}
	if filepath.Ext(session.File.Path) != ".json" && result.ContentReplacement != parser.ContentPreserveRicherArchive {
		return nil
	}

	store := e.archiveStore
	if store == nil {
		store = e.db
	}
	id := applyIDPrefixToID(e.idPrefix, session.ID)
	stored, err := store.GetSessionFull(ctx, id)
	if err != nil {
		return err
	}
	if stored == nil {
		return nil
	}
	incomingPath := session.File.Path
	if e.pathRewriter != nil {
		incomingPath = e.pathRewriter(incomingPath)
	}
	legacyStandalone := stored.SourceVersion == "" && stored.FilePath != nil &&
		(filepath.Ext(session.File.Path) == ".jsonl" || filepath.Ext(session.File.Path) == ".json") &&
		filepath.Clean(*stored.FilePath) == filepath.Clean(incomingPath)
	needsContentCheck := !legacyStandalone && stored.SourceVersion != session.SourceVersion &&
		(result.PreserveArchivedUsage || stored.SourceVersion != parser.HermesStateSourceVersion)
	if result.ContentReplacement == parser.ContentPreserveRicherArchive && needsContentCheck {
		archived, err := store.GetAllMessages(ctx, id)
		if err != nil {
			return err
		}
		options := ingest.ContentOptions{
			BlockedResultCategories: e.blockedResultCategories,
			ToolResultImages:        e.toolResultImages,
			ArchiveContent:          e.db.ArchiveContent(),
		}
		candidate, err := ingest.PrepareCandidate(ctx, result, options)
		if err != nil {
			return err
		}
		candidate.Messages, _ = e.db.ProjectToolResultImagesForComparison(candidate.Messages, e.toolResultImages)
		if err := e.applyRemoteRewritesContext(ctx, &candidate.Session, candidate.Messages); err != nil {
			return err
		}
		incoming, err := ingest.FinalizeRows(ctx, candidate, options)
		if err != nil {
			return err
		}
		archived, _ = e.db.ProjectToolResultImagesForComparison(archived, e.toolResultImages)
		_, archived = db.ProjectSessionForStoragePolicy(*stored, archived, options.ArchiveContent)
		if !hermesReplacementIncludesArchived(incoming.Messages, archived) {
			return fmt.Errorf("hermes source fallback cannot replace archived content for session %s", id)
		}
	}
	if stored.FilePath == nil || filepath.Ext(*stored.FilePath) != ".jsonl" || filepath.Ext(session.File.Path) != ".json" {
		return nil
	}

	incoming := session.File.Path
	preferred := filepath.Join(filepath.Dir(incoming), strings.TrimPrefix(filepath.Base(incoming), "session_"))
	preferred = strings.TrimSuffix(preferred, ".json") + ".jsonl"
	if e.pathRewriter != nil {
		preferred = e.pathRewriter(preferred)
	}
	if filepath.Clean(*stored.FilePath) != filepath.Clean(preferred) {
		return nil
	}

	return fmt.Errorf("hermes JSON snapshot cannot replace archived JSONL history for session %s", id)
}

func hermesReplacementIncludesArchived(incoming, archived []db.Message) bool {
	byOrdinal := make(map[int]hermesArchivedMessage, len(incoming))
	for _, message := range incoming {
		byOrdinal[message.Ordinal] = normalizedHermesMessage(message)
	}
	for _, message := range archived {
		candidate, ok := byOrdinal[message.Ordinal]
		if !ok || !candidate.includes(normalizedHermesMessage(message)) {
			return false
		}
	}
	return true
}

type hermesArchivedMessage struct {
	Ordinal      int
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
		Ordinal: message.Ordinal, Role: message.Role, Content: message.Content,
		ThinkingText: message.ThinkingText, IsSystem: message.IsSystem,
		ToolCalls: message.ToolCalls,
	}
}

func (e *Engine) preserveHermesUsage(ctx context.Context, result *parser.ParseResult) error {
	if !result.PreserveArchivedUsage || (result.Session.Agent != parser.AgentHermes && result.Session.Agent != parser.AgentAugureDesktop) {
		return nil
	}
	store := e.archiveStore
	if store == nil {
		store = e.db
	}
	id := applyIDPrefixToID(e.idPrefix, result.Session.ID)
	stored, err := store.GetSessionFull(ctx, id)
	if err != nil || stored == nil {
		return err
	}
	type usageReader interface {
		GetUsageEvents(context.Context, string) ([]db.UsageEvent, error)
	}
	reader, ok := store.(usageReader)
	if !ok {
		return fmt.Errorf("archive %T cannot read preserved usage for session %s", store, id)
	}
	events, err := reader.GetUsageEvents(ctx, id)
	if err != nil {
		return err
	}
	result.UsageEvents = make([]parser.ParsedUsageEvent, len(events))
	for i, event := range events {
		result.UsageEvents[i] = parser.ParsedUsageEvent{
			SessionID: result.Session.ID, MessageOrdinal: event.MessageOrdinal,
			Source: event.Source, Model: event.Model, ProviderID: event.ProviderID,
			InputTokens: event.InputTokens, OutputTokens: event.OutputTokens,
			CacheCreationInputTokens: event.CacheCreationInputTokens, CacheReadInputTokens: event.CacheReadInputTokens,
			ReasoningTokens: event.ReasoningTokens, Cost: event.Cost,
			CostStatus: event.CostStatus, CostSource: event.CostSource,
			OccurredAt: event.OccurredAt, DedupKey: event.DedupKey,
		}
	}
	result.Session.SourceSessionID = stored.SourceSessionID
	result.Session.TotalOutputTokens = stored.TotalOutputTokens
	result.Session.HasTotalOutputTokens = stored.HasTotalOutputTokens
	result.Session.PeakContextTokens = stored.PeakContextTokens
	result.Session.HasPeakContextTokens = stored.HasPeakContextTokens
	return nil
}
