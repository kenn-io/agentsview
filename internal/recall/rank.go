package recall

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

var tokenPattern = regexp.MustCompile(`[A-Za-z0-9_]+`)
var technicalPhrasePattern = regexp.MustCompile(
	`(?i)\b(?:[A-Za-z0-9_.-]+/)+[A-Za-z0-9_.-]+\b|\b[A-Za-z0-9_.-]+\.(?:go|py|js|jsx|ts|tsx|rs|java|c|cc|cpp|h|hpp|sql|json|ya?ml|toml|md|txt|sh)\b`,
)
var codeSymbolPattern = regexp.MustCompile(
	`\b[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+\b|\b[A-Za-z][A-Za-z0-9_]*[a-z][A-Z][A-Za-z0-9_]*\b`,
)
var errorPhrasePattern = regexp.MustCompile(
	"(?i)\\b(?:error|failure|panic|exception):\\s*" +
		"([A-Za-z][A-Za-z0-9 _.-]{2,80}?)(?:[.;,)\\]}`\"'\\n]|$)",
)
var quotedPhrasePattern = regexp.MustCompile(
	"`([^`]+)`|(?:^|\\s)'([^']+)'|\"([^\"]+)\"",
)
var scoringQuotedTextPattern = regexp.MustCompile(
	"`([^`]+)`|'([^']+)'|\"([^\"]+)\"",
)

// MaxScoringQueryTerms bounds the lexical terms used by both candidate
// retrieval and ranking. Keeping the limit here prevents the two stages from
// assigning different meanings to the same query.
const MaxScoringQueryTerms = 12

type promptInjectionBaitPattern struct {
	Reason  string
	Pattern *regexp.Regexp
}

var promptInjectionBaitPatterns = []promptInjectionBaitPattern{
	{
		Reason: "prior_instruction_override",
		Pattern: regexp.MustCompile(
			`(?is)\b(?:ignore|disregard)\s+(?:all\s+)?(?:previous|prior|above)\s+instructions\b[^\n.!?]*(?:[\n.!?]|$)`,
		),
	},
	{
		Reason: "role_marker_override",
		Pattern: regexp.MustCompile(
			`(?im)^\s*(?:(?:body|trigger|snippet):\s*)?(?:system|developer|assistant|user|tool)\s*:\s*(?:ignore|disregard|reveal|print|run|execute|fetch|follow|answer|delete|change)\b[^\n]*(?:\n|$)`,
		),
	},
	{
		Reason: "privileged_instruction_marker",
		Pattern: regexp.MustCompile(
			`(?im)^\s*(?:(?:body|trigger|snippet):\s*)?(?:(?:new|updated|override|higher-priority)\s+)?(?:system|developer)\s+(?:message|prompt|instructions?|directive)\s*:\s*(?:ignore|disregard|reveal|print|run|execute|fetch|follow|answer|delete|change|use|obey|treat)\b[^\n]*(?:\n|$)`,
		),
	},
	{
		Reason: "secret_exfiltration",
		Pattern: regexp.MustCompile(
			`(?im)^\s*(?:(?:body|trigger|snippet):\s*)?(?:reveal|print|dump|leak|exfiltrate)\b[^\n]*(?:system|developer)\s+prompt\b[^\n]*(?:\n|$)`,
		),
	},
	{
		Reason: "secret_exfiltration",
		Pattern: regexp.MustCompile(
			`(?im)^\s*(?:(?:body|trigger|snippet):\s*)?(?:reveal|print|dump|leak|exfiltrate)\b[^\n]*(?:api\s*key|secret|token|password)\b[^\n]*(?:\n|$)`,
		),
	},
	{
		Reason: "command_execution",
		Pattern: regexp.MustCompile(
			`(?im)^\s*(?:(?:body|trigger|snippet):\s*)?(?:run|execute)\b` +
				`[^\n]*(?:curl|wget|rm\s+-rf|bash|sh|python3?|osascript|powershell)\b` +
				`[^\n]*(?:before\s+(?:answering|responding)|then\s+(?:answer|respond)|` +
				`(?:reveal|print|dump|leak|exfiltrate)\b[^\n]*(?:system|developer)\s+prompt|` +
				`(?:reveal|print|dump|leak|exfiltrate)\b[^\n]*(?:api\s*key|secret|token|password))` +
				`[^\n]*(?:\n|$)`,
		),
	},
	{
		Reason: "external_instruction_fetch",
		Pattern: regexp.MustCompile(
			`(?im)^\s*(?:(?:body|trigger|snippet):\s*)?(?:fetch|visit|open)\s+https?://\S+[^\n]*(?:follow|obey|use)\s+(?:the\s+)?instructions?\b[^\n]*(?:\n|$)`,
		),
	},
}

