package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/parser"
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
	DropFTS() error
	RebuildFTS() error
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

func newLazyFTS(
	store db.Store, onIndexing func(),
) *lazyFTS {
	s, ok := store.(ftsSuspender)
	if !ok || !store.HasFTS() {
		return nil
	}
	return &lazyFTS{sus: s, onIndexing: onIndexing}
}

func (f *lazyFTS) suspend() {
	if f == nil || f.dropped {
		return
	}
	if err := f.sus.DropFTS(); err != nil {
		log.Printf("import: drop FTS: %v", err)
		return
	}
	f.dropped = true
}

func (f *lazyFTS) restore() error {
	if f == nil || !f.dropped {
		return nil
	}
	if f.onIndexing != nil {
		f.onIndexing()
	}
	if err := f.sus.RebuildFTS(); err != nil {
		return fmt.Errorf("rebuilding FTS index: %w", err)
	}
	return nil
}

// ImportClaudeAI reads a Claude.ai conversations.json export
// and upserts each conversation into the store. Existing
// sessions are updated (messages replaced); user-renamed
// display names are preserved. Excluded (deleted) sessions
// are counted as skipped.
func ImportClaudeAI(
	ctx context.Context,
	store db.Store,
	r io.Reader,
	cb *ImportCallbacks,
) (stats ImportStats, retErr error) {
	fts := newLazyFTS(store, cb.indexing)
	defer func() {
		if err := fts.restore(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()

	err := parser.ParseClaudeAIExport(r, func(
		result parser.ParseResult,
	) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}

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

	existing, err := store.GetSession(ctx, s.ID)
	if err != nil {
		return importNew, fmt.Errorf("checking session: %w", err)
	}
	isNew := existing == nil

	// Preserve user-renamed display_name on re-import.
	displayName := strPtr(s.DisplayName)
	if !isNew && existing != nil && existing.DisplayName != nil {
		importName := strPtr(s.DisplayName)
		nameChanged := importName == nil ||
			*existing.DisplayName != *importName
		if nameChanged {
			displayName = existing.DisplayName
		}
	}

	sess := db.Session{
		ID:               s.ID,
		Project:          s.Project,
		Machine:          s.Machine,
		Agent:            string(s.Agent),
		FirstMessage:     strPtr(s.FirstMessage),
		DisplayName:      displayName,
		StartedAt:        timeStr(s.StartedAt),
		EndedAt:          timeStr(s.EndedAt),
		MessageCount:     s.MessageCount,
		UserMessageCount: s.UserMessageCount,
	}

	if err := store.UpsertSession(sess); err != nil {
		if errors.Is(err, db.ErrSessionExcluded) {
			return importSkipped, nil
		}
		return importNew, fmt.Errorf("upserting session: %w", err)
	}

	// Skip expensive message replacement when the conversation
	// has not changed since the last import. Compare both
	// message count and ended_at (source updated_at) to detect
	// content/metadata changes even when count is unchanged.
	if !isNew && existing != nil && existing.MessageCount == s.MessageCount {
		newEnd := timeStr(s.EndedAt)
		if ptrEqual(existing.EndedAt, newEnd) {
			return importSkipped, nil
		}
	}

	// Suspend FTS before first message-changing operation to
	// avoid per-row trigger overhead during bulk work.
	fts.suspend()

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

	if err := store.ReplaceSessionMessages(s.ID, msgs); err != nil {
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
	return CopyAsset(srcPath, a.assetsDir)
}

// ImportChatGPT reads a ChatGPT export directory (containing
// conversations-*.json files) and imports each conversation into
// the store. Existing sessions are skipped to preserve archived
// data.
func ImportChatGPT(
	ctx context.Context,
	store db.Store,
	dir string,
	assetsDir string,
	cb *ImportCallbacks,
) (stats ImportStats, retErr error) {
	fts := newLazyFTS(store, cb.indexing)
	defer func() {
		if err := fts.restore(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()

	index := BuildAssetIndex(dir)
	resolver := &assetResolverAdapter{
		index:     index,
		assetsDir: assetsDir,
	}

	err := parser.ParseChatGPTExport(dir, resolver,
		func(result parser.ParseResult) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			s := result.Session

			existing, err := store.GetSession(ctx, s.ID)
			if err != nil {
				stats.Errors++
				log.Printf(
					"import: skipping %s: %v", s.ID, err,
				)
				cb.progress(stats)
				return nil
			}
			if existing != nil {
				stats.Skipped++
				cb.progress(stats)
				return nil
			}

			sess := db.Session{
				ID:               s.ID,
				Project:          s.Project,
				Machine:          s.Machine,
				Agent:            string(s.Agent),
				FirstMessage:     strPtr(s.FirstMessage),
				DisplayName:      strPtr(s.DisplayName),
				StartedAt:        timeStr(s.StartedAt),
				EndedAt:          timeStr(s.EndedAt),
				MessageCount:     s.MessageCount,
				UserMessageCount: s.UserMessageCount,
			}

			if err := store.UpsertSession(sess); err != nil {
				if errors.Is(err, db.ErrSessionExcluded) {
					stats.Skipped++
					cb.progress(stats)
					return nil
				}
				stats.Errors++
				log.Printf(
					"import: skipping %s: %v", s.ID, err,
				)
				cb.progress(stats)
				return nil
			}

			fts.suspend()

			msgs := make([]db.Message, len(result.Messages))
			for i, m := range result.Messages {
				msgs[i] = db.Message{
					SessionID: s.ID,
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
					ToolCalls: convertToolCalls(
						s.ID, m.ToolCalls,
					),
				}
			}

			if err := store.ReplaceSessionMessages(
				s.ID, msgs,
			); err != nil {
				stats.Errors++
				log.Printf(
					"import: skipping messages for %s: %v",
					s.ID, err,
				)
				cb.progress(stats)
				return nil
			}

			stats.Imported++
			cb.progress(stats)
			return nil
		},
	)

	retErr = err
	return
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
		calls[i] = db.ToolCall{
			SessionID: sessionID,
			ToolName:  tc.ToolName,
			Category:  tc.Category,
			ToolUseID: tc.ToolUseID,
			InputJSON: tc.InputJSON,
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
