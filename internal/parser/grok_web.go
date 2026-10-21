// ABOUTME: Parses Grok conversations read from grok.com by browser sync. The
// ABOUTME: web app returns every branch of a chat; the parser keeps the branch
// ABOUTME: the web app shows, which ends at the newest response with text.
package parser

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// GrokWebConversation is the conversation browser sync assembles from the
// Grok list entry and the chat's /responses body.
type GrokWebConversation struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// ModifyTime is the list entry's RFC 3339 update time.
	ModifyTime string `json:"modify_time"`
	// Responses is the /responses body as the web app returned it.
	Responses jsontext.Value `json:"responses"`
}

// grokWebResponse holds the fields the parser reads from one response.
// Responses form a tree through ParentResponseID.
type grokWebResponse struct {
	ResponseID       string `json:"responseId"`
	ParentResponseID string `json:"parentResponseId"`
	Sender           string `json:"sender"`
	Message          string `json:"message"`
	CreateTime       string `json:"createTime"`
	Model            string `json:"model"`
	Partial          bool   `json:"partial"`
}

type grokWebResponses struct {
	Responses *[]grokWebResponse `json:"responses"`
	Inflight  []jsontext.Value   `json:"inflightResponses"`
}

// ErrGrokWebFormat reports a Grok response this parser cannot read.
var ErrGrokWebFormat = errors.New("unrecognized Grok conversation format")

var grokWebID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidGrokWebID reports whether id is a Grok conversation or page ID.
func ValidGrokWebID(id string) bool {
	return grokWebID.MatchString(id)
}

// GrokWebTime decodes a Grok RFC 3339 time.
func GrokWebTime(value string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, value)
	return t, err == nil
}

func decodeGrokWebConversation(data []byte) (GrokWebConversation, grokWebResponses, error) {
	var conv GrokWebConversation
	if err := json.Unmarshal(data, &conv); err != nil {
		return conv, grokWebResponses{}, fmt.Errorf("decoding Grok conversation: %w", err)
	}
	if !ValidGrokWebID(conv.ID) {
		return conv, grokWebResponses{}, fmt.Errorf("%w: conversation ID %q", ErrGrokWebFormat, conv.ID)
	}
	var body grokWebResponses
	if err := json.Unmarshal(conv.Responses, &body); err != nil || body.Responses == nil {
		return conv, body, fmt.Errorf("%w: no responses array", ErrGrokWebFormat)
	}
	for _, r := range *body.Responses {
		if r.ResponseID == "" || r.Sender == "" {
			return conv, body, fmt.Errorf("%w: response without an ID or sender", ErrGrokWebFormat)
		}
	}
	return conv, body, nil
}

// grokWebPath returns the branch the web app shows, root first: the path to
// the newest response with text that has no descendant with text. Failed
// regenerations are empty, so they never end the shown branch, whether they
// sit beside a successful reply or end the chat.
func grokWebPath(responses []grokWebResponse) []grokWebResponse {
	byID := make(map[string]int, len(responses))
	for i, r := range responses {
		byID[r.ResponseID] = i
	}
	// textBelow marks every response that has a descendant with text.
	textBelow := make(map[string]bool, len(responses))
	for _, r := range responses {
		if strings.TrimSpace(r.Message) == "" {
			continue
		}
		seen := map[string]bool{r.ResponseID: true}
		for i, ok := byID[r.ParentResponseID]; ok && !seen[responses[i].ResponseID]; i, ok = byID[responses[i].ParentResponseID] {
			seen[responses[i].ResponseID] = true
			textBelow[responses[i].ResponseID] = true
		}
	}
	leaf, leafAt := -1, time.Time{}
	for i, r := range responses {
		if textBelow[r.ResponseID] || strings.TrimSpace(r.Message) == "" {
			continue
		}
		// Later entries win ties, matching the web app's oldest-first order.
		if at, _ := GrokWebTime(r.CreateTime); leaf < 0 || !at.Before(leafAt) {
			leaf, leafAt = i, at
		}
	}
	var path []grokWebResponse
	seen := make(map[string]bool)
	for i, ok := leaf, leaf >= 0; ok && !seen[responses[i].ResponseID]; i, ok = byID[responses[i].ParentResponseID] {
		seen[responses[i].ResponseID] = true
		path = append(path, responses[i])
	}
	slices.Reverse(path)
	return path
}

// ParseGrokWebConversation converts an assembled conversation into a
// session. A conversation with no text yields an empty result, which browser
// sync skips.
func ParseGrokWebConversation(data []byte) (ParseResult, error) {
	conv, body, err := decodeGrokWebConversation(data)
	if err != nil {
		return ParseResult{}, err
	}
	var msgs []ParsedMessage
	for _, r := range grokWebPath(*body.Responses) {
		if strings.TrimSpace(r.Message) == "" {
			continue
		}
		m := ParsedMessage{
			Ordinal: len(msgs), Role: RoleAssistant, Content: r.Message,
			ContentLength: len(r.Message), SourceUUID: r.ResponseID,
		}
		m.Timestamp, _ = GrokWebTime(r.CreateTime)
		if strings.EqualFold(r.Sender, "human") {
			m.Role = RoleUser
		} else {
			m.Model = r.Model
		}
		msgs = append(msgs, m)
	}
	if len(msgs) == 0 {
		return ParseResult{}, nil
	}

	sess := ParsedSession{
		ID:           "grok-web:" + conv.ID,
		Project:      "grok.com",
		Machine:      "local",
		Agent:        AgentGrokWeb,
		SessionName:  strings.TrimSpace(conv.Title),
		MessageCount: len(msgs),
	}
	for _, m := range msgs {
		if m.Role == RoleUser {
			sess.UserMessageCount++
			if sess.FirstMessage == "" {
				sess.FirstMessage = m.Content
			}
		}
		if sess.StartedAt.IsZero() {
			sess.StartedAt = m.Timestamp
		}
		if m.Timestamp.After(sess.EndedAt) {
			sess.EndedAt = m.Timestamp
		}
	}
	// The list's update time also moves on renames, so it is the end time
	// when it is readable.
	if updated, ok := GrokWebTime(conv.ModifyTime); ok {
		sess.EndedAt = updated
	}
	return ParseResult{Session: sess, Messages: msgs}, nil
}

// GrokWebPending reports whether Grok is still writing a reply in the
// conversation: it lists an in-flight response, or the shown branch has a
// partial one.
func GrokWebPending(data []byte) bool {
	_, body, err := decodeGrokWebConversation(data)
	if err != nil {
		return false
	}
	if len(body.Inflight) > 0 {
		return true
	}
	for _, r := range grokWebPath(*body.Responses) {
		if r.Partial {
			return true
		}
	}
	return false
}
