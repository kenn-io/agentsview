package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// GeminiAppsParseSummary reports records that did not become sessions.
type GeminiAppsParseSummary struct {
	Skipped int
	Errors  int
}

// GeminiAppsExportParser is implemented by the Gemini Apps import-only
// provider. Takeout activity has no durable conversation identifier, so each
// admitted prompt is represented as a one-turn session.
type GeminiAppsExportParser interface {
	ParseGeminiAppsExport(
		root string,
		onConversation func(ParseResult) error,
	) (GeminiAppsParseSummary, error)
}

type geminiAppsZone struct {
	name   string
	tokens []html.Token
}

var geminiAppsTimestampRE = regexp.MustCompile(
	`(?i)\b(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)` +
		`\s+\d{1,2},\s+\d{4},\s+\d{1,2}:\d{2}:\d{2}` +
		`[\x{00a0}\x{202f} ]*(?:AM|PM)` +
		`[\x{00a0}\x{202f} ]+(GMT[+-]\d{1,2}:\d{2}|[A-Za-z]{2,5})\b`,
)

// ParseGeminiAppsExport reads a Takeout directory or HTML file and streams
// admitted Prompted records to onConversation.
func (p *geminiAppsImportOnlyProvider) ParseGeminiAppsExport(
	root string,
	onConversation func(ParseResult) error,
) (summary GeminiAppsParseSummary, retErr error) {
	paths, err := geminiAppsHTMLPaths(root)
	if err != nil {
		return summary, err
	}
	if len(paths) == 0 {
		return summary, fmt.Errorf("no HTML files found in import source")
	}

	admitted := false
	emitted := 0
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return summary, fmt.Errorf("reading Takeout HTML: %w", err)
		}

		title, err := geminiAppsDocumentTitle(file)
		if err != nil {
			_ = file.Close()
			return summary, fmt.Errorf("scanning Takeout HTML: %w", err)
		}
		if err := file.Close(); err != nil {
			return summary, fmt.Errorf("closing Takeout HTML: %w", err)
		}
		if !geminiAppsTitleAdmitted(title) {
			continue
		}

		file, err = os.Open(path)
		if err != nil {
			return summary, fmt.Errorf("reading Takeout HTML: %w", err)
		}
		fileAdmitted := false
		scanErr := scanGeminiAppsHTML(file, func(tokens []html.Token) error {
			if geminiAppsHeaderProductTitle(tokens) == "" {
				return nil
			}
			fileAdmitted = true
			kind := geminiAppsRecordKind(tokens)
			if kind != "prompted" {
				summary.Skipped++
				return nil
			}

			result, err := parseGeminiAppsCell(tokens)
			if err != nil {
				summary.Errors++
				return nil
			}
			if err := onConversation(result); err != nil {
				return err
			}
			emitted++
			return nil
		})
		closeErr := file.Close()
		if scanErr != nil {
			return summary, fmt.Errorf("scanning Takeout HTML: %w", scanErr)
		}
		if closeErr != nil {
			return summary, fmt.Errorf("closing Takeout HTML: %w", closeErr)
		}
		admitted = admitted || fileAdmitted
	}

	if !admitted {
		return summary, fmt.Errorf(
			"input does not contain a Gemini Apps My Activity HTML document",
		)
	}
	if len(paths) > 0 && emitted == 0 {
		return summary, fmt.Errorf(
			"input contains no admissible Prompted records",
		)
	}
	return summary, nil
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
	err = filepath.WalkDir(root, func(
		path string, entry os.DirEntry, walkErr error,
	) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext == ".html" || ext == ".htm" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking import source: %w", err)
	}
	sort.Strings(paths)
	return paths, nil
}

func geminiAppsDocumentTitle(r io.Reader) (string, error) {
	tok := html.NewTokenizer(r)
	var title strings.Builder
	depth := 0
	titleDepth := 0
	for {
		tokenType := tok.Next()
		switch tokenType {
		case html.ErrorToken:
			if err := tok.Err(); err != io.EOF {
				return "", err
			}
			return title.String(), nil
		case html.StartTagToken, html.SelfClosingTagToken:
			t := tok.Token()
			if tokenType == html.StartTagToken && strings.EqualFold(t.Data, "title") {
				titleDepth = depth + 1
			}
			if tokenType == html.StartTagToken && !isGeminiAppsVoidElement(t.Data) {
				depth++
			}
		case html.EndTagToken:
			t := tok.Token()
			if titleDepth > 0 && depth == titleDepth && strings.EqualFold(t.Data, "title") {
				titleDepth = 0
			}
			if depth > 0 {
				depth--
			}
		case html.TextToken:
			if titleDepth > 0 {
				title.Write(tok.Text())
			}
		}
	}
}

