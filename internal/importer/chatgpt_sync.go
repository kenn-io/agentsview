package importer

import (
	"context"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/chromehost"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

//go:embed chatgpt_requests.txt
var chatGPTRequests string

const (
	chatGPTListRequest = iota
	chatGPTDetailRequest
	chatGPTPageSize = 50
)

// ErrChatGPTResponseTooLarge marks a conversation over the browser relay limit.
var ErrChatGPTResponseTooLarge = errors.New("ChatGPT response exceeds 32 MiB")

// ErrChatGPTAuthRequired reports a missing or expired ChatGPT browser session.
var ErrChatGPTAuthRequired = errors.New("ChatGPT sign-in required")

func chatGPTRequest(shape int, conversation string, offset int) string {
	return strings.NewReplacer(
		"{conversation}", url.PathEscape(conversation),
		"{offset}", strconv.Itoa(offset),
	).Replace(strings.Split(strings.TrimSpace(chatGPTRequests), "\n")[shape])
}

func chatGPTFreshness(updatedAt time.Time, session *db.Session) uint64 {
	hash := fnv.New64a()
	fmt.Fprintf(hash, "%d\x00%d", updatedAt.UnixMilli(), session.MessageCount)
	for _, field := range []*string{session.TranscriptRevision, session.SessionName, session.EndedAt} {
		fmt.Fprint(hash, "\x00")
		if field != nil {
			fmt.Fprint(hash, *field)
		}
	}
	return hash.Sum64()
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

type chatGPTHTTPError struct {
	status      int
	rateLimited bool
}

func (e *chatGPTHTTPError) Error() string {
	return fmt.Sprintf("ChatGPT returned HTTP %d", e.status)
}

// SyncChatGPT imports changed conversations through the same conversation
// importer used for ChatGPT exports, preserving its append and refusal rules.
func SyncChatGPT(ctx context.Context, store interface {
	db.Store
	IsSessionTrashed(context.Context, string) bool
	IsSessionExcluded(context.Context, string) bool
	GetProviderStatHash(context.Context, parser.AgentType, string) (uint64, bool, error)
	UpsertProviderStatHash(context.Context, parser.AgentType, string, uint64) error
}, fetch func(context.Context, string) (ClaudeAIResponse, error), cb *ImportCallbacks, machine ...string,
) (stats ImportStats, retErr error) {
	seen := make(map[string]struct{})
	failedLast := false
	for offset := 0; ; {
		raw, err := fetchBrowser(ctx, fetch, chromehost.ChatGPT, chatGPTRequest(chatGPTListRequest, "", offset))
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
			if page.Total != nil && offset < *page.Total {
				return stats, errors.New("ChatGPT list returned an empty page before total")
			}
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
			if err := json.Unmarshal(item, &marker); err != nil || !isUUID(marker.ID) {
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
			if store.IsSessionTrashed(ctx, id) || store.IsSessionExcluded(ctx, id) {
				failedLast = false
				stats.Skipped++
				cb.progress(stats)
				continue
			}
			detail, unchanged, fetchErr, err := fetchConversationDetailIfChanged(
				ctx, store, parser.AgentChatGPT, id,
				func(session *db.Session) uint64 {
					return chatGPTFreshness(updatedAt, session)
				},
				func(ctx context.Context) ([]byte, error) {
					if err := waitChatGPTDetailGap(ctx); err != nil {
						return nil, err
					}
					return fetchBrowser(ctx, fetch, chromehost.ChatGPT, chatGPTRequest(chatGPTDetailRequest, marker.ID, 0))
				},
			)
			if err != nil {
				return stats, err
			}
			if unchanged {
				failedLast = false
				stats.Skipped++
				cb.progress(stats)
				continue
			}
			if fetchErr != nil {
				if ctx.Err() != nil {
					return stats, ctx.Err()
				}
				if errors.Is(fetchErr, ErrChatGPTAuthRequired) {
					return stats, fetchErr
				}
				detailError, _ := errors.AsType[*chatGPTHTTPError](fetchErr)
				if detailError != nil && detailError.status == http.StatusNotFound {
					failedLast = false
					stats.Skipped++
				} else {
					stats.record(id, importSkipped, fetchErr)
					if detailError != nil && detailError.rateLimited {
						cb.progress(stats)
						return stats, fetchErr
					}
					if !errors.Is(fetchErr, ErrChatGPTResponseTooLarge) {
						if failedLast {
							cb.progress(stats)
							return stats, fetchErr
						}
						failedLast = true
					} else {
						failedLast = false
					}
				}
				cb.progress(stats)
				continue
			}
			failedLast = false
			result, err := parser.ParseChatGPTConversation(detail)
			if err == nil && result.Session.ID != id {
				err = errors.New("conversation ID differs from requested UUID")
			}
			if err != nil {
				stats.record(id, importSkipped, err)
				cb.progress(stats)
				continue
			}
			result.Session.Machine = resolvedImportMachine(result.Session.Machine, machine)
			write := func() error {
				before, err := store.GetSessionFull(ctx, id)
				if err != nil {
					stats.record(id, importSkipped, err)
					return nil
				}
				archived, err := store.GetAllMessages(ctx, id)
				if err != nil {
					stats.record(id, importSkipped, err)
					return nil
				}
				keepChatGPTImages(result.Messages, archived)
				status, err := upsertChatGPTConversation(ctx, store, result, nil)
				if errors.Is(err, db.ErrSessionTrashed) {
					stats.record(id, importSkipped, nil)
					return nil
				}
				if err == nil && status == importSkipped && before != nil {
					// Messages can match while a rename was saved; count that as an update.
					after, readErr := store.GetSessionFull(ctx, id)
					if readErr != nil {
						stats.record(id, importSkipped, readErr)
						return nil
					}
					if after != nil && !ptrEqual(before.SessionName, after.SessionName) {
						status = importUpdated
					}
				}
				stats.record(id, status, err)
				if err != nil {
					return nil //nolint:nilerr // The failure is already recorded in stats above.
				}
				stored, readErr := store.GetSessionFull(ctx, id)
				if readErr == nil && stored != nil && result.Session.EndedAt.UnixMilli() == updatedAt.UnixMilli() {
					readErr = store.UpsertProviderStatHash(ctx, parser.AgentChatGPT, id, chatGPTFreshness(updatedAt, stored))
				}
				if readErr != nil {
					stats.record(id, importSkipped, readErr)
					return readErr
				}
				return nil
			}
			if cb != nil && cb.SerializeWrite != nil {
				err = cb.SerializeWrite(write)
			} else {
				err = write()
			}
			cb.progress(stats)
			if err != nil {
				return stats, err
			}
		}
		if len(page.Items) < chatGPTPageSize || page.Total != nil && offset+len(page.Items) >= *page.Total {
			break
		}
		offset += len(page.Items)
	}
	return stats, nil
}

// A browser detail has placeholders where ZIP imports can retain image references.
// Substitute only for comparison, and only when the rest of the message matches.
var chatGPTImageToken = regexp.MustCompile(`\[image unavailable\]|!\[image\]\([^)]*\)`)

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
		tokens := chatGPTImageToken.FindAllString(message.Content, -1)
		index := 0
		comparison := chatGPTImageToken.ReplaceAllStringFunc(stored.Content, func(token string) string {
			defer func() { index++ }()
			if index < len(tokens) && tokens[index] == "[image unavailable]" {
				return tokens[index]
			}
			return token
		})
		if index == len(tokens) && comparison == message.Content {
			message.Content = stored.Content
			message.ContentLength = stored.ContentLength
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

func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
