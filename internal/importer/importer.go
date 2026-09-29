package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/assets"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// ImportStats reports the outcome of an import operation.
type ImportStats struct {
	Imported int `json:"imported"`
	Updated  int `json:"updated"`
	Skipped  int `json:"skipped"`
	Errors   int `json:"errors"`
}

// ImportCallbacks provides optional progress reporting.
type ImportCallbacks struct {
	// OnProgress fires after each conversation with current
	// cumulative stats.
	OnProgress func(ImportStats)
	// OnIndexing fires before the FTS index rebuild starts.
	OnIndexing func()
}

func (c *ImportCallbacks) progress(s ImportStats) {
	if c != nil && c.OnProgress != nil {
		c.OnProgress(s)
	}
}

func (c *ImportCallbacks) indexing() {
	if c != nil && c.OnIndexing != nil {
		c.OnIndexing()
	}
}

// ftsSuspender is optionally implemented by stores that
// support dropping and rebuilding FTS indexes.
type ftsSuspender interface {
	DropFTS(ctx context.Context) error
	RebuildFTS(ctx context.Context) error
}

// lazyFTS suspends FTS triggers on first call to suspend()
// and rebuilds on restore(). If suspend() is never called
// (no message work happened), restore() is a no-op. This
// avoids the expensive FTS rebuild when re-importing an
// unchanged archive.
type lazyFTS struct {
	sus        ftsSuspender
	dropped    bool
	onIndexing func()
}

func newLazyFTS(ctx context.Context,
	store db.Store, onIndexing func(),
) *lazyFTS {
	s, ok := store.(ftsSuspender)
	if !ok || !store.HasFTS(ctx) {
		return nil
	}
	return &lazyFTS{sus: s, onIndexing: onIndexing}
}

func (f *lazyFTS) suspend(ctx context.Context) {
	if f == nil || f.dropped {
		return
	}
	if err := f.sus.DropFTS(ctx); err != nil {
		log.Printf("import: drop FTS: %v", err)
		return
	}
	f.dropped = true
}

func (f *lazyFTS) restore(ctx context.Context) error {
	if f == nil || !f.dropped {
		return nil
	}
	if f.onIndexing != nil {
		f.onIndexing()
	}
	if err := f.sus.RebuildFTS(ctx); err != nil {
		return fmt.Errorf("rebuilding FTS index: %w", err)
	}
	return nil
}

// ImportClaudeAI reads a Claude.ai conversations.json export
// and upserts each conversation into the store. Existing
// sessions are updated (messages replaced) unless the export
// has fewer messages than the archive, which is refused.
// User-renamed display names are preserved. Excluded (deleted)
// sessions are counted as skipped.
func ImportClaudeAI(
	ctx context.Context,
	store db.Store,
	r io.Reader,
	cb *ImportCallbacks,
	machine ...string,
) (stats ImportStats, retErr error) {
	fts := newLazyFTS(ctx, store, cb.indexing)
	defer func() {
		if err := fts.restore(ctx); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()

	provider, ok := parser.NewProvider(
		parser.AgentClaudeAI, parser.ProviderConfig{},
	)
	if !ok {
		return stats, errors.New("claude.ai provider unavailable")
	}
	exporter, ok := provider.(parser.ClaudeAIExportParser)
	if !ok {
		return stats, errors.New("claude.ai provider does not support exports")
	}

	err := exporter.ParseClaudeAIExport(r, func(
		result parser.ParseResult,
	) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		result.Session.Machine = resolvedImportMachine(
			result.Session.Machine, machine,
		)
		status, err := upsertConversation(
			ctx, store, result, fts,
		)
		if err != nil {
			stats.Errors++
			log.Printf(
				"import: skipping %s: %v",
				result.Session.ID, err,
			)
			cb.progress(stats)
			return nil
		}

		switch status {
		case importNew:
			stats.Imported++
		case importUpdated:
			stats.Updated++
		case importSkipped:
			stats.Skipped++
		}

		cb.progress(stats)
		return nil
	})

	retErr = err
	return
}

type importStatus int

const (
	importNew importStatus = iota
	importUpdated
	importSkipped
)