func scanGeminiAppsHTML(r io.Reader, onCell func([]html.Token) error) error {
	tok := html.NewTokenizer(r)
	var (
		cell      []html.Token
		depth     int
		cellDepth int
		inCell    bool
	)
	flush := func() error {
		if !inCell || len(cell) == 0 {
			return nil
		}
		tokens := cell
		cell = nil
		inCell = false
		return onCell(tokens)
	}
	for {
		tokenType := tok.Next()
		switch tokenType {
		case html.ErrorToken:
			if err := tok.Err(); err != io.EOF {
				return err
			}
			return flush()
		case html.StartTagToken, html.SelfClosingTagToken:
			t := tok.Token()
			if !inCell && hasHTMLClass(t, "outer-cell") {
				inCell = true
				cellDepth = depth + 1
				cell = nil
			}
			if inCell {
				cell = append(cell, t)
			}
			if tokenType == html.StartTagToken && !isGeminiAppsVoidElement(t.Data) {
				depth++
			}
		case html.EndTagToken:
			t := tok.Token()
			if inCell {
				cell = append(cell, t)
				if depth == cellDepth && strings.EqualFold(t.Data, "div") {
					if err := flush(); err != nil {
						return err
					}
				}
			}
			if depth > 0 {
				depth--
			}
		case html.TextToken, html.CommentToken:
			if inCell {
				cell = append(cell, tok.Token())
			}
		}
	}
}

func geminiAppsTitleAdmitted(title string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(title), " "))
	return normalized == "my activity history" ||
		strings.Contains(normalized, "gemini apps")
}

func isGeminiAppsVoidElement(tag string) bool {
	switch strings.ToLower(tag) {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr":
		return true
	default:
		return false
	}
}

func geminiAppsHeaderProductTitle(tokens []html.Token) string {
	for _, zone := range geminiAppsZones(tokens) {
		if zone.name != "header" {
			continue
		}
		var (
			heading strings.Builder
			depth   int
		)
		for _, token := range zone.tokens {
			switch token.Type {
			case html.StartTagToken:
				if depth == 0 && isGeminiAppsHeading(token.Data) {
					depth = 1
					continue
				}
				if depth > 0 && !isGeminiAppsVoidElement(token.Data) {
					depth++
				}
			case html.EndTagToken:
				if depth > 0 {
					depth--
					if depth == 0 {
						value := strings.ToLower(strings.Join(
							strings.Fields(heading.String()), " "),
						)
						if containsWord(value, "gemini") &&
							containsWord(value, "apps") {
							return value
						}
						heading.Reset()
					}
				}
			case html.TextToken:
				if depth > 0 {
					heading.WriteString(token.Data)
				}
			}
		}
	}
	return ""
}

func isGeminiAppsHeading(tag string) bool {
	switch strings.ToLower(tag) {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		return true
	default:
		return false
	}
}

func geminiAppsRecordKind(tokens []html.Token) string {
	zones := geminiAppsZones(tokens)
	for _, zone := range zones {
		if zone.name != "header" {
			continue
		}
		for _, field := range geminiAppsTextFields(renderGeminiAppsTokens(zone.tokens)) {
			switch field {
			case "prompted":
				return "prompted"
			case "canvas":
				return "canvas"
			case "feedback":
				return "feedback"
			}
		}
	}
	return "unknown"
}

func geminiAppsTextFields(value string) []string {
	var fields []string
	for line := range strings.SplitSeq(value, "\n") {
		field := strings.ToLower(strings.Join(strings.Fields(line), " "))
		if field != "" {
			fields = append(fields, field)
		}
	}
	return fields
}

func containsWord(text, word string) bool {
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return r < 'A' || (r > 'Z' && r < 'a') || r > 'z'
	}) {
		if strings.EqualFold(field, word) {
			return true
		}
	}
	return false
}

func geminiAppsZones(tokens []html.Token) []geminiAppsZone {
	var zones []geminiAppsZone
	depth := 0
	active := make([]struct {
		name   string
		depth  int
		tokens []html.Token
	}, 0, 2)

	for _, token := range tokens {
		switch token.Type {
		case html.StartTagToken:
			for i := range active {
				active[i].tokens = append(active[i].tokens, token)
			}
			name := ""
			if hasHTMLClass(token, "header-cell") {
				name = "header"
			} else if hasHTMLClass(token, "content-cell") {
				name = "content"
			}
			if name != "" {
				active = append(active, struct {
					name   string
					depth  int
					tokens []html.Token
				}{name: name, depth: depth + 1, tokens: []html.Token{token}})
			}
			if !isGeminiAppsVoidElement(token.Data) {
				depth++
			}
		case html.SelfClosingTagToken, html.TextToken, html.CommentToken:
			for i := range active {
				active[i].tokens = append(active[i].tokens, token)
			}
		case html.EndTagToken:
			for i := range active {
				active[i].tokens = append(active[i].tokens, token)
			}
			if len(active) > 0 && depth == active[len(active)-1].depth {
				zone := active[len(active)-1]
				zones = append(zones, geminiAppsZone{name: zone.name, tokens: zone.tokens})
				active = active[:len(active)-1]
			}
			if depth > 0 {
				depth--
			}
		}
	}
	return zones
}

