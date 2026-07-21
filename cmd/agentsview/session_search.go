// ABOUTME: `session search` subcommand — substring/regex/fts content
// ABOUTME: search across messages and tool I/O with redacted snippets.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
	"golang.org/x/term"
)

func newSessionSearchCommand() *cobra.Command {
	var (
		useRegex, useFTS, useSemantic, useHybrid bool
		in, scope                                string
		excludeSystem, reveal                    bool
		project, excludeProject, agent           string
		machine, date, dateFrom, dateTo          string
		activeSince, since                       string
		includeChildren, includeAutomated        bool
		includeOneShot                           bool
		limit, cursor, contextN                  int
	)
	cmd := &cobra.Command{
		Use:          "search <pattern>",
		Short:        "Search message and tool content across sessions",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var sources []string
			for s := range strings.SplitSeq(in, ",") {
				if s = strings.TrimSpace(s); s != "" {
					sources = append(sources, s)
				}
			}
			mode, err := resolveContentSearchMode(
				useRegex, useFTS, useSemantic, useHybrid, sources)
			if err != nil {
				return err
			}
			if err := validateScopeFlag(scope, useSemantic, useHybrid); err != nil {
				return err
			}
			activeSince, err = resolveSinceFlag(since, activeSince)
			if err != nil {
				return err
			}
			svc, cleanup, err := resolveService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			res, err := svc.SearchContent(cmd.Context(), service.ContentSearchRequest{
				Pattern:          args[0],
				Mode:             mode,
				Sources:          sources,
				ExcludeSystem:    excludeSystem,
				Reveal:           reveal,
				Project:          project,
				ExcludeProject:   excludeProject,
				Machine:          machine,
				Agent:            agent,
				Date:             date,
				DateFrom:         dateFrom,
				DateTo:           dateTo,
				ActiveSince:      activeSince,
				IncludeChildren:  includeChildren,
				IncludeAutomated: includeAutomated,
				IncludeOneShot:   includeOneShot,
				Scope:            scope,
				Limit:            limit,
				Cursor:           cursor,
				Context:          contextN,
			})
			if err != nil {
				return err
			}
			if reveal {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"WARNING: --reveal prints full secret values; "+
						"this terminal/session may itself be recorded.")
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
			}
			return printContentSearchResult(cmd.OutOrStdout(), res, contextN)
		},
	}
	flags := cmd.Flags()
	flags.BoolVar(&useRegex, "regex", false, "Treat pattern as an RE2 regex")
	flags.BoolVar(&useFTS, "fts", false, "Fast tokenized FTS over messages only")
	flags.BoolVar(&useSemantic, "semantic", false,
		"Semantic (vector) search over user/assistant messages")
	flags.BoolVar(&useHybrid, "hybrid", false,
		"Hybrid semantic + full-text search (reciprocal rank fusion)")
	flags.StringVar(&in, "in", "",
		"Comma-separated sources: messages,tool_input,tool_result (default all)")
	flags.BoolVar(&excludeSystem, "exclude-system", false,
		"Exclude system messages (included by default)")
	flags.BoolVar(&reveal, "reveal", false, "Show full secret values (unredacted)")
	flags.StringVar(&project, "project", "", "Filter by project name")
	flags.StringVar(&excludeProject, "exclude-project", "", "Exclude project")
	flags.StringVar(&machine, "machine", "", "Filter by machine")
	flags.StringVar(&agent, "agent", "", "Filter by agent")
	flags.StringVar(&date, "date", "", "Sessions active on YYYY-MM-DD")
	flags.StringVar(&dateFrom, "date-from", "", "Sessions active on or after YYYY-MM-DD")
	flags.StringVar(&dateTo, "date-to", "", "Sessions active on or before YYYY-MM-DD")
	flags.StringVar(&activeSince, "active-since", "", "Active since RFC3339 timestamp")
	flags.StringVar(&since, "since", "",
		"Only sessions active since a relative duration (12h, 14d, 2w, 3m = 3 months, 1y) or YYYY-MM-DD")
	flags.StringVar(&scope, "scope", "",
		"Semantic/hybrid result scope: top, all, or subordinate (default all)")
	flags.BoolVar(&includeChildren, "include-children", false, "Include subagent sessions")
	flags.BoolVar(&includeAutomated, "include-automated", false, "Include automated sessions")
	flags.BoolVar(&includeOneShot, "include-one-shot", false, "Include one-shot sessions")
	flags.IntVar(&limit, "limit", 0, "Max results (default 50, max 500)")
	flags.IntVar(&cursor, "cursor", 0, "Pagination cursor from a previous response")
	flags.IntVar(&contextN, "context", 0,
		"Include N messages of context before and after each match (max 10)")
	return cmd
}