func upsertConversation(
	ctx context.Context,
	store db.Store,
	result parser.ParseResult,
	fts *lazyFTS,
) (importStatus, error) {
	s := result.Session

	msgs := make([]db.Message, len(result.Messages))
	for i, m := range result.Messages {
		msgs[i] = db.Message{
			SessionID:     s.ID,
			Ordinal:       m.Ordinal,
			Role:          string(m.Role),
			Content:       m.Content,
			Timestamp:     m.Timestamp.UTC().Format(time.RFC3339Nano),
			ContentLength: m.ContentLength,
		}
	}

	existing, err := store.GetSession(ctx, s.ID)
	if err != nil {
		return importNew, fmt.Errorf("checking session: %w", err)
	}
	isNew := existing == nil
	// A shorter export (for example an older archive or one with deleted
	// turns) would make the replacement below drop stored messages.
	// Refuse it before touching the session row.
	if existing != nil && len(msgs) < existing.MessageCount {
		return importNew, fmt.Errorf(
			"export has %d messages, archive has %d",
			len(msgs), existing.MessageCount,
		)
	}

	sess := db.Session{
		ID:               s.ID,
		Project:          s.Project,
		Machine:          s.Machine,
		FirstMessage:     strPtr(s.FirstMessage),
		SessionName:      db.ParsedSessionName(s),
		StartedAt:        timeStr(s.StartedAt),
		EndedAt:          timeStr(s.EndedAt),
		MessageCount:     s.MessageCount,
		UserMessageCount: s.UserMessageCount,
	}
	db.ApplyParsedSessionIdentity(&sess, s)

	if err := store.UpsertSession(ctx, sess); err != nil {
		if errors.Is(err, db.ErrSessionExcluded) {
			return importSkipped, nil
		}
		return importNew, fmt.Errorf("upserting session: %w", err)
	}

	// Bump local_modified_at so incremental PG push picks up session_name
	// changes even when the skip path below returns importSkipped (message
	// count unchanged) and ReplaceSessionMessages is never called.
	if localDB, ok := store.(*db.DB); ok {
		if err := localDB.BumpLocalModifiedAt(ctx, s.ID); err != nil {
			log.Printf("import: bumping local_modified_at for %s: %v", s.ID, err)
		}
	}

	// Skip expensive message replacement when the conversation
	// has not changed since the last import. Compare both
	// message count and ended_at (source updated_at) to detect
	// content/metadata changes even when count is unchanged.
	if !isNew && existing != nil && existing.MessageCount == s.MessageCount {
		newEnd := timeStr(s.EndedAt)
		if ptrEqual(existing.EndedAt, newEnd) {
			existingMsgs, err := store.GetAllMessages(ctx, s.ID)
			if err != nil {
				return importNew,
					fmt.Errorf("loading existing messages: %w", err)
			}
			// Compare in stored form: the write path sanitizes and
			// projects rows, so raw parser output can differ from an
			// unchanged archived copy.
			if sameMessages(existingMsgs, storedFormMessages(store, msgs)) {
				return importSkipped, nil
			}
		}
	}

	// Suspend FTS before first message-changing operation to
	// avoid per-row trigger overhead during bulk work.
	fts.suspend(ctx)

	if err := store.ReplaceSessionMessages(ctx, s.ID, msgs); err != nil {
		return importNew, fmt.Errorf("replacing messages: %w", err)
	}

	if isNew {
		return importNew, nil
	}
	return importUpdated, nil
}

// assetResolverAdapter bridges the importer's AssetIndex / CopyAsset
// pair to the parser.AssetResolver interface.
type assetResolverAdapter struct {
	index     AssetIndex
	assetsDir string
}

func (a *assetResolverAdapter) Resolve(
	pointer string,
) (string, bool) {
	return a.index.Resolve(pointer)
}

func (a *assetResolverAdapter) Copy(
	srcPath string,
) (string, error) {
	return assets.CopyAsset(srcPath, a.assetsDir)
}

// ImportChatGPT reads a ChatGPT export directory (containing
// conversations-*.json files) and imports each conversation into
// the store. Existing sessions are extended when the export contains
// their archived messages followed by new ones; see
// upsertChatGPTConversation.
func ImportChatGPT(
	ctx context.Context,
	store db.Store,
	dir string,
	assetsDir string,
	cb *ImportCallbacks,
	machine ...string,
) (stats ImportStats, retErr error) {
	fts := newLazyFTS(ctx, store, cb.indexing)
	defer func() {
		if err := fts.restore(ctx); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()

	index := BuildAssetIndex(dir)
	resolver := &assetResolverAdapter{
		index:     index,
		assetsDir: assetsDir,
	}

	provider, ok := parser.NewProvider(
		parser.AgentChatGPT, parser.ProviderConfig{},
	)
	if !ok {
		return stats, errors.New("chatgpt provider unavailable")
	}
	exporter, ok := provider.(parser.ChatGPTExportParser)
	if !ok {
		return stats, errors.New("chatgpt provider does not support exports")
	}

	err := exporter.ParseChatGPTExport(dir, resolver,
		func(result parser.ParseResult) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			result.Session.Machine = resolvedImportMachine(
				result.Session.Machine, machine,
			)
			status, err := upsertChatGPTConversation(
				ctx, store, result, fts,
			)
			if err != nil {
				stats.Errors++
				log.Printf(
					"import: skipping %s: %v",
					result.Session.ID, err,
				)
				cb.progress(stats)
				return nil
			}

			switch status {
			case importNew:
				stats.Imported++
			case importUpdated:
				stats.Updated++
			case importSkipped:
				stats.Skipped++
			}

			cb.progress(stats)
			return nil
		},
	)

	retErr = err
	return
}

