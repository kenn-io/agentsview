package sync

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

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
	if result.ContentReplacement == parser.ContentPreserveRicherArchive && stored.SourceVersion != parser.HermesStateSourceVersion {
		archived, err := store.GetAllMessages(ctx, id)
		if err != nil {
			return err
		}
		var incoming, previous hermesContentExtent
		for _, message := range result.Messages {
			incoming.addMessage(message.Content, message.ThinkingText, message.HasToolUse || len(message.ToolCalls) > 0, message.HasThinking)
		}
		for _, message := range archived {
			previous.addMessage(message.Content, message.ThinkingText, message.HasToolUse, message.HasThinking)
		}
		if !incoming.includes(previous) {
			return fmt.Errorf("hermes database fallback cannot replace richer archived history for session %s", id)
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

type hermesContentExtent struct {
	messages, textBytes, toolUses, thinkingMessages int
}

func (e *hermesContentExtent) addMessage(content, thinking string, toolUse, hasThinking bool) {
	e.messages++
	e.textBytes += len(content) + len(thinking)
	if toolUse {
		e.toolUses++
	}
	if hasThinking {
		e.thinkingMessages++
	}
}

func (e *hermesContentExtent) includes(other hermesContentExtent) bool {
	return e.messages >= other.messages && e.textBytes >= other.textBytes &&
		e.toolUses >= other.toolUses && e.thinkingMessages >= other.thinkingMessages
}
