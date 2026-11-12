package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

const geminiAppsTimestampPattern = `(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\s+\d{1,2},\s+\d{4},\s+\d{1,2}:\d{2}:\d{2}[\x{00a0}\x{202f} ]*(?:AM|PM)[\x{00a0}\x{202f} ]+(\S+)`

var (
	// geminiAppsTimestampLineRE matches the line Takeout writes between the
	// prompt and the response.
	geminiAppsTimestampLineRE = regexp.MustCompile(`(?i)^` + geminiAppsTimestampPattern + `$`)
	// geminiAppsTimestampLikeRE recognizes a date-and-clock line in a
	// rendering this parser does not support, such as another locale.
	geminiAppsTimestampLikeRE = regexp.MustCompile(`(?i)^.{0,40}(?:\b\d{1,2}\D+\d{4}\b|\b\d{4}\D+\d{1,2}\D+\d{1,2}\b).{0,10}\b\d{1,2}[:.]\d{2}[:.]\d{2}\b.{0,20}$`)
)

type geminiAppsFilePlan struct {
	results         []ParseResult
	skipped, errors int
}

type geminiAppsUnsupportedError struct{ err error }

func (e geminiAppsUnsupportedError) Error() string { return e.err.Error() }

func (p *geminiAppsImportOnlyProvider) ParseGeminiAppsExport(root string, callback func(ParseResult) error) (GeminiAppsParseSummary, error) {
	return parseGeminiAppsExport(root, time.Local, callback)
}

