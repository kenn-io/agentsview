package db

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxTrendTerms        = 12
	MaxTrendTermVariants = 8
)

type TrendTermInput struct {
	Term     string   `json:"term"`
	Variants []string `json:"variants"`
	Matchers []string `json:"-"`
}

type TrendBucket struct {
	Date         string `json:"date"`
	MessageCount int    `json:"message_count"`
}

type TrendPoint struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

type TrendSeries struct {
	Term     string       `json:"term"`
	Variants []string     `json:"variants"`
	Total    int          `json:"total"`
	Points   []TrendPoint `json:"points"`
}

type TrendsTermsResponse struct {
	Granularity  string        `json:"granularity"`
	From         string        `json:"from"`
	To           string        `json:"to"`
	MessageCount int           `json:"message_count"`
	Buckets      []TrendBucket `json:"buckets"`
	Series       []TrendSeries `json:"series"`
}

func (db *DB) GetTrendsTerms(
	ctx context.Context,
	f AnalyticsFilter,
	terms []TrendTermInput,
	granularity string,
) (TrendsTermsResponse, error) {
	release, err := db.acquireBulkRead(ctx)
	if err != nil {
		return TrendsTermsResponse{}, err
	}
	defer release()
	if granularity == "" {
		granularity = "week"
	}
	acc := NewTrendWorkers(ctx, f.From, f.To, granularity, terms)
	defer func() {
		if acc != nil {
			_, _ = acc.Finish()
		}
	}()
	loc := f.location()
	sessionFilter := f
	sessionFilter.From = ""
	sessionFilter.To = ""
	sessionFilter.DayOfWeek = nil
	sessionFilter.Hour = nil
	sessionFilter.Model = ""
	where, args := sessionFilter.buildWhereWithDate("", false, "s.id")
	from, to := f.messageWindowBoundsUTC()
	if sessionWindow, wargs := analyticsMessageWindowPred("COALESCE(NULLIF(started_at, ''), created_at)", from, to); sessionWindow != "" {
		messageWindow, messageArgs := analyticsMessageWindowPred("wm.timestamp", from, to)
		// Filter sessions before walking their messages.
		where += " AND s.id IN (SELECT sessions.id FROM sessions WHERE (" + sessionWindow + " OR EXISTS (SELECT 1 FROM messages wm WHERE wm.session_id = sessions.id AND wm.timestamp != '' AND " + messageWindow + ")))"
		args = append(args, wargs...)
		args = append(args, messageArgs...)
	}
	flt := f.MessageScopeFilter()
	modelFiltering := len(flt.Models) > 0
	content := "m.content"
	var selectArgs []any
	effectiveTime := "COALESCE(NULLIF(m.timestamp, ''), NULLIF(s.started_at, ''), s.created_at)"
	if inWindow, wargs := analyticsMessageWindowPred(effectiveTime, from, to); inWindow != "" {
		rowWindow := inWindow
		if modelFiltering {
			content = "CASE WHEN " + inWindow + " THEN m.content ELSE '' END"
			selectArgs = append(selectArgs, wargs...)
			rowWindow = "(" + inWindow + " OR m.role = 'assistant')"
		}
		where += " AND " + rowWindow
		args = append(args, wargs...)
	}
	args = append(selectArgs, args...)
	columns := `m.content, agentsview_trend_timestamp(COALESCE(m.timestamp, ''), COALESCE(s.started_at, ''), COALESCE(s.created_at, ''))`
	if modelFiltering {
		columns = `m.session_id, m.ordinal, m.role, m.is_system,
			m.model, ` + content + `, COALESCE(m.timestamp, ''), COALESCE(s.started_at, ''), s.created_at`
	}
	query := `SELECT ` + columns + `
		FROM sessions s
		JOIN messages m ON m.session_id = s.id
		WHERE ` + where + `
			AND m.role IN ('user', 'assistant')
			AND m.is_system = 0
			AND ` + SystemPrefixSQL("m.content", "m.role") + `
		ORDER BY m.session_id, m.ordinal`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return TrendsTermsResponse{}, fmt.Errorf("querying trends terms: %w", err)
	}
	defer rows.Close()

	type trendRow struct {
		sessionID string
		role      string
		isSystem  bool
		model     string
		content   string
		msgTS     string
		startedAt string
		createdAt string
	}
	emit := func(m ScopedMessage) {
		if m.HasLocalTime {
			acc.Add(m.Content, m.LocalTime)
		}
	}
	reducer := NewScopeReducer(flt, emit)

	for rows.Next() {
		var row trendRow
		var ordinal int
		if !modelFiltering {
			var timestamp string
			if err := rows.Scan(&row.content, &timestamp); err != nil {
				return TrendsTermsResponse{}, fmt.Errorf("scanning trends term row: %w", err)
			}
			local, ok := LocalTime(timestamp, loc)
			if ok && flt.MatchesDayHour(local, true) {
				acc.Add(row.content, local)
			}
			continue
		}
		if err := rows.Scan(
			&row.sessionID, &ordinal, &row.role, &row.isSystem,
			&row.model, &row.content, &row.msgTS, &row.startedAt,
			&row.createdAt,
		); err != nil {
			return TrendsTermsResponse{}, fmt.Errorf("scanning trends term row: %w", err)
		}

		msgTime, has := trendMessageLocalTime(row.msgTS, row.startedAt, row.createdAt, loc)
		if err := reducer.Push(MessageInput{
			SessionID:    row.sessionID,
			Ordinal:      ordinal,
			Role:         row.role,
			Model:        row.model,
			IsSystem:     row.isSystem,
			Timestamp:    row.msgTS,
			LocalTime:    msgTime,
			HasLocalTime: has,
			Content:      row.content,
		}); err != nil {
			return TrendsTermsResponse{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return TrendsTermsResponse{}, fmt.Errorf("iterating trends term rows: %w", err)
	}

	response, err := acc.Finish()
	acc = nil
	return response, err
}

// TrendAccumulator counts term occurrences per trend bucket. Backends feed it
// messages with their local timestamps; it owns date filtering and bucketing.
type TrendAccumulator struct {
	from, to, granularity string
	terms                 []TrendTermInput
	buckets               []TrendBucket
	index                 map[string]int
	counts                [][]int
	messageCounts         []int
	lower                 []byte
	matchers              [][]trendMatcher
}

// NewTrendAccumulator returns an accumulator for the buckets spanning
// [from, to] at the given granularity.
func NewTrendAccumulator(
	from, to, granularity string, terms []TrendTermInput,
) *TrendAccumulator {
	buckets := TrendBucketRange(from, to, granularity)
	counts := make([][]int, len(terms))
	for i := range counts {
		counts[i] = make([]int, len(buckets))
	}
	matchers := make([][]trendMatcher, len(terms))
	for i, term := range terms {
		matchers[i] = prepareTrendMatchers(term)
	}
	return &TrendAccumulator{
		matchers:      matchers,
		from:          from,
		to:            to,
		granularity:   granularity,
		terms:         terms,
		buckets:       buckets,
		index:         trendBucketIndex(buckets),
		counts:        counts,
		messageCounts: make([]int, len(buckets)),
	}
}

// Add counts content in the bucket holding local's calendar date. Messages
// whose local date falls outside [from, to] are ignored.
func (a *TrendAccumulator) Add(content string, local time.Time) {
	if !InDateRange(local.Format("2006-01-02"), a.from, a.to) {
		return
	}
	bucket, ok := a.index[TrendBucketDate(local, local.Location(), a.granularity)]
	if !ok {
		return
	}
	a.messageCounts[bucket]++
	a.lower = appendTrendLower(a.lower, content)
	for i, matchers := range a.matchers {
		a.counts[i][bucket] += countTrendOccurrences(a.lower, matchers)
	}
}

// Response builds the trends response from the accumulated counts.
func (a *TrendAccumulator) Response() TrendsTermsResponse {
	return BuildTrendsTermsResponse(
		a.from, a.to, a.granularity, a.buckets, a.terms, a.counts, a.messageCounts,
	)
}

func ParseTrendTerms(values []string) ([]TrendTermInput, error) {
	terms := make([]TrendTermInput, 0, min(len(values), MaxTrendTerms))
	for _, value := range values {
		variants := parseTrendTermVariants(value)
		if len(variants) == 0 {
			continue
		}
		if len(variants) > MaxTrendTermVariants {
			return nil, fmt.Errorf("trend terms can have at most %d variants", MaxTrendTermVariants)
		}
		matchers := make([]string, 0, len(variants)*3)
		for _, variant := range variants {
			matchers = append(matchers, variant)
			if plural, ok := simpleTrendPlural(variant); ok {
				matchers = append(matchers, plural)
			}
			matchers = append(matchers, simpleSilentEStemMatchers(variant)...)
		}
		matchers = dedupeCaseFolded(matchers)
		terms = append(terms, TrendTermInput{
			Term:     variants[0],
			Variants: variants,
			Matchers: matchers,
		})
	}
	if len(terms) == 0 {
		return nil, errors.New("at least one trend term is required")
	}
	if len(terms) > MaxTrendTerms {
		return nil, fmt.Errorf("trend query supports at most %d terms", MaxTrendTerms)
	}
	return terms, nil
}

func parseTrendTermVariants(value string) []string {
	parts := strings.Split(value, "|")
	variants := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		variant := strings.TrimSpace(part)
		if variant == "" {
			continue
		}
		key := strings.ToLower(variant)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		variants = append(variants, variant)
	}
	return variants
}

