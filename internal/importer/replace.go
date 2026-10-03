package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// ImportOptions selects opt-in import behavior. The zero value is the default import.
type ImportOptions struct {
	// Replace lists session IDs whose archived messages the export may replace when the default import refuses them; the previous version moves to the trash as a copy.
	Replace []string
}

func (o ImportOptions) replaces(id string) bool {
	return slices.Contains(o.Replace, id)
}

// ImportClaudeAI imports a Claude.ai export with default options; see ImportClaudeAIWithOptions.
func ImportClaudeAI(
	ctx context.Context,
	store db.Store,
	r io.Reader,
	cb *ImportCallbacks,
	machine ...string,
) (ImportStats, error) {
	return ImportClaudeAIWithOptions(ctx, store, r, cb, ImportOptions{}, machine...)
}

// ImportChatGPT imports a ChatGPT export directory with default options; see ImportChatGPTWithOptions.
func ImportChatGPT(
	ctx context.Context,
	store db.Store,
	dir string,
	assetsDir string,
	cb *ImportCallbacks,
	machine ...string,
) (ImportStats, error) {
	return ImportChatGPTWithOptions(ctx, store, dir, assetsDir, cb, ImportOptions{}, machine...)
}

// sessionReplacer is the archive capability replace mode needs.
type sessionReplacer interface {
	ReplaceSessionKeepingTrashedCopy(ctx context.Context, write db.SessionBatchWrite) (string, error)
}

// replaceable reports whether an explicit replace may override err: only history refusals, never trashed or transient ones.
func replaceable(err error) bool {
	switch refusalReason(err) {
	case RefusalDiverged, RefusalShorterExport:
		return true
	case RefusalTrashed, RefusalTransient:
		return false
	}
	return false
}

func importClaudeAIConversation(
	ctx context.Context,
	store db.Store,
	result parser.ParseResult,
	fts *lazyFTS,
	opts ImportOptions,
) (importStatus, error) {
	status, err := upsertConversation(ctx, store, result, fts)
	if err == nil || !replaceable(err) || !opts.replaces(result.Session.ID) {
		return status, err
	}
	// chatGPTSession builds the same row upsertConversation writes.
	return replaceConversation(ctx, store, parser.AgentClaudeAI, db.SessionBatchWrite{
		Session:  chatGPTSession(result.Session),
		Messages: claudeAIMessages(result.Session.ID, result.Messages),
	}, fts, err)
}

func importChatGPTConversation(
	ctx context.Context,
	store db.Store,
	result parser.ParseResult,
	fts *lazyFTS,
	opts ImportOptions,
) (importStatus, error) {
	status, err := upsertChatGPTConversation(ctx, store, result, fts)
	if err == nil || !replaceable(err) || !opts.replaces(result.Session.ID) {
		return status, err
	}
	return replaceConversation(ctx, store, parser.AgentChatGPT, db.SessionBatchWrite{
		Session:  chatGPTSession(result.Session),
		Messages: chatGPTMessages(result.Session.ID, result.Messages),
	}, fts, err)
}

// replaceConversation replaces a refused session's messages with the export's and keeps the previous version as a trashed copy.
func replaceConversation(
	ctx context.Context,
	store db.Store,
	agent parser.AgentType,
	write db.SessionBatchWrite,
	fts *lazyFTS,
	refused error,
) (importStatus, error) {
	existing, err := store.GetSession(ctx, write.Session.ID)
	if err != nil {
		return importNew, err
	}
	if existing == nil || existing.Agent != string(agent) {
		return importNew, refused
	}
	r, ok := store.(sessionReplacer)
	if !ok {
		return importNew, fmt.Errorf("replace needs a local archive: %w", refused)
	}
	fts.suspend(ctx)
	_, err = r.ReplaceSessionKeepingTrashedCopy(ctx, write)
	switch {
	// A trashed ChatGPT conversation is a skip and a trashed Claude.ai one a refusal, as on the default path.
	case errors.Is(err, db.ErrSessionTrashed) && agent == parser.AgentClaudeAI:
		return importNew, err
	case errors.Is(err, db.ErrReplaceUnchanged),
		errors.Is(err, db.ErrSessionTrashed),
		errors.Is(err, db.ErrSessionExcluded):
		return importSkipped, nil
	case err != nil:
		return importNew, fmt.Errorf("replacing messages: %w", err)
	}
	return importUpdated, nil
}

func claudeAIMessages(
	sessionID string, parsed []parser.ParsedMessage,
) []db.Message {
	msgs := make([]db.Message, len(parsed))
	for i, m := range parsed {
		msgs[i] = db.Message{
			SessionID:     sessionID,
			Ordinal:       m.Ordinal,
			Role:          string(m.Role),
			Content:       m.Content,
			Timestamp:     m.Timestamp.UTC().Format(time.RFC3339Nano),
			ContentLength: m.ContentLength,
		}
	}
	return msgs
}
