package friction

import (
	"fmt"
	"path"
	"strings"
)

const seatPlaceholder = "{seat}"

// SeatPattern is one compiled [friction] seat_patterns entry: a
// "/"-separated glob with exactly one whole-segment {seat} capture.
type SeatPattern struct {
	raw      string
	anchored bool
	segs     []string
	seatAt   int
}

// CompileSeatPatterns validates and compiles user seat patterns. The
// error names the offending entry; config validation adds the
// [friction] seat_patterns prefix.
func CompileSeatPatterns(patterns []string) ([]SeatPattern, error) {
	var out []SeatPattern
	for _, raw := range patterns {
		p, err := compileSeatPattern(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func compileSeatPattern(raw string) (SeatPattern, error) {
	if strings.Count(raw, seatPlaceholder) != 1 {
		return SeatPattern{}, fmt.Errorf(
			"seat pattern %q must contain exactly one %s", raw, seatPlaceholder)
	}
	p := SeatPattern{
		raw:      raw,
		anchored: strings.HasPrefix(raw, "/"),
		segs:     strings.FieldsFunc(raw, func(r rune) bool { return r == '/' }),
		seatAt:   -1,
	}
	for i, seg := range p.segs {
		if seg == seatPlaceholder {
			p.seatAt = i
			continue
		}
		if strings.Contains(seg, seatPlaceholder) {
			return SeatPattern{}, fmt.Errorf(
				"seat pattern %q: %s must be a whole path segment",
				raw, seatPlaceholder)
		}
		if _, err := path.Match(seg, ""); err != nil {
			return SeatPattern{}, fmt.Errorf("seat pattern %q: %w", raw, err)
		}
	}
	return p, nil
}

// SeatFromPath returns the {seat} capture of the first pattern that
// matches filePath, or "" when none does. A seat is a display
// dimension only and never selects a detector (spec §11.3).
func SeatFromPath(filePath string, patterns []SeatPattern) string {
	segs := strings.FieldsFunc(filePath, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	for _, p := range patterns {
		if seat, ok := p.match(segs); ok {
			return seat
		}
	}
	return ""
}

func (p SeatPattern) match(segs []string) (string, bool) {
	last := len(segs) - len(p.segs)
	if p.anchored && last > 0 {
		last = 0
	}
	for start := 0; start <= last; start++ {
		if seat, ok := p.matchAt(segs[start : start+len(p.segs)]); ok {
			return seat, true
		}
	}
	return "", false
}

func (p SeatPattern) matchAt(window []string) (string, bool) {
	for i, pat := range p.segs {
		if i == p.seatAt {
			continue
		}
		if ok, _ := path.Match(pat, window[i]); !ok {
			return "", false
		}
	}
	return window[p.seatAt], true
}
