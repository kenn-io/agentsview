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
// span regardless of suppression, except agentsview asset reference hashes
// (see maskableSpans).
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
	return redactSpans(text, maskableSpans(text))
}

// maskableSpans returns scanRaw(text) without spans that are only the hash
// path of an agentsview asset reference ("asset://<sha256 hex>.png"). The
// high-entropy-assignment candidate reads "asset:" as a key and the digest as
// its value, but the reference names a local image by the hash of its bytes
// and carries no secret. Masking it would break every exported image. Scan
// still reports the candidate, so findings and RulesVersion are unchanged.
//
// Text can also hold escaped JSON or Go string literals, such as a Codex
// terminal-input preview or the cut-off tail of a JSON Lines record. There the
// "n" of a "\n" escape removes a key's leading word boundary, and a key
// block's "\n" escapes fail the PEM body check. maskableSpans also scans a
// copy of text where each "\n", "\r", and "\t" escape is whitespace, so the
// mask covers those secrets too. Scan does not, so findings are unchanged.
func maskableSpans(text string) []Match {
	raw := scanRaw(text)
	if shadow, ok := unescapeWhitespace(text); ok {
		for _, m := range scanRaw(shadow) {
			// Mask the original bytes, so a mask that keeps a span's ends
			// never writes the copy's real newline into escaped text.
			m.Redacted = ruleMask(m.Rule, text[m.Start:m.End])
			raw = append(raw, m)
		}
		sort.Slice(raw, func(i, j int) bool {
			if raw[i].Start != raw[j].Start {
				return raw[i].Start < raw[j].Start
			}
			return raw[i].End > raw[j].End
		})
	}
	kept := raw[:0]
	for _, m := range raw {
		if !isAssetReferencePath(text, m) {
			kept = append(kept, m)
		}
	}
	return kept
}

// ruleMask masks span with the mask of the rule named name. Rule names are
// unique.
func ruleMask(name, span string) string {
	for i := range rules {
		if rules[i].name == name {
			return rules[i].mask(span)
		}
	}
	return maskKeepEnds(span, 0, 4)
}

// unescapeWhitespace returns text with each "\n", "\r", and "\t" escape
// replaced by a space and a newline, so offsets into the copy are offsets into
// text. An escaped backslash ("\\") is skipped, so "\\n" stays a backslash and
// a letter. ok is false when text has no such escape.
func unescapeWhitespace(text string) (string, bool) {
	var b []byte
	for i := 0; i+1 < len(text); i++ {
		if text[i] != '\\' {
			continue
		}
		switch text[i+1] {
		case 'n', 'r', 't':
			if b == nil {
				b = []byte(text)
			}
			b[i], b[i+1] = ' ', '\n'
		}
		i++
	}
	if b == nil {
		return "", false
	}
	return string(b), true
}

func isAssetReferencePath(text string, m Match) bool {
	const scheme = "asset:"
	if m.Start < len(scheme) || text[m.Start-len(scheme):m.Start] != scheme {
		return false
	}
	digest, ok := strings.CutPrefix(text[m.Start:m.End], "//")
	if !ok || len(digest) != 64 {
		return false
	}
	for i := range len(digest) {
		c := digest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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
	if len(raw) == 0 {
		return text
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
	return b.String()
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
	raw := maskableSpans(full)
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
