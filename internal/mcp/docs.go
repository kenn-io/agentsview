// ABOUTME: search_docs: keyword search over the public AgentsView guides
// ABOUTME: embedded in the binary, returning sections with site links.
package mcp

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	productdocs "go.kenn.io/agentsview/docs"
	"go.kenn.io/agentsview/internal/stringutil"
)

const (
	defaultDocsLimit    = 5
	maxDocsLimit        = 10
	defaultDocsMaxChars = 2000
	maxDocsMaxChars     = 8000
	maxDocsQueryLength  = 256
	docsSiteURL         = "https://agentsview.io/docs/"
)

type searchDocsIn struct {
	Query    string `json:"query" jsonschema:"Words to find in the AgentsView guides. Every word must appear in a section."`
	Topic    string `json:"topic,omitempty" jsonschema:"Only search one guide, such as mcp, recall, semantic-search, or token-usage."`
	Limit    int    `json:"limit,omitempty" jsonschema:"Max sections, default 5, max 10."`
	MaxChars int    `json:"max_chars,omitempty" jsonschema:"Truncate each section to this many characters, default 2000, max 8000."`
}

type docHit struct {
	Topic     string `json:"topic"`
	Title     string `json:"title"`
	Section   string `json:"section"`
	URL       string `json:"url" jsonschema:"Published page for this guide; use it when linking."`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

type searchDocsOut struct {
	Results []docHit `json:"results"`
}

// docSection is one heading-delimited block of a guide. The lower-cased
// copies are built once so each search only scans.
type docSection struct {
	topic, title, heading, body string
	lowerTitle, lowerHeading    string
	lowerAll                    string
}

var docSections = mustLoadDocSections(productdocs.Public)

func mustLoadDocSections(fsys fs.FS) []docSection {
	sections, err := loadDocSections(fsys)
	if err != nil {
		panic(err)
	}
	return sections
}

func loadDocSections(fsys fs.FS) ([]docSection, error) {
	names, err := fs.Glob(fsys, "*.md")
	if err != nil {
		return nil, err
	}
	var sections []docSection
	for _, name := range names {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		topic := strings.TrimSuffix(name, ".md")
		sections = append(sections, splitDocSections(topic, string(data))...)
	}
	return sections, nil
}

// splitDocSections reads the front matter title and splits the body at
// Markdown headings outside code fences.
func splitDocSections(topic, text string) []docSection {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	title := topic
	if rest, ok := strings.CutPrefix(text, "---\n"); ok {
		if front, body, found := strings.Cut(rest, "\n---\n"); found {
			for line := range strings.SplitSeq(front, "\n") {
				if value, ok := strings.CutPrefix(line, "title:"); ok {
					title = strings.Trim(strings.TrimSpace(value), `"'`)
				}
			}
			text = body
		}
	}
	var sections []docSection
	heading := title
	var lines []string
	flush := func() {
		body := strings.TrimSpace(strings.Join(lines, "\n"))
		lines = nil
		if body == "" {
			return
		}
		sections = append(sections, docSection{
			topic: topic, title: title, heading: heading, body: body,
			lowerTitle:   strings.ToLower(title),
			lowerHeading: strings.ToLower(heading),
			lowerAll:     strings.ToLower(title + "\n" + heading + "\n" + body),
		})
	}
	fenced := false
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			fenced = !fenced
		}
		if level := len(line) - len(strings.TrimLeft(line, "#")); !fenced &&
			level >= 1 && level <= 6 && strings.HasPrefix(line[level:], " ") {
			flush()
			heading = strings.TrimSpace(line[level:])
			continue
		}
		lines = append(lines, line)
	}
	flush()
	return sections
}

func docsURL(topic string) string {
	if topic == "index" {
		return docsSiteURL
	}
	return docsSiteURL + topic + "/"
}

func findDocs(sections []docSection, in searchDocsIn) ([]docHit, error) {
	terms := strings.Fields(strings.ToLower(in.Query))
	if len(terms) == 0 || utf8.RuneCountInString(in.Query) > maxDocsQueryLength {
		return nil, fmt.Errorf("query must contain 1 to %d characters", maxDocsQueryLength)
	}
	limit := clampLimit(in.Limit, defaultDocsLimit, maxDocsLimit)
	maxChars := clampLimit(in.MaxChars, defaultDocsMaxChars, maxDocsMaxChars)
	type ranked struct {
		section *docSection
		score   int
	}
	var matches []ranked
	topicFound := in.Topic == ""
	for i := range sections {
		s := &sections[i]
		if in.Topic != "" && s.topic != in.Topic {
			continue
		}
		topicFound = true
		score := 0
		for _, term := range terms {
			if !strings.Contains(s.lowerAll, term) {
				score = 0
				break
			}
			// Title and heading hits outrank body-only hits.
			score++
			if strings.Contains(s.lowerTitle, term) {
				score += 20
			}
			if strings.Contains(s.lowerHeading, term) {
				score += 10
			}
		}
		if score > 0 {
			matches = append(matches, ranked{s, score})
		}
	}
	if !topicFound {
		return nil, fmt.Errorf("unknown documentation topic %q", in.Topic)
	}
	slices.SortStableFunc(matches, func(a, b ranked) int { return cmp.Compare(b.score, a.score) })
	results := make([]docHit, 0, min(limit, len(matches)))
	for _, m := range matches[:min(limit, len(matches))] {
		text, cut := m.section.body, false
		if utf8.RuneCountInString(text) > maxChars {
			suffix := "..."
			if maxChars <= len(suffix) {
				suffix = ""
			}
			text, cut = stringutil.TruncateRunes(text, maxChars-len(suffix), suffix), true
		}
		results = append(results, docHit{
			Topic: m.section.topic, Title: m.section.title, Section: m.section.heading,
			URL: docsURL(m.section.topic), Text: text, Truncated: cut,
		})
	}
	return results, nil
}

func searchDocs(
	_ context.Context, _ *mcp.CallToolRequest, in searchDocsIn,
) (*mcp.CallToolResult, searchDocsOut, error) {
	results, err := findDocs(docSections, in)
	if err != nil {
		return nil, searchDocsOut{}, err
	}
	return nil, searchDocsOut{Results: results}, nil
}
