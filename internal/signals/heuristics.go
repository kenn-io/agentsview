package signals

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// HeuristicMessage is the message subset needed by deterministic
// session-quality heuristics.
type HeuristicMessage struct {
	SourceSubtype string
	Role          string
	Content       string
	IsSystem      bool
	Ordinal       int
	Timestamp     string
}

// HeuristicInput holds session data for deterministic prompt and
// workflow quality analysis.
type HeuristicInput struct {
	Messages []HeuristicMessage
	ToolRows []ToolCallRow
}

// HeuristicSignals holds Coach-derived deterministic session signals.
type HeuristicSignals struct {
	ShortPromptCount            int
	UnstructuredStart           bool
	MissingSuccessCriteriaCount int
	MissingVerificationCount    int
	DuplicatePromptCount        int
	NoCodeContextCount          int
	RunawayToolLoopCount        int
}

// CodeFencePattern matches one fenced code block. Replacing every match
// with a space is how IsFrustrationMarker strips pasted code.
const CodeFencePattern = "(?s)```.*?```"

// FrustrationPhrasePattern is the regular expression IsFrustrationMarker
// matches against a normalized prompt: lowercased, code fences replaced by
// a space, whitespace runs collapsed to one space, and trimmed. It uses
// RE2 syntax, so a store whose regular expressions are RE2 can apply it
// unchanged.
const FrustrationPhrasePattern = `(?i)(!{3,}|\?{3,}|\b(?:wtf|come on|why won't|this is broken|doesn't work|does not work|still broken|same error|you broke|fucking|fuck)\b)`

// Thresholds of IsFrustrationMarker. A prompt whose normalized form is
// shorter than FrustrationMinBytes is never a marker. Otherwise it is one
// when it matches FrustrationPhrasePattern, or when, after code fences are
// stripped, it has at least FrustrationCapsMinWords words and words of two
// or more letters, and at least FrustrationCapsRatio of those longer words
// are all capitals.
const (
	FrustrationMinBytes     = 10
	FrustrationCapsMinWords = 3
	FrustrationCapsRatio    = 0.4
)

var (
	codeFenceRe = regexp.MustCompile(CodeFencePattern)
	fileRefRe   = regexp.MustCompile(
		`(?i)(?:^|[\s"'` + "`" + `])(?:\.{0,2}/)?[a-z0-9_.-]+(?:/[a-z0-9_. -]+)+|[a-z0-9_.-]+\.(?:go|ts|tsx|js|jsx|py|rs|java|kt|rb|php|cs|cpp|c|h|hpp|sql|svelte|vue|css|scss|html|json|ya?ml|toml|md|sh|zsh|bash)`,
	)
	bulletRe            = regexp.MustCompile(`(?m)^\s*(?:[-*+]|\d+\.)\s+\S+`)
	frustrationPhraseRe = regexp.MustCompile(FrustrationPhrasePattern)
)

var controlPrompts = map[string]struct{}{
	"yes": {}, "y": {}, "no": {}, "n": {}, "ok": {}, "okay": {},
	"continue": {}, "go ahead": {}, "proceed": {},
	"do it": {}, "done": {}, "thanks": {}, "thank you": {},
	"please continue": {}, "keep going": {},
}

// AnalyzeHeuristics computes deterministic prompt/context/workflow
// quality signals. It is pure and does not call external services.
func AnalyzeHeuristics(in HeuristicInput) HeuristicSignals {
	prompts := userPrompts(in.Messages)
	codeTask := isCodeTask(prompts)

	var s HeuristicSignals
	s.ShortPromptCount = countShortStartPrompts(prompts)

	if codeTask {
		if first, ok := firstSubstantivePrompt(prompts); ok {
			s.UnstructuredStart = isUnstructuredStart(first)
		}
		if !hasSuccessCriteria(prompts) {
			s.MissingSuccessCriteriaCount = 1
		}
		if !hasVerificationLanguage(prompts) {
			s.MissingVerificationCount = 1
		}
		if !hasPromptContext(prompts) &&
			!hasContextToolActivity(in.ToolRows) {
			s.NoCodeContextCount = 1
		}
	}

	s.DuplicatePromptCount = countDuplicatePrompts(prompts)
	if hasRunawayToolLoop(in.ToolRows) {
		s.RunawayToolLoopCount = 1
	}

	return s
}

