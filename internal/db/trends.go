package db

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
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
	if granularity == "" {
		granularity = "week"
	}
	loc := f.location()
	buckets := TrendBucketRange(f.From, f.To, granularity)
	bucketIndex := trendBucketIndex(buckets)
	counts := make([][]int, len(terms))
	for i := range counts {
		counts[i] = make([]int, len(buckets))
	}
	messageCounts := make([]int, len(buckets))

	sessionFilter := f
	sessionFilter.From = ""
	sessionFilter.To = ""
	sessionFilter.DayOfWeek = nil
	sessionFilter.Hour = nil
	sessionFilter.Model = ""
	where, args := sessionFilter.buildWhereWithDate("", false, "s.id")
	flt := f.messageScopeFilter()
	modelFiltering := len(flt.Models) > 0
	query := `SELECT m.session_id, m.ordinal, m.role, m.is_system,
			COALESCE(m.model, ''), m.content, COALESCE(m.timestamp, ''),
			COALESCE(s.started_at, ''), s.created_at
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
	processRow := func(row trendRow) {
		msgTime, ok := trendMessageLocalTime(row.msgTS, row.startedAt, row.createdAt, loc)
		if !ok {
			return
		}
		msgDate := msgTime.Format("2006-01-02")
		if !inDateRange(msgDate, f.From, f.To) {
			return
		}
		bucketDate := trendBucketDate(msgTime, loc, granularity)
		bucket, ok := bucketIndex[bucketDate]
		if !ok {
			return
		}
		messageCounts[bucket]++
		for i, term := range terms {
			count := countTrendOccurrences(row.content, term)
			if count > 0 {
				counts[i][bucket] += count
			}
		}
	}
	rowStartedAt := make(map[string]string)
	rowCreatedAt := make(map[string]string)
	emit := func(m ScopedMessage) {
		processRow(trendRow{
			sessionID: m.SessionID,
			role:      m.Role,
			isSystem:  m.IsSystem,
			content:   m.Content,
			msgTS:     m.Timestamp,
			startedAt: rowStartedAt[m.SessionID],
			createdAt: rowCreatedAt[m.SessionID],
		})
	}
	reducer := NewScopeReducer(flt, emit)

	for rows.Next() {
		var row trendRow
		var ordinal int
		if err := rows.Scan(
			&row.sessionID, &ordinal, &row.role, &row.isSystem,
			&row.model, &row.content, &row.msgTS, &row.startedAt,
			&row.createdAt,
		); err != nil {
			return TrendsTermsResponse{}, fmt.Errorf("scanning trends term row: %w", err)
		}
		if !modelFiltering {
			msgTime, ok := trendMessageLocalTime(row.msgTS, row.startedAt, row.createdAt, loc)
			if ok && flt.MatchesDayHour(msgTime, true) {
				processRow(row)
			}
			continue
		}
		rowStartedAt[row.sessionID] = row.startedAt
		rowCreatedAt[row.sessionID] = row.createdAt
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

	return BuildTrendsTermsResponse(
		f.From, f.To, granularity, buckets, terms, counts, messageCounts,
	), nil
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
		return nil, fmt.Errorf("at least one trend term is required")
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

func countTrendOccurrences(text string, term TrendTermInput) int {
	return CountTrendOccurrences(text, term)
}

func CountTrendOccurrences(text string, term TrendTermInput) int {
	spans := make([]matchSpan, 0)
	for _, matcher := range term.Matchers {
		matcher = strings.TrimSpace(matcher)
		if matcher == "" {
			continue
		}
		wordBounded := isSingleWordMatcher(matcher)
		spans = append(spans, collectTrendSpans(text, matcher, wordBounded)...)
	}
	return mergeCountSpans(spans)
}

func collectTrendSpans(text string, matcher string, wordBounded bool) []matchSpan {
	needle := strings.ToLower(matcher)
	haystack := strings.ToLower(text)
	if needle == "" || haystack == "" {
		return nil
	}
	spans := make([]matchSpan, 0)
	for offset := 0; offset < len(haystack); {
		idx := strings.Index(haystack[offset:], needle)
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
	for i := 0; i < len(matcher); i++ {
		if !isWordByte(matcher[i]) {
			return false
		}
	}
	return true
}

func hasWordBoundaries(text string, start, end int) bool {
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
		if t, ok := localTime(ts, loc); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

func trendBucketDate(t time.Time, loc *time.Location, granularity string) string {
	return TrendBucketDate(t, loc, granularity)
}

func TrendBucketDate(t time.Time, loc *time.Location, granularity string) string {
	local := t.In(loc)
	switch granularity {
	case "week":
		weekday := int(local.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		start := local.AddDate(0, 0, -(weekday - 1))
		return time.Date(
			start.Year(), start.Month(), start.Day(),
			0, 0, 0, 0, loc,
		).Format("2006-01-02")
	case "month":
		return time.Date(
			local.Year(), local.Month(), 1,
			0, 0, 0, 0, loc,
		).Format("2006-01-02")
	default:
		return local.Format("2006-01-02")
	}
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
	startDate := trendBucketDate(start, time.UTC, granularity)
	endDate := trendBucketDate(end, time.UTC, granularity)
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
