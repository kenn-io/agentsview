package friction

import (
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/timeutil"
)

// ZoneEnvVar overrides every other digest zone source.
const ZoneEnvVar = "AGENTSVIEW_FRICTION_TZ"

// ResolveZone picks the zone used for digest dates: the environment override,
// then [friction] timezone, then the local zone. Explicit sources must name an
// IANA zone; a typo is an error rather than a silent fallback.
func ResolveZone(env, configured string) (*time.Location, error) {
	return resolveZone(env, configured, timeutil.LocalLocation)
}

func resolveZone(env, configured string, local func() *time.Location) (*time.Location, error) {
	if v := strings.TrimSpace(env); v != "" {
		return loadIANAZone(v, ZoneEnvVar)
	}
	if v := strings.TrimSpace(configured); v != "" {
		return loadIANAZone(v, "[friction] timezone")
	}
	return local(), nil
}

func loadIANAZone(name, source string) (*time.Location, error) {
	// time.LoadLocation accepts "Local", which makes digest dates host-dependent.
	if name == "Local" {
		return nil, fmt.Errorf("%s %q is not an IANA zone", source, name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%s %q is not an IANA zone: %w", source, name, err)
	}
	return loc, nil
}
