package db

import (
	"database/sql"
	"slices"
	"strings"
	"sync"
)

// automatedPrefixes are first_message prefixes that identify
// automated (roborev) sessions. Matched case-sensitively.
// Combined with the single-turn gate (user_message_count <= 1)
// to avoid misclassifying interactive sessions.
var automatedPrefixes = []string{
	"You are a code reviewer.",
	"You are a security code reviewer.",
	"You are a design reviewer.",
	"You are a code assistant. Your task is to address",
	"You are a code review insights analyst.",
	"You are reviewing whether an implementation matches",
	"You are a plan document reviewer.",
	"You are a spec document reviewer.",
	"You are summarizing a day of AI agent activity.",
	"You are analyzing AI agent sessions.",
	"## Analysis Request",
	"# Fix Request",
	"You are a helpful assistant working on a software project.",
	"You are combining multiple code review outputs into a single GitHub PR comment.",
	"You are generating a changelog",
	"<user_action>",
	"Review the code changes introduced by commit ",
	"Review the code changes in commit ",
	"Implement the following plan:",
}

// automatedSubstrings are patterns matched anywhere in the
// first message. Used for catch-all markers embedded in
// longer prompts.
var automatedSubstrings = []string{
	"invoked by roborev to perform this review",
	"You are a security code reviewer with an exploitability burden of proof. Review the code changes for concrete vulnerabilities",
	"You are a conversation title generator",
}

// automatedExactMatches are first messages that, after trimming
// surrounding whitespace, exactly equal one of these strings.
// Used for prompts too generic for prefix or substring matching
// (e.g., a single-word warmup ping).
var automatedExactMatches = []string{
	"Warmup",
	"Respond with exactly: OK",
	"Reply with exactly OK.",
}

const userPrefixMaxLen = 1024

var (
	userPrefixesMu sync.RWMutex
	userPrefixes   []string
)

// SetUserAutomationPrefixes replaces the user-pattern slice
// with a normalized copy of the input. Normalization (trim,
// drop empty, length cap, dedupe within input, drop entries
// that equal a built-in prefix) happens here so callers can
// pass the raw list straight from config. Pass nil to clear.
// Idempotent and silent — safe to call from quiet CLI paths
// (statusline, JSON output). Callers that want a startup
// summary should read len(UserAutomationPrefixes()).
func SetUserAutomationPrefixes(prefixes []string) {
	cleaned := normalizeUserPrefixes(prefixes)
	userPrefixesMu.Lock()
	defer userPrefixesMu.Unlock()
	userPrefixes = cleaned
}

// UserAutomationPrefixes returns a copy of the current
// user-prefix slice. Used by ClassifierHash and tests; the
// copy prevents callers from mutating singleton state.
func UserAutomationPrefixes() []string {
	userPrefixesMu.RLock()
	defer userPrefixesMu.RUnlock()
	return append([]string(nil), userPrefixes...)
}

// normalizeUserPrefixes applies the validation rules from the
// design spec ("Validation behavior" section). Built-in
// overlap is checked against the package-private
// automatedPrefixes directly.
func normalizeUserPrefixes(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if s == "" || len(s) > userPrefixMaxLen {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		if slices.Contains(automatedPrefixes, s) {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// IsAutomatedSession returns true if the first message
// matches a known automated review/fix prompt pattern.
func IsAutomatedSession(firstMessage string) bool {
	for _, prefix := range automatedPrefixes {
		if strings.HasPrefix(firstMessage, prefix) {
			return true
		}
	}
	userPrefixesMu.RLock()
	for _, prefix := range userPrefixes {
		if strings.HasPrefix(firstMessage, prefix) {
			userPrefixesMu.RUnlock()
			return true
		}
	}
	userPrefixesMu.RUnlock()
	for _, sub := range automatedSubstrings {
		if strings.Contains(firstMessage, sub) {
			return true
		}
	}
	trimmed := strings.TrimSpace(firstMessage)
	return slices.Contains(automatedExactMatches, trimmed)
}

// IsAutomatedTranscript classifies automation from the actual
// transcript and the stored first_message. The first_message
// candidate preserves legacy/imported rows whose messages are
// unavailable or less specific than the parser-owned preview.
func IsAutomatedTranscript(
	userMessageCount int,
	msgs []Message,
	firstMessage *string,
) bool {
	if userMessageCount > 1 {
		return false
	}
	if firstUser, ok := firstUserMessageContent(msgs); ok &&
		IsAutomatedSession(firstUser) {
		return true
	}
	return firstMessage != nil && IsAutomatedSession(*firstMessage)
}

func firstUserMessageContent(msgs []Message) (string, bool) {
	for _, m := range msgs {
		if m.Role != "user" || m.IsSystem {
			continue
		}
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		return m.Content, true
	}
	return "", false
}

func isAutomatedFromTextCandidates(
	userMessageCount int,
	firstUserMessage, firstMessage sql.NullString,
) bool {
	if userMessageCount > 1 {
		return false
	}
	if firstUserMessage.Valid &&
		strings.TrimSpace(firstUserMessage.String) != "" &&
		IsAutomatedSession(firstUserMessage.String) {
		return true
	}
	return firstMessage.Valid && IsAutomatedSession(firstMessage.String)
}
