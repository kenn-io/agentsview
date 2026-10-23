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
	"go.kenn.io/agentsview/internal/chromehost"
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
	return browserRequest(claudeAIRequests, shape, "{organization}", url.PathEscape(organization), "{conversation}", url.PathEscape(conversation), "{offset}", strconv.Itoa(offset))
}

func browserRequest(shapes string, shape int, substitutions ...string) string {
	return strings.NewReplacer(substitutions...).Replace(strings.Split(strings.TrimSpace(shapes), "\n")[shape])
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
	return browserFreshness(claudeAICountsMarker(store)+updatedAt+"\x00"+leaf, session)
}

func browserFreshness(marker string, session *db.Session) uint64 {
	if session == nil {
		session = &db.Session{}
	}
	hash := fnv.New64a()
	fmt.Fprintf(hash, "%s\x00%d", marker, session.MessageCount)
	for _, field := range []*string{session.TranscriptRevision, session.SessionName, session.EndedAt} {
		fmt.Fprint(hash, "\x00")
		if field != nil {
			fmt.Fprint(hash, *field)
		}
	}
	return hash.Sum64()
}

// ClaudeAIResponse carries the browser response without credentials.
type ClaudeAIResponse struct {
	Status     int
	Body       []byte
	RetryAfter string
}

// SyncClaudeAI imports changed conversations through the existing export importer.
func SyncClaudeAI(ctx context.Context, store BrowserSyncStore, fetch func(context.Context, string) (ClaudeAIResponse, error), cb *ImportCallbacks, machine ...string,
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

	runner := browserSync{
		store: store, agent: parser.AgentClaudeAI, stats: &stats, cb: cb, machine: machine,
		tooLarge: ErrClaudeAIResponseTooLarge, stop: func(err error) bool { return errors.Is(err, ErrClaudeAIAuthRequired) },
	}
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
					runner.failedLast = false
					stats.Skipped++
					cb.progress(stats)
					continue
				}
				id := "claude-ai:" + marker.UUID
				err := runner.conversation(ctx, id, browserConversation{
					freshness: func(session *db.Session) uint64 { return claudeAIFreshness(store, marker.UpdatedAt, leaf, session) },
					fetch: func(ctx context.Context) ([]byte, error) {
						return fetchClaudeAI(ctx, fetch, claudeAIRequest(claudeAIConversationRequest, org.UUID, marker.UUID, 0))
					},
					parse: func(detail []byte) (parser.ParseResult, error) {
						result, err := parser.ParseClaudeAIDetail(detail)
						if err == nil && result.Session.ID != id {
							err = fmt.Errorf("conversation uuid differs from requested %s", marker.UUID)
						}
						return result, err
					},
					write: syncConversation,
					matches: func(detail []byte, _ parser.ParseResult) bool {
						var current struct {
							UpdatedAt string `json:"updated_at"`
							Leaf      string `json:"current_leaf_message_uuid"`
						}
						return json.Unmarshal(detail, &current) == nil && current.UpdatedAt == marker.UpdatedAt && current.Leaf == leaf
					},
				})
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

func syncConversation(ctx context.Context, store db.Store, result parser.ParseResult) (importStatus, error) {
	id := result.Session.ID
	msgs := chatGPTMessages(id, result.Messages)
	if result.Session.Agent == parser.AgentClaudeAI {
		msgs = claudeAIMessages(id, result.Messages)
	}
	existing, err := store.GetSessionFull(ctx, id)
	if err != nil {
		return importNew, err
	}
	if existing == nil {
		_, err := store.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
			Session: chatGPTSession(result.Session), Messages: msgs,
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
	if result.Session.Agent == parser.AgentChatGPT {
		keepArchivedChatGPTModels(msgs, archived)
	}
	stored := storedFormMessages(store, msgs)
	if sameMessages(archived, stored) {
		// Equal rows can still be another branch: identical text, or text
		// that usage-only storage does not keep.
		if keepsReplacedHistory(result.Session.Agent) &&
			(len(msgs) < existing.MessageCount || sourceIdentityChanged(archived, stored)) {
			return replaceSyncedConversation(ctx, store, result, msgs, true)
		}
		// Equal rows ignore models, so a reply that only gained its model
		// (a completed reply, or any change under usage-only storage, which
		// keeps no text) gets it filled in place.
		usageOnly := storeArchiveContent(store).UsageOnly()
		if keepsReplacedHistory(result.Session.Agent) && (usageOnly || result.Session.Agent != parser.AgentChatGPT) {
			if rows := chatGPTModelRows(archived, stored); len(rows) > 0 {
				return importUpdated, appendChatGPTMessages(ctx, store, chatGPTSession(result.Session), rows)
			}
			if usageOnly {
				return refreshChatGPTTimestamp(ctx, store, chatGPTSession(result.Session))
			}
		}
		status, err := upsertConversation(ctx, store, result, nil)
		if refusalReason(err) == RefusalShorterExport {
			// Stored messages already match; update the selected branch's counts
			// without replacing rows that may have pins.
			_, err = store.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
				Session: chatGPTSession(result.Session), Messages: msgs,
				SkipSignalUpdates: true,
			}})
			if errors.Is(err, db.ErrSessionExcluded) {
				return importSkipped, nil
			}
			return importUpdated, err
		}
		return status, err
	}
	keepCopy := false
	if keepsReplacedHistory(result.Session.Agent) {
		prefix := false
		if stored != nil && len(stored) >= len(archived) {
			var filled []int
			filled, prefix = compareChatGPTPrefix(archived, stored[:len(archived)], true)
			if prefix && result.Session.Agent != parser.AgentChatGPT {
				// Appends and completed replies keep stored rows, their IDs, and
				// pins. The fill indices count stored-form rows, which usage-only
				// storage thins, so the rows written come from the same slice.
				rows := chatGPTFillRows(archived, stored, filled, true)
				rows = append(rows, stored[len(archived):]...)
				return importUpdated, appendChatGPTMessages(ctx, store, chatGPTSession(result.Session), rows)
			}
		}
		keepCopy = !prefix
	}
	return replaceSyncedConversation(ctx, store, result, msgs, keepCopy)
}

