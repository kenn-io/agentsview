package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/html"
)

type GeminiAppsParseSummary struct{ Skipped, Errors int }

type GeminiAppsExportParser interface {
	ParseGeminiAppsExport(string, func(ParseResult) error) (GeminiAppsParseSummary, error)
}

var geminiAppsTimestampRE = regexp.MustCompile(`(?i)\b(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\s+\d{1,2},\s+\d{4},\s+\d{1,2}:\d{2}:\d{2}[\x{00a0}\x{202f} ]*(?:AM|PM)[\x{00a0}\x{202f} ]+(\S+)`)
var geminiAppsTimestampLikeRE = regexp.MustCompile(`(?i)(?:\b\d{1,2}\D+\d{4}\b|\b\d{4}\D+\d{1,2}\D+\d{1,2}\b)`)

type geminiAppsFilePlan struct {
	results         []ParseResult
	skipped, errors int
}

type geminiAppsUnsupportedError struct{ err error }

func (e geminiAppsUnsupportedError) Error() string { return e.err.Error() }

func (p *geminiAppsImportOnlyProvider) ParseGeminiAppsExport(root string, callback func(ParseResult) error) (summary GeminiAppsParseSummary, retErr error) {
	paths, err := geminiAppsHTMLPaths(root)
	if err != nil {
		return summary, err
	}
	if len(paths) == 0 {
		return summary, fmt.Errorf("no HTML files found in import source")
	}

	plans := make([]geminiAppsFilePlan, 0, len(paths))
	admitted := false
	for _, path := range paths {
		plan, ok, err := planGeminiAppsFile(path)
		if err != nil {
			return summary, err
		}
		if !ok {
			continue
		}
		admitted = true
		plans = append(plans, plan)
	}
	if !admitted {
		return summary, fmt.Errorf("input does not contain a Gemini Apps My Activity HTML document")
	}
	for _, plan := range plans {
		summary.Skipped += plan.skipped
		summary.Errors += plan.errors
	}
	occurrencesByTimestamp := make(map[string]int)
	for planIndex := range plans {
		for resultIndex := range plans[planIndex].results {
			result := &plans[planIndex].results[resultIndex]
			timestampKey := result.Session.StartedAt.UTC().Format(time.RFC3339Nano)
			occurrence := occurrencesByTimestamp[timestampKey]
			occurrencesByTimestamp[timestampKey] = occurrence + 1
			idInput := timestampKey + "\x00" + strconv.Itoa(occurrence)
			hash := sha256.Sum256([]byte(idInput))
			result.Session.ID = "gemini-apps:" + hex.EncodeToString(hash[:])
		}
	}
	for _, plan := range plans {
		for _, result := range plan.results {
			if err := callback(result); err != nil {
				return summary, err
			}
		}
	}
	if len(plans) == 0 || countPlannedResults(plans) == 0 {
		return summary, fmt.Errorf("input contains no admissible Prompted records")
	}
	return summary, nil
}

func countPlannedResults(plans []geminiAppsFilePlan) int {
	n := 0
	for _, p := range plans {
		n += len(p.results)
	}
	return n
}

func planGeminiAppsFile(path string) (geminiAppsFilePlan, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return geminiAppsFilePlan{}, false, fmt.Errorf("reading Takeout HTML: %w", err)
	}
	doc, parseErr := html.Parse(file)
	closeErr := file.Close()
	if parseErr != nil {
		return geminiAppsFilePlan{}, false, fmt.Errorf("parsing Takeout HTML: %w", parseErr)
	}
	if closeErr != nil {
		return geminiAppsFilePlan{}, false, fmt.Errorf("closing Takeout HTML: %w", closeErr)
	}

	info := geminiAppsDocumentInfo(doc)
	plan := geminiAppsFilePlan{}
	cells := geminiAppsOuterCells(doc)
	geminiCells := make([]*html.Node, 0, len(cells))
	for _, cell := range cells {
		if geminiAppsProductHeading(cell) != "gemini apps" {
			continue
		}
		geminiCells = append(geminiCells, cell)
	}
	if len(geminiCells) == 0 {
		return plan, false, nil
	}
	if info.language != "" && !strings.EqualFold(strings.SplitN(info.language, "-", 2)[0], "en") {
		return geminiAppsFilePlan{}, false, fmt.Errorf("unsupported Gemini Apps Takeout locale")
	}
	if !geminiAppsTitleAdmitted(info.title) {
		return geminiAppsFilePlan{}, false, fmt.Errorf("unsupported localized or changed Gemini Apps Takeout format")
	}
	for _, cell := range geminiCells {
		admittedCell, result, err := planGeminiAppsCell(cell)
		if !admittedCell {
			continue
		}
		if err != nil {
			if _, unsupported := err.(geminiAppsUnsupportedError); unsupported {
				return geminiAppsFilePlan{}, false, err
			}
			plan.errors++
			continue
		}
		if result.Session.FirstMessage == "" {
			plan.skipped++
			continue
		}
		plan.results = append(plan.results, result)
	}
	return plan, true, nil
}