var commandPhraseHeads = map[string]struct{}{
	"agentsview":    {},
	"cargo":         {},
	"docker":        {},
	"git":           {},
	"go":            {},
	"golangci":      {},
	"golangci-lint": {},
	"make":          {},
	"node":          {},
	"npm":           {},
	"npx":           {},
	"pnpm":          {},
	"python":        {},
	"python3":       {},
	"sqlite3":       {},
	"uv":            {},
	"uvx":           {},
}

var monthTokens = map[string]time.Month{
	"jan":       time.January,
	"january":   time.January,
	"feb":       time.February,
	"february":  time.February,
	"mar":       time.March,
	"march":     time.March,
	"apr":       time.April,
	"april":     time.April,
	"may":       time.May,
	"jun":       time.June,
	"june":      time.June,
	"jul":       time.July,
	"july":      time.July,
	"aug":       time.August,
	"august":    time.August,
	"sep":       time.September,
	"sept":      time.September,
	"september": time.September,
	"oct":       time.October,
	"october":   time.October,
	"nov":       time.November,
	"november":  time.November,
	"dec":       time.December,
	"december":  time.December,
}

var smallNumberTokens = map[string]int{
	"one":   1,
	"two":   2,
	"three": 3,
	"four":  4,
	"five":  5,
	"six":   6,
	"seven": 7,
	"eight": 8,
	"nine":  9,
	"ten":   10,
	"1":     1,
	"2":     2,
	"3":     3,
	"4":     4,
	"5":     5,
	"6":     6,
	"7":     7,
	"8":     8,
	"9":     9,
	"10":    10,
}