// sourceIdentityChanged reports whether a stored row and its incoming row
// name different source messages, such as another Gemini draft or Grok
// response with the same text.
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

// keepsReplacedHistory reports whether Sync keeps a Trash copy whenever a
// chat stops being an append or text extension of the archive. Claude.ai
// keeps one only when a pin or note would be lost.
func keepsReplacedHistory(agent parser.AgentType) bool {
	return agent == parser.AgentChatGPT || agent == parser.AgentGeminiApps || agent == parser.AgentGrokWeb
}

func replaceSyncedConversation(ctx context.Context, store db.Store, result parser.ParseResult, msgs []db.Message, keepCopy bool) (importStatus, error) {
	replacer, ok := store.(sessionReplacer)
	if !ok {
		return importUpdated, errors.New("store cannot preserve replaced chats")
	}
	_, err := replacer.ReplaceSessionKeepingTrashedCopy(ctx, db.SessionBatchWrite{
		Session: chatGPTSession(result.Session), Messages: msgs,
		KeepTrashedCopyOnlyOnPinLoss: !keepCopy,
	})
	return importUpdated, err
}

func fetchClaudeAI(ctx context.Context, fetch func(context.Context, string) (ClaudeAIResponse, error), path string) ([]byte, error) {
	return fetchBrowser(ctx, fetch, chromehost.ClaudeAI, path)
}

// fetchBrowser shares browser response limits and retries across platforms.
// browserPlatform holds what differs between browser-synced services.
type browserPlatform struct {
	service      string // service name in error text
	tooLarge     error
	authRequired error
	// stopOnRateLimit stops the run on HTTP 429 or a long Retry-After
	// instead of retrying inside the request.
	stopOnRateLimit bool
}

var browserPlatforms = map[chromehost.Platform]browserPlatform{
	chromehost.ClaudeAI: {service: "claude", tooLarge: ErrClaudeAIResponseTooLarge, authRequired: ErrClaudeAIAuthRequired},
	chromehost.ChatGPT:  {service: "ChatGPT", tooLarge: ErrChatGPTResponseTooLarge, authRequired: ErrChatGPTAuthRequired, stopOnRateLimit: true},
	chromehost.Gemini:   {service: "Gemini", tooLarge: ErrGeminiResponseTooLarge, authRequired: ErrGeminiAuthRequired, stopOnRateLimit: true},
	chromehost.Grok:     {service: "Grok", tooLarge: ErrGrokResponseTooLarge, authRequired: ErrGrokAuthRequired, stopOnRateLimit: true},
}

func fetchBrowser(ctx context.Context, fetch func(context.Context, string) (ClaudeAIResponse, error), platform chromehost.Platform, path string) ([]byte, error) {
	config, ok := browserPlatforms[platform]
	if !ok {
		return nil, fmt.Errorf("unsupported browser platform %q", platform)
	}
	tooLarge, authRequired := config.tooLarge, config.authRequired
	attempt := 0
	body, err := backoff.Retry(ctx, func() ([]byte, error) {
		response, err := fetch(ctx, path)
		if err != nil {
			return nil, backoff.Permanent(err)
		}
		if len(response.Body) > ClaudeAIResponseLimit {
			return nil, backoff.Permanent(tooLarge)
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
		if status == 401 || platform == chromehost.Grok && status == 403 ||
			platform == chromehost.ClaudeAI && apiError.Error.Details.Code == "account_session_invalid" {
			return nil, backoff.Permanent(authRequired)
		}
		if status == 413 {
			return nil, backoff.Permanent(tooLarge)
		}
		httpErr := &browserHTTPError{service: config.service, status: status, rateLimited: config.stopOnRateLimit && status == 429}
		if status != 429 && status < 500 {
			return nil, backoff.Permanent(httpErr)
		}
		delay := time.Duration(1<<attempt) * time.Second
		attempt++
		if retryAfter := httputil.ParseRetryAfter(response.RetryAfter); retryAfter > 0 {
			if config.stopOnRateLimit && retryAfter > 60*time.Second {
				httpErr.rateLimited = true
				return nil, backoff.Permanent(httpErr)
			}
			delay = min(retryAfter, 60*time.Second)
		}
		return nil, backoff.RetryAfter(delay, httpErr)
	}, backoff.WithMaxTries(5), backoff.WithMaxElapsedTime(0))
	if retryErr := backoff.AsRetryError(err); retryErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		err = retryErr.LastErr
	}
	if httpErr, ok := errors.AsType[*browserHTTPError](err); ok && httpErr.rateLimited {
		err = fmt.Errorf("%s rate limited; sync stopped early, run Sync again later: %w", config.service, err)
	}
	return body, err
}
