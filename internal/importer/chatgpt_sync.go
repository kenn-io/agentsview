package importer

import (
	"context"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math/rand/v2"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/agentsview/internal/chromehost"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

//go:embed chatgpt_requests.txt
var chatGPTRequests string

const (
	chatGPTListRequest = iota
	chatGPTDetailRequest
)

// ErrChatGPTResponseTooLarge marks a conversation over the browser relay limit.
var ErrChatGPTResponseTooLarge = errors.New("ChatGPT response exceeds 32 MiB")

// ErrChatGPTAuthRequired reports a missing or expired ChatGPT browser session.
var ErrChatGPTAuthRequired = errors.New("ChatGPT sign-in required")

func chatGPTRequest(shape int, conversation string, offset int, archived bool) string {
	return browserRequest(chatGPTRequests, shape,
		"{conversation}", url.PathEscape(conversation), "{offset}", strconv.Itoa(offset), "{archived}", strconv.FormatBool(archived))
}

func chatGPTFreshness(updatedAt time.Time, session *db.Session) uint64 {
	return browserFreshness(strconv.FormatInt(updatedAt.UnixMilli(), 10), session)
}

// chatGPTDetailGap spaces conversation detail requests. hstry's extension
// used 1.5 s; the first live run uses 3 s for a wider margin. Tests set it to 0.
var chatGPTDetailGap = 3 * time.Second

func waitChatGPTDetailGap(ctx context.Context) error {
	if chatGPTDetailGap <= 0 {
		return nil
	}
	jitter := time.Duration(rand.Int64N(int64(800*time.Millisecond))) - 400*time.Millisecond
	timer := time.NewTimer(chatGPTDetailGap + jitter)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SyncChatGPT appends new turns and replaces edited conversation branches.
func SyncChatGPT(ctx context.Context, store BrowserSyncStore, fetch func(context.Context, string) (ClaudeAIResponse, error), cb *ImportCallbacks, machine ...string,
) (stats ImportStats, retErr error) {
	seen := make(map[string]struct{})
	runner := browserSync{
		store: store, agent: parser.AgentChatGPT, stats: &stats, cb: cb, machine: machine,
		tooLarge: ErrChatGPTResponseTooLarge, stop: func(err error) bool {
			httpErr, _ := errors.AsType[*browserHTTPError](err)
			return errors.Is(err, ErrChatGPTAuthRequired) || httpErr != nil && httpErr.rateLimited
		},
	}
	for _, archived := range []bool{false, true} {
		for offset := 0; ; {
			raw, err := fetchBrowser(ctx, fetch, chromehost.ChatGPT, chatGPTRequest(chatGPTListRequest, "", offset, archived))
			if err != nil {
				return stats, err
			}
			var page struct {
				Items []jsontext.Value `json:"items"`
				Total *int             `json:"total"`
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				return stats, err
			}
			if page.Items == nil {
				return stats, errors.New("ChatGPT list had no items")
			}
			if len(page.Items) == 0 {
				break
			}
			for _, item := range page.Items {
				if err := ctx.Err(); err != nil {
					return stats, err
				}
				var marker struct {
					ID        string         `json:"id"`
					UpdatedAt jsontext.Value `json:"update_time"`
				}
				parseErr := json.Unmarshal(item, &marker)
				_, uuidErr := uuid.Parse(marker.ID)
				if parseErr != nil || uuidErr != nil {
					stats.Errors++
					cb.progress(stats)
					continue
				}
				updatedAt, validTime := chatGPTUpdateTime(marker.UpdatedAt)
				if !validTime {
					stats.Errors++
					cb.progress(stats)
					continue
				}
				if _, ok := seen[marker.ID]; ok {
					continue
				}
				seen[marker.ID] = struct{}{}
				id := "chatgpt:" + marker.ID
				err := runner.conversation(ctx, id, browserConversation{
					freshness: func(session *db.Session) uint64 { return chatGPTFreshness(updatedAt, session) },
					fetch: func(ctx context.Context) ([]byte, error) {
						if err := waitChatGPTDetailGap(ctx); err != nil {
							return nil, err
						}
						return fetchBrowser(ctx, fetch, chromehost.ChatGPT, chatGPTRequest(chatGPTDetailRequest, marker.ID, 0, false))
					},
					parse: func(detail []byte) (parser.ParseResult, error) {
						var shape struct {
							Mapping jsontext.Value `json:"mapping"`
						}
						if err := json.Unmarshal(detail, &shape); err == nil && shape.Mapping.Kind() != '{' {
							return parser.ParseResult{}, errChatGPTFormat
						}
						result, err := parser.ParseChatGPTConversation(detail)
						if err == nil && result.Session.ID != "" && result.Session.ID != id {
							err = errors.New("conversation ID differs from requested UUID")
						}
						return result, err
					},
					write: syncChatGPTConversation,
					matches: func(_ []byte, result parser.ParseResult) bool {
						return result.Session.EndedAt.Sub(updatedAt).Abs() <= time.Millisecond
					},
				})
				if err != nil {
					return stats, err
				}
			}
			if page.Total != nil && offset+len(page.Items) >= *page.Total {
				break
			}
			offset += len(page.Items)
		}
	}
	return stats, nil
}

var errChatGPTFormat = errors.New("ChatGPT returned a conversation format this version can't read. Update AgentsView and try again")

func syncChatGPTConversation(ctx context.Context, store db.Store, result parser.ParseResult) (importStatus, error) {
	for _, message := range result.Messages {
		if strings.Contains(message.Content, "[image unavailable]") {
			archived, err := store.GetAllMessages(ctx, result.Session.ID)
			if err != nil {
				return importSkipped, err
			}
			keepChatGPTImages(result.Messages, archived)
			break
		}
	}
	for _, message := range result.Messages {
		if message.Role == parser.RoleUser && !message.IsSystem && message.Content != "" {
			result.Session.FirstMessage = message.Content
			break
		}
	}
	status, err := upsertChatGPTConversation(ctx, store, result, nil)
	if replaceable(err) {
		return replaceSyncedConversation(ctx, store, result, chatGPTMessages(result.Session.ID, result.Messages))
	}
	return status, err
}

var chatGPTImageLink = regexp.MustCompile(`!\[image\]\([^)]*\)`)

func keepChatGPTImages(incoming []parser.ParsedMessage, archived []db.Message) {
	byOrdinal := make(map[int]db.Message, len(archived))
	for _, message := range archived {
		byOrdinal[message.Ordinal] = message
	}
	for i := range incoming {
		message := &incoming[i]
		stored, ok := byOrdinal[message.Ordinal]
		if !ok || stored.Role != string(message.Role) || !strings.Contains(message.Content, "[image unavailable]") {
			continue
		}
		comparison := chatGPTImageLink.ReplaceAllString(stored.Content, "[image unavailable]")
		if comparison == message.Content || db.IsTextExtension(comparison, message.Content) {
			message.Content = stored.Content + message.Content[len(comparison):]
			message.ContentLength = len(message.Content)
		}
	}
}

func chatGPTUpdateTime(raw jsontext.Value) (time.Time, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if instant, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return instant, true
		}
		raw = jsontext.Value(text)
	}
	var numeric float64
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &numeric) != nil {
		return time.Time{}, false
	}
	seconds := int64(numeric)
	return time.Unix(seconds, int64((numeric-float64(seconds))*1e9)), true
}