// upsertChatGPTConversation imports a new ChatGPT conversation or
// appends new messages to an archived one. An existing session is only
// extended when its archived messages are an exact prefix of the export;
// shorter exports and exports that rewrite archived history are refused
// so a re-import can never lose or silently change stored messages.
func upsertChatGPTConversation(
	ctx context.Context,
	store db.Store,
	result parser.ParseResult,
	fts *lazyFTS,
) (importStatus, error) {
	s := result.Session
	msgs := chatGPTMessages(s.ID, result.Messages)

	existing, err := store.GetSession(ctx, s.ID)
	if err != nil {
		return importNew, fmt.Errorf("checking session: %w", err)
	}
	if existing == nil {
		fts.suspend(ctx)
		err := writeChatGPTSession(ctx, store, chatGPTSession(s), msgs)
		if errors.Is(err, db.ErrSessionExcluded) {
			return importSkipped, nil
		}
		if err != nil {
			return importNew, fmt.Errorf("writing session: %w", err)
		}
		return importNew, nil
	}

	if existing.Agent != string(parser.AgentChatGPT) {
		return importNew, fmt.Errorf(
			"existing session belongs to agent %q", existing.Agent,
		)
	}
	policy := storeArchiveContent(store)
	if policy.UsageOnly() {
		// A usage archive keeps no transcript text and drops rows without
		// token usage, so the archived history cannot be verified as a
		// prefix of the export. Leave the stored session untouched.
		return importSkipped, nil
	}
	archived, err := store.GetAllMessages(ctx, s.ID)
	if err != nil {
		return importNew, fmt.Errorf("loading existing messages: %w", err)
	}
	// Compare against the export as it would be stored, not as parsed:
	// the write path sanitizes and projects rows, so raw parser output
	// can differ from an unchanged archived copy.
	canonical := canonicalChatGPTMessages(store, chatGPTSession(s), msgs, policy)
	if len(canonical) < len(archived) {
		return importNew, fmt.Errorf(
			"export has %d messages, archive has %d",
			len(canonical), len(archived),
		)
	}
	if len(canonical) != len(msgs) ||
		!sameMessages(archived, canonical[:len(archived)]) {
		return importNew, errors.New(
			"export history diverges from the archived messages",
		)
	}

	if len(msgs) == len(archived) {
		// Refresh session_name without touching any other fields —
		// a partial UpsertSession would overwrite first_message,
		// timestamps, and counts with zero values.
		if localDB, ok := store.(*db.DB); ok {
			if err := localDB.RefreshSessionName(
				ctx, s.ID, db.ParsedSessionName(s),
			); err != nil {
				return importNew, fmt.Errorf(
					"refreshing session_name: %w", err,
				)
			}
		}
		return importSkipped, nil
	}

	// Insert only the rows past the verified prefix. A full replacement
	// would delete and reinsert archived rows, changing message IDs and
	// risking pins that cannot be re-matched without source UUIDs.
	fts.suspend(ctx)
	// The transcript grew, so stored quality signals and secret findings
	// describe the shorter history. Clear them to version zero in the same
	// write so the signal backfill recomputes them from the new rows.
	if err := appendChatGPTMessages(
		ctx, store, chatGPTSession(s), msgs[len(archived):],
	); errors.Is(err, db.ErrSessionExcluded) {
		return importSkipped, nil
	} else if err != nil {
		return importNew, fmt.Errorf("appending messages: %w", err)
	}
	return importUpdated, nil
}

func chatGPTSession(s parser.ParsedSession) db.Session {
	sess := db.Session{
		ID:               s.ID,
		Project:          s.Project,
		Machine:          s.Machine,
		FirstMessage:     strPtr(s.FirstMessage),
		SessionName:      db.ParsedSessionName(s),
		StartedAt:        timeStr(s.StartedAt),
		EndedAt:          timeStr(s.EndedAt),
		MessageCount:     s.MessageCount,
		UserMessageCount: s.UserMessageCount,
	}
	db.ApplyParsedSessionIdentity(&sess, s)
	return sess
}

func chatGPTMessages(
	sessionID string, parsed []parser.ParsedMessage,
) []db.Message {
	msgs := make([]db.Message, len(parsed))
	for i, m := range parsed {
		msgs[i] = db.Message{
			SessionID: sessionID,
			Ordinal:   m.Ordinal,
			Role:      string(m.Role),
			Content:   m.Content,
			Timestamp: m.Timestamp.UTC().Format(
				time.RFC3339Nano,
			),
			HasThinking:   m.HasThinking,
			HasToolUse:    m.HasToolUse,
			ContentLength: m.ContentLength,
			IsSystem:      m.IsSystem,
			Model:         m.Model,
			ToolCalls:     convertToolCalls(sessionID, m.ToolCalls),
		}
	}
	return msgs
}

