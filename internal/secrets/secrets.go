// Package secrets detects secret-shaped strings (API keys, tokens,
// private keys, credentials) in arbitrary text. It is pure: no DB,
// no IO. It backs both search-snippet redaction and persisted
// secret findings, and knows about neither.
package secrets

import (
	"sort"
	"strings"
)

// Confidence levels. Definite rules are well-anchored vendor formats;
// candidate rules are FP-prone heuristics that are recorded but do not,
// on their own, mark a session as leaking.
const (
	ConfidenceDefinite  = "definite"
	ConfidenceCandidate = "candidate"
)

// Match is a single secret finding within one scanned field of text.
type Match struct {
	Rule       string // rule name, e.g. "aws-access-key"
	Confidence string // ConfidenceDefinite | ConfidenceCandidate
	Start, End int    // byte offsets of the secret span within the text
	Index      int    // 0-based occurrence within this scanned text
	Redacted   string // masked form of text[Start:End]
}

// Scan returns the findings in a single field of text, sorted by Start.
// A candidate match is suppressed when its span overlaps any definite match
// (e.g. KEY=sk-ant-... yields only the anthropic-key finding) or is fully
// contained in a longer candidate (e.g. a JWT's header segment is not
// reported separately from the JWT). Redact, by contrast, masks every raw
// span regardless of suppression.
//
// Matches whose span hashes to agentsview's own test fixture deny-list are
// dropped (see agentsviewTestFixtureHashes); the filter
// prevents a development conversation that recorded a fixture from
// reporting it as a leak on every subsequent scan. Tests inside this
// package opt out via disableFixtureDenyForTest.
func Scan(text string) []Match {
	raw := scanRaw(text)
	var defs []Match
	for _, m := range raw {
		if m.Confidence == ConfidenceDefinite {
			defs = append(defs, m)
		}
	}
	kept := make([]Match, 0, len(raw))
	denyFixtures := fixtureDenyEnabled.Load()
	for _, m := range raw {
		if m.Confidence == ConfidenceCandidate {
			if overlapsAny(m, defs) || containedInLongerCandidate(m, raw) {
				continue
			}
		}
		if denyFixtures && isAgentsviewTestFixture(text[m.Start:m.End]) {
			continue
		}
		kept = append(kept, m)
	}
	for i := range kept {
		kept[i].Index = i
	}
	return kept
}

// ScanDefinite returns only the definite (well-anchored vendor format)
// findings in text, sorted by Start. It is the fast inline-sync path: it skips
// the FP-prone, CPU-heavy candidate rules (high-entropy assignments, JWTs,
// basic-auth URLs) entirely. Definite findings are never suppressed by Scan, so
// the spans returned here are exactly the definite-confidence subset Scan would
// report; only candidate findings are omitted. Applies the same
// agentsview-test-fixture hash deny-list as Scan.
func ScanDefinite(text string) []Match {
	raw := scanRulesRaw(text, definiteRules)
	kept := raw[:0]
	denyFixtures := fixtureDenyEnabled.Load()
	for _, m := range raw {
		if denyFixtures && isAgentsviewTestFixture(text[m.Start:m.End]) {
			continue
		}
		kept = append(kept, m)
	}
	for i := range kept {
		kept[i].Index = i
	}
	return kept
}

// scanRaw returns every rule match in text with no overlap suppression,
// sorted by Start ascending then End descending (longest span first at a
// given start). Index is left at 0; Scan/Redact assign meaning.
func scanRaw(text string) []Match {
	return scanRulesRaw(text, rules)
}