// IsFrustrationMarker reports whether a user prompt matches the
// reference Coach frustration rules: repeated punctuation, high
// caps-word ratio, or direct hostile/frustrated language. It strips
// fenced code first so pasted logs do not become tone signals.
//
// Signals runs it over every prompt in range after each restart, so it
// reads each prompt in few passes: one strips fences, one builds the
// normalized form and counts capitalized words, and one looks for every
// phrase literal. The regular expression runs only from the first literal.
func IsFrustrationMarker(content string) bool {
	withoutCode := stripCodeFences(content)
	var words capsWords
	normalized := appendNormalizedPrompt(make([]byte, 0, len(withoutCode)), withoutCode, &words)
	if len(normalized) < FrustrationMinBytes {
		return false
	}
	if start := frustrationPhraseStart(normalized); start >= 0 && frustrationPhraseRe.Match(normalized[start:]) {
		return true
	}
	return words.ratio(FrustrationCapsMinWords) >= FrustrationCapsRatio
}

// frustrationPhraseStart returns where frustrationPhraseRe must start
// reading normalized to find any match it has, or -1 when it has none.
//
// Every alternative of the expression requires one of
// frustrationPhraseLowercase, so checking them first skips the expression
// for the common prompt that cannot match; the expression is an unanchored
// alternation that the Go engine runs as a full NFA, which dominates signals
// analytics for long prompts. normalized is already lowercase, and the
// expression matches with simple case folding, under which long s (U+017F)
// is the only lowercase non-ASCII rune that folds to an ASCII letter.
//
// Without long s, every match therefore begins at an occurrence of a
// literal, so none begins before the first one. Starting one byte earlier
// keeps the byte that decides the leading word boundary; a cut multibyte
// rune reads as U+FFFD, which is no more a word character than the rune.
// With long s, the literals are searched in a copy where it is s, and the
// expression reads all of normalized.
func frustrationPhraseStart(normalized []byte) int {
	if bytes.Contains(normalized, longS) {
		if firstFrustrationLiteral(bytes.ReplaceAll(normalized, longS, []byte("s"))) < 0 {
			return -1
		}
		return 0
	}
	i := firstFrustrationLiteral(normalized)
	if i <= 0 {
		return i
	}
	return i - 1
}

// firstFrustrationLiteral returns the offset of the first occurrence of
// any of frustrationPhraseLowercase in s, or -1.
func firstFrustrationLiteral(s []byte) int {
	for i := 0; i+1 < len(s); i++ {
		pair := uint16(s[i])<<8 | uint16(s[i+1])
		if frustrationPhrasePairs[pair>>6]&(1<<(pair&63)) == 0 {
			continue
		}
		for _, literal := range frustrationPhraseByFirstByte[s[i]] {
			if len(s)-i >= len(literal) && string(s[i:i+len(literal)]) == literal {
				return i
			}
		}
	}
	return -1
}

var longS = []byte("\u017f")

var frustrationPhraseLowercase = []string{
	"!!!", "???", "wtf", "come on", "why won't", "this is broken",
	"doesn't work", "does not work", "still broken", "same error",
	"you broke", "fuck",
}

// frustrationPhraseByFirstByte lists frustrationPhraseLowercase by first
// byte, and frustrationPhrasePairs is a bit set of their first two bytes.
// One scan of a prompt checks every literal, comparing only where the pair
// matches; most letters start no pair, which keeps the scan's branches
// predictable.
var frustrationPhraseByFirstByte, frustrationPhrasePairs = func() (index [256][]string, pairs [1 << 10]uint64) {
	for _, literal := range frustrationPhraseLowercase {
		index[literal[0]] = append(index[literal[0]], literal)
		pair := uint16(literal[0])<<8 | uint16(literal[1])
		pairs[pair>>6] |= 1 << (pair & 63)
	}
	return index, pairs
}()

// CountFrustrationMarkers counts user prompts that indicate the
// session is going badly. This is an analytics marker, not a
// standalone prompt-quality penalty.
func CountFrustrationMarkers(msgs []HeuristicMessage) int {
	count := 0
	for _, m := range msgs {
		if m.IsSystem || m.SourceSubtype == "tool_result" || m.Role != "user" {
			continue
		}
		if IsFrustrationMarker(m.Content) {
			count++
		}
	}
	return count
}

type promptInfo struct {
	Content                     string
	Normalized                  string
	Tokens                      []string
	Index                       int
	Ordinal                     int
	Timestamp                   string
	HasPreviousAssistant        bool
	PreviousAssistantTimestamp  string
	FirstUserAfterLastAssistant bool
}

