package importer

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// The Grok allowlist names the read-only grok.com requests: the first list
// page, a later list page, and one chat's responses.
var grokWebRequests = browserRequests(parser.AgentGrokWeb)

const (
	grokListRequest = iota
	grokListPageRequest
	grokResponsesRequest
)

// grokDetailGap delays each conversation request by 3 s with jitter, the
// same pacing as ChatGPT. Tests set it to zero to disable pacing.
var grokDetailGap = 3 * time.Second

func grokRequest(shape int, conversation, pageToken string) string {
	return browserRequest(grokWebRequests, shape, "{conversation}", url.PathEscape(conversation), "{pageToken}", url.QueryEscape(pageToken))
}

func grokWebPlatform() BrowserPlatform {
	return BrowserPlatform{
		Agent: parser.AgentGrokWeb, Label: "Grok", StopOnRateLimit: true,
		// grok.com answers a signed-out request with 401 or 403.
		AuthRequired: func(status int, _ []byte) bool { return status == 401 || status == 403 },
		Sync:         syncGrokWeb,
	}
}

type grokListPage struct {
	Conversations *[]jsontext.Value `json:"conversations"`
	NextPageToken string            `json:"nextPageToken"`
}

// grokListEntry is one chat in a list page. Only the ID must be readable;
// the title and update time are decoded separately so an unexpected value
// there does not drop the chat.
type grokListEntry struct {
	ConversationID string         `json:"conversationId"`
	Title          jsontext.Value `json:"title"`
	ModifyTime     jsontext.Value `json:"modifyTime"`
}

type grokChat struct {
	ConversationID string
	Title          string
	ModifyTime     string
}

// grokListChat reads one list entry. It fails only when the entry is not
// a JSON object or has no valid chat ID.
func grokListChat(item jsontext.Value) (grokChat, bool) {
	var entry grokListEntry
	if json.Unmarshal(item, &entry) != nil || !parser.ValidGrokWebID(entry.ConversationID) {
		return grokChat{}, false
	}
	chat := grokChat{ConversationID: entry.ConversationID}
	_ = json.Unmarshal(entry.Title, &chat.Title)
	_ = json.Unmarshal(entry.ModifyTime, &chat.ModifyTime)
	return chat, true
}

// syncGrokWeb imports new and changed Grok web conversations.
func syncGrokWeb(ctx context.Context, store BrowserSyncStore, fetch BrowserFetch, opts BrowserSyncOptions, cb *ImportCallbacks) (stats ImportStats, retErr error) {
	platform := grokWebPlatform()
	seen := make(map[string]struct{})
	runner := newBrowserSync(store, platform, &stats, opts, cb)
	// A page token seen before would loop forever. Paging has no count
	// limit, so a long list still finishes.
	token, tokens := "", map[string]struct{}{}
	for {
		path := grokRequest(grokListRequest, "", "")
		if token != "" {
			path = grokRequest(grokListPageRequest, "", token)
		}
		raw, err := fetchBrowser(ctx, platform, fetch, path)
		if err != nil {
			return stats, err
		}
		var list grokListPage
		if json.Unmarshal(raw, &list) != nil || list.Conversations == nil {
			return stats, platform.wrap(ErrBrowserFormat)
		}
		for _, item := range *list.Conversations {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			chat, ok := grokListChat(item)
			if !ok {
				stats.Errors++
				cb.progress(stats)
				continue
			}
			// An unreadable update time leaves updatedAt zero instead of
			// dropping the chat: freshness then cannot see list changes, and
			// the pending rule treats the chat as recently updated.
			updatedAt, _ := parser.GrokWebTime(chat.ModifyTime)
			if _, ok := seen[chat.ConversationID]; ok {
				continue
			}
			seen[chat.ConversationID] = struct{}{}
			err := runner.conversation(ctx, "grok-web:"+chat.ConversationID, browserConversation{
				freshness: func(session *db.Session) uint64 {
					marker := ""
					if !updatedAt.IsZero() {
						marker = strconv.FormatInt(updatedAt.UnixNano(), 10)
					}
					return browserFreshness(marker, session)
				},
				fetch: func(ctx context.Context) ([]byte, error) {
					return fetchGrokConversation(ctx, platform, fetch, chat)
				},
				parse: func(detail []byte) (parser.ParseResult, error) {
					result, err := parser.ParseGrokWebConversation(detail)
					if errors.Is(err, parser.ErrGrokWebFormat) {
						err = platform.wrap(ErrBrowserFormat)
					}
					return result, err
				},
				write: func(ctx context.Context, store db.Store, result parser.ParseResult) (importStatus, error) {
					return syncKeepingHistory(ctx, store, result, nil)
				},
				// A reply that never completes stops being refetched once the
				// chat has been quiet for a day.
				pending: func(detail []byte) bool {
					return parser.GrokWebPending(detail) && recentlyUpdated(updatedAt, opts.now())
				},
			})
			if err != nil {
				return stats, err
			}
		}
		next := strings.TrimSpace(list.NextPageToken)
		if next == "" || len(*list.Conversations) == 0 {
			break
		}
		if !parser.ValidGrokWebID(next) {
			return stats, platform.wrap(ErrBrowserFormat)
		}
		if _, ok := tokens[next]; ok {
			return stats, errors.New("the Grok chat list repeated a page")
		}
		token, tokens[next] = next, struct{}{}
	}
	return stats, runner.formatErr
}

// fetchGrokConversation reads one chat's responses and wraps them with the
// list entry for parser.ParseGrokWebConversation. Every branch arrives in
// one body; the web app was not seen to page it.
func fetchGrokConversation(ctx context.Context, platform BrowserPlatform, fetch BrowserFetch, chat grokChat) ([]byte, error) {
	if err := waitBrowserGap(ctx, grokDetailGap); err != nil {
		return nil, err
	}
	raw, err := fetchBrowser(ctx, platform, fetch, grokRequest(grokResponsesRequest, chat.ConversationID, ""))
	if err != nil {
		return nil, err
	}
	if !jsontext.Value(raw).IsValid() {
		return nil, platform.wrap(ErrBrowserFormat)
	}
	return json.Marshal(parser.GrokWebConversation{
		ID: chat.ConversationID, Title: chat.Title, ModifyTime: chat.ModifyTime, Responses: raw,
	})
}
