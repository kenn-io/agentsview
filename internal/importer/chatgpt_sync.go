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
	"slices"
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

// The list and detail endpoints stamp update_time independently; one real capture showed detail 0.68 s later.
const chatGPTUpdateSkew = 5 * time.Second

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

// chatGPTDetailGap delays each detail request by 3 s with jitter.
// Tests set it to zero to disable pacing.
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
	var formatErr error
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
						result, err := parseChatGPTSyncConversation(detail, id)
						if errors.Is(err, errChatGPTFormat) {
							formatErr = err
						}
						return result, err
					},
					write: syncChatGPTConversation,
					matches: func(_ []byte, result parser.ParseResult) bool {
						return result.Session.EndedAt.Sub(updatedAt).Abs() <= chatGPTUpdateSkew
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
	return stats, formatErr
}

var errChatGPTFormat = errors.New("ChatGPT returned a conversation format this version can't read. Update AgentsView and try again")

func parseChatGPTSyncConversation(detail []byte, id string) (parser.ParseResult, error) {
	var shape struct {
		Mapping        jsontext.Value `json:"mapping"`
		ConversationID string         `json:"conversation_id"`
		ID             string         `json:"id"`
	}
	if err := json.Unmarshal(detail, &shape); err == nil &&
		(shape.Mapping.Kind() != '{' || shape.ConversationID == "" && shape.ID == "") {
		return parser.ParseResult{}, errChatGPTFormat
	}
	result, err := parser.ParseChatGPTConversation(detail)
	if err == nil && result.Session.ID != "" && result.Session.ID != id {
		err = errors.New("conversation ID differs from requested UUID")
	}
	return result, err
}

func syncChatGPTConversation(ctx context.Context, store db.Store, result parser.ParseResult) (importStatus, error) {
	if err := fillChatGPTSyncStart(ctx, store, &result); err != nil {
		return importSkipped, err
	}
	if storeArchiveContent(store).UsageOnly() {
		return syncConversation(ctx, store, result)
	}
	archived, err := store.GetAllMessages(ctx, result.Session.ID)
	if err != nil {
		return importSkipped, err
	}
	keepChatGPTImages(result.Messages, archived)
	for _, message := range result.Messages {
		if message.Role == parser.RoleUser && !message.IsSystem && message.Content != "" {
			result.Session.FirstMessage = message.Content
			break
		}
	}
	msgs := chatGPTMessages(result.Session.ID, result.Messages)
	keepArchivedChatGPTModels(msgs, archived)
	canonical := canonicalChatGPTMessages(store, chatGPTSession(result.Session), msgs, storeArchiveContent(store))
	if chatGPTToolsChanged(archived, canonical) {
		status, err := replaceSyncedConversation(ctx, store, result, msgs, chatGPTImagesLost(archived, canonical))
		if errors.Is(err, db.ErrReplaceUnchanged) {
			// Length-only changes still need a write under the transcripts policy.
			err = writeChatGPTSession(ctx, store, chatGPTSession(result.Session), msgs)
		}
		return status, err
	}
	status, err := upsertChatGPTConversation(ctx, store, result, nil, true)
	if replaceable(err) {
		return replaceSyncedConversation(ctx, store, result, msgs, chatGPTImagesLost(archived, canonical))
	}
	if err == nil && status == importSkipped {
		// Sync also refreshes the source timestamp when the messages match.
		return refreshChatGPTTimestamp(ctx, store, chatGPTSession(result.Session))
	}
	return status, err
}

func keepArchivedChatGPTModels(msgs, archived []db.Message) {
	for _, row := range archived {
		if row.Model != "" && row.Ordinal >= 0 && row.Ordinal < len(msgs) && sameTurn(row, msgs[row.Ordinal]) {
			msgs[row.Ordinal].Model = row.Model
		}
	}
}

func fillChatGPTSyncStart(ctx context.Context, store db.Store, result *parser.ParseResult) error {
	if !result.Session.StartedAt.IsZero() {
		return nil
	}
	stored, err := store.GetSessionFull(ctx, result.Session.ID)
	if err != nil {
		return err
	}
	if stored != nil && stored.StartedAt != nil {
		result.Session.StartedAt, _ = db.ParseStoredTimestamp(*stored.StartedAt)
	}
	if result.Session.StartedAt.IsZero() && len(result.Messages) > 0 {
		result.Session.StartedAt = result.Messages[0].Timestamp
	}
	return nil
}

func refreshChatGPTTimestamp(ctx context.Context, store db.Store, session db.Session) (importStatus, error) {
	stored, err := store.GetSessionFull(ctx, session.ID)
	if err != nil {
		return importSkipped, err
	}
	if err := appendChatGPTMessages(ctx, store, session, nil); err != nil {
		return importSkipped, err
	}
	if stored != nil && session.EndedAt != nil && !ptrEqual(stored.EndedAt, session.EndedAt) {
		return importUpdated, nil
	}
	return importSkipped, nil
}

func chatGPTToolsChanged(archived, incoming []db.Message) bool {
	if archived == nil || incoming == nil {
		return false
	}
	for i := range min(len(archived), len(incoming)) {
		if !slices.EqualFunc(archived[i].ToolCalls, incoming[i].ToolCalls, func(a, b db.ToolCall) bool {
			return a.ToolName == b.ToolName && a.Category == b.Category &&
				a.InputJSON == b.InputJSON && (emptyToolResult(a) ||
				a.ResultContent == b.ResultContent && a.ResultContentLength == b.ResultContentLength)
		}) {
			return true
		}
	}
	return false
}

var chatGPTImageLink = regexp.MustCompile(`!\[image\]\(asset://[^)]*\)`)

func chatGPTImagesLost(archived, incoming []db.Message) bool {
	links := make(map[string]struct{})
	for _, message := range incoming {
		for _, link := range chatGPTImageLink.FindAllString(message.Content, -1) {
			links[link] = struct{}{}
		}
	}
	for _, message := range archived {
		for _, link := range chatGPTImageLink.FindAllString(message.Content, -1) {
			if _, ok := links[link]; !ok {
				return true
			}
		}
	}
	return false
}

func keepChatGPTImages(incoming []parser.ParsedMessage, archived []db.Message) {
	byOrdinal := make(map[int]db.Message, len(archived))
	for _, message := range archived {
		byOrdinal[message.Ordinal] = message
	}
	for i := range incoming {
		message := &incoming[i]
		clean := db.SanitizeUTF8(message.Content)
		message.ContentLength -= len(message.Content) - len(clean)
		message.Content = clean
		stored, ok := byOrdinal[message.Ordinal]
		// Generated images can change without changing the assistant text.
		// Only retain uploads on the same user turn.
		if !ok || message.Role != parser.RoleUser || stored.Role != string(message.Role) ||
			message.Timestamp.IsZero() || stored.SourceUUID != "" && stored.SourceUUID != message.SourceUUID ||
			stored.SourceParentUUID != "" && stored.SourceParentUUID != message.SourceParentUUID ||
			stored.Timestamp != message.Timestamp.UTC().Format(time.RFC3339Nano) ||
			!strings.Contains(message.Content, "[image unavailable]") {
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
