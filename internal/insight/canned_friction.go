package insight

import (
	"fmt"
	"sort"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
)

// MaxCannedFrictionPatterns bounds the patterns sent to the model (spec §19).
const MaxCannedFrictionPatterns = 20

// CannedFrictionReviewInput is the deterministic Friction Log input for the
// friction_review template. It carries the stored digest summary object,
// pattern titles and counts, and the P0 list. Finding text, evidence and
// transcript content never enter it, and it is never written back to any
// friction table.
type CannedFrictionReviewInput struct {
	Date         string                  `json:"date"`
	Timezone     string                  `json:"timezone"`
	RulesVersion string                  `json:"rules_version"`
	Revision     int                     `json:"revision"`
	Summary      map[string]any          `json:"summary"`
	TopPatterns  []CannedFrictionPattern `json:"top_patterns"`
	P0Alerts     []CannedFrictionP0      `json:"p0_alerts"`
}

// CannedFrictionPattern is one fingerprint as seen in a single digest. Counts
// are digest-scoped so the payload, and therefore the cache key, stays stable
// when later digests raise the global friction_patterns counters.
type CannedFrictionPattern struct {
	Ref               string `json:"ref"`
	Fingerprint       string `json:"fingerprint"`
	Kind              string `json:"kind"`
	Title             string `json:"title,omitempty"` // "" for frustration: its title quotes the user
	DigestOccurrences int    `json:"digest_occurrences"`
	DigestSessions    int    `json:"digest_sessions"`
	FirstSeenDate     string `json:"first_seen_date"`
	Recurring         bool   `json:"recurring"`
}

// CannedFrictionP0 is one P0 alert: a tool failing across sessions.
type CannedFrictionP0 struct {
	Tool     string `json:"tool"`
	Sessions int    `json:"sessions"`
}

// CannedFrictionPatternRef is the evidence ref for the pattern at rank (1-based).
func CannedFrictionPatternRef(rank int) string {
	return fmt.Sprintf("friction:pattern:%02d", rank)
}

// RankCannedFrictionPatterns folds a digest's findings by fingerprint and
// returns the top patterns by in-digest occurrences, then distinct sessions,
// then fingerprint. firstSeen maps fingerprint to friction_patterns
// first_seen_date; a missing entry means first seen on date. limit <= 0
// keeps every pattern. Frustration titles are dropped because they quote the
// user; every other kind keeps its title (spec §19).
func RankCannedFrictionPatterns(
	date string,
	findings []db.FrictionFinding,
	firstSeen map[string]string,
	limit int,
) []CannedFrictionPattern {
	type accum struct {
		kind, title string
		occurrences int
		sessions    map[string]struct{}
	}
	byFingerprint := make(map[string]*accum)
	for _, f := range findings {
		a, ok := byFingerprint[f.Fingerprint]
		if !ok {
			title := f.Title
			if f.Kind == string(friction.KindFrustration) {
				title = ""
			}
			a = &accum{kind: f.Kind, title: title, sessions: make(map[string]struct{})}
			byFingerprint[f.Fingerprint] = a
		}
		a.occurrences++
		a.sessions[f.SessionID] = struct{}{}
	}
	out := make([]CannedFrictionPattern, 0, len(byFingerprint))
	for fingerprint, a := range byFingerprint {
		first := firstSeen[fingerprint]
		if first == "" {
			first = date
		}
		out = append(out, CannedFrictionPattern{
			Fingerprint:       fingerprint,
			Kind:              a.kind,
			Title:             a.title,
			DigestOccurrences: a.occurrences,
			DigestSessions:    len(a.sessions),
			FirstSeenDate:     first,
			Recurring:         first < date,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DigestOccurrences != out[j].DigestOccurrences {
			return out[i].DigestOccurrences > out[j].DigestOccurrences
		}
		if out[i].DigestSessions != out[j].DigestSessions {
			return out[i].DigestSessions > out[j].DigestSessions
		}
		return out[i].Fingerprint < out[j].Fingerprint
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		out[i].Ref = CannedFrictionPatternRef(i + 1)
	}
	return out
}

// CannedFrictionP0Alerts converts the summary's p0_alerts map into a list
// sorted by tool, keeping only the session count.
func CannedFrictionP0Alerts(p0 map[string][]string) []CannedFrictionP0 {
	out := make([]CannedFrictionP0, 0, len(p0))
	for tool, sessions := range p0 {
		out = append(out, CannedFrictionP0{Tool: tool, Sessions: len(sessions)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tool < out[j].Tool })
	return out
}

// CannedFrictionEvidenceRefs lists the refs a friction_review envelope may
// cite, sorted by ID. An empty digest yields only aggregate:empty, the same
// convention CannedEvidenceRefs uses.
func CannedFrictionEvidenceRefs(r CannedFrictionReviewInput) []CannedEvidenceRef {
	if summaryInt(r.Summary, "sessions_scanned") == 0 &&
		len(r.TopPatterns) == 0 && len(r.P0Alerts) == 0 {
		return []CannedEvidenceRef{{
			ID:          "aggregate:empty",
			Description: "The friction digest for this date scanned no sessions.",
		}}
	}
	refs := []CannedEvidenceRef{{
		ID:          "friction:summary",
		Description: "Digest counts by kind, sessions scanned, personas, and spend.",
	}}
	if len(r.P0Alerts) > 0 {
		refs = append(refs, CannedEvidenceRef{
			ID:          "friction:p0_alerts",
			Description: "Tools failing in three or more distinct sessions.",
		})
	}
	if spend, ok := r.Summary["spend"]; ok && spend != nil {
		refs = append(refs, CannedEvidenceRef{
			ID:          "friction:spend",
			Description: "Observed spend for the sessions in this digest.",
		})
	}
	for _, p := range r.TopPatterns {
		refs = append(refs, CannedEvidenceRef{
			ID: p.Ref,
			Description: fmt.Sprintf("%s pattern seen %d time(s) in %d session(s).",
				p.Kind, p.DigestOccurrences, p.DigestSessions),
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	return refs
}

func summaryInt(summary map[string]any, key string) int {
	switch v := summary[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}