type geminiAppsDocument struct{ title, language string }

func geminiAppsDocumentInfo(doc *html.Node) geminiAppsDocument {
	info := geminiAppsDocument{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && strings.EqualFold(n.Data, "html") {
			info.language = attr(n, "lang")
		}
		if n.Type == html.ElementNode && strings.EqualFold(n.Data, "title") {
			info.title = strings.TrimSpace(geminiAppsText(n, true))
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return info
}

func geminiAppsOuterCells(doc *html.Node) []*html.Node {
	var cells []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && hasClass(n, "outer-cell") {
			cells = append(cells, n)
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return cells
}

func planGeminiAppsCell(cell *html.Node) (bool, ParseResult, error) {
	header := firstDescendantClass(cell, "header-cell")
	if header == nil {
		return false, ParseResult{}, nil
	}
	if geminiAppsActivityLabel(header) != "prompted" {
		return true, ParseResult{}, nil
	}
	content := firstDescendantClass(cell, "content-cell")
	headerText := normalizeMetadata(geminiAppsText(header, false))
	match := geminiAppsTimestampRE.FindStringSubmatch(headerText)
	if match == nil {
		if geminiAppsTimestampLikeRE.MatchString(headerText) {
			return true, ParseResult{}, geminiAppsUnsupportedError{fmt.Errorf("activity record has no supported header timestamp")}
		}
		return true, ParseResult{}, fmt.Errorf("activity record has no supported header timestamp")
	}
	ts, err := parseGeminiAppsTimestamp(match[0], match[1])
	if err != nil {
		return true, ParseResult{}, geminiAppsUnsupportedError{err}
	}
	if content == nil {
		return true, ParseResult{}, fmt.Errorf("prompted activity record has no content cell")
	}
	payload := geminiAppsContentText(content, match[0])
	if payload == "" {
		return true, ParseResult{}, fmt.Errorf("prompted activity record has no prompt")
	}
	result := ParseResult{Session: ParsedSession{Project: "gemini.google.com", Machine: "local", Agent: AgentGeminiApps, FirstMessage: payload, SessionName: payload, StartedAt: ts, EndedAt: ts, MessageCount: 1, UserMessageCount: 1}}
	result.Messages = []ParsedMessage{{Ordinal: 0, Role: RoleUser, Content: payload, Timestamp: ts, ContentLength: len(payload)}}
	return true, result, nil
}

func geminiAppsContentText(content *html.Node, headerTimestamp string) string {
	var b strings.Builder
	for child := content.FirstChild; child != nil; child = child.NextSibling {
		text := geminiAppsVisibleText(child)
		if normalizeMetadata(text) == normalizeMetadata(headerTimestamp) {
			continue
		}
		b.WriteString(geminiAppsVisibleTextInContext(child, false))
	}
	return normalizeGeminiAppsContentText(b.String())
}

func geminiAppsActivityLabel(header *html.Node) string {
	var label string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if label != "" {
			return
		}
		if n.Type == html.ElementNode && isGeminiAppsIgnored(strings.ToLower(n.Data)) {
			return
		}
		if n.Type == html.ElementNode && strings.EqualFold(n.Data, "p") {
			label = strings.ToLower(normalizeMetadata(geminiAppsText(n, false)))
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(header)
	return label
}

func geminiAppsVisibleText(n *html.Node) string {
	return normalizeGeminiAppsVisibleText(geminiAppsVisibleTextInContext(n, false))
}

func normalizeGeminiAppsVisibleText(value string) string {
	const start, end = "\ue000", "\ue001"
	value = normalizeGeminiAppsVisibleTextWithMarkers(value)
	return strings.NewReplacer(start, "", end, "").Replace(value)
}

func normalizeGeminiAppsContentText(value string) string {
	const start, end = "\ue000", "\ue001"
	value = normalizeGeminiAppsVisibleTextWithMarkers(value)
	value = strings.TrimLeft(value, "\n")
	value = strings.TrimRight(value, "\n")
	return strings.NewReplacer(start, "", end, "").Replace(value)
}

func normalizeGeminiAppsVisibleTextWithMarkers(value string) string {
	const start, end = "\ue000", "\ue001"
	clean := func(part string) string {
		var out []rune
		pendingSpace := false
		for _, r := range part {
			if r == '\n' {
				for len(out) > 0 && out[len(out)-1] == ' ' {
					out = out[:len(out)-1]
				}
				out = append(out, '\n')
				pendingSpace = false
				continue
			}
			if unicode.IsSpace(r) {
				pendingSpace = true
				continue
			}
			if pendingSpace && len(out) > 0 && out[len(out)-1] != '\n' {
				out = append(out, ' ')
			}
			out = append(out, r)
			pendingSpace = false
		}
		for len(out) > 0 && out[len(out)-1] == ' ' {
			out = out[:len(out)-1]
		}
		for i := 0; i+2 < len(out); i++ {
			if out[i] == '\n' && out[i+1] == '\n' && out[i+2] == '\n' {
				out = append(out[:i+2], out[i+3:]...)
				i--
			}
		}
		return string(out)
	}
	var b strings.Builder
	for value != "" {
		startIndex := strings.Index(value, start)
		if startIndex < 0 {
			b.WriteString(clean(value))
			break
		}
		b.WriteString(clean(value[:startIndex]))
		value = value[startIndex+len(start):]
		endIndex := strings.Index(value, end)
		if endIndex < 0 {
			b.WriteString(value)
			break
		}
		b.WriteString(value[:endIndex])
		value = value[endIndex+len(end):]
	}
	return b.String()
}

func geminiAppsVisibleTextInContext(n *html.Node, preserved bool) string {
	if n.Type == html.TextNode {
		return geminiAppsTextValue(n.Data, preserved)
	}
	if n.Type != html.ElementNode {
		return ""
	}
	tag := strings.ToLower(n.Data)
	if isGeminiAppsIgnored(tag) {
		return ""
	}
	preserved = preserved || tag == "pre"
	var b strings.Builder
	switch tag {
	case "br":
		b.WriteByte('\n')
	case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "pre", "li", "tr", "th", "td", "table", "ul", "ol":
		b.WriteByte('\n')
	}
	if tag == "pre" {
		b.WriteString("\ue000")
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		b.WriteString(geminiAppsVisibleTextInContext(child, preserved))
	}
	if tag == "pre" {
		b.WriteString("\ue001")
	}
	switch tag {
	case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "pre", "li", "tr", "th", "td", "table", "ul", "ol":
		b.WriteByte('\n')
	}
	if preserved {
		return strings.ReplaceAll(b.String(), "\r\n", "\n")
	}
	return b.String()
}

func geminiAppsTextValue(value string, preserved bool) string {
	value = sanitizeGeminiAppsText(value)
	if preserved {
		return strings.ReplaceAll(value, "\r\n", "\n")
	}
	if value == "" {
		return ""
	}
	if strings.TrimSpace(value) == "" {
		return " "
	}
	leading, trailing := unicode.IsSpace([]rune(value)[0]), unicode.IsSpace([]rune(value)[len([]rune(value))-1])
	value = strings.Join(strings.Fields(value), " ")
	if leading && value != "" {
		value = " " + value
	}
	if trailing && value != "" {
		value += " "
	}
	return value
}

func geminiAppsText(n *html.Node, preserved bool) string {
	var b strings.Builder
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		b.WriteString(geminiAppsVisibleTextInContext(child, preserved))
	}
	return b.String()
}

func sanitizeGeminiAppsText(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r == 0 || r == 0x1b || r == 0x7f || (r < 0x20 && r != '\n' && r != '\r' && r != '\t') || (r >= 0x80 && r <= 0x9f) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
func isGeminiAppsIgnored(tag string) bool {
	switch tag {
	case "script", "style", "template", "noscript":
		return true
	}
	return false
}
func hasClass(n *html.Node, wanted string) bool {
	for c := range strings.FieldsSeq(attr(n, "class")) {
		if strings.EqualFold(c, wanted) {
			return true
		}
	}
	return false
}
func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}
func firstDescendantClass(n *html.Node, class string) *html.Node {
	if n.Type == html.ElementNode && hasClass(n, class) {
		return n
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if found := firstDescendantClass(child, class); found != nil {
			return found
		}
	}
	return nil
}
func geminiAppsProductHeading(n *html.Node) string {
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && strings.HasPrefix(strings.ToLower(child.Data), "h") {
			text := normalizeMetadata(geminiAppsText(child, false))
			if text != "" {
				return strings.ToLower(text)
			}
		}
		if found := geminiAppsProductHeading(child); found != "" {
			return found
		}
	}
	return ""
}
func normalizeMetadata(value string) string {
	value = strings.NewReplacer("\u00a0", " ", "\u202f", " ").Replace(value)
	return strings.Join(strings.Fields(value), " ")
}
func geminiAppsTitleAdmitted(title string) bool {
	value := strings.ToLower(normalizeMetadata(title))
	return value == "my activity history" || strings.Contains(value, "gemini apps")
}

func geminiAppsHTMLPaths(root string) ([]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat import source: %w", err)
	}
	if !info.IsDir() {
		return []string{root}, nil
	}
	var paths []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			ext := strings.ToLower(filepath.Ext(entry.Name()))
			if ext == ".html" || ext == ".htm" {
				paths = append(paths, path)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking import source: %w", err)
	}
	sort.Strings(paths)
	return paths, nil
}

func parseGeminiAppsTimestamp(value, zone string) (time.Time, error) {
	value = normalizeMetadata(value)
	zone = strings.ToUpper(zone)
	offset, ok := geminiAppsZoneOffset(zone)
	if !ok {
		return time.Time{}, fmt.Errorf("unsupported Gemini Apps timestamp zone %q", zone)
	}
	index := strings.LastIndex(value, " ")
	if index < 0 {
		return time.Time{}, fmt.Errorf("invalid Gemini Apps timestamp %q", value)
	}
	loc := time.FixedZone(zone, offset)
	parsed, err := time.ParseInLocation("Jan 2, 2006, 3:04:05 PM", strings.TrimSpace(value[:index]), loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing Gemini Apps timestamp: %w", err)
	}
	return parsed, nil
}
func geminiAppsZoneOffset(zone string) (int, bool) {
	switch zone {
	case "UTC", "GMT":
		return 0, true
	case "EST":
		return -5 * 60 * 60, true
	case "EDT":
		return -4 * 60 * 60, true
	case "CST":
		return -6 * 60 * 60, true
	case "CDT":
		return -5 * 60 * 60, true
	case "MST":
		return -7 * 60 * 60, true
	case "MDT":
		return -6 * 60 * 60, true
	case "PST":
		return -8 * 60 * 60, true
	case "PDT":
		return -7 * 60 * 60, true
	}
	if strings.HasPrefix(zone, "GMT+") || strings.HasPrefix(zone, "GMT-") {
		numeric := strings.TrimPrefix(strings.TrimPrefix(zone, "GMT+"), "GMT-")
		if numeric == "" {
			return 0, false
		}
		sign := 1
		if zone[3] == '-' {
			sign = -1
		}
		parts := strings.Split(numeric, ":")
		if len(parts) > 2 || len(parts[0]) < 1 || len(parts[0]) > 2 {
			return 0, false
		}
		for _, part := range parts {
			if part == "" {
				return 0, false
			}
			for _, char := range part {
				if char < '0' || char > '9' {
					return 0, false
				}
			}
		}
		if len(parts) == 2 && len(parts[1]) != 2 {
			return 0, false
		}
		hours, e1 := strconv.Atoi(parts[0])
		minutes := 0
		var e2 error
		if len(parts) == 2 {
			minutes, e2 = strconv.Atoi(parts[1])
		}
		if e1 != nil || e2 != nil || hours > 23 || minutes > 59 {
			return 0, false
		}
		return sign * (hours*3600 + minutes*60), true
	}
	return 0, false
}
