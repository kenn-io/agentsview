package signals

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

// HeuristicMessage is the message subset needed by deterministic
// session-quality heuristics.
type HeuristicMessage struct {
	Role      string
	Content   string
	IsSystem  bool
	Ordinal   int
	Timestamp string
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

var (
	codeFenceRe = regexp.MustCompile("(?s)```.*?```")
	spaceRe     = regexp.MustCompile(`\s+`)
	fileRefRe   = regexp.MustCompile(
		`(?i)(?:^|[\s"'` + "`" + `])(?:\.{0,2}/)?[a-z0-9_.-]+(?:/[a-z0-9_. -]+)+|[a-z0-9_.-]+\.(?:go|ts|tsx|js|jsx|py|rs|java|kt|rb|php|cs|cpp|c|h|hpp|sql|svelte|vue|css|scss|html|json|ya?ml|toml|md|sh|zsh|bash)`,
	)
	bulletRe            = regexp.MustCompile(`(?m)^\s*(?:[-*+]|\d+\.)\s+\S+`)
	frustrationPhraseRe = regexp.MustCompile(
		`(?i)(!{3,}|\?{3,}|\b(?:wtf|come on|why won't|this is broken|doesn't work|does not work|still broken|same error|you broke|fucking|fuck)\b)`,
	)
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
			s.UnstructuredStart = isUnstructuredStart(first.Content)
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
func IsFrustrationMarker(content string) bool {
	normalized := normalizePrompt(content)
	if len(normalized) < 10 {
		return false
	}
	if frustrationPhraseRe.MatchString(normalized) {
		return true
	}
	return capsWordRatio(content, 3) >= 0.4
}

// CountFrustrationMarkers counts user prompts that indicate the
// session is going badly. This is an analytics marker, not a
// standalone prompt-quality penalty.
func CountFrustrationMarkers(msgs []HeuristicMessage) int {
	count := 0
	for _, m := range msgs {
		if m.IsSystem || m.Role != "user" {
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
		if m.IsSystem {
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
	withoutCode := codeFenceRe.ReplaceAllString(content, " ")
	lower := strings.ToLower(strings.TrimSpace(withoutCode))
	return spaceRe.ReplaceAllString(lower, " ")
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

func capsWordRatio(content string, minWords int) float64 {
	withoutCode := codeFenceRe.ReplaceAllString(content, " ")
	words := strings.FieldsFunc(withoutCode, func(r rune) bool {
		return !unicode.IsLetter(r)
	})
	if len(words) < minWords {
		return 0
	}
	total := 0
	caps := 0
	for _, word := range words {
		if len([]rune(word)) < 2 {
			continue
		}
		total++
		hasLower := false
		hasUpper := false
		for _, r := range word {
			if unicode.IsLower(r) {
				hasLower = true
			}
			if unicode.IsUpper(r) {
				hasUpper = true
			}
		}
		if hasUpper && !hasLower {
			caps++
		}
	}
	if total < minWords {
		return 0
	}
	return float64(caps) / float64(total)
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
		if hasFileRef(p.Content) && hasCodeAction(text) {
			return true
		}
		if hasCodeAction(text) && hasCodeObject(text) {
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

func hasCodeAction(text string) bool {
	phrases := []string{
		"implement", "fix", "debug", "refactor", "update",
		"change", "add", "remove", "create", "write",
		"test", "lint", "compile", "build", "wire",
	}
	return containsAnyWord(text, phrases)
}

func hasCodeObject(text string) bool {
	phrases := []string{
		"code", "codebase", "repo", "repository", "app",
		"backend", "frontend", "api", "endpoint", "component",
		"function", "class", "module", "package", "schema",
		"migration", "test", "tests", "bug", "error",
	}
	return containsAnyWord(text, phrases)
}

func containsAnyWord(text string, words []string) bool {
	for _, word := range words {
		if containsWord(text, word) {
			return true
		}
	}
	return false
}

func containsWord(text, word string) bool {
	return slices.Contains(promptTokens(text), word)
}

func isUnstructuredStart(content string) bool {
	normalized := normalizePrompt(content)
	if hasFileRef(content) || hasConstraintLanguage(normalized) ||
		hasSpecStructure(content, normalized) {
		return false
	}
	return true
}

func hasConstraintLanguage(text string) bool {
	phrases := []string{
		"must", "never", "only", "preserve", "keep", "avoid",
		"require", "requires", "constraint", "constraints",
		"acceptance", "criteria", "success", "expected",
		"output", "format", "verify", "validation", "test",
		"tests",
	}
	return containsAnyWord(text, phrases)
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
			if containsWord(text, phrase) || strings.Contains(text, phrase) {
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

func countDuplicatePrompts(prompts []promptInfo) int {
	seen := make([]promptInfo, 0, len(prompts))
	repeats := 0
	for _, p := range prompts {
		if isControlPrompt(p.Normalized) ||
			len(p.Normalized) < 20 || len(p.Tokens) < 4 {
			continue
		}
		duplicate := false
		for _, prev := range seen {
			if p.Normalized == prev.Normalized ||
				jaccard(p.Tokens, prev.Tokens) >= 0.85 {
				duplicate = true
				break
			}
		}
		if duplicate {
			repeats++
			continue
		}
		seen = append(seen, p)
	}
	return repeats
}

func jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	seen := map[string]struct{}{}
	for _, token := range a {
		seen[token] = struct{}{}
	}
	intersections := 0
	union := len(seen)
	for _, token := range b {
		if _, ok := seen[token]; ok {
			intersections++
		} else {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(intersections) / float64(union)
}

func hasContextToolActivity(calls []ToolCallRow) bool {
	for _, c := range calls {
		switch c.Category {
		case "Read", "Grep", "Glob":
			return true
		case "Bash":
			if isContextCommand(commandText(c.InputJSON)) {
				return true
			}
		}
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

func hasRunawayToolLoop(calls []ToolCallRow) bool {
	if len(calls) < 12 {
		return false
	}
	if hasRepeatedFailingExactToolRun(calls, 5, 3) {
		return true
	}
	for start := 0; start+12 <= len(calls); start++ {
		window := calls[start : start+12]
		if countWindowFailures(window) >= 6 {
			return true
		}
		if dominantToolSignatureCount(window) >= 10 &&
			countWindowFailures(window) >= 3 {
			return true
		}
	}
	return false
}

func hasRepeatedFailingExactToolRun(
	calls []ToolCallRow,
	threshold int,
	failureThreshold int,
) bool {
	run := 1
	failures := 0
	if len(calls) > 0 && IsFailure(calls[0]) {
		failures = 1
	}
	for i := 1; i < len(calls); i++ {
		if toolSignature(calls[i]) == toolSignature(calls[i-1]) {
			run++
			if IsFailure(calls[i]) {
				failures++
			}
			if run >= threshold && failures >= failureThreshold {
				return true
			}
		} else {
			run = 1
			failures = 0
			if IsFailure(calls[i]) {
				failures = 1
			}
		}
	}
	return false
}

func countWindowFailures(calls []ToolCallRow) int {
	failures := 0
	for _, c := range calls {
		if IsFailure(c) {
			failures++
		}
	}
	return failures
}

func dominantToolSignatureCount(calls []ToolCallRow) int {
	counts := map[string]int{}
	maxCount := 0
	for _, c := range calls {
		sig := commandClass(c)
		counts[sig]++
		if counts[sig] > maxCount {
			maxCount = counts[sig]
		}
	}
	return maxCount
}

func commandClass(c ToolCallRow) string {
	if c.Category != "Bash" {
		return c.Category + ":" + c.ToolName
	}
	fields := strings.Fields(commandText(c.InputJSON))
	if len(fields) == 0 {
		return c.Category + ":" + c.ToolName
	}
	return c.Category + ":" + fields[0]
}

func toolSignature(c ToolCallRow) string {
	return c.ToolName + "\x00" + c.Category + "\x00" + c.InputJSON
}

func commandText(inputJSON string) string {
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
