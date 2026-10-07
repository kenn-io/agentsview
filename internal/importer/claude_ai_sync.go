package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// ClaudeAIRetryAfter carries the browser's response header without credentials.
type ClaudeAIRetryAfter string

func (e ClaudeAIRetryAfter) Error() string { return string(e) }

// SyncClaudeAI imports changed conversations through the existing export importer.
func SyncClaudeAI(ctx context.Context, store interface {
	db.Store
	GetSyncState(context.Context, string) (string, error)
	SetSyncState(context.Context, string, string) error
	IsSessionTrashed(context.Context, string) bool
	IsSessionExcluded(context.Context, string) bool
}, org string, fetch func(context.Context, string) (int, []byte, error), cb *ImportCallbacks) (stats ImportStats, retErr error) {
	if !safeConversationID(org) {
		return stats, errors.New("invalid Claude organization")
	}
	base := "/api/organizations/" + url.PathEscape(org)
	wrote := false
	notify := func() {
		if wrote && cb != nil && cb.OnPage != nil {
			cb.OnPage()
		}
		wrote = false
	}
	defer notify()
	for offset := 0; ; {
		raw, err := fetchClaudeAI(ctx, fetch, fmt.Sprintf("%s/chat_conversations_v2?limit=50&offset=%d", base, offset))
		if err != nil {
			return stats, err
		}
		var page struct {
			Conversations []json.RawMessage `json:"conversations"`
			Items         []json.RawMessage `json:"items"`
			Data          []json.RawMessage `json:"data"`
			Results       []json.RawMessage `json:"results"`
			HasMore       *bool             `json:"has_more"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return stats, err
		}
		items := page.Conversations
		if items == nil {
			items = page.Items
		}
		if items == nil {
			items = page.Data
		}
		if items == nil {
			items = page.Results
		}
		if items == nil {
			return stats, errors.New("Claude list had no conversations")
		}
		if len(items) == 0 {
			return stats, nil
		}
		for _, summary := range items {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			var marker struct {
				UUID      string `json:"uuid"`
				UpdatedAt string `json:"updated_at"`
			}
			if err := json.Unmarshal(summary, &marker); err != nil || !safeConversationID(marker.UUID) || marker.UpdatedAt == "" {
				stats.Errors++
				cb.progress(stats)
				continue
			}
			id := "claude-ai:" + marker.UUID
			if store.IsSessionTrashed(ctx, id) {
				stats.record(id, importSkipped, db.ErrSessionTrashed)
				cb.progress(stats)
				continue
			}
			if store.IsSessionExcluded(ctx, id) {
				stats.Skipped++
				cb.progress(stats)
				continue
			}
			key := "claude_ai_sync:" + org + ":" + marker.UUID
			checkpoint, err := store.GetSyncState(ctx, key)
			if err != nil {
				return stats, err
			}
			existing, err := store.GetSession(ctx, id)
			if err != nil {
				return stats, err
			}
			if existing != nil && checkpoint == marker.UpdatedAt {
				stats.Skipped++
				cb.progress(stats)
				continue
			}
			detail, err := fetchClaudeAI(ctx, fetch, base+"/chat_conversations/"+url.PathEscape(marker.UUID)+"?tree=True")
			if err != nil {
				return stats, err
			}
			merged, err := mergeSummary(detail, summary)
			if err == nil {
				merged, err = normalizeConversation(merged)
			}
			if err != nil {
				stats.record(id, importSkipped, err)
				cb.progress(stats)
				continue
			}
			var imported ImportStats
			write := func() error {
				var err error
				imported, err = ImportClaudeAIWithOptions(ctx, store, bytes.NewReader(append(append([]byte{'['}, merged...), ']')), nil, ImportOptions{IncrementalFTS: true})
				if err != nil {
					return err
				}
				if imported.Errors == 0 && imported.Imported+imported.Updated+imported.Skipped > 0 && !store.IsSessionExcluded(ctx, id) && !store.IsSessionTrashed(ctx, id) {
					return store.SetSyncState(ctx, key, marker.UpdatedAt)
				}
				return nil
			}
			if cb != nil && cb.SerializeWrite != nil {
				err = cb.SerializeWrite(write)
			} else {
				err = write()
			}
			stats.Imported += imported.Imported
			stats.Updated += imported.Updated
			stats.Skipped += imported.Skipped
			stats.Errors += imported.Errors
			stats.Refusals = append(stats.Refusals, imported.Refusals...)
			wrote = wrote || imported.Imported+imported.Updated > 0
			cb.progress(stats)
			if ctx.Err() != nil {
				return stats, ctx.Err()
			}
			if err != nil {
				return stats, err
			}
		}
		notify()
		offset += len(items)
		if page.HasMore != nil && !*page.HasMore {
			return stats, nil
		}
	}
}

func fetchClaudeAI(ctx context.Context, fetch func(context.Context, string) (int, []byte, error), path string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		status, body, err := fetch(ctx, path)
		var retryAfter ClaudeAIRetryAfter
		if err != nil && !errors.As(err, &retryAfter) {
			return nil, err
		}
		if status == 401 || status == 403 {
			return nil, errors.New("Claude sign-in expired. Reconnect Claude.ai and try again")
		}
		if status != 429 && status < 500 || attempt == 4 {
			if status < 200 || status >= 300 {
				return nil, fmt.Errorf("Claude returned HTTP %d", status)
			}
			return body, nil
		}
		delay := time.Duration(1<<attempt) * time.Second
		if seconds, e := strconv.Atoi(string(retryAfter)); e == nil && seconds >= 0 {
			delay = time.Duration(min(seconds, 60)) * time.Second
		} else if date, e := http.ParseTime(string(retryAfter)); e == nil {
			delay = max(time.Duration(0), min(time.Until(date), 60*time.Second))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func safeConversationID(id string) bool {
	return id != "" && !strings.ContainsAny(id, `/\\?#%`) && id != "." && id != ".."
}

func mergeSummary(detail, summary json.RawMessage) (json.RawMessage, error) {
	var conversation map[string]json.RawMessage
	if err := json.Unmarshal(detail, &conversation); err != nil {
		return nil, err
	}
	var listed map[string]json.RawMessage
	if err := json.Unmarshal(summary, &listed); err != nil {
		return nil, err
	}
	for _, key := range []string{"uuid", "name", "created_at", "updated_at"} {
		if len(conversation[key]) == 0 || string(conversation[key]) == `""` || string(conversation[key]) == "null" {
			conversation[key] = listed[key]
		}
	}
	return json.Marshal(conversation)
}

// Keep the active leaf so regenerated replies don't appear twice in the archive.
func normalizeConversation(raw json.RawMessage) (json.RawMessage, error) {
	var conversation struct {
		UUID        string            `json:"uuid"`
		Name        string            `json:"name"`
		CreatedAt   string            `json:"created_at"`
		UpdatedAt   string            `json:"updated_at"`
		CurrentLeaf string            `json:"current_leaf_message_uuid"`
		Messages    []json.RawMessage `json:"chat_messages"`
	}
	if err := json.Unmarshal(raw, &conversation); err != nil {
		return nil, err
	}
	if !safeConversationID(conversation.UUID) {
		return nil, fmt.Errorf("missing conversation UUID")
	}
	if conversation.CreatedAt == "" || conversation.UpdatedAt == "" {
		return nil, fmt.Errorf("missing conversation timestamps")
	}
	messages, err := activeMessages(conversation.Messages, conversation.CurrentLeaf)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		UUID      string            `json:"uuid"`
		Name      string            `json:"name"`
		CreatedAt string            `json:"created_at"`
		UpdatedAt string            `json:"updated_at"`
		Messages  []json.RawMessage `json:"chat_messages"`
	}{conversation.UUID, conversation.Name, conversation.CreatedAt, conversation.UpdatedAt, messages})
}

func activeMessages(messages []json.RawMessage, leaf string) ([]json.RawMessage, error) {
	if leaf == "" {
		return messages, nil
	}
	type node struct {
		UUID   string `json:"uuid"`
		Parent string `json:"parent_message_uuid"`
		Raw    json.RawMessage
	}
	byID := make(map[string]node, len(messages))
	for _, raw := range messages {
		var item node
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		item.Raw = raw
		if item.UUID != "" {
			byID[item.UUID] = item
		}
	}
	chain := make([]json.RawMessage, 0, len(messages))
	seen := make(map[string]struct{})
	for cursor := leaf; cursor != ""; {
		item, ok := byID[cursor]
		if !ok {
			return messages, nil
		}
		if _, exists := seen[cursor]; exists {
			return nil, fmt.Errorf("conversation message tree contains a cycle")
		}
		seen[cursor] = struct{}{}
		chain = append(chain, item.Raw)
		cursor = item.Parent
	}
	for left, right := 0, len(chain)-1; left < right; left, right = left+1, right-1 {
		chain[left], chain[right] = chain[right], chain[left]
	}
	return chain, nil
}