func userPrompts(msgs []HeuristicMessage) []promptInfo {
	prompts := make([]promptInfo, 0)
	var previousAssistantTimestamp string
	hasPreviousAssistant := false
	userSinceLastAssistant := false
	for _, m := range msgs {
		if m.IsSystem || m.SourceSubtype == "tool_result" {
			continue
		}
		if m.Role == "assistant" {
			previousAssistantTimestamp = m.Timestamp
			hasPreviousAssistant = true
			userSinceLastAssistant = false
			continue
		}
		if m.Role != "user" {
			continue
		}
		normalized := normalizePrompt(m.Content)
		if normalized == "" {
			continue
		}
		firstAfterAssistant := !userSinceLastAssistant
		prompts = append(prompts, promptInfo{
			Content:                     m.Content,
			Normalized:                  normalized,
			Tokens:                      promptTokens(normalized),
			Index:                       len(prompts),
			Ordinal:                     m.Ordinal,
			Timestamp:                   m.Timestamp,
			HasPreviousAssistant:        hasPreviousAssistant,
			PreviousAssistantTimestamp:  previousAssistantTimestamp,
			FirstUserAfterLastAssistant: firstAfterAssistant,
		})
		if !isControlPrompt(normalized) {
			userSinceLastAssistant = true
		}
	}
	return prompts
}

func countShortStartPrompts(prompts []promptInfo) int {
	first, ok := firstSubstantivePrompt(prompts)
	if !ok {
		return 0
	}
	count := 0
	for _, p := range prompts {
		if !isShortPrompt(p) {
			continue
		}
		if p.Index == first.Index {
			count++
			continue
		}
		if p.FirstUserAfterLastAssistant &&
			hasStaleAssistantBefore(p) {
			count++
		}
	}
	return count
}

func isShortPrompt(p promptInfo) bool {
	return !isControlPrompt(p.Normalized) &&
		len(p.Normalized) > 0 &&
		len(p.Normalized) < 30
}

func hasStaleAssistantBefore(p promptInfo) bool {
	if !p.HasPreviousAssistant {
		return false
	}
	userTime, ok := parsePromptTime(p.Timestamp)
	if !ok {
		return false
	}
	assistantTime, ok := parsePromptTime(
		p.PreviousAssistantTimestamp,
	)
	if !ok {
		return false
	}
	return userTime.Sub(assistantTime) > 30*time.Minute
}

func parsePromptTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
	} {
		t, err := time.Parse(layout, raw)
		if err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func normalizePrompt(content string) string {
	withoutCode := stripCodeFences(content)
	var words capsWords
	return string(appendNormalizedPrompt(make([]byte, 0, len(withoutCode)), withoutCode, &words))
}

// stripCodeFences replaces every fenced block with one space, matching
// codeFenceRe.ReplaceAllString(content, " "): each match starts at the
// leftmost unconsumed fence and ends at the next fence, and an opening fence
// without a closer is left as is. The scan avoids running the regular
// expression over long pasted prompts.
func stripCodeFences(content string) string {
	const fence = "```"
	var b strings.Builder
	rest := content
	for {
		before, inside, opened := strings.Cut(rest, fence)
		if !opened {
			break
		}
		_, after, closed := strings.Cut(inside, fence)
		if !closed {
			break
		}
		b.WriteString(before)
		b.WriteByte(' ')
		rest = after
	}
	if b.Len() == 0 {
		return content
	}
	b.WriteString(rest)
	return b.String()
}

// appendNormalizedPrompt appends s lowercased, with each run of
// whitespace collapsed to one space and the ends trimmed, and counts the
// words of s in words. The text equals strings.ToLower followed by
// collapsing whitespace, in one pass: a byte that is not valid UTF-8
// becomes U+FFFD, as strings.ToLower writes it.
func appendNormalizedPrompt(dst []byte, s string, words *capsWords) []byte {
	inSpace, wrote := false, false
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			i++
			switch {
			case 'a' <= c && c <= 'z':
				words.letters++
				words.hasLower = true
			case 'A' <= c && c <= 'Z':
				words.letters++
				words.hasUpper = true
				c += 'a' - 'A'
			case c == ' ' || '\t' <= c && c <= '\r':
				words.end()
				inSpace = wrote
				continue
			default:
				words.end()
			}
			if inSpace {
				dst = append(dst, ' ')
				inSpace = false
			}
			dst = append(dst, c)
			wrote = true
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if unicode.IsLetter(r) {
			words.letters++
			words.hasLower = words.hasLower || unicode.IsLower(r)
			words.hasUpper = words.hasUpper || unicode.IsUpper(r)
		} else {
			words.end()
		}
		r = unicode.ToLower(r)
		if unicode.IsSpace(r) {
			inSpace = wrote
			continue
		}
		if inSpace {
			dst = append(dst, ' ')
			inSpace = false
		}
		dst = utf8.AppendRune(dst, r)
		wrote = true
	}
	words.end()
	return dst
}

