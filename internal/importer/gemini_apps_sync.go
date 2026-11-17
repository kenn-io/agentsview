package importer

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// The Gemini allowlist names the two read-only batchexecute RPCs. The
// extension's gemini_fetch.js turns each named parameter into the RPC's
// f.req argument and adds the page's request token, so the shapes are not
// literal URLs.
var geminiAppsRequests = browserRequests(parser.AgentGeminiApps)

const (
	geminiListRequest = iota
	geminiDetailRequest
)

const (
	geminiListRPC   = "MaZiqc"
	geminiDetailRPC = "hNvQHb"
	// geminiPageLimit bounds list and turn paging against a cursor loop.
	geminiPageLimit = 200
)

// errGeminiRejected marks one conversation that Gemini refused to return.
var errGeminiRejected = errors.New("request rejected by Gemini")

// geminiDetailGap delays each conversation page request by 3 s with jitter,
// the same pacing as ChatGPT. Tests set it to zero to disable pacing.
var geminiDetailGap = 3 * time.Second

func geminiRequest(shape int, chat, cursor string) string {
	return browserRequest(geminiAppsRequests, shape, "{chat}", url.QueryEscape(chat), "{cursor}", url.QueryEscape(cursor))
}

func geminiAppsPlatform() BrowserPlatform {
	return BrowserPlatform{
		Agent: parser.AgentGeminiApps, Label: "Gemini", StopOnRateLimit: true,
		AuthRequired: func(status int, _ []byte) bool { return status == 401 },
		Sync:         syncGeminiApps,
	}
}

// geminiRPCPayload unwraps one batchexecute response: an anti-JSON prefix,
// length lines, and chunks whose "wrb.fr" entry for rpc carries the payload
// as a JSON string.
func geminiRPCPayload(platform BrowserPlatform, body []byte, rpc string) (jsontext.Value, error) {
	text := strings.TrimPrefix(strings.TrimSpace(string(body)), ")]}'")
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[") {
			continue
		}
		var entries []jsontext.Value
		if json.Unmarshal([]byte(line), &entries) != nil {
			continue
		}
		for _, entry := range entries {
			var fields []jsontext.Value
			if json.Unmarshal(entry, &fields) != nil || len(fields) < 3 {
				continue
			}
			var kind, id, payload string
			if json.Unmarshal(fields[0], &kind) != nil || kind != "wrb.fr" ||
				json.Unmarshal(fields[1], &id) != nil || id != rpc {
				continue
			}
			if json.Unmarshal(fields[2], &payload) != nil || payload == "" {
				// A null payload is Gemini refusing the call.
				return nil, errGeminiRejected
			}
			value := jsontext.Value(payload)
			if !value.IsValid() {
				return nil, platform.wrap(ErrBrowserFormat)
			}
			return value, nil
		}
	}
	return nil, platform.wrap(ErrBrowserFormat)
}

// geminiPage reads the cursor at index 1 and the array at index items.
func geminiPage(platform BrowserPlatform, payload jsontext.Value, items int) (jsontext.Value, string, error) {
	var page []jsontext.Value
	if json.Unmarshal(payload, &page) != nil {
		return nil, "", platform.wrap(ErrBrowserFormat)
	}
	var cursor string
	if len(page) > 1 {
		_ = json.Unmarshal(page[1], &cursor)
	}
	if len(page) <= items || string(page[items]) == "null" {
		return jsontext.Value("[]"), cursor, nil
	}
	if page[items].Kind() != '[' {
		return nil, "", platform.wrap(ErrBrowserFormat)
	}
	return page[items], cursor, nil
}

type geminiChat struct {
	id, title string
	updated   jsontext.Value
	updatedAt time.Time
}

// Positions inside one Gemini Apps list entry.
const (
	geminiChatID      = 0
	geminiChatTitle   = 1
	geminiChatUpdated = 5
)

func geminiListEntry(raw jsontext.Value) (geminiChat, bool) {
	var fields []jsontext.Value
	if json.Unmarshal(raw, &fields) != nil || len(fields) <= geminiChatUpdated {
		return geminiChat{}, false
	}
	var chat geminiChat
	if json.Unmarshal(fields[geminiChatID], &chat.id) != nil || !parser.ValidGeminiAppsChatID(chat.id) {
		return geminiChat{}, false
	}
	_ = json.Unmarshal(fields[geminiChatTitle], &chat.title)
	updatedAt, ok := parser.GeminiAppsTime(fields[geminiChatUpdated])
	if !ok {
		return geminiChat{}, false
	}
	chat.updated, chat.updatedAt = fields[geminiChatUpdated], updatedAt
	return chat, true
}

