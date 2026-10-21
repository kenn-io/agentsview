package importer

import (
	"context"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/chromehost"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// grok_requests.txt names the read-only grok.com requests: the first list
// page, a later list page, and one chat's responses.
//
//go:embed grok_requests.txt
var grokRequests string

const (
	grokListRequest = iota
	grokListPageRequest
	grokResponsesRequest
)

// grokPageLimit bounds list paging against a token loop.
const grokPageLimit = 200

// ErrGrokResponseTooLarge marks a conversation over the browser relay limit.
var ErrGrokResponseTooLarge = errors.New("Grok response exceeds 32 MiB")

// ErrGrokAuthRequired reports a missing or expired Grok browser session.
var ErrGrokAuthRequired = errors.New("Grok sign-in required")

var errGrokFormat = errors.New("Grok returned a conversation format this version can't read. Update AgentsView and try again")

// grokDetailGap delays each conversation request by 3 s with jitter, the
// same pacing as ChatGPT. Tests set it to zero to disable pacing.
var grokDetailGap = 3 * time.Second

func grokRequest(shape int, conversation, pageToken string) string {
	return browserRequest(grokRequests, shape, "{conversation}", url.PathEscape(conversation), "{pageToken}", url.QueryEscape(pageToken))
}

type grokListPage struct {
	Conversations *[]jsontext.Value `json:"conversations"`
	NextPageToken string            `json:"nextPageToken"`
}

type grokChat struct {
	ConversationID string `json:"conversationId"`
	Title          string `json:"title"`
	ModifyTime     string `json:"modifyTime"`
}

// SyncGrok imports new and changed Grok conversations through Chrome.
func SyncGrok(ctx context.Context, store BrowserSyncStore, fetch func(context.Context, string) (ClaudeAIResponse, error), cb *ImportCallbacks, machine ...string,
) (stats ImportStats, retErr error) {
	seen := make(map[string]struct{})
	var formatErr error
	runner := browserSync{
		store: store, agent: parser.AgentGrokWeb, stats: &stats, cb: cb, machine: machine,
		tooLarge: ErrGrokResponseTooLarge, stop: func(err error) bool {
			httpErr, _ := errors.AsType[*browserHTTPError](err)
			return errors.Is(err, ErrGrokAuthRequired) || httpErr != nil && httpErr.rateLimited
		},
	}
	token := ""
	for page := 0; ; page++ {
		if page == grokPageLimit {
			return stats, errors.New("Grok chat list exceeded the page limit")
		}
		path := grokRequest(grokListRequest, "", "")
		if token != "" {
			path = grokRequest(grokListPageRequest, "", token)
		}
		raw, err := fetchBrowser(ctx, fetch, chromehost.Grok, path)
		if err != nil {
			return stats, err
		}
		var list grokListPage
		if json.Unmarshal(raw, &list) != nil || list.Conversations == nil {
			return stats, errGrokFormat
		}
		for _, item := range *list.Conversations {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			var chat grokChat
			updatedAt, ok := time.Time{}, false
			if json.Unmarshal(item, &chat) == nil && parser.ValidGrokWebID(chat.ConversationID) {
				updatedAt, ok = parser.GrokWebTime(chat.ModifyTime)
			}
			if !ok {
				stats.Errors++
				cb.progress(stats)
				continue
			}
			if _, ok := seen[chat.ConversationID]; ok {
				continue
			}
			seen[chat.ConversationID] = struct{}{}
			err := runner.conversation(ctx, "grok-web:"+chat.ConversationID, browserConversation{
				freshness: func(session *db.Session) uint64 {
					return browserFreshness(updatedAt.UTC().Format(time.RFC3339Nano), session)
				},
				fetch: func(ctx context.Context) ([]byte, error) {
					detail, err := fetchGrokConversation(ctx, fetch, chat)
					if errors.Is(err, errGrokFormat) {
						formatErr = errGrokFormat
					}
					return detail, err
				},
				parse: func(detail []byte) (parser.ParseResult, error) {
					result, err := parser.ParseGrokWebConversation(detail)
					if errors.Is(err, parser.ErrGrokWebFormat) {
						formatErr = errGrokFormat
					}
					return result, err
				},
				write:   syncConversation,
				matches: func([]byte, parser.ParseResult) bool { return true },
				pending: parser.GrokWebPending,
			})
			if err != nil {
				return stats, err
			}
		}
		next := strings.TrimSpace(list.NextPageToken)
		if next == "" || len(*list.Conversations) == 0 {
			break
		}
		if next == token {
			return stats, errors.New("Grok chat list repeated its page token")
		}
		if !parser.ValidGrokWebID(next) {
			return stats, errGrokFormat
		}
		token = next
	}
	return stats, formatErr
}

// fetchGrokConversation reads one chat's responses and wraps them with the
// list entry for parser.ParseGrokWebConversation.
func fetchGrokConversation(ctx context.Context, fetch func(context.Context, string) (ClaudeAIResponse, error), chat grokChat) ([]byte, error) {
	if err := waitBrowserGap(ctx, grokDetailGap); err != nil {
		return nil, err
	}
	raw, err := fetchBrowser(ctx, fetch, chromehost.Grok, grokRequest(grokResponsesRequest, chat.ConversationID, ""))
	if err != nil {
		return nil, err
	}
	if !jsontext.Value(raw).IsValid() {
		return nil, errGrokFormat
	}
	return json.Marshal(parser.GrokWebConversation{
		ID: chat.ConversationID, Title: chat.Title, ModifyTime: chat.ModifyTime, Responses: raw,
	})
}
