package commitlinks

import (
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/secrets"
	"go.kenn.io/agentsview/internal/stringutil"
)

// TurnEntry contains only the stored prompt/tool data needed by the heuristic.
// The caller excludes system and synthetic prompts before populating Prompt.
type TurnEntry struct {
	Ordinal int
	At      string
	Prompt  string
	Calls   []string
}

// Turn is context, not proof that the session authored every changed file.
type Turn struct {
	Ordinal         int
	At, Prompt, Why string
}

// SelectTurn uses the last matching tool input before commit+5m, then its user
// prompt. It falls back to the last eligible prompt, or a first-prompt match.
// The heuristic is adapted from Pando worker/src/understand/lookups.ts,
// turnThatWrote (Apache 2.0), with bounded input and stricter timestamp handling.
func SelectTurn(entries []TurnEntry, at string, paths []string) *Turn {
	committed, valid := parseTurnTime(at)
	cutoff := committed.Add(5 * time.Minute)
	var first, before, wrote *Turn
	tails := make([]string, 0, len(paths))
	for _, file := range paths {
		file = strings.ReplaceAll(file, `\`, "/")
		clean := strings.TrimPrefix(path.Clean(file), "./")
		parts := strings.Split(clean, "/")
		if len(parts) > 2 {
			parts = parts[len(parts)-2:]
		}
		tail := strings.Join(parts, "/")
		if tail != "" && tail != "." {
			tails = append(tails, tail)
		}
	}
	for _, entry := range entries {
		when, known := parseTurnTime(entry.At)
		eligible := valid && known && !when.After(cutoff)
		if entry.Prompt != "" {
			prompt := &Turn{Ordinal: entry.Ordinal, At: entry.At, Prompt: stringutil.TruncateRunes(secrets.Redact(entry.Prompt), 600, ""), Why: "before"}
			if first == nil {
				copy := *prompt
				copy.Why = "match"
				first = &copy
			}
			if eligible {
				before = prompt
			}
		}
		if !eligible || before == nil {
			continue
		}
		for _, input := range entry.Calls {
			if len(input) > MaxCommandBytes {
				continue
			}
			input = strings.ReplaceAll(input, `\\`, "/")
			for _, tail := range tails {
				if namesPath(input, tail) {
					copy := *before
					copy.Why = "wrote"
					wrote = &copy
					break
				}
			}
		}
	}
	if wrote != nil {
		return wrote
	}
	if before != nil {
		return before
	}
	return first
}

func parseTurnTime(value string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, value)
	return t, err == nil
}

func namesPath(input, tail string) bool {
	for start := 0; start <= len(input); {
		pos := strings.Index(input[start:], tail)
		if pos < 0 {
			return false
		}
		pos += start
		end := pos + len(tail)
		left, right := true, true
		if pos > 0 {
			r, _ := utf8.DecodeLastRuneInString(input[:pos])
			left = !pathComponentRune(r)
		}
		if end < len(input) {
			r, _ := utf8.DecodeRuneInString(input[end:])
			right = !pathComponentRune(r) && r != '/'
		}
		if left && right {
			return true
		}
		start = pos + 1
	}
	return false
}

func pathComponentRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.'
}
