package friction

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

// Adapts zone.rs resolution_order_is_override_config_tz_system_utc.
// The TZ/system/UTC steps collapse into timeutil.LocalLocation, injected as local.
func TestResolveZone(t *testing.T) {
	paris := mustLoad(t, "Europe/Paris")
	local := func() *time.Location { return paris }
	tests := []struct {
		name       string
		env, cfg   string
		want       string
		errContain []string
	}{
		{name: "env_override_wins", env: "Asia/Tokyo", cfg: "Asia/Dhaka", want: "Asia/Tokyo"},
		{name: "config_second", cfg: "Asia/Dhaka", want: "Asia/Dhaka"},
		{name: "local_location_last", want: "Europe/Paris"},
		{name: "config_is_trimmed", cfg: " Asia/Dhaka ", want: "Asia/Dhaka"},
		{name: "blank_env_falls_through", env: "   ", cfg: "Asia/Dhaka", want: "Asia/Dhaka"},
		{name: "blank_config_is_unset", cfg: "  ", want: "Europe/Paris"},
		{name: "utc_is_a_zone", cfg: "UTC", want: "UTC"},
		{name: "env_typo_is_an_error", env: "Asia/Tokio", cfg: "Asia/Dhaka",
			errContain: []string{"AGENTSVIEW_FRICTION_TZ", `"Asia/Tokio"`, "is not an IANA zone"}},
		{name: "config_typo_is_an_error", cfg: "Asia/Dhakka",
			errContain: []string{`[friction] timezone "Asia/Dhakka" is not an IANA zone`}},
		{name: "local_is_not_an_iana_zone", cfg: "Local",
			errContain: []string{`[friction] timezone "Local" is not an IANA zone`}},
		{name: "posix_rule_is_not_a_zone", env: "JST-9",
			errContain: []string{"AGENTSVIEW_FRICTION_TZ", `"JST-9"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc, err := resolveZone(tt.env, tt.cfg, local)
			if len(tt.errContain) > 0 {
				require.Error(t, err)
				for _, s := range tt.errContain {
					assert.Contains(t, err.Error(), s)
				}
				assert.Nil(t, loc)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, loc.String())
		})
	}
}

func TestResolveZoneUsesLocalLocation(t *testing.T) {
	loc, err := ResolveZone("", "")
	require.NoError(t, err)
	require.NotNil(t, loc)
}

// Ports zone.rs digest_date_and_window_follow_the_zone with Asia/Dhaka.
func TestDigestDateAndWindowFollowTheZone(t *testing.T) {
	dhaka := mustLoad(t, "Asia/Dhaka")
	tokyo := mustLoad(t, "Asia/Tokyo")
	tests := []struct {
		name     string
		now      time.Time
		loc      *time.Location
		date     string
		from, to string
	}{
		{"late_utc_is_next_day_at_plus_six", time.Date(2026, 9, 17, 22, 30, 0, 0, time.UTC), dhaka, "2026-09-18", "2026-09-11", "2026-09-17"},
		{"same_instant_in_utc", time.Date(2026, 9, 17, 22, 30, 0, 0, time.UTC), time.UTC, "2026-09-17", "2026-09-10", "2026-09-16"},
		{"evening_run_is_still_today_at_plus_six", time.Date(2026, 9, 17, 16, 50, 0, 0, time.UTC), dhaka, "2026-09-17", "2026-09-10", "2026-09-16"},
		{"tokyo_has_turned_over", time.Date(2026, 9, 17, 16, 50, 0, 0, time.UTC), tokyo, "2026-09-18", "2026-09-11", "2026-09-17"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			date := tt.now.In(tt.loc).Format(time.DateOnly)
			assert.Equal(t, tt.date, date)
			from, to, err := ArchiveWindow(date)
			require.NoError(t, err)
			assert.Equal(t, tt.from, from)
			assert.Equal(t, tt.to, to)
		})
	}
}
