package why

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"strings"
	"unicode"

	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/stringutil"
)

// TerminalText removes control and formatting characters from a display field.
func TerminalText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "\uFFFD"))
}

// WriteHuman displays canonical cards once, so prompts never migrate to another commit.
func WriteHuman(w io.Writer, r *Result) error {
	var out strings.Builder
	shown := map[int]bool{}
	for _, c := range r.commits {
		fmt.Fprintf(&out, "%s  %s  %s  %s", shortSHA(c.SHA), TerminalText(c.At), TerminalText(c.Author), TerminalText(c.Subject))
		if r.Question.Path != "" {
			fmt.Fprintf(&out, " (%d selected lines)", c.Covered)
		}
		out.WriteByte('\n')
		for _, index := range r.associations[c.SHA] {
			ref := r.Refs[index]
			if shown[index] {
				fmt.Fprintf(&out, "  Session %s: see %s", TerminalText(strings.TrimPrefix(ref.Key, "session:")), ref.ID)
				if r.ambiguousAssociations[c.SHA][index] {
					out.WriteString(" [ambiguous evidence]")
				}
				out.WriteString(" (context shown with its first linked commit)\n")
				continue
			}
			shown[index] = true
			card := ref.Card.(service.CommitSessionLink)
			fmt.Fprintf(&out, "  %s Session %s", ref.ID, TerminalText(card.SessionID))
			if card.Ambiguous || r.ambiguousAssociations[c.SHA][index] {
				out.WriteString(" [ambiguous evidence]")
			}
			if card.Gone || card.Session == nil {
				out.WriteString(" [gone or unavailable]\n")
				continue
			}
			fmt.Fprintf(&out, "  %s  %s  %s\n", TerminalText(card.Agent), TerminalText(card.Project), TerminalText(card.SessionStartedAt))
			if card.WebURL != "" {
				fmt.Fprintf(&out, "    %s\n", TerminalText(card.WebURL))
			}
			if card.Turn == nil {
				out.WriteString("    No recorded user prompt is available.\n")
				continue
			}
			why := card.Turn.Why
			if why == "commit-call" {
				why = "recorded commit call"
			} else {
				why = "commit-level heuristic: " + why
			}
			fmt.Fprintf(&out, "    Associated context for %s (%s, prompt %d): %s\n", shortSHA(card.CommitSHA), TerminalText(why), card.Turn.Ordinal, TerminalText(card.Turn.At))
			fmt.Fprintf(&out, "    %s\n", stringutil.TruncateRunes(TerminalText(card.Turn.Prompt), 600, ""))
		}
	}
	for _, ref := range r.Refs {
		if ref.How == "named" {
			fmt.Fprintf(&out, "%s %s (%s)\n", ref.ID, TerminalText(ref.Key), TerminalText(ref.FoundBy))
		}
	}
	dirty := 0
	for _, line := range r.Lines {
		if line.SHA == nil {
			dirty++
		}
	}
	if dirty > 0 {
		fmt.Fprintf(&out, "%d selected lines are not committed yet.\n", dirty)
	}
	if r.Older > 0 {
		fmt.Fprintf(&out, "%d committed lines excluded by the six-commit lookup limit.\n", r.Older)
	}
	if !r.Explained {
		if r.Question.Commit != "" {
			out.WriteString("No record explains this commit.\n")
		} else {
			out.WriteString("No record explains these lines.\n")
		}
		out.WriteString("No available, unambiguous session record was found within these limits.\n")
	}
	out.WriteString("How this was found:\n")
	for _, step := range r.Steps {
		fmt.Fprintf(&out, "  %s: %s; found %d (%d ms)\n", step.Tool, TerminalText(step.Asked), step.Found, step.MS)
	}
	_, err := io.WriteString(w, out.String())
	return err
}
func shortSHA(sha string) string {
	return stringutil.TruncateRunes(TerminalText(sha), 8, "")
}

// WriteJSON serializes before writing, so encoding errors cannot publish partial success.
func WriteJSON(w io.Writer, r *Result) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	_, err = w.Write(raw)
	return err
}
