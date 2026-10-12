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
		candidate.Messages, _ = e.projectToolResultImagesForPrepare(candidate.Messages)
		if err := e.applyRemoteRewritesContext(ctx, &candidate.Session, candidate.Messages); err != nil {
			return err
		}
		incoming, err := ingest.FinalizeRows(ctx, candidate, options)
		if err != nil {
			return err
		}
		archived, _ = e.projectToolResultImagesForPrepare(archived)
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
		if !ok || !reflect.DeepEqual(candidate, normalizedHermesMessage(message)) {
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

func normalizedHermesMessage(message db.Message) hermesArchivedMessage {
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
