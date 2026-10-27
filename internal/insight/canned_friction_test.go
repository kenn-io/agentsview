package insight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testFrictionFinding(session, kind, title, fingerprint string) CannedFrictionOccurrence {
	return CannedFrictionOccurrence{SubjectID: session, Kind: kind, Title: title, Fingerprint: fingerprint}
}

func TestRankCannedFrictionPatterns(t *testing.T) {
	const date = "2025-01-15"
	errTitle := "[friction/error] Bash: exit status 1"
	wkTitle := "[friction/workaround] for now: hardcode the port"
	dfTitle := "[friction/deferral] s3: defer"
	inTitle := "[friction/interruption] s4"
	frTitle := "[friction/frustration] s4: FRUSTRATION-RAW this is still broken"
	findings := []CannedFrictionOccurrence{
		testFrictionFinding("s1", "error", errTitle, "fl1:bb"),
		testFrictionFinding("s2", "error", errTitle, "fl1:bb"),
		testFrictionFinding("s1", "workaround", wkTitle, "fl1:cc"),
		testFrictionFinding("s1", "workaround", wkTitle, "fl1:cc"),
		testFrictionFinding("s3", "deferral", dfTitle, "fl1:aa"),
		testFrictionFinding("s4", "interruption", inTitle, "fl1:dd"),
		testFrictionFinding("s4", "interruption", inTitle, "fl1:dd"),
		testFrictionFinding("s4", "frustration", frTitle, "fl1:ee"),
	}
	firstSeen := map[string]string{"fl1:bb": "2025-01-10", "fl1:cc": date}
	all := []CannedFrictionPattern{
		{Ref: "friction:pattern:01", Fingerprint: "fl1:bb", Kind: "error", Title: errTitle, DigestOccurrences: 2, DigestSessions: 2, FirstSeenDate: "2025-01-10", Recurring: true},
		{Ref: "friction:pattern:02", Fingerprint: "fl1:cc", Kind: "workaround", Title: wkTitle, DigestOccurrences: 2, DigestSessions: 1, FirstSeenDate: date, Recurring: false},
		{Ref: "friction:pattern:03", Fingerprint: "fl1:dd", Kind: "interruption", Title: inTitle, DigestOccurrences: 2, DigestSessions: 1, FirstSeenDate: date, Recurring: false},
		{Ref: "friction:pattern:04", Fingerprint: "fl1:aa", Kind: "deferral", Title: dfTitle, DigestOccurrences: 1, DigestSessions: 1, FirstSeenDate: date, Recurring: false},
		// Frustration titles quote the user, so the title is withheld.
		{Ref: "friction:pattern:05", Fingerprint: "fl1:ee", Kind: "frustration", Title: "", DigestOccurrences: 1, DigestSessions: 1, FirstSeenDate: date, Recurring: false},
	}
	tests := []struct {
		name     string
		findings []CannedFrictionOccurrence
		limit    int
		want     []CannedFrictionPattern
	}{
		{name: "empty_digest", findings: nil, limit: MaxCannedFrictionPatterns, want: []CannedFrictionPattern{}},
		{name: "orders_by_occurrences_then_sessions_then_fingerprint", findings: findings, limit: MaxCannedFrictionPatterns, want: all},
		{name: "limit_applies_after_ranking", findings: findings, limit: 1, want: all[:1]},
		{name: "zero_limit_keeps_all", findings: findings, limit: 0, want: all},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RankCannedFrictionPatterns(date, tt.findings, firstSeen, tt.limit)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("caps_at_twenty", func(t *testing.T) {
		var many []CannedFrictionOccurrence
		for i := range 25 {
			fp := "fl1:" + string(rune('a'+i))
			many = append(many, testFrictionFinding("s1", "error", "t"+fp, fp))
		}
		got := RankCannedFrictionPatterns(date, many, nil, MaxCannedFrictionPatterns)
		require.Len(t, got, 20)
		assert.Equal(t, "friction:pattern:20", got[19].Ref)
	})
}

func TestCannedFrictionP0Alerts(t *testing.T) {
	tests := []struct {
		name string
		in   map[string][]string
		want []CannedFrictionP0
	}{
		{name: "nil", in: nil, want: []CannedFrictionP0{}},
		{
			name: "sorted_by_tool_with_session_counts",
			in:   map[string][]string{"Read": {"a", "b", "c"}, "Bash": {"a", "b", "c", "d"}},
			want: []CannedFrictionP0{{Tool: "Bash", Sessions: 4}, {Tool: "Read", Sessions: 3}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, CannedFrictionP0Alerts(tt.in))
		})
	}
}

func TestCannedFrictionEvidenceRefs(t *testing.T) {
	tests := []struct {
		name   string
		review CannedFrictionReviewInput
		want   []string
	}{
		{
			name:   "empty_digest",
			review: CannedFrictionReviewInput{Summary: map[string]any{"sessions_scanned": float64(0)}},
			want:   []string{"aggregate:empty"},
		},
		{
			name:   "summary_only",
			review: CannedFrictionReviewInput{Summary: map[string]any{"sessions_scanned": float64(2)}},
			want:   []string{"friction:summary"},
		},
		{
			name: "all_refs_sorted",
			review: CannedFrictionReviewInput{
				Summary:     map[string]any{"sessions_scanned": float64(3)},
				TopPatterns: []CannedFrictionPattern{{Ref: "friction:pattern:01", Kind: "error", DigestOccurrences: 2, DigestSessions: 2}, {Ref: "friction:pattern:02", Kind: "workaround", DigestOccurrences: 1, DigestSessions: 1}},
				P0Alerts:    []CannedFrictionP0{{Tool: "Bash", Sessions: 3}},
			},
			want: []string{"friction:p0_alerts", "friction:pattern:01", "friction:pattern:02", "friction:summary"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs := CannedFrictionEvidenceRefs(tt.review)
			ids := make([]string, 0, len(refs))
			for _, ref := range refs {
				ids = append(ids, ref.ID)
				assert.NotEmpty(t, ref.Description)
			}
			assert.Equal(t, tt.want, ids)
		})
	}
}
