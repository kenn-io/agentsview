// ABOUTME: Parses Gemini Apps conversations read from gemini.google.com by
// ABOUTME: browser sync. The web app returns positional arrays, so every
// ABOUTME: index this file reads is named once below.
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

// GeminiAppsWebConversation is the conversation browser sync assembles from
// the Gemini Apps list entry and every page of its turns.
type GeminiAppsWebConversation struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// UpdatedAt is the list entry's [seconds, nanos] update time.
	UpdatedAt jsontext.Value `json:"updated_at"`
	// Turns holds the pages in request order; each page lists its turns
	// newest first, as the web app returns them.
	Turns []jsontext.Value `json:"turns"`
}

// ErrGeminiAppsWebFormat reports a Gemini Apps response whose arrays no
// longer match the positions this parser reads.
var ErrGeminiAppsWebFormat = errors.New("unrecognized Gemini Apps conversation format")

var geminiAppsChatID = regexp.MustCompile(`^c_[0-9A-Za-z_-]+$`)

// ValidGeminiAppsChatID reports whether id is a Gemini Apps conversation ID.
func ValidGeminiAppsChatID(id string) bool {
	return geminiAppsChatID.MatchString(id)
}

// Positions inside one Gemini Apps turn, checked against the web app on
// 2026-10-21. Index 1 names the previous turn ([conversation, response,
// selected candidate]), not this one, so it is not read.
const (
	geminiTurnIDs       = 0 // [conversation, response]
	geminiTurnPrompt    = 2 // [[prompt text, ...], ...]
	geminiTurnReply     = 3 // [[candidate, ...], _, _, selected candidate ID, ...]
	geminiReplySelected = 3 // index inside geminiTurnReply
	geminiCandidateID   = 0
	geminiCandidateText = 1 // [text, ...]
)

// A [seconds, nanos] pair is accepted only inside 2015..2096, which keeps
// unrelated small integer pairs from being read as timestamps.
const (
	geminiMinEpoch = 1_420_070_400
	geminiMaxEpoch = 4_000_000_000
)

// GeminiAppsTime decodes a [seconds, nanos] pair.
func GeminiAppsTime(raw jsontext.Value) (time.Time, bool) {
	// Both values fit a float64 exactly inside the accepted range.
	var pair []jsontext.Value
	if json.Unmarshal(raw, &pair) != nil || len(pair) < 2 {
		return time.Time{}, false
	}
	var seconds, nanos float64
	if json.Unmarshal(pair[0], &seconds) != nil || json.Unmarshal(pair[1], &nanos) != nil ||
		seconds != float64(int64(seconds)) || seconds < geminiMinEpoch || seconds > geminiMaxEpoch ||
		nanos != float64(int64(nanos)) || nanos < 0 || nanos >= float64(time.Second) {
		return time.Time{}, false
	}
	return time.Unix(int64(seconds), int64(nanos)), true
}