func Rank(entries []Entry, q Query) []Result {
	limit := q.Limit
	if limit <= 0 {
		limit = len(entries)
	}

	lexicalQueryText := LexicalQueryText(q.Text)
	queryTokens := scoringQueryTokens(q.Text)
	candidates := eligibleEntries(entries, q)
	idf := queryIDF(candidates, queryTokens)
	temporalBoosts := temporalBoosts(
		candidates,
		tokenize(lexicalQueryText),
		orderedQueryTokens(lexicalQueryText),
	)

	var results []Result
	for _, m := range candidates {
		breakdown, matchedTerms := scoreEntry(
			m, lexicalQueryText, queryTokens, idf, temporalBoosts[m.ID],
		)
		if breakdown.Total <= 0 {
			continue
		}
		results = append(results, Result{
			Entry:        m,
			Score:        breakdown.Total,
			Breakdown:    breakdown,
			MatchedTerms: matchedTerms,
		})
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Entry.ID < results[j].Entry.ID
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results
}

// LexicalQueryText removes query content that should not influence lexical
// retrieval, such as prompt-injection bait.
func LexicalQueryText(text string) string {
	return stripPromptInjectionBait(text)
}

// ScoringQueryTerms returns the canonical bounded lexical term set used for
// candidate retrieval and ranking. Quoted terms are retained first, followed
// by the most specific remaining terms.
func ScoringQueryTerms(text string) []string {
	text = LexicalQueryText(text)
	seen := make(map[string]struct{})
	terms := make([]string, 0, MaxScoringQueryTerms)
	appendTerm := func(token string) {
		if !validScoringQueryTerm(token) {
			return
		}
		if _, ok := seen[token]; ok {
			return
		}
		seen[token] = struct{}{}
		terms = append(terms, token)
	}
	var quotedTerms []string
	for _, match := range scoringQuotedTextPattern.FindAllStringSubmatch(text, -1) {
		quoted := firstNonEmpty(match[1:])
		for _, token := range orderedQueryTokens(quoted) {
			if validScoringQueryTerm(token) {
				quotedTerms = append(quotedTerms, token)
			}
		}
	}
	sortScoringQueryTerms(quotedTerms)
	for _, token := range quotedTerms {
		appendTerm(token)
	}
	var remaining []string
	for _, token := range orderedQueryTokens(text) {
		if !validScoringQueryTerm(token) {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		remaining = append(remaining, token)
	}
	sortScoringQueryTerms(remaining)
	for _, token := range remaining {
		appendTerm(token)
	}
	if len(terms) > MaxScoringQueryTerms {
		terms = terms[:MaxScoringQueryTerms]
	}
	return terms
}

// QueryUsesTemporalSignals reports whether ranking can score entries from
// their timestamps independently of lexical or structured-entity matches.
func QueryUsesTemporalSignals(text string) bool {
	text = LexicalQueryText(text)
	queryTokens := tokenize(text)
	orderedTokens := orderedQueryTokens(text)
	if len(queryCalendarWindows(orderedTokens)) > 0 || queryWantsRecent(queryTokens) {
		return true
	}
	if hasQueryToken(queryTokens, "yesterday") {
		return true
	}
	if hasQueryToken(queryTokens, "ago") &&
		hasAnyQueryToken(
			queryTokens, "day", "days", "week", "weeks", "month", "months",
		) {
		if _, ok := queryAgoNumber(orderedTokens); ok {
			return true
		}
	}
	if hasAnyQueryToken(queryTokens, "last", "past", "previous") &&
		hasQueryToken(queryTokens, "week") {
		return true
	}
	return hasAnyQueryToken(queryTokens, "this", "current") &&
		hasAnyQueryToken(queryTokens, "week", "month")
}

// ContainsPromptInjectionBait reports common prompt-injection bait that should
// be treated as historical evidence rather than current instructions.
func ContainsPromptInjectionBait(text string) bool {
	return len(PromptInjectionBaitReasons(text)) > 0
}

// PromptInjectionBaitReasons returns stable detector reason labels for common
// prompt-injection bait in the order the detectors matched.
func PromptInjectionBaitReasons(text string) []string {
	var reasons []string
	seen := map[string]struct{}{}
	for _, detector := range promptInjectionBaitPatterns {
		if !detector.Pattern.MatchString(text) {
			continue
		}
		if _, ok := seen[detector.Reason]; ok {
			continue
		}
		reasons = append(reasons, detector.Reason)
		seen[detector.Reason] = struct{}{}
	}
	return reasons
}

func stripPromptInjectionBait(text string) string {
	for _, detector := range promptInjectionBaitPatterns {
		text = detector.Pattern.ReplaceAllString(text, " ")
	}
	return text
}

func eligibleEntries(entries []Entry, q Query) []Entry {
	// Default to accepted-only recall, but honor an explicitly requested
	// status (e.g. archived) so status-filtered queries return matches.
	wantStatus := q.Status
	if wantStatus == "" {
		wantStatus = StatusAccepted
	}
	candidates := make([]Entry, 0, len(entries))
	for _, m := range entries {
		status := m.Status
		if status == "" {
			status = StatusAccepted
		}
		if status != wantStatus {
			continue
		}
		if !matchesContext(m, q) {
			continue
		}
		candidates = append(candidates, m)
	}
	return candidates
}

func queryIDF(entries []Entry, queryTokens map[string]struct{}) map[string]float64 {
	if len(queryTokens) == 0 || len(entries) == 0 {
		return nil
	}
	docFreq := make(map[string]int, len(queryTokens))
	for _, m := range entries {
		recallTokens := allEntryTokens(m)
		for token := range queryTokens {
			if _, ok := recallTokens[token]; ok {
				docFreq[token]++
			}
		}
	}
	idf := make(map[string]float64, len(queryTokens))
	totalDocs := float64(len(entries))
	for token := range queryTokens {
		df := float64(docFreq[token])
		idf[token] = math.Log(1+(totalDocs-df+0.5)/(df+0.5)) + 1
	}
	return idf
}

func scoringQueryTokens(text string) map[string]struct{} {
	terms := ScoringQueryTerms(text)
	tokens := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		tokens[term] = struct{}{}
	}
	return tokens
}

func validScoringQueryTerm(token string) bool {
	if lexicalRankStopwords[token] {
		return false
	}
	return len(token) >= 3 || scoringShortToken(token)
}

func sortScoringQueryTerms(terms []string) {
	sort.SliceStable(terms, func(i, j int) bool {
		if len(terms[i]) != len(terms[j]) {
			return len(terms[i]) > len(terms[j])
		}
		return terms[i] < terms[j]
	})
}

func scoringShortToken(token string) bool {
	switch token {
	case "go", "js", "ts", "py", "rs", "id":
		return true
	default:
		return false
	}
}

var lexicalRankStopwords = map[string]bool{
	"about":      true,
	"accomplish": true,
	"agent":      true,
	"a":          true,
	"am":         true,
	"and":        true,
	"answer":     true,
	"are":        true,
	"asked":      true,
	"based":      true,
	"be":         true,
	"between":    true,
	"can":        true,
	"contain":    true,
	"contains":   true,
	"custom":     true,
	"directly":   true,
	"false":      true,
	"final":      true,
	"first":      true,
	"following":  true,
	"for":        true,
	"from":       true,
	"given":      true,
	"have":       true,
	"help":       true,
	"in":         true,
	"is":         true,
	"i":          true,
	"label":      true,
	"labels":     true,
	"mentions":   true,
	"more":       true,
	"name":       true,
	"names":      true,
	"of":         true,
	"on":         true,
	"or":         true,
	"our":        true,
	"past":       true,
	"phrases":    true,
	"question":   true,
	"retrieve":   true,
	"second":     true,
	"several":    true,
	"short":      true,
	"should":     true,
	"specific":   true,
	"task":       true,
	"tell":       true,
	"that":       true,
	"the":        true,
	"there":      true,
	"this":       true,
	"to":         true,
	"trajectory": true,
	"true":       true,
	"typically":  true,
	"using":      true,
	"what":       true,
	"when":       true,
	"where":      true,
	"which":      true,
	"with":       true,
}

func matchesContext(m Entry, q Query) bool {
	if q.Project != "" && m.Project != q.Project {
		return false
	}
	if q.CWD != "" && m.CWD != q.CWD {
		return false
	}
	if q.GitBranch != "" && m.GitBranch != q.GitBranch {
		return false
	}
	if q.Agent != "" && m.Agent != q.Agent {
		return false
	}
	return true
}

func scoreEntry(
	m Entry,
	queryText string,
	queryTokens map[string]struct{},
	idf map[string]float64,
	temporalBoost float64,
) (ScoreBreakdown, []string) {
	confidence := confidenceBonus(m)
	entityBoost := structuredEntityBoost(m, queryText)
	phraseBoost := exactPhraseBoost(m, queryText)
	if len(queryTokens) == 0 {
		const emptyQueryBaseScore = 0.1
		total := emptyQueryBaseScore +
			confidence + phraseBoost + entityBoost + temporalBoost
		return ScoreBreakdown{
			PhraseBoost:     phraseBoost,
			EntityBoost:     entityBoost,
			TemporalBoost:   temporalBoost,
			ConfidenceBonus: confidence,
			BaseScore:       emptyQueryBaseScore,
			Total:           total,
		}, nil
	}
	directTokens := directEntryTokens(m)
	evidenceTokens := evidenceEntryTokens(m)
	var overlap, evidenceOverlap, scoredEvidenceOverlap int
	var idfScore, evidenceIDFScore, identifierBoost float64
	var matchedTerms []string
	for token := range queryTokens {
		directMatched := false
		matched := false
		if _, ok := directTokens[token]; ok {
			overlap++
			idfScore += idf[token]
			directMatched = true
			matched = true
		}
		if _, ok := evidenceTokens[token]; ok {
			evidenceOverlap++
			matched = true
			if !directMatched {
				scoredEvidenceOverlap++
				evidenceIDFScore += idf[token]
			}
		}
		if matched && isIdentifierToken(token) {
			identifierBoost += 2.0
		}
		if matched {
			matchedTerms = append(matchedTerms, token)
		}
	}
	if overlap == 0 && evidenceOverlap == 0 && phraseBoost == 0 && entityBoost == 0 && temporalBoost == 0 {
		return ScoreBreakdown{}, nil
	}
	if idfScore == 0 {
		idfScore = float64(overlap)
	}
	if evidenceIDFScore == 0 {
		evidenceIDFScore = float64(scoredEvidenceOverlap)
	}
	total := idfScore + evidenceIDFScore + identifierBoost + phraseBoost + entityBoost + temporalBoost + confidence
	sort.Strings(matchedTerms)
	return ScoreBreakdown{
		KeywordOverlap:         overlap,
		KeywordIDFScore:        idfScore,
		EvidenceKeywordOverlap: evidenceOverlap,
		EvidenceIDFScore:       evidenceIDFScore,
		IdentifierBoost:        identifierBoost,
		PhraseBoost:            phraseBoost,
		EntityBoost:            entityBoost,
		TemporalBoost:          temporalBoost,
		ConfidenceBonus:        confidence,
		Total:                  total,
	}, matchedTerms
}

func allEntryTokens(m Entry) map[string]struct{} {
	tokens := directEntryTokens(m)
	for token := range evidenceEntryTokens(m) {
		tokens[token] = struct{}{}
	}
	return tokens
}

func directEntryTokens(m Entry) map[string]struct{} {
	return tokenize(strings.Join(
		[]string{m.Title, m.Body, m.Trigger}, " ",
	))
}

func evidenceEntryTokens(m Entry) map[string]struct{} {
	parts := make([]string, 0, len(m.Evidence))
	for _, evidence := range m.Evidence {
		parts = append(parts, evidence.Snippet)
	}
	return tokenize(strings.Join(parts, " "))
}

func isIdentifierToken(token string) bool {
	if strings.Contains(token, "_") {
		return true
	}
	// Tokens are lowercased, so camelCase is gone; an alphanumeric mix
	// (utf8, sha256, fts5) still signals a code identifier. Plain words
	// (however long) and plain numbers do not.
	var hasLetter, hasDigit bool
	for _, r := range token {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			hasLetter = true
		}
	}
	return hasLetter && hasDigit
}