func hasHTMLClass(token html.Token, wanted string) bool {
	for _, attr := range token.Attr {
		if strings.EqualFold(attr.Key, "class") {
			for className := range strings.FieldsSeq(attr.Val) {
				if strings.EqualFold(className, wanted) {
					return true
				}
			}
		}
	}
	return false
}

func parseGeminiAppsCell(tokens []html.Token) (ParseResult, error) {
	zones := geminiAppsZones(tokens)
	var headerText string
	var contentZones [][]html.Token
	for _, zone := range zones {
		switch zone.name {
		case "header":
			headerText = renderGeminiAppsTokens(zone.tokens)
		case "content":
			contentZones = append(contentZones, zone.tokens)
		}
	}
	match := geminiAppsTimestampRE.FindStringSubmatch(headerText)
	if len(match) != 2 {
		return ParseResult{}, fmt.Errorf("activity record has no supported header timestamp")
	}
	ts, err := parseGeminiAppsTimestamp(match[0], match[1])
	if err != nil {
		return ParseResult{}, err
	}

	if len(contentZones) == 0 {
		return ParseResult{}, fmt.Errorf(
			"prompted activity record has no content cell",
		)
	}
	prompt, response := geminiAppsPromptAndResponse(contentZones)
	if strings.TrimSpace(prompt) == "" {
		return ParseResult{}, fmt.Errorf("prompted activity record has no prompt")
	}

	idInput := ts.UTC().Format(time.RFC3339Nano) + "\x00" + prompt
	hash := sha256.Sum256([]byte(idInput))
	sessionID := "gemini-apps:" + hex.EncodeToString(hash[:])
	messages := []ParsedMessage{{
		Ordinal:       0,
		Role:          RoleUser,
		Content:       prompt,
		Timestamp:     ts,
		ContentLength: len(prompt),
	}}
	if strings.TrimSpace(response) != "" {
		messages = append(messages, ParsedMessage{
			Ordinal:       1,
			Role:          RoleAssistant,
			Content:       response,
			Timestamp:     ts,
			ContentLength: len(response),
		})
	}

	return ParseResult{
		Session: ParsedSession{
			ID:               sessionID,
			Project:          "gemini.google.com",
			Machine:          "local",
			Agent:            AgentGeminiApps,
			FirstMessage:     prompt,
			SessionName:      prompt,
			StartedAt:        ts,
			EndedAt:          ts,
			MessageCount:     len(messages),
			UserMessageCount: 1,
		},
		Messages: messages,
	}, nil
}

func geminiAppsPromptAndResponse(zones [][]html.Token) (string, string) {
	var rendered []string
	for _, zone := range zones {
		value := strings.TrimSpace(renderGeminiAppsTokens(zone))
		if value != "" {
			rendered = append(rendered, value)
		}
	}
	if len(rendered) == 0 {
		return "", ""
	}
	if len(rendered) > 1 {
		return rendered[0], strings.TrimSpace(strings.Join(rendered[1:], "\n\n"))
	}

	value := rendered[0]
	blocks := splitGeminiAppsBlocks(zones[0])
	if len(blocks) > 1 {
		return blocks[0], strings.TrimSpace(strings.Join(blocks[1:], "\n\n"))
	}
	return value, ""
}

func splitGeminiAppsBlocks(tokens []html.Token) []string {
	var blocks []string
	var block []html.Token
	blockDepth := 0
	flush := func() {
		if block == nil {
			return
		}
		value := strings.TrimSpace(renderGeminiAppsTokens(block))
		if !geminiAppsTimestampRE.MatchString(value) {
			blocks = append(blocks, value)
		}
		block = nil
	}
	for _, token := range tokens {
		if token.Type == html.StartTagToken {
			if blockDepth == 0 &&
				isGeminiAppsBlockTag(token.Data) &&
				!hasHTMLClass(token, "content-cell") {
				block = []html.Token{token}
				blockDepth = 1
				continue
			}
			if blockDepth > 0 {
				block = append(block, token)
				if !isGeminiAppsVoidElement(token.Data) {
					blockDepth++
				}
			}
			continue
		}
		if token.Type == html.EndTagToken {
			if blockDepth > 0 {
				block = append(block, token)
				blockDepth--
				if blockDepth == 0 {
					flush()
				}
			}
			continue
		}
		if blockDepth > 0 {
			block = append(block, token)
		}
	}
	flush()
	return blocks
}

