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

// syncKeepingHistory writes a synced chat so stored rows keep their IDs and
// pins whenever the chat only grew, and keeps the previous version in the
// Trash whenever it did not. adjust, when set, runs on the converted rows
// after the archived rows load.
func syncKeepingHistory(ctx context.Context, store db.Store, result parser.ParseResult, adjust func(msgs, archived []db.Message)) (importStatus, error) {
	id := result.Session.ID
	msgs := identityMessages(id, result.Messages)
	existing, err := store.GetSessionFull(ctx, id)
	if err != nil {
		return importNew, err
	}
	if existing == nil {
		_, err := store.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
			Session: importedSession(result.Session), Messages: msgs,
			SkipSignalUpdates: true,
		}})
		if errors.Is(err, db.ErrSessionExcluded) {
			return importSkipped, nil
		}
		return importNew, err
	}
	archived, err := store.GetAllMessages(ctx, id)
	if err != nil {
		return importUpdated, err
	}
	if adjust != nil {
		adjust(msgs, archived)
	}
	stored := storedFormMessages(store, msgs)
	if sameMessages(archived, stored) {
		// Equal rows can still be another branch: identical text, or text
		// that usage-only storage does not keep.
		if len(msgs) < existing.MessageCount || sourceIdentityChanged(archived, stored) {
			return replaceSyncedConversation(ctx, store, result, msgs, true)
		}
		// Equal rows ignore models, so a reply that only gained its model
		// gets it filled in place.
		if rows := missingModelRows(archived, stored); len(rows) > 0 {
			return importUpdated, appendSyncedMessages(ctx, store, importedSession(result.Session), rows)
		}
		if storeArchiveContent(store).UsageOnly() {
			return refreshSyncedTimestamp(ctx, store, importedSession(result.Session))
		}
		return upsertConversation(ctx, store, result, nil, identityMessages)
	}
	// Appends and completed replies keep stored rows, their IDs, and pins. The
	// fill indices count stored-form rows, which usage-only storage thins, so
	// the rows written come from the same slice. A chat with fewer messages
	// than stored is not an append even when its retained rows match.
	if stored != nil && len(stored) >= len(archived) && len(msgs) >= existing.MessageCount {
		if completed, prefix := compareSyncedPrefix(archived, stored[:len(archived)], true); prefix {
			rows := syncedFillRows(archived, stored, completed, true)
			rows = append(rows, stored[len(archived):]...)
			err := appendSyncedMessages(ctx, store, importedSession(result.Session), rows)
			if errors.Is(err, db.ErrSessionExcluded) {
				return importSkipped, nil
			}
			return importUpdated, err
		}
	}
	return replaceSyncedConversation(ctx, store, result, msgs, true)
}

// refreshSyncedTimestamp writes a synced chat's session row when its messages
// match the archive, so the source timestamp stays current.
func refreshSyncedTimestamp(ctx context.Context, store db.Store, session db.Session) (importStatus, error) {
	stored, err := store.GetSessionFull(ctx, session.ID)
	if err != nil {
		return importSkipped, err
	}
	if err := appendSyncedMessages(ctx, store, session, nil); err != nil {
		return importSkipped, err
	}
	if stored != nil && session.EndedAt != nil && !ptrEqual(stored.EndedAt, session.EndedAt) {
		return importUpdated, nil
	}
	return importSkipped, nil
}

// sourceIdentityChanged reports whether a stored row and its incoming row
// name different source messages with the same stored content.
func sourceIdentityChanged(archived, incoming []db.Message) bool {
	for i, row := range incoming {
		if i >= len(archived) {
			break
		}
		a, b := archived[i].SourceUUID, row.SourceUUID
		if a != "" && b != "" && a != b {
			return true
		}
	}
	return false
}