// validateScopeFlag gates --scope at the CLI boundary: it is only
// meaningful for --semantic/--hybrid and must name a known scope.
func validateScopeFlag(scope string, useSemantic, useHybrid bool) error {
	if scope == "" {
		return nil
	}
	if !useSemantic && !useHybrid {
		return fmt.Errorf("--scope requires --semantic or --hybrid")
	}
	switch scope {
	case "top", "all", "subordinate":
		return nil
	}
	return fmt.Errorf("--scope must be top, all, or subordinate (got %q)", scope)
}

// resolveContentSearchMode picks the search mode from the mutually exclusive
// --regex/--fts/--semantic/--hybrid flags and, for the modes that only search
// message content ("fts", "semantic", "hybrid"), rejects an explicit --in
// naming other sources.
func resolveContentSearchMode(
	useRegex, useFTS, useSemantic, useHybrid bool, sources []string,
) (string, error) {
	modes := 0
	for _, b := range []bool{useRegex, useFTS, useSemantic, useHybrid} {
		if b {
			modes++
		}
	}
	if modes > 1 {
		return "", fmt.Errorf(
			"--regex, --fts, --semantic and --hybrid are mutually exclusive")
	}
	mode := "substring"
	switch {
	case useRegex:
		mode = "regex"
	case useFTS:
		mode = "fts"
	case useSemantic:
		mode = "semantic"
	case useHybrid:
		mode = "hybrid"
	}
	if useFTS {
		for _, s := range sources {
			if s != "messages" {
				return "", fmt.Errorf(
					"--fts searches messages only; drop --in or --fts")
			}
		}
	}
	if useSemantic || useHybrid {
		for _, s := range sources {
			if s != "messages" {
				return "", fmt.Errorf(
					"--%s searches messages only; drop --in", mode)
			}
		}
	}
	return mode, nil
}

// printContentSearchResult renders a content search result for humans.
// Flat results (no --context) render as an aligned table sized to the
// terminal; --context requests keep the record-style output because
// per-match context lines cannot live inside table rows. The clock is
// captured once here and threaded into the row loop so tests can inject a
// fixed time.
func printContentSearchResult(
	w io.Writer, res *service.ContentSearchResult, contextN int,
) error {
	now := time.Now()
	if contextN > 0 {
		return printContentMatchesHuman(w, res, now)
	}
	return printContentMatchesTable(w, res, contentTerminalWidth(w), now)
}

// humanizeMatchAge renders a content match's timestamp for the search AGE
// column. It parses the RFC3339/RFC3339Nano string via parseSessionTime
// (returning emDash when empty or unparseable), uses the shared relative
// buckets under a week, and beyond a week formats an absolute date that
// disambiguates the year: "Jan 02" when the timestamp falls in now's year,
// "Jan 2006" (e.g. "Jan 2025") for prior years. Search spans the whole
// multi-year archive, so the year matters here even though session list omits
// it.
func humanizeMatchAge(ts string, now time.Time) string {
	t, ok := parseSessionTime(ts)
	if !ok {
		return emDash
	}
	if rel, ok := humanizeAgeRelative(t, now); ok {
		return rel
	}
	if t.Year() == now.Year() {
		return t.Format("Jan 02")
	}
	return t.Format("Jan 2006")
}

// contentTerminalWidth reports the terminal width of w, or 0 when w is not
// an interactive terminal (pipes, files, tests). Follows the flagHelpWidth
// pattern: 0 tells the table renderer to print snippets untruncated.
func contentTerminalWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return 0
	}
	width, _, err := term.GetSize(fd)
	if err != nil || width <= 0 {
		return 0
	}
	return width
}

// contentLocationMaxWidth caps the LOCATION column so a huge tool name
// cannot starve the snippet column.
const contentLocationMaxWidth = 48