func promptTokens(normalized string) []string {
	parts := strings.FieldsFunc(normalized, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) &&
			r != '_' && r != '-'
	})
	tokens := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) >= 3 {
			tokens = append(tokens, p)
		}
	}
	return tokens
}

// capsWords counts the words of a prompt, where a word is a run of
// letters, and tracks the word being read.
type capsWords struct {
	words, total, caps int
	letters            int
	hasLower, hasUpper bool
}

// end closes the current word, if any.
func (w *capsWords) end() {
	if w.letters == 0 {
		return
	}
	w.words++
	if w.letters >= 2 {
		w.total++
		if w.hasUpper && !w.hasLower {
			w.caps++
		}
	}
	w.letters = 0
	w.hasLower, w.hasUpper = false, false
}

// ratio returns the share of words with two or more letters that are all
// capitals, or 0 when there are fewer than minWords words or longer words.
func (w *capsWords) ratio(minWords int) float64 {
	if w.words < minWords || w.total < minWords {
		return 0
	}
	return float64(w.caps) / float64(w.total)
}

func isControlPrompt(normalized string) bool {
	_, ok := controlPrompts[normalized]
	return ok
}

func firstSubstantivePrompt(prompts []promptInfo) (promptInfo, bool) {
	for _, p := range prompts {
		if !isControlPrompt(p.Normalized) {
			return p, true
		}
	}
	return promptInfo{}, false
}

func isCodeTask(prompts []promptInfo) bool {
	for _, p := range prompts {
		text := p.Normalized
		// Check tokens before scanning the whole prompt for file references.
		if hasCodeAction(p.Tokens) && (hasCodeObject(p.Tokens) || hasFileRef(p.Content)) {
			return true
		}
		if strings.Contains(text, "failing test") ||
			strings.Contains(text, "stack trace") ||
			strings.Contains(text, "build error") ||
			strings.Contains(text, "compile error") {
			return true
		}
	}
	return false
}

func hasCodeAction(tokens []string) bool {
	phrases := []string{
		"implement", "fix", "debug", "refactor", "update",
		"change", "add", "remove", "create", "write",
		"test", "lint", "compile", "build", "wire",
	}
	return containsAnyToken(tokens, phrases)
}

func hasCodeObject(tokens []string) bool {
	phrases := []string{
		"code", "codebase", "repo", "repository", "app",
		"backend", "frontend", "api", "endpoint", "component",
		"function", "class", "module", "package", "schema",
		"migration", "test", "tests", "bug", "error",
	}
	return containsAnyToken(tokens, phrases)
}

func containsAnyToken(tokens []string, words []string) bool {
	for _, word := range words {
		if slices.Contains(tokens, word) {
			return true
		}
	}
	return false
}

func isUnstructuredStart(p promptInfo) bool {
	if hasFileRef(p.Content) || hasConstraintLanguage(p.Tokens) ||
		hasSpecStructure(p.Content, p.Normalized) {
		return false
	}
	return true
}

func hasConstraintLanguage(tokens []string) bool {
	phrases := []string{
		"must", "never", "only", "preserve", "keep", "avoid",
		"require", "requires", "constraint", "constraints",
		"acceptance", "criteria", "success", "expected",
		"output", "format", "verify", "validation", "test",
		"tests",
	}
	return containsAnyToken(tokens, phrases)
}

func hasSpecStructure(content, normalized string) bool {
	if strings.Contains(content, "\n#") || bulletRe.MatchString(content) {
		return true
	}
	phrases := []string{
		"acceptance criteria", "success criteria", "requirements",
		"steps", "plan", "scope", "non-scope",
	}
	for _, phrase := range phrases {
		if strings.Contains(normalized, phrase) {
			return true
		}
	}
	return false
}

func hasSuccessCriteria(prompts []promptInfo) bool {
	for _, p := range prompts {
		text := p.Normalized
		for _, phrase := range []string{
			"success", "acceptance", "expected", "done when",
			"should result", "output", "criteria",
		} {
			if strings.Contains(text, phrase) {
				return true
			}
		}
	}
	return false
}

