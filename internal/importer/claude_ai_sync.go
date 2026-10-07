package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

var errClaudeAINotFound = errors.New("Claude returned HTTP 404")

// ClaudeAIRetryAfter carries the browser's response header without credentials.
type ClaudeAIRetryAfter string

func (e ClaudeAIRetryAfter) Error() string { return string(e) }

// SyncClaudeAI imports changed conversations through the existing export importer.
func SyncClaudeAI(ctx context.Context, store interface {
	db.Store
	IsSessionTrashed(context.Context, string) bool
	IsSessionExcluded(context.Context, string) bool
}, fetch func(context.Context, string) (int, []byte, error), cb *ImportCallbacks) (stats ImportStats, retErr error) {
	raw, err := fetchClaudeAI(ctx, fetch, "/api/organizations")
	if err != nil {
		return stats, err
	}
	var organizations []struct {
		UUID         string   `json:"uuid"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &organizations); err != nil {
		return stats, err
	}

	wrote := false
	notify := func() {
		if wrote && cb != nil && cb.OnPage != nil {
			cb.OnPage()
		}
		wrote = false
	}
	defer notify()
	for _, org := range organizations {
		if !slices.Contains(org.Capabilities, "chat") {
			continue
		}
		if !safeConversationID(org.UUID) {
			return stats, errors.New("invalid Claude organization")
		}
		base := "/api/organizations/" + url.PathEscape(org.UUID)
		for offset := 0; ; {
			raw, err := fetchClaudeAI(ctx, fetch, fmt.Sprintf("%s/chat_conversations_v2?limit=50&offset=%d", base, offset))
			if err != nil {
				return stats, err
			}
			var page struct {
				Conversations []json.RawMessage `json:"conversations"`
				HasMore       *bool             `json:"has_more"`
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				return stats, err
			}
			items := page.Conversations
			if items == nil {
				return stats, errors.New("Claude list had no conversations")
			}
			if len(items) == 0 {
				break
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
					stats.Skipped++
					cb.progress(stats)
					continue
				}
				if store.IsSessionExcluded(ctx, id) {
					stats.Skipped++
					cb.progress(stats)
					continue
				}
				existing, err := store.GetSession(ctx, id)
				if err != nil {
					return stats, err
				}
				updatedAt, parseErr := time.Parse(time.RFC3339Nano, marker.UpdatedAt)
				if existing != nil && parseErr == nil && ptrEqual(existing.EndedAt, timeStr(updatedAt)) {
					stats.Skipped++
					cb.progress(stats)
					continue
				}
				detail, err := fetchClaudeAI(ctx, fetch, base+"/chat_conversations/"+url.PathEscape(marker.UUID)+"?tree=True")
				if errors.Is(err, errClaudeAINotFound) {
					stats.Skipped++
					cb.progress(stats)
					continue
				}
				if err != nil {
					return stats, err
				}
				var imported ImportStats
				write := func() error {
					var err error
					imported, err = ImportClaudeAIWithOptions(ctx, store, bytes.NewReader(append(append([]byte{'['}, detail...), ']')), nil, ImportOptions{IncrementalFTS: true})
					if err != nil {
						imported.record(id, importSkipped, err)
					}
					if imported.Imported+imported.Updated+imported.Skipped+imported.Errors == 0 {
						imported.Skipped++
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
				break
			}
		}
	}
	return stats, nil
}

func fetchClaudeAI(ctx context.Context, fetch func(context.Context, string) (int, []byte, error), path string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		status, body, err := fetch(ctx, path)
		var retryAfter ClaudeAIRetryAfter
		if err != nil && !errors.As(err, &retryAfter) {
			return nil, err
		}
		if status == http.StatusNotFound {
			return nil, errClaudeAINotFound
		}
		if status == 401 || status == 403 {
			return nil, errors.New("Sign in to Claude.ai, then Sync again")
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