// contentProjectMaxWidth caps the PROJECT column so an oversized project
// name cannot starve the snippet column.
const contentProjectMaxWidth = 32

// truncCell collapses whitespace and truncates s to at most max terminal
// display cells, ellipsizing when it cut anything. A max of 0 disables
// truncation (non-TTY output keeps full values). The display-cell
// counterpart of truncName for the search table, where full-width runes
// must not break column alignment.
func truncCell(s string, max int) string {
	s = collapseWhitespace(s)
	if max <= 0 || runewidth.StringWidth(s) <= max {
		return s
	}
	return runewidth.Truncate(s, max, "…")
}

// contentSnippetMinWidth keeps the snippet readable on narrow terminals;
// below this the row is allowed to wrap instead of the snippet vanishing.
const contentSnippetMinWidth = 40

// contentColumnGap is the number of spaces between table columns.
const contentColumnGap = 2

// contentSnippetBudget returns the rune budget for the trailing SNIPPET
// column: the terminal width minus the earlier columns and their gaps,
// floored at contentSnippetMinWidth. A termWidth of 0 (unknown terminal)
// returns 0, meaning untruncated.
func contentSnippetBudget(termWidth int, otherWidths []int) int {
	if termWidth <= 0 {
		return 0
	}
	remaining := termWidth
	for _, w := range otherWidths {
		remaining -= w + contentColumnGap
	}
	if remaining < contentSnippetMinWidth {
		return contentSnippetMinWidth
	}
	return remaining
}

// printContentMatchesTable writes one aligned row per match under a header
// line: ID, MATCH (ordinal/range plus "sub" marker), SCORE (only when any
// match is scored), PROJECT (capped), LOCATION (capped), and SNIPPET. The snippet
// expands to fill the remaining terminal width when termWidth is known and
// prints untruncated when it is 0. Every cell is terminal-sanitized.
func printContentMatchesTable(
	w io.Writer, res *service.ContentSearchResult, termWidth int, now time.Time,
) error {
	if len(res.Matches) == 0 {
		fmt.Fprintln(w, "(no matches)")
		return nil
	}
	hasScore := false
	for _, m := range res.Matches {
		if m.Score != nil {
			hasScore = true
			break
		}
	}
	headers := []string{"ID", "MATCH", "AGE"}
	if hasScore {
		headers = append(headers, "SCORE")
	}
	headers = append(headers, "PROJECT", "LOCATION", "SNIPPET")

	// Column caps only apply when a real terminal width constrains the
	// row; piped output keeps full values, like the untruncated snippet.
	projectCap, locationCap := 0, 0
	if termWidth > 0 {
		projectCap = contentProjectMaxWidth
		locationCap = contentLocationMaxWidth
	}

	rows := make([][]string, 0, len(res.Matches))
	for _, m := range res.Matches {
		loc := m.Location
		if m.ToolName != "" {
			loc += ":" + m.ToolName
		}
		match := formatMatchOrdinal(m)
		if m.Subordinate {
			match += " sub"
		}
		cells := []string{
			sanitizeTerminal(m.SessionID), match, humanizeMatchAge(m.Timestamp, now),
		}
		if hasScore {
			score := emDash
			if m.Score != nil {
				score = fmt.Sprintf("%.2f", *m.Score)
			}
			cells = append(cells, score)
		}
		cells = append(cells,
			truncCell(sanitizeTerminal(orEmDash(m.Project)), projectCap),
			truncCell(sanitizeTerminal(loc), locationCap),
			sanitizeTerminal(collapseWhitespace(m.Snippet)),
		)
		rows = append(rows, cells)
	}

	// Size every column but the trailing snippet from the data, then give
	// the snippet whatever terminal width remains. All sizing, padding,
	// and truncation use terminal display cells (runewidth), not rune
	// counts, so full-width CJK runes and emoji stay aligned.
	widths := make([]int, len(headers)-1)
	for i := range widths {
		widths[i] = runewidth.StringWidth(headers[i])
		for _, cells := range rows {
			if n := runewidth.StringWidth(cells[i]); n > widths[i] {
				widths[i] = n
			}
		}
	}
	budget := contentSnippetBudget(termWidth, widths)
	gap := strings.Repeat(" ", contentColumnGap)

	printRow := func(cells []string) {
		var b strings.Builder
		for i, w := range widths {
			b.WriteString(cells[i])
			b.WriteString(strings.Repeat(" ",
				w-runewidth.StringWidth(cells[i])))
			b.WriteString(gap)
		}
		snippet := cells[len(cells)-1]
		// Truncate only when the snippet actually overflows the budget;
		// an exact fit prints unmodified.
		if budget > 0 && runewidth.StringWidth(snippet) > budget {
			snippet = runewidth.Truncate(snippet, budget, "…")
		}
		b.WriteString(snippet)
		fmt.Fprintln(w, b.String())
	}
	printRow(headers)
	for _, cells := range rows {
		printRow(cells)
	}
	if res.NextCursor != 0 {
		fmt.Fprintf(w, "\nMore results: --cursor %d\n", res.NextCursor)
	}
	return nil
}

