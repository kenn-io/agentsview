package importer

import (
	"context"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/chromehost"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// gemini_requests.txt names the two read-only batchexecute RPCs. The
// extension's gemini_fetch.js turns each named parameter into the RPC's
// f.req argument and adds the page's request token, so the shapes are not
// literal URLs.
//
//go:embed gemini_requests.txt
var geminiRequests string

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

// ErrGeminiResponseTooLarge marks a conversation over the browser relay limit.
var ErrGeminiResponseTooLarge = errors.New("Gemini response exceeds 32 MiB")

// ErrGeminiAuthRequired reports a missing or expired Gemini browser session.
var ErrGeminiAuthRequired = errors.New("Gemini sign-in required")

var errGeminiFormat = errors.New("Gemini returned a conversation format this version can't read. Update AgentsView and try again")

// geminiDetailGap delays each conversation page request by 3 s with jitter,
// the same pacing as ChatGPT. Tests set it to zero to disable pacing.
var geminiDetailGap = 3 * time.Second

func geminiRequest(shape int, chat, cursor string) string {
	return browserRequest(geminiRequests, shape, "{chat}", url.QueryEscape(chat), "{cursor}", url.QueryEscape(cursor))
}

// geminiRPCPayload unwraps one batchexecute response: an anti-JSON prefix,
// length lines, and chunks whose "wrb.fr" entry for rpc carries the payload
// as a JSON string.
func geminiRPCPayload(body []byte, rpc string) (jsontext.Value, error) {
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
				return nil, fmt.Errorf("Gemini rejected the %s request", rpc)
			}
			value := jsontext.Value(payload)
			if !value.IsValid() {
				return nil, errGeminiFormat
			}
			return value, nil
		}
	}
	return nil, errGeminiFormat
}

// geminiPage reads the cursor at index 1 and the array at index items.
func geminiPage(payload jsontext.Value, items int) (jsontext.Value, string, error) {
	var page []jsontext.Value
	if json.Unmarshal(payload, &page) != nil {
		return nil, "", errGeminiFormat
	}
	var cursor string
	if len(page) > 1 {
		_ = json.Unmarshal(page[1], &cursor)
	}
	if len(page) <= items || string(page[items]) == "null" {
		return jsontext.Value("[]"), cursor, nil
	}
	if page[items].Kind() != '[' {
		return nil, "", errGeminiFormat
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

// SyncGemini imports new and changed Gemini Apps conversations through Chrome.
func SyncGemini(ctx context.Context, store BrowserSyncStore, fetch func(context.Context, string) (ClaudeAIResponse, error), cb *ImportCallbacks, machine ...string,
) (stats ImportStats, retErr error) {
	seen := make(map[string]struct{})
	var formatErr error
	runner := browserSync{
		store: store, agent: parser.AgentGeminiApps, stats: &stats, cb: cb, machine: machine,
		tooLarge: ErrGeminiResponseTooLarge, stop: func(err error) bool {
			httpErr, _ := errors.AsType[*browserHTTPError](err)
			return errors.Is(err, ErrGeminiAuthRequired) || httpErr != nil && httpErr.rateLimited
		},
	}
	cursor := ""
	for page := 0; ; page++ {
		if page == geminiPageLimit {
			return stats, errors.New("Gemini chat list exceeded the page limit")
		}
		raw, err := fetchBrowser(ctx, fetch, chromehost.Gemini, geminiRequest(geminiListRequest, "", cursor))
		if err != nil {
			return stats, err
		}
		payload, err := geminiRPCPayload(raw, geminiListRPC)
		if err != nil {
			return stats, err
		}
		items, next, err := geminiPage(payload, 2)
		if err != nil {
			return stats, err
		}
		var entries []jsontext.Value
		if err := json.Unmarshal(items, &entries); err != nil {
			return stats, errGeminiFormat
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
			id := "gemini-apps:" + chat.id
			err := runner.conversation(ctx, id, browserConversation{
				freshness: func(session *db.Session) uint64 {
					return browserFreshness(strconv.FormatInt(chat.updatedAt.UnixNano(), 10), session)
				},
				fetch: func(ctx context.Context) ([]byte, error) {
					return fetchGeminiConversation(ctx, fetch, chat)
				},
				parse: func(detail []byte) (parser.ParseResult, error) {
					result, err := parser.ParseGeminiAppsWebConversation(detail)
					if errors.Is(err, parser.ErrGeminiAppsWebFormat) {
						formatErr = errGeminiFormat
					}
					return result, err
				},
				write:   syncConversation,
				matches: func([]byte, parser.ParseResult) bool { return true },
				pending: parser.GeminiAppsWebPending,
			})
			if err != nil {
				return stats, err
			}
		}
		if next == "" {
			break
		}
		if next == cursor {
			return stats, errors.New("Gemini chat list repeated its page cursor")
		}
		cursor = next
	}
	return stats, formatErr
}

// fetchGeminiConversation follows the turn cursor and assembles the pages
// for parser.ParseGeminiAppsWebConversation. The assembled conversation has
// the same 32 MiB limit as a single response.
func fetchGeminiConversation(ctx context.Context, fetch func(context.Context, string) (ClaudeAIResponse, error), chat geminiChat) ([]byte, error) {
	conv := parser.GeminiAppsWebConversation{ID: chat.id, Title: chat.title, UpdatedAt: chat.updated}
	size, cursor := 0, ""
	for page := 0; ; page++ {
		if page == geminiPageLimit {
			return nil, fmt.Errorf("Gemini conversation %s exceeded the page limit", chat.id)
		}
		if err := waitBrowserGap(ctx, geminiDetailGap); err != nil {
			return nil, err
		}
		raw, err := fetchBrowser(ctx, fetch, chromehost.Gemini, geminiRequest(geminiDetailRequest, chat.id, cursor))
		if err != nil {
			return nil, err
		}
		payload, err := geminiRPCPayload(raw, geminiDetailRPC)
		if err != nil {
			return nil, err
		}
		turns, next, err := geminiPage(payload, 0)
		if err != nil {
			return nil, err
		}
		if size += len(turns); size > ClaudeAIResponseLimit {
			return nil, ErrGeminiResponseTooLarge
		}
		conv.Turns = append(conv.Turns, turns)
		if next == "" {
			break
		}
		if next == cursor {
			return nil, fmt.Errorf("Gemini conversation %s repeated its page cursor", chat.id)
		}
		cursor = next
	}
	return json.Marshal(conv)
}
