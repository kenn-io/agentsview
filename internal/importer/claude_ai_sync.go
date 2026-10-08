package importer

import (
	"context"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v7"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/httputil"
	"go.kenn.io/agentsview/internal/parser"
)

// ErrClaudeAIResponseTooLarge marks a chat that exceeds the browser relay limit.
var ErrClaudeAIResponseTooLarge = errors.New("claude response exceeds 32 MiB")

// ClaudeAIResponseLimit also caps responses from hosts other than the bundled desktop app.
const ClaudeAIResponseLimit = 32 << 20

// ErrClaudeAIAuthRequired reports expired or missing browser credentials.
var ErrClaudeAIAuthRequired = errors.New("claude.ai sign-in required")

//go:embed claude_ai_requests.txt
var claudeAIRequests string

const (
	claudeAIOrganizationsRequest = iota
	claudeAIConversationsRequest
	claudeAIConversationRequest
)

func claudeAIRequest(shape int, organization, conversation string, offset int) string {
	return strings.NewReplacer("{organization}", url.PathEscape(organization), "{conversation}", url.PathEscape(conversation), "{offset}", strconv.Itoa(offset)).Replace(strings.Split(claudeAIRequests, "\n")[shape])
}

// Bump the marker version when parser output changes.
const claudeAIMarkerVersion = 1

func claudeAIMarker(store db.Store, leaf string) string {
	return "claude-ai:v" + strconv.Itoa(claudeAIMarkerVersion) + ":" + string(storeArchiveContent(store)) + ":" + leaf
}

type claudeAIHTTPError struct{ status int }

func (e *claudeAIHTTPError) Error() string {
	if e.status == 403 {
		return "claude.ai access denied (HTTP 403)"
	}
	return fmt.Sprintf("claude returned HTTP %d", e.status)
}

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
}, fetch func(context.Context, string) (ClaudeAIResponse, error), cb *ImportCallbacks, machine ...string,
) (stats ImportStats, retErr error) {
	raw, err := fetchClaudeAI(ctx, fetch, claudeAIRequest(claudeAIOrganizationsRequest, "", "", 0))
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
	if organizations == nil {
		return stats, errors.New("claude organizations must be an array")
	}

	failedLast := false
	for _, org := range organizations {
		if !slices.Contains(org.Capabilities, "chat") {
			continue
		}
		for offset := 0; ; {
			raw, err := fetchClaudeAI(ctx, fetch, claudeAIRequest(claudeAIConversationsRequest, org.UUID, "", offset))
			if err != nil {
				return stats, err
			}
			var page struct {
				Data    []jsontext.Value `json:"data"`
				HasMore *bool            `json:"has_more"`
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				return stats, err
			}
			items := page.Data
			if items == nil {
				return stats, errors.New("claude list had no data")
			}
			if len(items) == 0 {
				if page.HasMore != nil && *page.HasMore {
					return stats, errors.New("claude list returned an empty page with has_more: true")
				}
				break
			}
			for _, summary := range items {
				if err := ctx.Err(); err != nil {
					return stats, err
				}
				var marker struct {
					UUID        string         `json:"uuid"`
					UpdatedAt   string         `json:"updated_at"`
					CurrentLeaf jsontext.Value `json:"current_leaf_message_uuid"`
				}
				if err := json.Unmarshal(summary, &marker); err != nil || marker.UUID == "" || marker.UpdatedAt == "" {
					stats.Errors++
					cb.progress(stats)
					continue
				}
				var leaf string
				if err := json.Unmarshal(marker.CurrentLeaf, &leaf); err != nil || leaf == "" && string(marker.CurrentLeaf) != "null" {
					stats.Errors++
					cb.progress(stats)
					continue
				}
				if leaf == "" || leaf == "00000000-0000-4000-8000-000000000000" {
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
				if existing != nil && existing.LastEntryUUID != nil {
					parts := strings.SplitN(*existing.LastEntryUUID, ":", 3)
					if len(parts) == 3 && parts[0] == "claude-ai" && strings.HasPrefix(parts[1], "v") {
						version, err := strconv.Atoi(strings.TrimPrefix(parts[1], "v"))
						if err == nil && version > claudeAIMarkerVersion {
							stats.record(id, importSkipped, refuse(RefusalNewerMarker, fmt.Errorf("stored Claude.ai marker version %d is newer than supported version %d", version, claudeAIMarkerVersion)))
							cb.progress(stats)
							continue
						}
					}
				}
				updatedAt, parseErr := time.Parse(time.RFC3339Nano, marker.UpdatedAt)
				if existing != nil && parseErr == nil && ptrEqual(existing.EndedAt, timeStr(updatedAt)) && existing.LastEntryUUID != nil && *existing.LastEntryUUID == claudeAIMarker(store, leaf) {
					stats.Skipped++
					cb.progress(stats)
					continue
				}
				detail, err := fetchClaudeAI(ctx, fetch, claudeAIRequest(claudeAIConversationRequest, org.UUID, marker.UUID, 0))
				if err != nil {
					if ctx.Err() != nil {
						return stats, ctx.Err()
					}
					if errors.Is(err, ErrClaudeAIAuthRequired) {
						return stats, err
					}
					detailError, _ := errors.AsType[*claudeAIHTTPError](err)
					if detailError != nil && detailError.status == 404 {
						stats.record(id, importSkipped, nil)
					} else {
						if !errors.Is(err, ErrClaudeAIResponseTooLarge) {
							if failedLast {
								return stats, err
							}
							failedLast = true
						}
						stats.record(id, importSkipped, err)
					}
					cb.progress(stats)
					continue
				}
				failedLast = false
				write := func() error {
					result, err := parser.ParseClaudeAIDetail(detail)
					if err == nil && result.Session.ID != id {
						err = fmt.Errorf("conversation uuid differs from requested %s", marker.UUID)
					}
					if err != nil {
						stats.record(id, importSkipped, err)
						return nil
					}
					result.Session.Machine = resolvedImportMachine(result.Session.Machine, machine)
					status, err := claudeAIImport.importConversation(ctx, store, result, nil, ImportOptions{})
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
	attempt := 0
	body, err := backoff.Retry(ctx, func() ([]byte, error) {
		response, err := fetch(ctx, path)
		if err != nil {
			return nil, backoff.Permanent(err)
		}
		status := response.Status
		if status >= 200 && status < 300 {
			return response.Body, nil
		}
		var apiError struct {
			Error struct {
				Details struct {
					Code string `json:"error_code"`
				} `json:"details"`
			} `json:"error"`
		}
		_ = json.Unmarshal(response.Body, &apiError)
		if status == 401 || apiError.Error.Details.Code == "account_session_invalid" {
			return nil, backoff.Permanent(ErrClaudeAIAuthRequired)
		}
		if status == 413 {
			return nil, backoff.Permanent(ErrClaudeAIResponseTooLarge)
		}
		if status == 403 {
			return nil, backoff.Permanent(&claudeAIHTTPError{status: status})
		}
		httpErr := &claudeAIHTTPError{status: status}
		if status != 429 && status < 500 {
			return nil, backoff.Permanent(httpErr)
		}
		delay := time.Duration(1<<attempt) * time.Second
		attempt++
		if retryAfter := httputil.ParseRetryAfter(response.RetryAfter); retryAfter > 0 {
			delay = min(retryAfter, 60*time.Second)
		}
		return nil, backoff.RetryAfter(delay, httpErr)
	}, backoff.WithMaxTries(5), backoff.WithMaxElapsedTime(0))
	if retryErr := backoff.AsRetryError(err); retryErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, retryErr.LastErr
	}
	return body, err
}
