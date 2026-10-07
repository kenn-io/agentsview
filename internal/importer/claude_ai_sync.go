package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/httputil"
	"go.kenn.io/agentsview/internal/parser"
)

// ErrClaudeAIResponseTooLarge marks a chat that exceeds the browser relay limit.
var ErrClaudeAIResponseTooLarge = errors.New("Claude response exceeds 32 MiB")

type claudeAIHTTPError struct{ status int }

func (e *claudeAIHTTPError) Error() string { return fmt.Sprintf("Claude returned HTTP %d", e.status) }

// ClaudeAIResponse carries the browser response without credentials.
type ClaudeAIResponse struct {
	Status     int
	Body       []byte
	RetryAfter string
}

// SyncClaudeAI imports changed conversations through the existing export importer.
func SyncClaudeAI(ctx context.Context, store interface {
	db.Store
	IsSessionTrashed(context.Context, string) bool
	IsSessionExcluded(context.Context, string) bool
}, fetch func(context.Context, string) (ClaudeAIResponse, error), cb *ImportCallbacks) (stats ImportStats, retErr error) {
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
				Data    []json.RawMessage `json:"data"`
				HasMore *bool             `json:"has_more"`
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				return stats, err
			}
			items := page.Data
			if items == nil {
				return stats, errors.New("Claude list had no data")
			}
			if len(items) == 0 {
				if page.HasMore != nil && *page.HasMore {
					return stats, errors.New("Claude list returned an empty page with has_more: true")
				}
				break
			}
			for _, summary := range items {
				if err := ctx.Err(); err != nil {
					return stats, err
				}
				var marker struct {
					UUID        string `json:"uuid"`
					UpdatedAt   string `json:"updated_at"`
					CurrentLeaf string `json:"current_leaf_message_uuid"`
				}
				if err := json.Unmarshal(summary, &marker); err != nil || !safeConversationID(marker.UUID) || marker.UpdatedAt == "" {
					stats.Errors++
					cb.progress(stats)
					continue
				}
				if marker.CurrentLeaf == "" {
					stats.Skipped++
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
				existing, err := store.GetSessionFull(ctx, id)
				if err != nil {
					return stats, err
				}
				updatedAt, parseErr := time.Parse(time.RFC3339Nano, marker.UpdatedAt)
				if existing != nil && parseErr == nil && ptrEqual(existing.EndedAt, timeStr(updatedAt)) && existing.LastEntryUUID != nil && *existing.LastEntryUUID == marker.CurrentLeaf {
					stats.Skipped++
					cb.progress(stats)
					continue
				}
				detail, err := fetchClaudeAI(ctx, fetch, base+"/chat_conversations/"+url.PathEscape(marker.UUID)+"?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true")
				var detailError *claudeAIHTTPError
				if errors.As(err, &detailError) || errors.Is(err, ErrClaudeAIResponseTooLarge) {
					if detailError != nil && detailError.status == 404 {
						stats.record(id, importSkipped, nil)
					} else {
						stats.record(id, importSkipped, err)
					}
					cb.progress(stats)
					continue
				}
				if err != nil {
					return stats, err
				}
				write := func() error {
					result, err := parser.ParseClaudeAIDetail(detail)
					if err == nil && result.Session.ID != id {
						err = fmt.Errorf("conversation uuid differs from requested %s", marker.UUID)
					}
					if err != nil {
						stats.record(id, importSkipped, err)
						return nil
					}
					status, err := claudeAIImport.importConversation(ctx, store, result, nil, ImportOptions{Replace: []string{id}})
					if errors.Is(err, db.ErrSessionTrashed) {
						status, err = importSkipped, nil
					}
					stats.record(id, status, err)
					return nil
				}
				if cb != nil && cb.SerializeWrite != nil {
					err = cb.SerializeWrite(write)
				} else {
					err = write()
				}
				cb.progress(stats)
				if ctx.Err() != nil {
					return stats, ctx.Err()
				}
				if err != nil {
					return stats, err
				}
			}
			offset += len(items)
			if page.HasMore != nil && !*page.HasMore {
				break
			}
		}
	}
	return stats, nil
}

func fetchClaudeAI(ctx context.Context, fetch func(context.Context, string) (ClaudeAIResponse, error), path string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		response, err := fetch(ctx, path)
		if err != nil {
			return nil, err
		}
		status := response.Status
		if status == 401 || status == 403 {
			return nil, errors.New("Sign in to Claude.ai, then Sync again")
		}
		if status != 429 && status < 500 || attempt == 4 {
			if status < 200 || status >= 300 {
				return nil, &claudeAIHTTPError{status: status}
			}
			return response.Body, nil
		}
		delay := time.Duration(1<<attempt) * time.Second
		if retryAfter := httputil.ParseRetryAfter(response.RetryAfter); retryAfter > 0 {
			delay = min(retryAfter, 60*time.Second)
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