// syncGeminiApps imports new and changed Gemini Apps conversations.
func syncGeminiApps(ctx context.Context, store BrowserSyncStore, fetch BrowserFetch, opts BrowserSyncOptions, cb *ImportCallbacks) (stats ImportStats, retErr error) {
	platform := geminiAppsPlatform()
	seen := make(map[string]struct{})
	runner := newBrowserSync(store, platform, &stats, opts, cb)
	runner.refused = errGeminiRejected
	cursor := ""
	for page := 0; ; page++ {
		if page == geminiPageLimit {
			return stats, errors.New("the Gemini chat list exceeded the page limit")
		}
		raw, err := fetchBrowser(ctx, platform, fetch, geminiRequest(geminiListRequest, "", cursor))
		if err != nil {
			return stats, err
		}
		payload, err := geminiRPCPayload(platform, raw, geminiListRPC)
		if err != nil {
			return stats, err
		}
		items, next, err := geminiPage(platform, payload, 2)
		if err != nil {
			return stats, err
		}
		var entries []jsontext.Value
		if err := json.Unmarshal(items, &entries); err != nil {
			return stats, platform.wrap(ErrBrowserFormat)
		}
		for _, item := range entries {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			chat, ok := geminiListEntry(item)
			if !ok {
				stats.Errors++
				cb.progress(stats)
				continue
			}
			if _, ok := seen[chat.id]; ok {
				continue
			}
			seen[chat.id] = struct{}{}
			err := runner.conversation(ctx, "gemini-apps:"+chat.id, browserConversation{
				freshness: func(session *db.Session) uint64 {
					return browserFreshness(strconv.FormatInt(chat.updatedAt.UnixNano(), 10), session)
				},
				fetch: func(ctx context.Context) ([]byte, error) {
					return fetchGeminiConversation(ctx, platform, fetch, chat)
				},
				parse: func(detail []byte) (parser.ParseResult, error) {
					result, err := parser.ParseGeminiAppsWebConversation(detail)
					if errors.Is(err, parser.ErrGeminiAppsWebFormat) {
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
					return parser.GeminiAppsWebPending(detail) && recentlyUpdated(chat.updatedAt, opts.now())
				},
			})
			if err != nil {
				return stats, err
			}
		}
		if next == "" {
			break
		}
		if next == cursor {
			return stats, errors.New("the Gemini chat list repeated a page")
		}
		cursor = next
	}
	return stats, runner.formatErr
}

// fetchGeminiConversation follows the turn cursor and assembles the pages
// for parser.ParseGeminiAppsWebConversation. The assembled conversation has
// the same limit as a single response.
func fetchGeminiConversation(ctx context.Context, platform BrowserPlatform, fetch BrowserFetch, chat geminiChat) ([]byte, error) {
	conv := parser.GeminiAppsWebConversation{ID: chat.id, Title: chat.title, UpdatedAt: chat.updated}
	size, cursor := 0, ""
	for page := 0; ; page++ {
		if page == geminiPageLimit {
			return nil, fmt.Errorf("the Gemini conversation %s exceeded the page limit", chat.id)
		}
		if err := waitBrowserGap(ctx, geminiDetailGap); err != nil {
			return nil, err
		}
		raw, err := fetchBrowser(ctx, platform, fetch, geminiRequest(geminiDetailRequest, chat.id, cursor))
		if err != nil {
			return nil, err
		}
		payload, err := geminiRPCPayload(platform, raw, geminiDetailRPC)
		if err != nil {
			return nil, err
		}
		turns, next, err := geminiPage(platform, payload, 0)
		if err != nil {
			return nil, err
		}
		if size += len(turns); size > BrowserResponseLimit {
			return nil, platform.wrap(ErrBrowserResponseTooLarge)
		}
		conv.Turns = append(conv.Turns, turns)
		if next == "" {
			break
		}
		if next == cursor {
			return nil, fmt.Errorf("the Gemini conversation %s repeated a page", chat.id)
		}
		cursor = next
	}
	return json.Marshal(conv)
}