// ParseGeminiAppsWebConversation converts an assembled conversation into a
// session. A conversation with no text yields an empty result, which browser
// sync skips.
func ParseGeminiAppsWebConversation(data []byte) (ParseResult, error) {
	var conv GeminiAppsWebConversation
	if err := json.Unmarshal(data, &conv); err != nil {
		return ParseResult{}, fmt.Errorf("decoding Gemini Apps conversation: %w", err)
	}
	if !ValidGeminiAppsChatID(conv.ID) {
		return ParseResult{}, fmt.Errorf("%w: conversation ID %q", ErrGeminiAppsWebFormat, conv.ID)
	}
	var turns []jsontext.Value
	for _, page := range conv.Turns {
		var pageTurns []jsontext.Value
		if err := json.Unmarshal(page, &pageTurns); err != nil {
			return ParseResult{}, fmt.Errorf("%w: turn page is not an array", ErrGeminiAppsWebFormat)
		}
		turns = append(turns, pageTurns...)
	}

	var msgs []ParsedMessage
	// Pages arrive newest first, so walk the combined list backwards.
	for _, raw := range slices.Backward(turns) {
		var turn []jsontext.Value
		if err := json.Unmarshal(raw, &turn); err != nil || len(turn) <= geminiTurnReply {
			return ParseResult{}, fmt.Errorf("%w: turn is not an array", ErrGeminiAppsWebFormat)
		}
		timestamp := geminiTurnTime(turn)
		// Source IDs name the turn and the shown draft, so switching to
		// another draft reads as a rewrite even when its text extends the old.
		if prompt := geminiString(turn[geminiTurnPrompt], 0, 0); strings.TrimSpace(prompt) != "" {
			msgs = append(msgs, ParsedMessage{
				Ordinal: len(msgs), Role: RoleUser, Content: prompt,
				Timestamp: timestamp, ContentLength: len(prompt),
				SourceUUID: geminiString(turn[geminiTurnIDs], 1),
			})
		}
		if draft, reply := geminiSelectedReply(turn); strings.TrimSpace(reply) != "" {
			msgs = append(msgs, ParsedMessage{
				Ordinal: len(msgs), Role: RoleAssistant, Content: reply,
				Timestamp: timestamp, ContentLength: len(reply), SourceUUID: draft,
			})
		}
	}
	if len(msgs) == 0 {
		return ParseResult{}, nil
	}

	sess := ParsedSession{
		ID:           "gemini-apps:" + conv.ID,
		Project:      "gemini.google.com",
		Machine:      "local",
		Agent:        AgentGeminiApps,
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
	if updated, ok := GeminiAppsTime(conv.UpdatedAt); ok {
		sess.EndedAt = updated
	}
	return ParseResult{Session: sess, Messages: msgs}, nil
}

// GeminiAppsWebPending reports whether the newest turn of an assembled
// conversation has no reply text yet, as while Gemini is still writing it.
// It reads the raw turn, so a turn the parser drops (no prompt text, only an
// attachment) still counts.
func GeminiAppsWebPending(data []byte) bool {
	var conv GeminiAppsWebConversation
	if json.Unmarshal(data, &conv) != nil {
		return false
	}
	for _, page := range conv.Turns {
		var turns [][]jsontext.Value
		if json.Unmarshal(page, &turns) != nil {
			return false
		}
		if len(turns) > 0 {
			// The first page's first turn is the newest.
			return len(turns[0]) <= geminiTurnReply || geminiReplyText(turns[0]) == ""
		}
	}
	return false
}

// geminiTurnTime returns the turn's own [seconds, nanos] pair. Its index has
// moved between web app versions, so scan from the end for the last
// plausible pair. A turn without one keeps a zero time rather than borrowing
// the list time, which changes on every update.
func geminiTurnTime(turn []jsontext.Value) time.Time {
	for i := len(turn) - 1; i > geminiTurnReply; i-- {
		if t, ok := GeminiAppsTime(turn[i]); ok {
			return t
		}
	}
	return time.Time{}
}

// geminiSelectedReply returns the draft the web app shows: the candidate the
// turn selects, or the first candidate when the selection is missing.
func geminiSelectedReply(turn []jsontext.Value) (draft, text string) {
	var reply []jsontext.Value
	var candidates []jsontext.Value
	if json.Unmarshal(turn[geminiTurnReply], &reply) != nil || len(reply) == 0 ||
		json.Unmarshal(reply[0], &candidates) != nil || len(candidates) == 0 {
		return "", ""
	}
	selected := geminiString(turn[geminiTurnReply], geminiReplySelected)
	if selected != "" {
		for _, candidate := range candidates {
			if geminiString(candidate, geminiCandidateID) == selected {
				return selected, geminiString(candidate, geminiCandidateText, 0)
			}
		}
	}
	return geminiString(candidates[0], geminiCandidateID), geminiString(candidates[0], geminiCandidateText, 0)
}

// geminiString follows array indices and returns the string found there, or
// "" when any step is missing or of another type.
func geminiString(raw jsontext.Value, path ...int) string {
	for _, index := range path {
		var items []jsontext.Value
		if json.Unmarshal(raw, &items) != nil || index >= len(items) {
			return ""
		}
		raw = items[index]
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return ""
	}
	return text
}

func geminiReplyText(turn []jsontext.Value) string {
	_, text := geminiSelectedReply(turn)
	return strings.TrimSpace(text)
}