// parseGeminiAppsExport parses every Gemini Apps HTML file under root. host
// resolves zone abbreviations; see parseGeminiAppsTimestamp.
func parseGeminiAppsExport(root string, host *time.Location, callback func(ParseResult) error) (summary GeminiAppsParseSummary, retErr error) {
	paths, err := geminiAppsHTMLPaths(root)
	if err != nil {
		return summary, err
	}
	if len(paths) == 0 {
		return summary, errors.New("no HTML files found in import source")
	}

	plans := make([]geminiAppsFilePlan, 0, len(paths))
	admitted := false
	for _, path := range paths {
		plan, ok, err := planGeminiAppsFile(path, host)
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
		return summary, errors.New("input does not contain a Gemini Apps My Activity HTML document")
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
		return summary, errors.New("input contains no admissible Prompted records")
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

// planGeminiAppsFile reads one Takeout HTML file. host names the importing
// machine's time zone, which resolves zone abbreviations the fixed table does
// not know.
func planGeminiAppsFile(path string, host *time.Location) (geminiAppsFilePlan, bool, error) {
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
		return geminiAppsFilePlan{}, false, errors.New("unsupported Gemini Apps Takeout locale")
	}
	if !geminiAppsTitleAdmitted(info.title) {
		return geminiAppsFilePlan{}, false, errors.New("unsupported localized or changed Gemini Apps Takeout format")
	}
	for _, cell := range geminiCells {
		prompted, result, err := planGeminiAppsCell(cell, host)
		if err != nil {
			if _, ok := errors.AsType[geminiAppsUnsupportedError](err); ok {
				return geminiAppsFilePlan{}, false, err
			}
			plan.errors++
			continue
		}
		if !prompted {
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

// planGeminiAppsCell reads one Gemini Apps activity cell. Takeout writes the
// activity line, such as "Prompted <prompt>", then the timestamp line, then
// the response HTML, all as direct children of the body content cell with
// <br> between lines. It returns false with no error for activity that is not
// a prompt.
func planGeminiAppsCell(cell *html.Node, host *time.Location) (bool, ParseResult, error) {
	content := geminiAppsBodyCell(cell)
	if content == nil {
		return false, ParseResult{}, errors.New("activity record has no content cell")
	}
	var children []*html.Node
	for child := content.FirstChild; child != nil; child = child.NextSibling {
		children = append(children, child)
	}
	lines := geminiAppsLines(children)
	if len(lines) == 0 || !geminiAppsIsPrompted(geminiAppsRender(children[lines[0][0]:lines[0][1]])) {
		return false, ParseResult{}, nil
	}

	stampLine := -1
	var match []string
	texts := make([]string, len(lines))
	for i, line := range lines {
		texts[i] = normalizeMetadata(geminiAppsRender(children[line[0]:line[1]]))
		if i > 0 && stampLine < 0 {
			if match = geminiAppsTimestampLineRE.FindStringSubmatch(texts[i]); match != nil {
				stampLine = i
			}
		}
	}
	if stampLine < 0 {
		// Look for another rendering only when no supported timestamp exists,
		// so dates the user typed in a prompt cannot abort the import.
		for _, text := range texts[1:] {
			if geminiAppsTimestampLikeRE.MatchString(text) {
				return false, ParseResult{}, geminiAppsUnsupportedError{fmt.Errorf("unsupported Gemini Apps timestamp %q", text)}
			}
		}
		return false, ParseResult{}, errors.New("prompted activity record has no timestamp line")
	}
	ts, err := parseGeminiAppsTimestamp(match[0], match[1], host)
	if err != nil {
		return false, ParseResult{}, geminiAppsUnsupportedError{err}
	}

	prompt := geminiAppsPromptText(children[:lines[stampLine][0]])
	if prompt == "" {
		return false, ParseResult{}, errors.New("prompted activity record has no prompt")
	}
	response := normalizeGeminiAppsContentText(geminiAppsRender(children[lines[stampLine][1]:]))

	messages := []ParsedMessage{{Ordinal: 0, Role: RoleUser, Content: prompt, Timestamp: ts, ContentLength: len(prompt)}}
	if response != "" {
		messages = append(messages, ParsedMessage{Ordinal: 1, Role: RoleAssistant, Content: response, Timestamp: ts, ContentLength: len(response)})
	}
	result := ParseResult{
		Session: ParsedSession{
			Project: "gemini.google.com", Machine: "local", Agent: AgentGeminiApps,
			FirstMessage: prompt, SessionName: prompt, StartedAt: ts, EndedAt: ts,
			MessageCount: len(messages), UserMessageCount: 1,
		},
		Messages: messages,
	}
	return true, result, nil
}

// geminiAppsBodyCell returns the content cell that holds the activity text.
// Takeout also writes an empty right-aligned cell and a caption cell that
// lists products and details.
func geminiAppsBodyCell(cell *html.Node) *html.Node {
	var found *html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found != nil {
			return
		}
		if n.Type == html.ElementNode && hasClass(n, "content-cell") &&
			!hasClass(n, "mdl-typography--text-right") && !hasClass(n, "mdl-typography--caption") {
			found = n
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(cell)
	return found
}

// geminiAppsLines splits direct children at <br> elements into [start, end)
// index ranges, dropping lines with no visible text.
func geminiAppsLines(children []*html.Node) [][2]int {
	var lines [][2]int
	start := 0
	flush := func(end int) {
		if strings.TrimSpace(normalizeMetadata(geminiAppsRender(children[start:end]))) != "" {
			lines = append(lines, [2]int{start, end})
		}
	}
	for i, child := range children {
		if child.Type == html.ElementNode && strings.EqualFold(child.Data, "br") {
			flush(i)
			start = i + 1
		}
	}
	flush(len(children))
	return lines
}

func geminiAppsRender(nodes []*html.Node) string {
	var b strings.Builder
	for _, n := range nodes {
		b.WriteString(geminiAppsVisibleTextInContext(n, false))
	}
	return b.String()
}

const geminiAppsPromptedLabel = "prompted"

// geminiAppsIsPrompted reports whether an activity line starts with the
// Prompted label as a whole word.
func geminiAppsIsPrompted(line string) bool {
	fields := strings.Fields(normalizeMetadata(line))
	return len(fields) > 0 && strings.EqualFold(fields[0], geminiAppsPromptedLabel)
}

// geminiAppsPromptText renders the lines before the timestamp and removes the
// leading Prompted label.
func geminiAppsPromptText(nodes []*html.Node) string {
	text := strings.TrimLeftFunc(normalizeGeminiAppsContentText(geminiAppsRender(nodes)), unicode.IsSpace)
	if len(text) < len(geminiAppsPromptedLabel) || !strings.EqualFold(text[:len(geminiAppsPromptedLabel)], geminiAppsPromptedLabel) {
		return ""
	}
	return strings.TrimFunc(text[len(geminiAppsPromptedLabel):], unicode.IsSpace)
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

// geminiAppsProductHeading returns the lowercased product name Takeout writes
// in each cell's header, such as "gemini apps".
func geminiAppsProductHeading(cell *html.Node) string {
	header := firstDescendantClass(cell, "header-cell")
	if header == nil {
		return ""
	}
	return strings.ToLower(normalizeMetadata(geminiAppsText(header, false)))
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

// parseGeminiAppsTimestamp resolves a Takeout timestamp such as
// "Nov 10, 2026, 3:33:10 AM IST". Takeout writes the account's local time with
// a zone abbreviation. When the importing machine's zone uses the same
// abbreviation, its offset wins, because abbreviations like IST and CST name
// different zones in different countries. Otherwise the fixed table and
// GMT offsets apply.
func parseGeminiAppsTimestamp(value, zone string, host *time.Location) (time.Time, error) {
	value = normalizeMetadata(value)
	zone = strings.ToUpper(zone)
	index := strings.LastIndex(value, " ")
	if index < 0 {
		return time.Time{}, fmt.Errorf("invalid Gemini Apps timestamp %q", value)
	}
	wall, err := time.Parse("Jan 2, 2006, 3:04:05 PM", strings.TrimSpace(value[:index]))
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing Gemini Apps timestamp: %w", err)
	}
	offset, ok := geminiAppsHostZoneOffset(zone, wall, host)
	if !ok {
		offset, ok = geminiAppsZoneOffset(zone)
	}
	if !ok {
		return time.Time{}, fmt.Errorf("unsupported Gemini Apps timestamp zone %q; rerun the import with TZ set to the Google account's time zone", zone)
	}
	return wall.Add(-time.Duration(offset) * time.Second).In(time.FixedZone(zone, offset)), nil
}

// geminiAppsHostZoneOffset returns the offset host uses with the abbreviation
// zone. Takeout labels every record with the zone in effect when the export
// ran, so a summer abbreviation can appear on a winter record. The check
// therefore looks at the record's date and at both halves of its year.
func geminiAppsHostZoneOffset(zone string, wall time.Time, host *time.Location) (int, bool) {
	if host == nil {
		return 0, false
	}
	year := wall.Year()
	for _, probe := range []time.Time{
		time.Date(year, wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), 0, host),
		time.Date(year, time.January, 1, 12, 0, 0, 0, host),
		time.Date(year, time.July, 1, 12, 0, 0, 0, host),
	} {
		if name, offset := probe.Zone(); strings.EqualFold(name, zone) {
			return offset, true
		}
	}
	return 0, false
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