func hasVerificationLanguage(prompts []promptInfo) bool {
	for _, p := range prompts {
		text := p.Normalized
		for _, phrase := range []string{
			"test", "tests", "verify", "verification",
			"validate", "validation", "check", "reproduce",
			"proof", "run",
		} {
			if strings.Contains(text, phrase) {
				return true
			}
		}
	}
	return false
}

func hasPromptContext(prompts []promptInfo) bool {
	for _, p := range prompts {
		if hasFileRef(p.Content) {
			return true
		}
	}
	return false
}

func hasFileRef(content string) bool {
	return fileRefRe.MatchString(content)
}

type promptTokenPosting struct {
	promptIndex int
	frequency   int
}

func countDuplicatePrompts(prompts []promptInfo) int {
	// The existing score treats the current prompt as a token set and each
	// previous prompt as a token multiset. Keep that behavior, but index the
	// previous frequencies so prompts only visit candidates that share a token.
	seenNormalized := make(map[string]struct{}, len(prompts))
	seenTokenCounts := make([]int, 0, len(prompts))
	postings := make(map[string][]promptTokenPosting)
	overlaps := make([]int, len(prompts))
	touched := make([]int, 0, len(prompts))
	repeats := 0
	for _, p := range prompts {
		if isControlPrompt(p.Normalized) ||
			len(p.Normalized) < 20 || len(p.Tokens) < 4 {
			continue
		}
		if _, ok := seenNormalized[p.Normalized]; ok {
			repeats++
			continue
		}

		tokenFrequencies := make(map[string]int, len(p.Tokens))
		for _, token := range p.Tokens {
			tokenFrequencies[token]++
		}

		touched = touched[:0]
		for token := range tokenFrequencies {
			for _, posting := range postings[token] {
				if overlaps[posting.promptIndex] == 0 {
					touched = append(touched, posting.promptIndex)
				}
				overlaps[posting.promptIndex] += posting.frequency
			}
		}

		duplicate := false
		for _, promptIndex := range touched {
			if jaccardFromOverlap(
				len(tokenFrequencies),
				seenTokenCounts[promptIndex],
				overlaps[promptIndex],
			) >= 0.85 {
				duplicate = true
				break
			}
		}
		for _, promptIndex := range touched {
			overlaps[promptIndex] = 0
		}
		if duplicate {
			repeats++
			continue
		}

		promptIndex := len(seenTokenCounts)
		seenNormalized[p.Normalized] = struct{}{}
		seenTokenCounts = append(seenTokenCounts, len(p.Tokens))
		for token, frequency := range tokenFrequencies {
			postings[token] = append(postings[token], promptTokenPosting{
				promptIndex: promptIndex,
				frequency:   frequency,
			})
		}
	}
	return repeats
}

func jaccardFromOverlap(currentUnique, previousTotal, intersections int) float64 {
	if currentUnique == 0 || previousTotal == 0 {
		return 0
	}
	union := currentUnique + previousTotal - intersections
	if union == 0 {
		return 0
	}
	return float64(intersections) / float64(union)
}

func hasContextToolActivity(calls []ToolCallRow) bool {
	return slices.ContainsFunc(calls, IsContextToolCall)
}

// IsContextToolCall reports whether a tool call counts as context-gathering
// activity for the no-code-context heuristic (Read/Grep/Glob or a Bash
// context command).
func IsContextToolCall(c ToolCallRow) bool {
	switch c.Category {
	case "Read", "Grep", "Glob":
		return true
	case "Bash":
		return isContextCommand(CommandText(c.InputJSON))
	}
	return false
}

func isContextCommand(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	name := fields[0]
	if slices.Contains([]string{
		"rg", "grep", "git", "ls", "find", "cat", "sed",
		"awk", "go", "npm", "pnpm", "yarn", "pytest",
		"cargo", "make",
	}, name) {
		return true
	}
	return strings.Contains(command, " test") ||
		strings.Contains(command, " lint")
}