func confidenceBonus(m Entry) float64 {
	if m.Confidence == nil {
		return 0
	}
	if *m.Confidence < 0 {
		return 0
	}
	if *m.Confidence > 1 {
		return 0.1
	}
	return *m.Confidence * 0.1
}

// rankEntity is a structured value extracted from an entry that can boost its
// score when present in the query. Filenames and paths set exact, so they are
// matched with their punctuation preserved ("recall_entries.go" must appear with its
// dot) rather than tokenized, which would let "entries go" match recall_entries.go.
type rankEntity struct {
	value string
	exact bool
}

func structuredEntityBoost(m Entry, queryText string) float64 {
	normalizedQuery := normalizeEntityText(queryText)
	if normalizedQuery == "" {
		return 0
	}
	lowerQuery := strings.ToLower(queryText)
	seen := make(map[string]struct{})
	var boost float64
	for _, entity := range structuredEntityValues(m) {
		var key string
		var matched bool
		if entity.exact {
			value := strings.ToLower(strings.TrimSpace(entity.value))
			if len(value) < 3 {
				continue
			}
			key = "x:" + value
			matched = queryContainsExactEntity(lowerQuery, value)
		} else {
			normalizedEntity := normalizeEntityText(entity.value)
			if len(normalizedEntity) < 3 {
				continue
			}
			key = "n:" + normalizedEntity
			matched = containsNormalizedPhrase(normalizedQuery, normalizedEntity)
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if matched {
			boost += 1.5
		}
	}
	return boost
}

// queryContainsExactEntity reports whether entity appears in lowerQuery as a
// whole token run (bounded by non-identifier characters), so punctuation in the
// entity must be present in the query.
func queryContainsExactEntity(lowerQuery, entity string) bool {
	for from := 0; from < len(lowerQuery); {
		i := strings.Index(lowerQuery[from:], entity)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(entity)
		leftOK := start == 0 || !isWordByte(lowerQuery[start-1])
		rightOK := end == len(lowerQuery) || !isWordByte(lowerQuery[end])
		if leftOK && rightOK {
			return true
		}
		from = start + 1
	}
	return false
}

func isWordByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

func exactPhraseBoost(m Entry, queryText string) float64 {
	phrases := queryPhrases(queryText)
	if len(phrases) == 0 {
		return 0
	}
	recallText := normalizeEntityText(strings.Join(recallTextParts(m), " "))
	if recallText == "" {
		return 0
	}
	var boost float64
	for _, phrase := range phrases {
		if containsNormalizedPhrase(recallText, phrase.text) {
			boost += 0.75 * float64(phrase.length)
		}
	}
	return boost
}

type queryPhrase struct {
	text   string
	length int
}

func queryPhrases(queryText string) []queryPhrase {
	tokens := tokenPattern.FindAllString(strings.ToLower(queryText), -1)
	var meaningful []string
	for _, token := range tokens {
		if rankPhraseStopwords[token] || lexicalRankStopwords[token] {
			continue
		}
		meaningful = append(meaningful, token)
	}
	const minPhraseTokens = 3
	const maxPhraseTokens = 5
	if len(meaningful) < minPhraseTokens {
		return nil
	}
	seen := map[string]struct{}{}
	var phrases []queryPhrase
	for size := maxPhraseTokens; size >= minPhraseTokens; size-- {
		if len(meaningful) < size {
			continue
		}
		for start := 0; start+size <= len(meaningful); start++ {
			text := strings.Join(meaningful[start:start+size], " ")
			if _, ok := seen[text]; ok {
				continue
			}
			seen[text] = struct{}{}
			phrases = append(phrases, queryPhrase{text: text, length: size})
		}
	}
	return phrases
}

var rankPhraseStopwords = map[string]bool{
	"about":    true,
	"and":      true,
	"are":      true,
	"for":      true,
	"from":     true,
	"mentions": true,
	"note":     true,
	"that":     true,
	"the":      true,
	"this":     true,
	"what":     true,
	"when":     true,
	"where":    true,
	"which":    true,
	"with":     true,
}

func structuredEntityValues(m Entry) []rankEntity {
	entities := []rankEntity{
		{value: m.Project}, {value: m.CWD},
		{value: m.GitBranch}, {value: m.Agent},
	}
	if base := pathBase(m.CWD); base != "" {
		entities = append(entities, rankEntity{value: base})
	}
	if base := pathBase(m.GitBranch); base != "" {
		entities = append(entities, rankEntity{value: base})
	}
	entities = append(entities, technicalPhraseEntities(m)...)
	return entities
}

func technicalPhraseEntities(m Entry) []rankEntity {
	text := strings.Join(recallTextParts(m), " ")
	var entities []rankEntity
	// Filenames and paths match with punctuation preserved, and their
	// basename is added so a filename-only query (recall_entries.go) matches a
	// full-path entity (internal/db/recall_entries.go).
	addPathLike := func(value string) {
		entities = append(entities, rankEntity{value: value, exact: true})
		if base := pathBase(value); base != "" && base != value {
			entities = append(entities, rankEntity{value: base, exact: true})
		}
	}
	for _, value := range technicalPhrasePattern.FindAllString(text, -1) {
		addPathLike(value)
	}
	for _, value := range codeSymbolPattern.FindAllString(text, -1) {
		if strings.Contains(value, ".") {
			addPathLike(value)
		} else {
			entities = append(entities, rankEntity{value: value})
		}
	}
	for _, value := range errorPhraseValues(text) {
		entities = append(entities, rankEntity{value: value})
	}
	for _, value := range quotedCommandPhraseValues(text) {
		entities = append(entities, rankEntity{value: value})
	}
	return entities
}

func recallTextParts(m Entry) []string {
	parts := []string{m.Title, m.Body, m.Trigger}
	for _, evidence := range m.Evidence {
		parts = append(parts, evidence.Snippet)
	}
	return parts
}

func errorPhraseValues(text string) []string {
	var values []string
	for _, match := range errorPhrasePattern.FindAllStringSubmatch(text, -1) {
		phrase := strings.TrimSpace(match[1])
		if len(tokenPattern.FindAllString(phrase, -1)) >= 2 {
			values = append(values, phrase)
		}
	}
	return values
}

func quotedCommandPhraseValues(text string) []string {
	var values []string
	for _, match := range quotedPhrasePattern.FindAllStringSubmatch(text, -1) {
		phrase := firstNonEmpty(match[1:])
		if isCommandPhrase(phrase) {
			values = append(values, phrase)
		}
	}
	return values
}

func firstNonEmpty(values []string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func isCommandPhrase(phrase string) bool {
	tokens := tokenPattern.FindAllString(strings.ToLower(phrase), -1)
	if len(tokens) < 2 {
		return false
	}
	_, ok := commandPhraseHeads[tokens[0]]
	return ok
}

func pathBase(path string) string {
	path = strings.TrimRight(strings.TrimSpace(path), `/\`)
	if path == "" {
		return ""
	}
	if idx := strings.LastIndexAny(path, `/\`); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

func containsNormalizedPhrase(normalizedQuery, normalizedEntity string) bool {
	return strings.Contains(" "+normalizedQuery+" ", " "+normalizedEntity+" ")
}

func normalizeEntityText(value string) string {
	parts := tokenPattern.FindAllString(strings.ToLower(value), -1)
	return strings.Join(parts, " ")
}

func temporalBoosts(
	entries []Entry,
	queryTokens map[string]struct{},
	orderedTokens []string,
) map[string]float64 {
	windows := queryCalendarWindows(orderedTokens)
	if len(windows) > 0 {
		return calendarWindowBoosts(entries, windows)
	}
	windows = queryRelativeWindows(entries, queryTokens, orderedTokens)
	if len(windows) > 0 {
		return calendarWindowBoosts(entries, windows)
	}
	if !queryWantsRecent(queryTokens) {
		return nil
	}
	timestamps := make(map[string]time.Time, len(entries))
	var minTime, maxTime time.Time
	for _, m := range entries {
		ts, ok := effectiveRecallTime(m)
		if !ok {
			continue
		}
		timestamps[m.ID] = ts
		if minTime.IsZero() || ts.Before(minTime) {
			minTime = ts
		}
		if maxTime.IsZero() || ts.After(maxTime) {
			maxTime = ts
		}
	}
	if len(timestamps) == 0 {
		return nil
	}
	boosts := make(map[string]float64, len(timestamps))
	span := maxTime.Sub(minTime)
	for id, ts := range timestamps {
		if span <= 0 {
			boosts[id] = 1.0
			continue
		}
		boosts[id] = 0.25 + 0.75*(float64(ts.Sub(minTime))/float64(span))
	}
	return boosts
}

type timeWindow struct {
	start time.Time
	end   time.Time
}

// queryCalendarWindows returns month windows for explicit "<month> <year>"
// phrases. The month and year tokens must be adjacent in the query so a bare
// month like "may" only forms a window when paired with a neighboring year, and
// a stray year elsewhere in the query does not combine with an unrelated month
// name.
func queryCalendarWindows(orderedTokens []string) []timeWindow {
	var windows []timeWindow
	for i := 0; i+1 < len(orderedTokens); i++ {
		month, year, ok := monthYearPair(orderedTokens[i], orderedTokens[i+1])
		if !ok {
			continue
		}
		start := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
		windows = append(windows, timeWindow{
			start: start,
			end:   start.AddDate(0, 1, 0),
		})
	}
	return windows
}

// monthYearPair reports the month and year named by two adjacent tokens in
// either order ("february 2024" or "2024 february").
func monthYearPair(a, b string) (time.Month, int, bool) {
	if month, ok := monthTokens[a]; ok {
		if year, ok := parseYearToken(b); ok {
			return month, year, true
		}
	}
	if month, ok := monthTokens[b]; ok {
		if year, ok := parseYearToken(a); ok {
			return month, year, true
		}
	}
	return 0, 0, false
}

func queryRelativeWindows(
	entries []Entry,
	queryTokens map[string]struct{},
	orderedTokens []string,
) []timeWindow {
	reference, ok := newestRecallTime(entries)
	if !ok {
		return nil
	}
	referenceDay := time.Date(
		reference.Year(), reference.Month(), reference.Day(),
		0, 0, 0, 0, time.UTC,
	)
	currentMonth := time.Date(reference.Year(), reference.Month(), 1, 0, 0, 0, 0, time.UTC)
	if windows := queryAgoWindows(
		reference, referenceDay, queryTokens, orderedTokens,
	); len(windows) > 0 {
		return windows
	}
	if hasQueryToken(queryTokens, "yesterday") {
		start := referenceDay.AddDate(0, 0, -1)
		return []timeWindow{{start: start, end: referenceDay}}
	}
	if hasAnyQueryToken(queryTokens, "last", "past", "previous") &&
		hasQueryToken(queryTokens, "week") {
		start := reference.AddDate(0, 0, -7)
		return []timeWindow{{start: start, end: reference}}
	}
	if hasAnyQueryToken(queryTokens, "this", "current") &&
		hasQueryToken(queryTokens, "week") {
		weekStart := startOfISOWeek(referenceDay)
		return []timeWindow{{start: weekStart, end: weekStart.AddDate(0, 0, 7)}}
	}
	if hasAnyQueryToken(queryTokens, "this", "current") &&
		hasQueryToken(queryTokens, "month") {
		return []timeWindow{{start: currentMonth, end: currentMonth.AddDate(0, 1, 0)}}
	}
	if !orderedTokensHaveAdjacent(orderedTokens, "last", "month") {
		return nil
	}
	start := currentMonth.AddDate(0, -1, 0)
	return []timeWindow{{start: start, end: currentMonth}}
}

// startOfISOWeek returns midnight on the Monday of the ISO week containing day.
func startOfISOWeek(day time.Time) time.Time {
	daysSinceMonday := (int(day.Weekday()) + 6) % 7
	monday := day.AddDate(0, 0, -daysSinceMonday)
	return time.Date(
		monday.Year(), monday.Month(), monday.Day(),
		0, 0, 0, 0, monday.Location(),
	)
}

// orderedTokensHaveAdjacent reports whether first is immediately followed by
// second in the ordered query tokens, so a relative phrase like "last month"
// only matches when its words are adjacent rather than scattered.
func orderedTokensHaveAdjacent(orderedTokens []string, first, second string) bool {
	for i := 0; i+1 < len(orderedTokens); i++ {
		if orderedTokens[i] == first && orderedTokens[i+1] == second {
			return true
		}
	}
	return false
}

func queryAgoWindows(
	reference, referenceDay time.Time,
	queryTokens map[string]struct{},
	orderedTokens []string,
) []timeWindow {
	if !hasQueryToken(queryTokens, "ago") {
		return nil
	}
	n, ok := queryAgoNumber(orderedTokens)
	if !ok || n <= 0 {
		return nil
	}
	if hasAnyQueryToken(queryTokens, "day", "days") {
		start := referenceDay.AddDate(0, 0, -n)
		return []timeWindow{{start: start, end: start.AddDate(0, 0, 1)}}
	}
	if hasAnyQueryToken(queryTokens, "week", "weeks") {
		end := reference.AddDate(0, 0, -7*(n-1))
		start := reference.AddDate(0, 0, -7*n)
		return []timeWindow{{start: start, end: end}}
	}
	if hasAnyQueryToken(queryTokens, "month", "months") {
		currentMonth := time.Date(
			reference.Year(), reference.Month(), 1,
			0, 0, 0, 0, time.UTC,
		)
		end := currentMonth.AddDate(0, -(n - 1), 0)
		start := currentMonth.AddDate(0, -n, 0)
		return []timeWindow{{start: start, end: end}}
	}
	return nil
}

// agoUnitTokens are the time units that an "N ago" window can target.
var agoUnitTokens = map[string]struct{}{
	"day": {}, "days": {}, "week": {}, "weeks": {},
	"month": {}, "months": {}, "year": {}, "years": {},
}

// queryAgoNumber returns the count for an "N <unit> ago" phrase by reading the
// number immediately preceding a time-unit token, rather than the smallest
// number anywhere in the query. This keeps "10 days ago issue 3" anchored to
// 10 days rather than 3.
func queryAgoNumber(orderedTokens []string) (int, bool) {
	for i := 1; i < len(orderedTokens); i++ {
		if _, ok := agoUnitTokens[orderedTokens[i]]; !ok {
			continue
		}
		if n, ok := smallNumberTokens[orderedTokens[i-1]]; ok {
			return n, true
		}
	}
	return 0, false
}

func orderedQueryTokens(text string) []string {
	return tokenPattern.FindAllString(strings.ToLower(text), -1)
}

func newestRecallTime(entries []Entry) (time.Time, bool) {
	var newest time.Time
	for _, m := range entries {
		ts, ok := effectiveRecallTime(m)
		if !ok {
			continue
		}
		if newest.IsZero() || ts.After(newest) {
			newest = ts
		}
	}
	return newest, !newest.IsZero()
}

func parseYearToken(token string) (int, bool) {
	var year int
	for _, r := range token {
		if r < '0' || r > '9' {
			return 0, false
		}
		year = year*10 + int(r-'0')
	}
	if year < 1970 || year > 2100 {
		return 0, false
	}
	return year, true
}

func calendarWindowBoosts(entries []Entry, windows []timeWindow) map[string]float64 {
	boosts := make(map[string]float64)
	for _, m := range entries {
		ts, ok := effectiveRecallTime(m)
		if !ok {
			continue
		}
		if timeInAnyWindow(ts, windows) {
			boosts[m.ID] = 1.0
		}
	}
	if len(boosts) == 0 {
		return nil
	}
	return boosts
}

func timeInAnyWindow(ts time.Time, windows []timeWindow) bool {
	for _, window := range windows {
		if !ts.Before(window.start) && ts.Before(window.end) {
			return true
		}
	}
	return false
}

func queryWantsRecent(queryTokens map[string]struct{}) bool {
	for _, token := range []string{"recent", "recently", "latest", "newest", "last", "current"} {
		if hasQueryToken(queryTokens, token) {
			return true
		}
	}
	return false
}

func hasQueryToken(queryTokens map[string]struct{}, token string) bool {
	_, ok := queryTokens[token]
	return ok
}

func hasAnyQueryToken(queryTokens map[string]struct{}, tokens ...string) bool {
	for _, token := range tokens {
		if hasQueryToken(queryTokens, token) {
			return true
		}
	}
	return false
}

func effectiveRecallTime(m Entry) (time.Time, bool) {
	if ts, ok := parseRecallTime(m.UpdatedAt); ok {
		return ts, true
	}
	return parseRecallTime(m.CreatedAt)
}

func parseRecallTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func tokenize(s string) map[string]struct{} {
	tokens := make(map[string]struct{})
	for _, token := range tokenPattern.FindAllString(strings.ToLower(s), -1) {
		tokens[token] = struct{}{}
	}
	return tokens
}