func simpleTrendPlural(value string) (string, bool) {
	if value == "" || strings.ContainsAny(value, " \t\n\r") {
		return "", false
	}
	if strings.HasSuffix(strings.ToLower(value), "s") {
		return "", false
	}
	return value + "s", true
}

func simpleSilentEStemMatchers(value string) []string {
	if len(value) < 3 || !isSingleWordMatcher(value) {
		return nil
	}
	lower := strings.ToLower(value)
	if !strings.HasSuffix(lower, "c") ||
		strings.HasSuffix(lower, "e") ||
		strings.HasSuffix(lower, "s") {
		return nil
	}
	withE := value + "e"
	return []string{
		withE,
		withE + "s",
		withE + "d",
		value + "ing",
	}
}

func dedupeCaseFolded(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

type matchSpan struct {
	start int
	end   int
}

type trendMatcher struct {
	needle      []byte
	wordBounded bool
}

func prepareTrendMatchers(term TrendTermInput) []trendMatcher {
	var matchers []trendMatcher
	for _, matcher := range term.Matchers {
		matcher = strings.TrimSpace(matcher)
		if matcher != "" {
			matchers = append(matchers, trendMatcher{[]byte(strings.ToLower(matcher)), isSingleWordMatcher(matcher)})
		}
	}
	return matchers
}

func appendTrendLower(dst []byte, text string) []byte {
	dst = append(dst[:0], text...)
	for i, c := range dst {
		if c >= utf8.RuneSelf {
			dst = dst[:0]
			for _, r := range text {
				dst = utf8.AppendRune(dst, unicode.ToLower(r))
			}
			return dst
		}
		if 'A' <= c && c <= 'Z' {
			dst[i] = c + ('a' - 'A')
		}
	}
	return dst
}

func CountTrendOccurrences(text string, term TrendTermInput) int {
	return countTrendOccurrences(appendTrendLower(nil, text), prepareTrendMatchers(term))
}

func countTrendOccurrences(haystack []byte, matchers []trendMatcher) int {
	var spans []matchSpan
	for _, matcher := range matchers {
		spans = append(spans, collectTrendSpans(haystack, matcher.needle, matcher.wordBounded)...)
	}
	return mergeCountSpans(spans)
}

func collectTrendSpans(haystack, needle []byte, wordBounded bool) []matchSpan {
	if len(needle) == 0 || len(haystack) == 0 {
		return nil
	}
	var spans []matchSpan
	for offset := 0; offset < len(haystack); {
		idx := bytes.Index(haystack[offset:], needle)
		if idx < 0 {
			break
		}
		start := offset + idx
		end := start + len(needle)
		if !wordBounded || hasWordBoundaries(haystack, start, end) {
			spans = append(spans, matchSpan{start: start, end: end})
		}
		offset = start + 1
	}
	return spans
}

func isSingleWordMatcher(matcher string) bool {
	if matcher == "" {
		return false
	}
	for i := range len(matcher) {
		if !isWordByte(matcher[i]) {
			return false
		}
	}
	return true
}

func hasWordBoundaries(text []byte, start, end int) bool {
	if start > 0 && isWordByte(text[start-1]) {
		return false
	}
	if end < len(text) && isWordByte(text[end]) {
		return false
	}
	return true
}

func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '_'
}