// RunawayToolLoopSpan reports the calls that make a session a runaway
// tool loop. It returns the same verdict as hasRunawayToolLoop. The
// exact-signature path wins: its span is the whole run of identical
// calls containing the trigger. Otherwise the span is the first
// 12-call window that qualifies. n counts the calls in the span.
func RunawayToolLoopSpan(
	calls []ToolCallRow,
) (first, last CallPos, n int, ok bool) {
	if len(calls) < 12 {
		return CallPos{}, CallPos{}, 0, false
	}
	facts := make([]toolLoopFact, len(calls))
	for i, c := range calls {
		facts[i] = toolLoopFact{
			failure:        IsFailure(c),
			exactSignature: toolSignature(c),
			commandClass:   commandClass(c),
		}
	}
	start, end, found := repeatedFailingExactToolRunSpan(facts, 5, 3)
	if !found {
		start, end, found = runawayToolWindowSpan(facts)
	}
	if !found {
		return CallPos{}, CallPos{}, 0, false
	}
	return toolCallPos(calls[start]), toolCallPos(calls[end]),
		end - start + 1, true
}

func hasRunawayToolLoop(calls []ToolCallRow) bool {
	_, _, _, ok := RunawayToolLoopSpan(calls)
	return ok
}

type toolLoopFact struct {
	failure        bool
	exactSignature string
	commandClass   string
}

func repeatedFailingExactToolRunSpan(
	facts []toolLoopFact,
	threshold int,
	failureThreshold int,
) (start, end int, ok bool) {
	run := 1
	failures := 0
	if len(facts) > 0 && facts[0].failure {
		failures = 1
	}
	for i := 1; i < len(facts); i++ {
		if facts[i].exactSignature == facts[i-1].exactSignature {
			run++
			if facts[i].failure {
				failures++
			}
			if run >= threshold && failures >= failureThreshold {
				start, end = i-run+1, i
				for end+1 < len(facts) &&
					facts[end+1].exactSignature == facts[end].exactSignature {
					end++
				}
				return start, end, true
			}
		} else {
			run = 1
			failures = 0
			if facts[i].failure {
				failures = 1
			}
		}
	}
	return 0, 0, false
}

func runawayToolWindowSpan(facts []toolLoopFact) (start, end int, ok bool) {
	const windowSize = 12
	failures := 0
	classCounts := make(map[string]int, windowSize)
	for _, fact := range facts[:windowSize] {
		if fact.failure {
			failures++
		}
		classCounts[fact.commandClass]++
	}
	if isRunawayToolWindow(failures, classCounts) {
		return 0, windowSize - 1, true
	}
	for start := 1; start+windowSize <= len(facts); start++ {
		removed := facts[start-1]
		if removed.failure {
			failures--
		}
		if classCounts[removed.commandClass] == 1 {
			delete(classCounts, removed.commandClass)
		} else {
			classCounts[removed.commandClass]--
		}
		added := facts[start+windowSize-1]
		if added.failure {
			failures++
		}
		classCounts[added.commandClass]++
		if isRunawayToolWindow(failures, classCounts) {
			return start, start + windowSize - 1, true
		}
	}
	return 0, 0, false
}

func isRunawayToolWindow(failures int, classCounts map[string]int) bool {
	if failures >= 6 {
		return true
	}
	return failures >= 3 && dominantCount(classCounts) >= 10
}

func dominantCount(counts map[string]int) int {
	maxCount := 0
	for _, count := range counts {
		if count > maxCount {
			maxCount = count
		}
	}
	return maxCount
}

func commandClass(c ToolCallRow) string {
	if c.Category != "Bash" {
		return c.Category + ":" + c.ToolName
	}
	if c.InputJSON == "" {
		// Without the command, shell calls cannot be shown to run the
		// same program.
		return unretainedInputKey(c)
	}
	fields := strings.Fields(CommandText(c.InputJSON))
	if len(fields) == 0 {
		return c.Category + ":" + c.ToolName
	}
	return c.Category + ":" + fields[0]
}

func toolSignature(c ToolCallRow) string {
	if c.InputJSON == "" {
		return unretainedInputKey(c)
	}
	return c.ToolName + "\x00" + c.Category + "\x00" + c.InputJSON
}

// unretainedInputKey stands in for a call whose input the archive did not
// keep. The key is unique to the call's position, so it matches no other
// call.
func unretainedInputKey(c ToolCallRow) string {
	return fmt.Sprintf("\x01%d:%d", c.MessageOrdinal, c.CallIndex)
}

// CommandText extracts a shell script from stored tool input.
func CommandText(inputJSON string) string {
	var payload map[string]any
	if err := json.Unmarshal([]byte(inputJSON), &payload); err != nil {
		return inputJSON
	}
	for _, key := range []string{"command", "cmd"} {
		if v, ok := payload[key].(string); ok {
			return v
		}
	}
	return inputJSON
}
