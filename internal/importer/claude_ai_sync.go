package importer

import (
	"context"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash/fnv"
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

// claudeAICountsMarker refreshes usage-only fingerprints that cached a shorter
// branch with older counts. Full archives already stored the selected branch.
func claudeAICountsMarker(store db.Store) string {
	if storeArchiveContent(store).UsageOnly() {
		return "selected-branch-counts\x00"
	}
	return ""
}

func claudeAIFreshness(store db.Store, updatedAt, leaf string, session *db.Session) uint64 {
	hash := fnv.New64a()
	fmt.Fprintf(hash, "%s%s\x00%s\x00%d", claudeAICountsMarker(store), updatedAt, leaf, session.MessageCount)
	for _, field := range []*string{session.TranscriptRevision, session.SessionName, session.EndedAt} {
		fmt.Fprint(hash, "\x00")
		if field != nil {
			fmt.Fprint(hash, *field)
		}
	}
	return hash.Sum64()
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
// A non-empty orgs lists the lowercase organization UUIDs to import.
func SyncClaudeAI(ctx context.Context, store interface {
	db.Store
	IsSessionTrashed(context.Context, string) bool
	IsSessionExcluded(context.Context, string) bool
	GetProviderStatHash(context.Context, parser.AgentType, string) (uint64, bool, error)
	UpsertProviderStatHash(context.Context, parser.AgentType, string, uint64) error
}, fetch func(context.Context, string) (ClaudeAIResponse, error), orgs []string,
	cb *ImportCallbacks, machine ...string,
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
		if !slices.Contains(org.Capabilities, "chat") || (len(orgs) > 0 && !slices.Contains(orgs, strings.ToLower(org.UUID))) {
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
					failedLast = false
					stats.Skipped++
					cb.progress(stats)
					continue
				}
				id := "claude-ai:" + marker.UUID
				if store.IsSessionTrashed(ctx, id) {
					failedLast = false
					stats.Skipped++
					cb.progress(stats)
					continue
				}
				if store.IsSessionExcluded(ctx, id) {
					failedLast = false
					stats.Skipped++
					cb.progress(stats)
					continue
				}
				detail, unchanged, fetchErr, err := fetchConversationDetailIfChanged(
					ctx, store, parser.AgentClaudeAI, id,
					func(session *db.Session) uint64 { return claudeAIFreshness(marker.UpdatedAt, leaf, session) },
					func(ctx context.Context) ([]byte, error) {
						return fetchClaudeAI(ctx, fetch, claudeAIRequest(claudeAIConversationRequest, org.UUID, marker.UUID, 0))
					},
				)
				if err != nil {
					return stats, err
				}
				if fetchErr != nil {