// storeArchiveContent reports the content policy the store enforces.
// Stores that do not expose one keep full content.
func storeArchiveContent(store db.Store) config.ArchiveContent {
	if s, ok := store.(interface {
		ArchiveContent() config.ArchiveContent
	}); ok {
		return s.ArchiveContent()
	}
	return config.ArchiveContentFull
}

// canonicalChatGPTMessages applies the same validation and storage
// projection the batch write applies, without writing anything.
func canonicalChatGPTMessages(
	store db.Store, sess db.Session, msgs []db.Message,
	policy config.ArchiveContent,
) []db.Message {
	out := make([]db.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		out[i].ToolCalls = slices.Clone(out[i].ToolCalls)
	}
	db.ValidateAndSanitize(&sess, out, nil)
	if localDB, ok := store.(*db.DB); ok {
		out, _ = localDB.ProjectToolResultImagesWithPolicy(
			out, localDB.ToolResultImages(),
		)
	}
	_, out = db.ProjectSessionForStoragePolicy(sess, out, policy)
	return out
}

func writeChatGPTSession(
	ctx context.Context, store db.Store, sess db.Session, msgs []db.Message,
) error {
	return writeChatGPTBatch(ctx, store, db.SessionBatchWrite{
		Session:                    sess,
		Messages:                   msgs,
		SkipSignalUpdates:          true,
		ReplaceMessages:            true,
		RejectMessageCountDecrease: true,
	})
}

// appendChatGPTMessages inserts rows after the archived transcript without
// touching existing rows. Signals are written as zero values so the
// backfill recomputes them for the longer transcript.
func appendChatGPTMessages(
	ctx context.Context, store db.Store, sess db.Session, tail []db.Message,
) error {
	return writeChatGPTBatch(ctx, store, db.SessionBatchWrite{
		Session:  sess,
		Messages: tail,
	})
}

func writeChatGPTBatch(
	ctx context.Context, store db.Store, write db.SessionBatchWrite,
) error {
	result, err := store.WriteSessionBatchAtomic(
		ctx, []db.SessionBatchWrite{write},
	)
	if err != nil {
		return err
	}
	if result.ExcludedSessions > 0 {
		return db.ErrSessionExcluded
	}
	if result.FailedSessions > 0 && len(result.Errors) > 0 {
		return result.Errors[0]
	}
	return nil
}

func resolvedImportMachine(current string, override []string) string {
	if len(override) > 0 && strings.TrimSpace(override[0]) != "" {
		return override[0]
	}
	return current
}

func ptrEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// storedFormMessages applies the validation and archive-content
// projection the message write applies, without writing anything.
func storedFormMessages(store db.Store, msgs []db.Message) []db.Message {
	out := slices.Clone(msgs)
	db.ValidateAndSanitize(nil, out, nil)
	_, out = db.ProjectSessionForStoragePolicy(
		db.Session{}, out, storeArchiveContent(store),
	)
	return out
}

func sameMessages(existing, incoming []db.Message) bool {
	if len(existing) != len(incoming) {
		return false
	}
	for i := range existing {
		if existing[i].Ordinal != incoming[i].Ordinal ||
			existing[i].Role != incoming[i].Role ||
			existing[i].Content != incoming[i].Content ||
			existing[i].Timestamp != incoming[i].Timestamp ||
			existing[i].ContentLength != incoming[i].ContentLength {
			return false
		}
	}
	return true
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func timeStr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

func convertToolCalls(
	sessionID string, parsed []parser.ParsedToolCall,
) []db.ToolCall {
	if len(parsed) == 0 {
		return nil
	}
	calls := make([]db.ToolCall, len(parsed))
	for i, tc := range parsed {
		filePath := tc.FilePath
		if filePath == "" {
			filePath = parser.ResolveFilePathFromJSON(tc.InputJSON)
		}
		calls[i] = db.ToolCall{
			SessionID: sessionID,
			ToolName:  tc.ToolName,
			Category:  tc.Category,
			ToolUseID: tc.ToolUseID,
			InputJSON: tc.InputJSON,
			FilePath:  filePath,
			SkillName: tc.SkillName,
		}
		// Map execution output from ResultEvents to
		// ResultContent for display in the UI.
		for _, ev := range tc.ResultEvents {
			if ev.Content != "" {
				calls[i].ResultContent = ev.Content
				calls[i].ResultContentLength = len(ev.Content)
				break
			}
		}
	}
	return calls
}
