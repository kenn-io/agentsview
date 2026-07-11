package db

import (
	"bytes"
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

var (
	automatedPrefixBytes    = automationPatternsAsBytes(automatedPrefixes)
	automatedSubstringBytes = automationPatternsAsBytes(automatedSubstrings)
)

const userPatternMaxLen = 1024

// AutomationEvidencePrefixBytes is the bounded prefix size used by backend
// integrity audits. It is large enough to hold every accepted user pattern.
const AutomationEvidencePrefixBytes = userPatternMaxLen

var (
	userPatternsMu   sync.RWMutex
	userPrefixes     []string
	userSubstrings   []string
	userExactMatches []string
)

// SetUserAutomationPrefixes replaces the user-prefix slice
// with a normalized copy of the input. Normalization (trim,
// drop empty, length cap, dedupe within input, drop entries
// that equal a built-in prefix) happens here so callers can
// pass the raw list straight from config. Pass nil to clear.
// Idempotent and silent, safe to call from quiet CLI paths
// (statusline, JSON output). Callers that want a startup
// summary should read the relevant getter(s).
func SetUserAutomationPrefixes(prefixes []string) {
	setUserAutomationPatterns(&userPrefixes, prefixes, automatedPrefixes)
}

// SetUserAutomationSubstrings replaces the user-substring
// slice with a normalized copy of the input. The same trim,
// dedupe, and length rules apply; overlap filtering only
// removes built-in substrings.
func SetUserAutomationSubstrings(substrings []string) {
	setUserAutomationPatterns(&userSubstrings, substrings, automatedSubstrings)
}

// SetUserAutomationExactMatches replaces the user exact-match
// slice with a normalized copy of the input. Matching trims
// the message before comparing against these literals.
func SetUserAutomationExactMatches(matches []string) {
	setUserAutomationPatterns(&userExactMatches, matches, automatedExactMatches)
}

// UserAutomationPrefixes returns a copy of the current
// user-prefix slice. Used by ClassifierHash and tests; the
// copy prevents callers from mutating singleton state.
func UserAutomationPrefixes() []string {
	return copyUserAutomationPatterns(&userPrefixes)
}

// UserAutomationSubstrings returns a copy of the current
// user-substring slice.
func UserAutomationSubstrings() []string {
	return copyUserAutomationPatterns(&userSubstrings)
}

// UserAutomationExactMatches returns a copy of the current
// user exact-match slice.
func UserAutomationExactMatches() []string {
	return copyUserAutomationPatterns(&userExactMatches)
}

func setUserAutomationPatterns(dst *[]string, in []string, builtIns []string) {
	cleaned := normalizeUserAutomationPatterns(in, builtIns)
	userPatternsMu.Lock()
	defer userPatternsMu.Unlock()
	*dst = cleaned
}

func copyUserAutomationPatterns(src *[]string) []string {
	userPatternsMu.RLock()
	defer userPatternsMu.RUnlock()
	return append([]string(nil), (*src)...)
}

func snapshotUserAutomationPatterns() (
	prefixes, substrings, exactMatches []string,
) {
	userPatternsMu.RLock()
	defer userPatternsMu.RUnlock()
	return append([]string(nil), userPrefixes...),
		append([]string(nil), userSubstrings...),
		append([]string(nil), userExactMatches...)
}

type automationPatternSnapshot struct {
	userPrefixes       []string
	userSubstrings     []string
	userExactMatches   []string
	userPrefixBytes    [][]byte
	userSubstringBytes [][]byte
}

// AutomationClassifier is an immutable pattern snapshot for a complete
// storage audit. Reusing it keeps every row on the same classifier version and
// avoids rebuilding user-pattern byte slices per row.
type AutomationClassifier struct {
	patterns automationPatternSnapshot
}

// SnapshotAutomationClassifier captures the current built-in and user
// automation patterns for reuse across one backend audit.
func SnapshotAutomationClassifier() AutomationClassifier {
	return AutomationClassifier{patterns: snapshotAutomationPatterns()}
}

func snapshotAutomationPatterns() automationPatternSnapshot {
	prefixes, substrings, exactMatches := snapshotUserAutomationPatterns()
	return automationPatternSnapshot{
		userPrefixes:       prefixes,
		userSubstrings:     substrings,
		userExactMatches:   exactMatches,
		userPrefixBytes:    automationPatternsAsBytes(prefixes),
		userSubstringBytes: automationPatternsAsBytes(substrings),
	}
}

func automationPatternsAsBytes(patterns []string) [][]byte {
	if len(patterns) == 0 {
		return nil
	}
	out := make([][]byte, len(patterns))
	for i, pattern := range patterns {
		out[i] = []byte(pattern)
	}
	return out
}

// normalizeUserAutomationPatterns applies the validation rules from the
// design spec ("Validation behavior" section). Built-in overlap is checked
// against the category-specific pattern slice directly.
func normalizeUserAutomationPatterns(in []string, builtIns []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if s == "" || len(s) > userPatternMaxLen {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		if slices.Contains(builtIns, s) {
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
	return snapshotAutomationPatterns().matches(firstMessage)
}

func (patterns automationPatternSnapshot) matches(firstMessage string) bool {
	for _, prefix := range automatedPrefixes {
		if strings.HasPrefix(firstMessage, prefix) {
			return true
		}
	}
	for _, prefix := range patterns.userPrefixes {
		if strings.HasPrefix(firstMessage, prefix) {
			return true
		}
	}
	for _, sub := range automatedSubstrings {
		if strings.Contains(firstMessage, sub) {
			return true
		}
	}
	for _, sub := range patterns.userSubstrings {
		if strings.Contains(firstMessage, sub) {
			return true
		}
	}
	trimmed := strings.TrimSpace(firstMessage)
	if slices.Contains(automatedExactMatches, trimmed) {
		return true
	}
	if slices.Contains(patterns.userExactMatches, trimmed) {
		return true
	}
	return false
}

// AutomationVerdictFromPrefix classifies bounded byte evidence. A visible
// prefix or substring match is definitive even when text was truncated. A
// non-match is definitive only when the prefix contains the complete value;
// unseen bytes may contain a substring or change an exact-match verdict.
func AutomationVerdictFromPrefix(
	prefix []byte, fullByteLength int64,
) (matched, conclusive bool) {
	return snapshotAutomationPatterns().verdictFromPrefix(
		prefix, fullByteLength,
	)
}

func (patterns automationPatternSnapshot) verdictFromPrefix(
	prefix []byte, fullByteLength int64,
) (matched, conclusive bool) {
	for _, pattern := range automatedPrefixBytes {
		if bytes.HasPrefix(prefix, pattern) {
			return true, true
		}
	}
	for _, pattern := range patterns.userPrefixBytes {
		if bytes.HasPrefix(prefix, pattern) {
			return true, true
		}
	}
	for _, pattern := range automatedSubstringBytes {
		if bytes.Contains(prefix, pattern) {
			return true, true
		}
	}
	for _, pattern := range patterns.userSubstringBytes {
		if bytes.Contains(prefix, pattern) {
			return true, true
		}
	}
	if fullByteLength != int64(len(prefix)) {
		return false, false
	}
	trimmed := strings.TrimSpace(string(prefix))
	return slices.Contains(automatedExactMatches, trimmed) ||
		slices.Contains(patterns.userExactMatches, trimmed), true
}

// AutomationTextEvidence is bounded text and its complete byte length. Valid
// distinguishes a missing SQL value from an empty complete string.
type AutomationTextEvidence struct {
	Prefix         []byte
	FullByteLength int64
	Valid          bool
}

// AutomationVerdictFromEvidence combines the first stored user message and
// fallback first_message using the same ordering and single-turn rule as the
// full-text classifier. An inconclusive result requires full-text fallback.
func AutomationVerdictFromEvidence(
	userMessageCount int,
	firstUser, firstMessage AutomationTextEvidence,
) (matched, conclusive bool) {
	return SnapshotAutomationClassifier().VerdictFromEvidence(
		userMessageCount, firstUser, firstMessage,
	)
}

// VerdictFromEvidence classifies bounded evidence using this snapshot.
func (classifier AutomationClassifier) VerdictFromEvidence(
	userMessageCount int,
	firstUser, firstMessage AutomationTextEvidence,
) (matched, conclusive bool) {
	return classifier.patterns.verdictFromEvidence(
		userMessageCount, firstUser, firstMessage,
	)
}

func (patterns automationPatternSnapshot) verdictFromEvidence(
	userMessageCount int,
	firstUser, firstMessage AutomationTextEvidence,
) (matched, conclusive bool) {
	if userMessageCount > 1 {
		return false, true
	}

	unresolved := false
	for _, candidate := range [...]AutomationTextEvidence{firstUser, firstMessage} {
		if !candidate.Valid {
			continue
		}
		matched, conclusive := patterns.verdictFromPrefix(
			candidate.Prefix, candidate.FullByteLength,
		)
		if matched {
			return true, true
		}
		if !conclusive {
			unresolved = true
		}
	}
	return false, !unresolved
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
	return IsAutomatedFromTextCandidates(
		userMessageCount, firstUserMessage, firstMessage,
	)
}

// IsAutomatedFromTextCandidates applies the authoritative transcript fallback
// order to storage values shared by the SQLite and PostgreSQL audits.
func IsAutomatedFromTextCandidates(
	userMessageCount int,
	firstUserMessage, firstMessage sql.NullString,
) bool {
	return SnapshotAutomationClassifier().IsAutomatedFromTextCandidates(
		userMessageCount, firstUserMessage, firstMessage,
	)
}

// IsAutomatedFromTextCandidates applies the full-text fallback order using
// this classifier snapshot.
func (classifier AutomationClassifier) IsAutomatedFromTextCandidates(
	userMessageCount int,
	firstUserMessage, firstMessage sql.NullString,
) bool {
	return classifier.patterns.matchesTextCandidates(
		userMessageCount, firstUserMessage, firstMessage,
	)
}

func (patterns automationPatternSnapshot) matchesTextCandidates(
	userMessageCount int,
	firstUserMessage, firstMessage sql.NullString,
) bool {
	if userMessageCount > 1 {
		return false
	}
	if firstUserMessage.Valid &&
		strings.TrimSpace(firstUserMessage.String) != "" &&
		patterns.matches(firstUserMessage.String) {
		return true
	}
	return firstMessage.Valid && patterns.matches(firstMessage.String)
}