// printContentMatchesHuman writes one line per match, terminal-sanitized.
// Scored matches (semantic/hybrid modes) show "score=0.83" after the
// ordinal; unscored matches (substring/regex/fts) omit it. A match spanning
// a multi-message unit renders "#<start>-<end> @<anchor>" instead of the
// plain "#<ordinal>", and a subordinate unit gains a "sub" marker. When
// --context requested inline context, ContextBefore/ContextAfter print as
// indented "role: content" lines around the match line.
func printContentMatchesHuman(w io.Writer, res *service.ContentSearchResult, now time.Time) error {
	if len(res.Matches) == 0 {
		fmt.Fprintln(w, "(no matches)")
		return nil
	}
	for _, m := range res.Matches {
		loc := m.Location
		if m.ToolName != "" {
			loc = m.Location + ":" + m.ToolName
		}
		for _, cm := range m.ContextBefore {
			printContentContextLine(w, cm)
		}
		fmt.Fprintf(w, "%s  %s", sanitizeTerminal(m.SessionID), formatMatchOrdinal(m))
		if m.Subordinate {
			fmt.Fprint(w, " sub")
		}
		if m.Score != nil {
			fmt.Fprintf(w, " score=%.2f", *m.Score)
		}
		fmt.Fprintf(w, "  %s  %s  %s\n",
			humanizeMatchAge(m.Timestamp, now),
			sanitizeTerminal(m.Project), sanitizeTerminal(loc))
		fmt.Fprintf(w, "    %s\n",
			sanitizeTerminal(strings.ReplaceAll(m.Snippet, "\n", " ")))
		for _, cm := range m.ContextAfter {
			printContentContextLine(w, cm)
		}
	}
	if res.NextCursor != 0 {
		fmt.Fprintf(w, "\nMore results: --cursor %d\n", res.NextCursor)
	}
	return nil
}

// formatMatchOrdinal renders a match's position. A match whose unit is a
// single message keeps the plain "#<ordinal>" form; a match whose
// conversation unit spans multiple messages — possible in every mode now
// that lexical rows carry derived unit ranges — renders the range with the
// anchor marked, e.g. "#12-40 @19".
func formatMatchOrdinal(m db.ContentMatch) string {
	if m.OrdinalRange[1] > m.OrdinalRange[0] {
		return fmt.Sprintf("#%d-%d @%d", m.OrdinalRange[0], m.OrdinalRange[1], m.Ordinal)
	}
	return fmt.Sprintf("#%d", m.Ordinal)
}

// contentContextLineMaxChars caps a printed context line's length so a long
// stored message cannot blow out the human-format search output.
const contentContextLineMaxChars = 200

// printContentContextLine writes one --context line: two-space indent,
// "role: " prefix, terminal-sanitized and truncated to
// contentContextLineMaxChars runes (with an ellipsis marker when cut).
func printContentContextLine(w io.Writer, m db.Message) {
	content := strings.ReplaceAll(m.Content, "\n", " ")
	if truncated, cut := truncateRunes(content, contentContextLineMaxChars); cut {
		content = truncated + "…"
	} else {
		content = truncated
	}
	fmt.Fprintf(w, "  %s: %s\n",
		sanitizeTerminal(m.Role), sanitizeTerminal(content))
}
