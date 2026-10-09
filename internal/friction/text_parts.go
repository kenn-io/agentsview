package friction

import (
	"slices"
	"strings"
)

// textParts borrows the remaining pieces of an assistant message. Removing a
// rendering splits pieces instead of copying the rest of the message each time.
type textParts []textPart

type textPart struct {
	text    string
	bracket int
}

func newTextPart(text string) textPart {
	return textPart{text: text, bracket: strings.IndexByte(text, '[')}
}

func (parts textParts) index(needle string, from int) int {
	base := 0
	for i, part := range parts {
		start := max(0, from-base)
		if needle[0] == '[' {
			// Every current tool renderer starts with '['. Retained prose
			// without one cannot match, even across a removed block.
			if part.bracket < 0 {
				base += len(part.text)
				continue
			}
			start = max(start, part.bracket)
		}
		if start < len(part.text) {
			if at := strings.Index(part.text[start:], needle); at >= 0 {
				return base + start + at
			}
			// A match can also cross a boundary left by an earlier removal.
			// Only the final len(needle)-1 bytes can start such a match.
			for at := max(start, len(part.text)-len(needle)+1); at < len(part.text); {
				next := strings.IndexByte(part.text[at:], needle[0])
				if next < 0 {
					break
				}
				at += next
				if parts.hasPrefixAt(i, at, needle) {
					return base + at
				}
				at++
			}
		}
		base += len(part.text)
	}
	return -1
}

func (parts textParts) locate(at int) (int, int) {
	for i, part := range parts {
		if at < len(part.text) || i == len(parts)-1 {
			return i, at
		}
		at -= len(part.text)
	}
	return 0, 0
}

func (parts textParts) hasPrefixAt(i, at int, needle string) bool {
	for ; i < len(parts); i++ {
		text := parts[i].text[at:]
		n := min(len(text), len(needle))
		if text[:n] != needle[:n] {
			return false
		}
		needle = needle[n:]
		if needle == "" {
			return true
		}
		at = 0
	}
	return false
}

func (parts textParts) hasPrefix(at int, needle string) bool {
	i, offset := parts.locate(at)
	return parts.hasPrefixAt(i, offset, needle)
}

// remove retains removePart's preference for the preceding newline, including
// when that separator belongs to a different piece.
func (parts textParts) remove(at, n int) textParts {
	end := at + n
	switch {
	case at > 0 && parts.hasPrefix(at-1, "\n"):
		at--
	case parts.hasPrefix(end, "\n"):
		end++
	}
	i, start := parts.locate(at)
	j, finish := parts.locate(end)
	var kept [2]textPart
	count := 0
	if start > 0 {
		kept[count] = parts[i]
		kept[count].text = kept[count].text[:start]
		if kept[count].bracket >= start {
			kept[count].bracket = -1
		}
		count++
	}
	if finish < len(parts[j].text) {
		kept[count] = newTextPart(parts[j].text[finish:])
		count++
	}
	return slices.Replace(parts, i, j+1, kept[:count]...)
}

func (parts textParts) String() string {
	if len(parts) == 1 {
		return parts[0].text
	}
	n := 0
	for _, part := range parts {
		n += len(part.text)
	}
	var out strings.Builder
	out.Grow(n)
	for _, part := range parts {
		out.WriteString(part.text)
	}
	return out.String()
}