func isGeminiAppsBlockTag(tag string) bool {
	switch strings.ToLower(tag) {
	case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "li", "blockquote", "pre", "table":
		return true
	default:
		return false
	}
}

func parseGeminiAppsTimestamp(value, zone string) (time.Time, error) {
	value = strings.NewReplacer("\u00a0", " ", "\u202f", " ").Replace(value)
	zone = strings.ToUpper(zone)
	offset, ok := geminiAppsZoneOffset(zone)
	if !ok {
		return time.Time{}, fmt.Errorf("unsupported Gemini Apps timestamp zone %q", zone)
	}
	zoneStart := strings.LastIndex(value, " ")
	if zoneStart < 0 {
		return time.Time{}, fmt.Errorf("invalid Gemini Apps timestamp %q", value)
	}
	datePart := strings.TrimSpace(value[:zoneStart])
	loc := time.FixedZone(zone, offset)
	parsed, err := time.ParseInLocation("Jan 2, 2006, 3:04:05 PM", datePart, loc)
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
		parts := strings.Split(strings.TrimPrefix(zone, "GMT"), ":")
		if len(parts) != 2 {
			return 0, false
		}
		hourPart := parts[0]
		sign := 1
		if strings.HasPrefix(hourPart, "-") {
			sign = -1
		}
		hourDigits := strings.TrimPrefix(hourPart, "+")
		hourDigits = strings.TrimPrefix(hourDigits, "-")
		hours, errHour := strconv.Atoi(hourDigits)
		minutes, errMinute := strconv.Atoi(parts[1])
		if errHour != nil || errMinute != nil || minutes < 0 || minutes > 59 {
			return 0, false
		}
		if hours > 23 {
			return 0, false
		}
		return sign * (hours*60*60 + minutes*60), true
	}
	return 0, false
}

func renderGeminiAppsTokens(tokens []html.Token) string {
	var out strings.Builder
	skipDepth := 0
	for _, token := range tokens {
		switch token.Type {
		case html.StartTagToken:
			if isGeminiAppsIgnoredTag(token.Data) {
				skipDepth = 1
				continue
			}
			if skipDepth > 0 {
				skipDepth++
				continue
			}
			renderGeminiAppsStart(&out, token.Data)
		case html.EndTagToken:
			if skipDepth > 0 {
				skipDepth--
				continue
			}
			renderGeminiAppsEnd(&out, token.Data)
		case html.SelfClosingTagToken:
			if skipDepth == 0 {
				renderGeminiAppsStart(&out, token.Data)
			}
		case html.TextToken:
			if skipDepth == 0 {
				out.WriteString(sanitizeGeminiAppsText(string(token.Data)))
			}
		}
	}
	return cleanGeminiAppsText(out.String())
}

func isGeminiAppsIgnoredTag(tag string) bool {
	switch strings.ToLower(tag) {
	case "script", "style", "template", "noscript":
		return true
	default:
		return false
	}
}

func renderGeminiAppsStart(out *strings.Builder, tag string) {
	switch strings.ToLower(tag) {
	case "br":
		out.WriteByte('\n')
	case "li":
		out.WriteString("\n- ")
	case "td", "th":
		out.WriteString(" | ")
	case "strong", "b":
		out.WriteString("**")
	case "em", "i":
		out.WriteByte('*')
	case "code":
		out.WriteByte('`')
	case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "pre", "tr", "table":
		out.WriteByte('\n')
	}
}

func renderGeminiAppsEnd(out *strings.Builder, tag string) {
	switch strings.ToLower(tag) {
	case "strong", "b":
		out.WriteString("**")
	case "em", "i":
		out.WriteByte('*')
	case "code":
		out.WriteByte('`')
	case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "pre", "tr", "table":
		out.WriteByte('\n')
	}
}

func sanitizeGeminiAppsText(value string) string {
	var out strings.Builder
	for _, r := range value {
		if r == '\x00' || r == '\x1b' || (r < 0x20 && r != '\n' && r != '\r' && r != '\t') ||
			r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

func cleanGeminiAppsText(value string) string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	var out []string
	for _, line := range lines {
		if line == "" {
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			continue
		}
		out = append(out, strings.Join(strings.Fields(line), " "))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
