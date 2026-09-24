package ledger

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ports jilog query.rs:442-494.
func TestGlobAndSubsystemMatch(t *testing.T) {
	objOnly := Event{
		EventID: uuid.Nil, Zone: "z", Source: "s", SourceSeq: 1,
		ObjectRef: new("subsystem:opsctl"), EventClass: ClassStateChange,
		PayloadTier: TierStructured,
	}
	hookObj := objOnly
	hookObj.ObjectRef = new("subsystem:hook-test")
	tests := []struct {
		name string
		got  bool
		want bool
	}{
		{"glob_match_prefix_wildcard/hook-kata-logger", GlobMatch("hook-*", "hook-kata-logger"), true},
		{"glob_match_prefix_wildcard/hook-feature-capture", GlobMatch("hook-*", "hook-feature-capture"), true},
		{"glob_match_prefix_wildcard/bare_prefix", GlobMatch("hook-*", "hook-"), true},
		{"glob_match_prefix_wildcard/nightly-extractor", GlobMatch("hook-*", "nightly-extractor"), false},
		{"glob_match_prefix_wildcard/other-hook", GlobMatch("hook-*", "other-hook"), false},
		{"glob_match_exact/equal", GlobMatch("nightly-extractor", "nightly-extractor"), true},
		{"glob_match_exact/longer", GlobMatch("nightly-extractor", "nightly-extractor-v2"), false},
		{"glob_match_exact/prefix_only", GlobMatch("opsctl", "opsctl-something"), false},
		{"subsystem_matches_empty_patterns_passes", SubsystemMatches(nil, hookObj), true},
		{"subsystem_matches_falls_back_to_object_ref/match", SubsystemMatches([]string{"opsctl"}, objOnly), true},
		{"subsystem_matches_falls_back_to_object_ref/no_match", SubsystemMatches([]string{"hook-*"}, objOnly), false},
		{"no_subsystem_fails_any_pattern", SubsystemMatches([]string{"*"}, Event{EventClass: ClassHealth}), false},
		{"empty_subsystem_is_still_a_subsystem", SubsystemMatches([]string{"*"}, Event{ObjectRef: new("subsystem:")}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.got)
		})
	}
}

func TestSubsystemAndSummaryDerivation(t *testing.T) {
	a2 := loadFixture(t, "fixture-a-000002.json")
	a1 := loadFixture(t, "fixture-a-000001.json")
	tests := []struct {
		name      string
		event     Event
		subsystem string
		summary   string
	}{
		{"object_ref_fallback", a2.Events[0], "from-object", ""},
		{"non_string_payload_subsystem_falls_back", a2.Events[1], "ignored", ""},
		{"array_payload_and_other_ref", a2.Events[2], "", ""},
		{"payload_wins", a1.Events[1], "fixture", "floats and ints"},
		{"unicode_summary", a1.Events[2], "hook-unicode", "unicode <>&  "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.subsystem, Subsystem(tt.event))
			assert.Equal(t, tt.summary, Summary(tt.event))
		})
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 30, 0, 0, time.UTC)
	tests := []struct {
		in      string
		want    time.Time
		wantErr string
	}{
		{in: "24h", want: now.Add(-24 * time.Hour)},
		{in: "7d", want: now.Add(-7 * 24 * time.Hour)},
		{in: "4w", want: now.Add(-28 * 24 * time.Hour)},
		{in: "-2h", want: now.Add(2 * time.Hour)},
		{in: "+3d", want: now.Add(-3 * 24 * time.Hour)},
		{in: "2026-04-01", want: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
		{in: "xh", wantErr: "invalid hour count: xh"},
		{in: "1.5d", wantErr: "invalid day count: 1.5d"},
		{in: "w", wantErr: "invalid week count: w"},
		{in: "yesterday", wantErr: "invalid date: yesterday (expected Nh, Nd, Nw, or YYYY-MM-DD)"},
		{in: "2026-02-30", wantErr: "invalid date: 2026-02-30 (expected Nh, Nd, Nw, or YYYY-MM-DD)"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseSince(tt.in, now)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.True(t, tt.want.Equal(got), "got %s want %s", got, tt.want)
		})
	}
}

// goldenResults rebuilds the generator's query input: zone "default" is
// fixture-a-000001 newest first; zone "ops" is the newest three events of
// host-with-dash-000007.
func goldenResults(t *testing.T) []ZoneEvents {
	t.Helper()
	a1 := slices.Clone(loadFixture(t, "fixture-a-000001.json").Events)
	slices.Reverse(a1)
	b := slices.Clone(loadFixture(t, "host-with-dash-000007.json").Events)
	slices.Reverse(b)
	return []ZoneEvents{{Zone: "default", Events: a1}, {Zone: "ops", Events: b[:3]}}
}

func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}

func TestFormatTextMatchesJilog(t *testing.T) {
	got := FormatText(goldenResults(t), "7d", []string{"hook-*", `sub-"q"`})
	assert.Equal(t, readGolden(t, "query-text.golden"), got)
}

func TestFormatTextEmptyMatchesJilog(t *testing.T) {
	got := FormatText(nil, "2026-01-01", []string{"a\tb", "é"})
	assert.Equal(t, readGolden(t, "query-text-empty.golden"), got)
	assert.Equal(t, "No events found since 7d.\n", FormatText([]ZoneEvents{{Zone: "z"}}, "7d", nil))
}

func TestFormatJSONMatchesJilog(t *testing.T) {
	got, err := FormatJSON(goldenResults(t))
	require.NoError(t, err)
	assert.Equal(t, readGolden(t, "query-json.golden"), string(got)+"\n")
	empty, err := FormatJSON(nil)
	require.NoError(t, err)
	assert.Equal(t, "[]", string(empty))
}