// scanRulesRaw is scanRaw restricted to the rules in rs, so callers can scan a
// subset (e.g. definite-only) through the identical match/sort logic.
func scanRulesRaw(text string, rs []rule) []Match {
	var out []Match
	for i := range rs {
		r := &rs[i]
		if !hasPrefilter(text, r.prefilters) {
			continue
		}
		for _, loc := range r.re.FindAllStringSubmatchIndex(text, -1) {
			s, e := loc[0], loc[1]
			if r.group > 0 {
				gs, ge := loc[2*r.group], loc[2*r.group+1]
				if gs < 0 {
					continue
				}
				s, e = gs, ge
			}
			span := text[s:e]
			if r.validate != nil && !r.validate(span) {
				continue
			}
			out = append(out, Match{
				Rule:       r.name,
				Confidence: r.confidence,
				Start:      s,
				End:        e,
				Redacted:   r.mask(span),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		return out[i].End > out[j].End
	})
	return out
}

func hasPrefilter(text string, prefilters []string) bool {
	if len(prefilters) == 0 {
		return true
	}
	for _, p := range prefilters {
		if strings.Contains(text, p) {
			return true
		}
	}
	return false
}

func overlapsAny(m Match, others []Match) bool {
	for _, o := range others {
		if m.Start < o.End && o.Start < m.End {
			return true
		}
	}
	return false
}

// containedInLongerCandidate reports whether m's span is fully contained in
// another candidate with a strictly larger span. This drops a sub-span
// finding (e.g. a JWT header segment matched as high-entropy) in favor of
// the longer, more specific candidate.
func containedInLongerCandidate(m Match, raw []Match) bool {
	for _, o := range raw {
		if o.Confidence != ConfidenceCandidate {
			continue
		}
		if o.Start <= m.Start && o.End >= m.End && (o.Start < m.Start || o.End > m.End) {
			return true
		}
	}
	return false
}

// Redact returns text with every secret-shaped span replaced by a masked
// form. Overlapping spans (e.g. a high-entropy candidate covering a vendor
// token) are merged so no full secret survives; a merged interval is
// masked generically, a lone span uses its rule's mask.
func Redact(text string) string {
	return redactSpans(text, scanRaw(text))
}

// RedactWithOffsets scans the complete source and maps its byte boundaries to
// the masked text. A boundary inside a secret maps after its replacement,
// assigning the mask to the first native part without duplicating it.
func RedactWithOffsets(text string) (string, func(int) int) {
	return redactSpansWithOffsets(text, scanRaw(text))
}

// redactSpans masks the secret spans raw within text and returns the result.
// raw must be the matches for text (offsets relative to text), sorted by Start
// ascending then End descending — the order scanRaw produces. Overlapping spans
// are merged into one generically-masked region; a lone span uses its rule's
// mask. Callers that scanned a larger string and want to redact a slice must
// first translate offsets to be slice-relative (see RedactWindow); this avoids
// re-scanning the slice, which would drop grouped-rule secrets whose anchoring
// context lies outside the slice.
func redactSpans(text string, raw []Match) string {
	redacted, _ := redactSpansWithOffsets(text, raw)
	return redacted
}

func redactSpansWithOffsets(text string, raw []Match) (string, func(int) int) {
	if len(raw) == 0 {
		return text, func(offset int) int { return offset }
	}
	type span struct {
		start, end int
		rep        string
	}
	spans := make([]span, 0, len(raw))
	for _, m := range raw {
		n := len(spans)
		if n > 0 && m.Start < spans[n-1].end {
			if m.End > spans[n-1].end {
				// Span grew: use a generic mask covering the merged region.
				spans[n-1].end = m.End
				spans[n-1].rep = maskKeepEnds(text[spans[n-1].start:spans[n-1].end], 0, 4)
			}
			// Span contained within existing: keep existing rep intact.
			continue
		}
		spans = append(spans, span{m.Start, m.End, m.Redacted})
	}
	var b strings.Builder
	prev := 0
	for _, s := range spans {
		b.WriteString(text[prev:s.start])
		b.WriteString(s.rep)
		prev = s.end
	}
	b.WriteString(text[prev:])
	return b.String(), func(offset int) int {
		delta := 0
		for _, span := range spans {
			if offset <= span.start {
				break
			}
			if offset < span.end {
				return span.start + delta + len(span.rep)
			}
			delta += len(span.rep) - (span.end - span.start)
		}
		return offset + delta
	}
}

// RedactWindow returns full[lo:hi] with every secret-shaped span overlapping
// that window masked. Unlike redacting the substring full[lo:hi] directly, it
// recognizes secrets that extend past the window edges (e.g. a private-key
// block whose END line falls outside the window): it scans the full text and
// widens the window to fully cover any secret it straddles, then masks those
// spans by their precomputed offsets. Masking precomputed spans — rather than
// re-scanning the (possibly widened) slice — is what makes grouped rules safe:
// the basic-auth password and high-entropy-assignment value are reported as
// capture groups whose anchoring context ("scheme://user:", "key=") sits before
// the span, so a re-scan of a slice starting inside the secret would fail to
// match and leak it. lo/hi are byte offsets, clamped to full and assumed
// rune-aligned (callers pass snippetBounds output; secret spans are
// rune-aligned because they come from regex matches over valid UTF-8).
func RedactWindow(full string, lo, hi int) string {
	if lo < 0 {
		lo = 0
	}
	if hi > len(full) {
		hi = len(full)
	}
	if lo >= hi {
		return ""
	}
	raw := scanRaw(full)
	lo, hi = expandToCoverSecrets(raw, lo, hi)
	// expandToCoverSecrets guarantees every span overlapping the window is now
	// fully inside it; translate those spans to slice-relative offsets and mask
	// them directly, preserving each rule's mask without re-detecting.
	var windowed []Match
	for _, m := range raw {
		if m.Start >= lo && m.End <= hi {
			m.Start -= lo
			m.End -= lo
			windowed = append(windowed, m)
		}
	}
	return redactSpans(full[lo:hi], windowed)
}

// expandToCoverSecrets widens [lo,hi) until no secret span straddles an edge:
// any span overlapping the window is brought fully inside. It iterates to a
// fixpoint because widening can pull in a further span; this terminates since
// each step strictly grows a bounded window over a finite span set.
func expandToCoverSecrets(spans []Match, lo, hi int) (int, int) {
	for {
		grew := false
		for _, m := range spans {
			if m.Start < hi && lo < m.End { // overlaps the window
				if m.Start < lo {
					lo, grew = m.Start, true
				}
				if m.End > hi {
					hi, grew = m.End, true
				}
			}
		}
		if !grew {
			return lo, hi
		}
	}
}

// maskKeepEnds keeps the first `prefix` and last `suffix` runes of s,
// replacing the middle with an ellipsis. Strings too short to keep both
// ends are fully masked with bullets so nothing leaks.
func maskKeepEnds(s string, prefix, suffix int) string {
	r := []rune(s)
	if len(r) <= prefix+suffix {
		return strings.Repeat("•", len(r))
	}
	return string(r[:prefix]) + "…" + string(r[len(r)-suffix:])
}
