package importer

import (
	"context"
	"errors"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// syncReplacing writes a synced chat by replacing its stored messages when
// they differ. It keeps a Trash copy only when the replacement would lose a
// pin or note. Rows keep no models or source IDs.
func syncReplacing(ctx context.Context, store db.Store, result parser.ParseResult) (importStatus, error) {
	id := result.Session.ID
	existing, err := store.GetSessionFull(ctx, id)
	if err != nil {
		return importNew, err
	}
	if existing == nil {
		_, err := store.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
			Session: importedSession(result.Session), Messages: claudeAIMessages(id, result.Messages),
			SkipSignalUpdates: true,
		}})
		if errors.Is(err, db.ErrSessionExcluded) {
			return importSkipped, nil
		}
		return importNew, err
	}
	msgs := claudeAIMessages(id, result.Messages)
	archived, err := store.GetAllMessages(ctx, id)
	if err != nil {
		return importUpdated, err
	}
	if sameMessages(archived, storedFormMessages(store, msgs)) {
		status, err := upsertConversation(ctx, store, result, nil, claudeAIMessages)
		if refusalReason(err) == RefusalShorterExport {
			// Stored messages already match; update the selected branch's counts
			// without replacing rows that may have pins.
			_, err = store.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
				Session: importedSession(result.Session), Messages: msgs,
				SkipSignalUpdates: true,
			}})
			if errors.Is(err, db.ErrSessionExcluded) {
				return importSkipped, nil
			}
			return importUpdated, err
		}
		return status, err
	}
	return replaceSyncedConversation(ctx, store, result, msgs, false)
}

// replaceSyncedConversation replaces a chat's messages. keepCopy keeps the
// previous version in the Trash; otherwise a copy is kept only when the
// replacement would lose a pin or note.
func replaceSyncedConversation(ctx context.Context, store db.Store, result parser.ParseResult, msgs []db.Message, keepCopy bool) (importStatus, error) {
	replacer, ok := store.(sessionReplacer)
	if !ok {
		return importUpdated, errors.New("store cannot preserve replaced chats")
	}
	_, err := replacer.ReplaceSessionKeepingTrashedCopy(ctx, db.SessionBatchWrite{
		Session: importedSession(result.Session), Messages: msgs,
		KeepTrashedCopyOnlyOnPinLoss: !keepCopy,
	})
	return importUpdated, err
}