func mergeCountSpans(spans []matchSpan) int {
	if len(spans) == 0 {
		return 0
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start == spans[j].start {
			return spans[i].end < spans[j].end
		}
		return spans[i].start < spans[j].start
	})
	count := 0
	mergedEnd := -1
	for _, span := range spans {
		if span.start >= mergedEnd {
			count++
			mergedEnd = span.end
			continue
		}
		if span.end > mergedEnd {
			mergedEnd = span.end
		}
	}
	return count
}

func trendMessageLocalTime(
	messageTS string,
	startedAt string,
	createdAt string,
	loc *time.Location,
) (time.Time, bool) {
	for _, ts := range []string{messageTS, startedAt, createdAt} {
		if t, ok := LocalTime(ts, loc); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

func TrendBucketDate(t time.Time, loc *time.Location, granularity string) string {
	local := t.In(loc)
	// Calendar math runs in UTC because local midnight does not exist on some
	// DST transition days, and time.Date would move it to the previous day.
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	switch granularity {
	case "week":
		weekday := int(day.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		day = day.AddDate(0, 0, -(weekday - 1))
	case "month":
		day = day.AddDate(0, 0, 1-day.Day())
	}
	return day.Format("2006-01-02")
}

func TrendBucketRange(from, to, granularity string) []TrendBucket {
	if from == "" || to == "" {
		return nil
	}
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		return nil
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil {
		return nil
	}
	startDate := TrendBucketDate(start, time.UTC, granularity)
	endDate := TrendBucketDate(end, time.UTC, granularity)
	cur, err := time.Parse("2006-01-02", startDate)
	if err != nil {
		return nil
	}
	last, err := time.Parse("2006-01-02", endDate)
	if err != nil {
		return nil
	}
	buckets := make([]TrendBucket, 0)
	for !cur.After(last) {
		buckets = append(buckets, TrendBucket{Date: cur.Format("2006-01-02")})
		switch granularity {
		case "month":
			cur = cur.AddDate(0, 1, 0)
		case "week":
			cur = cur.AddDate(0, 0, 7)
		default:
			cur = cur.AddDate(0, 0, 1)
		}
	}
	return buckets
}

func trendBucketIndex(buckets []TrendBucket) map[string]int {
	index := make(map[string]int, len(buckets))
	for i, bucket := range buckets {
		index[bucket.Date] = i
	}
	return index
}

func BuildTrendsTermsResponse(
	from string,
	to string,
	granularity string,
	buckets []TrendBucket,
	terms []TrendTermInput,
	counts [][]int,
	messageCounts []int,
) TrendsTermsResponse {
	outBuckets := make([]TrendBucket, len(buckets))
	totalMessages := 0
	for i, bucket := range buckets {
		outBuckets[i] = bucket
		if i < len(messageCounts) {
			outBuckets[i].MessageCount = messageCounts[i]
			totalMessages += messageCounts[i]
		}
	}
	series := make([]TrendSeries, len(terms))
	for i, term := range terms {
		points := make([]TrendPoint, len(buckets))
		total := 0
		for j, bucket := range buckets {
			count := 0
			if i < len(counts) && j < len(counts[i]) {
				count = counts[i][j]
			}
			total += count
			points[j] = TrendPoint{
				Date:  bucket.Date,
				Count: count,
			}
		}
		series[i] = TrendSeries{
			Term:     term.Term,
			Variants: append([]string(nil), term.Variants...),
			Total:    total,
			Points:   points,
		}
	}
	return TrendsTermsResponse{
		Granularity:  granularity,
		From:         from,
		To:           to,
		MessageCount: totalMessages,
		Buckets:      outBuckets,
		Series:       series,
	}
}
