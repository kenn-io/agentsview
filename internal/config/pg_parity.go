package config

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

var paritySchemaName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,62}$`)

// PGParityBaseline binds a friendly profile name to an existing PostgreSQL
// target and the immutable identity provisioned in that target.
type PGParityBaseline struct {
	Target   string `toml:"target" json:"target"`
	Identity string `toml:"identity" json:"identity"`
}

// ValidateParity validates the owner-controlled parity scheduler settings.
func (p PGConfig) ValidateParity(requireAuth bool) error {
	if !p.ParityEnabled {
		return nil
	}
	if strings.TrimSpace(p.RawTenant) == "" || p.RawTenant != strings.TrimSpace(p.RawTenant) || len(p.RawTenant) > 128 {
		return fmt.Errorf("migration parity requires raw_tenant")
	}
	if !requireAuth {
		return fmt.Errorf("migration parity requires authentication")
	}
	if !paritySchemaName.MatchString(p.Schema) {
		return fmt.Errorf("migration parity schema is invalid")
	}
	if len(p.ParityBaselines) == 0 {
		return fmt.Errorf("migration parity requires at least one baseline profile")
	}
	for name, profile := range p.ParityBaselines {
		if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) {
			return fmt.Errorf("migration parity baseline profile name is invalid")
		}
		if strings.TrimSpace(profile.Target) == "" || profile.Target != strings.TrimSpace(profile.Target) {
			return fmt.Errorf("migration parity baseline %q requires a named target", name)
		}
		id, err := uuid.Parse(profile.Identity)
		if err != nil || id.String() != profile.Identity {
			return fmt.Errorf("migration parity baseline %q identity must be a canonical lowercase UUID", name)
		}
	}
	if p.ParityPollSeconds < 0 || p.ParityPollSeconds > 60 ||
		p.ParityAttemptSeconds < 0 || p.ParityAttemptSeconds > 1800 ||
		p.ParitySnapshotSeconds < 0 || p.ParitySnapshotSeconds > 1800 {
		return fmt.Errorf("migration parity worker bounds: poll 1-60 seconds, attempt and snapshot 1-1800 seconds; zero selects defaults")
	}
	return nil
}

// ParityWorkerBounds returns bounded scheduler defaults.
func (p PGConfig) ParityWorkerBounds() (poll, attempt, snapshot int) {
	poll, attempt, snapshot = p.ParityPollSeconds, p.ParityAttemptSeconds, p.ParitySnapshotSeconds
	if poll == 0 {
		poll = 2
	}
	if attempt == 0 {
		attempt = 300
	}
	if snapshot == 0 {
		snapshot = 300
	}
	return
}

// ResolveParityBaseline resolves only an explicitly named profile and named PG
// target. It never falls back to the default or the legacy single-target block.
func (c Config) ResolveParityBaseline(runtime PGConfig, profile string) (ResolvedPGTarget, PGParityBaseline, error) {
	if err := runtime.ValidateParity(c.RequireAuth); err != nil {
		return ResolvedPGTarget{}, PGParityBaseline{}, err
	}
	if profile == "" || profile != strings.TrimSpace(profile) {
		return ResolvedPGTarget{}, PGParityBaseline{}, fmt.Errorf("migration parity baseline profile is required")
	}
	baseline, ok := runtime.ParityBaselines[profile]
	if !ok {
		return ResolvedPGTarget{}, PGParityBaseline{}, fmt.Errorf("migration parity baseline profile %q is not configured", profile)
	}
	targetName := normalizePGTargetName(baseline.Target)
	if targetName == "" || len(c.PGTargets) == 0 {
		return ResolvedPGTarget{}, PGParityBaseline{}, fmt.Errorf("migration parity baseline %q must reference an existing named PG target", profile)
	}
	raw, err := c.RawPGTarget(targetName)
	if err != nil {
		return ResolvedPGTarget{}, PGParityBaseline{}, err
	}
	resolved, err := c.resolvePGConfig(raw, false)
	if err != nil {
		return ResolvedPGTarget{}, PGParityBaseline{}, err
	}
	if resolved.URL == "" {
		return ResolvedPGTarget{}, PGParityBaseline{}, fmt.Errorf("migration parity baseline target %q requires a URL", targetName)
	}
	if !paritySchemaName.MatchString(resolved.Schema) {
		return ResolvedPGTarget{}, PGParityBaseline{}, fmt.Errorf("migration parity baseline target %q schema is invalid", targetName)
	}
	if resolved.RawTenant != runtime.RawTenant {
		return ResolvedPGTarget{}, PGParityBaseline{}, fmt.Errorf("migration parity baseline tenant does not match runtime tenant")
	}
	defaultName, _ := c.DefaultPGTargetName()
	return ResolvedPGTarget{Name: targetName, Config: resolved, IsDefault: targetName == defaultName}, baseline, nil
}
