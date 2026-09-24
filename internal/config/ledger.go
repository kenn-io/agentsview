package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/pathutil"
)

// DefaultLedgerZone is the zone used when [ledger] default_zone is unset.
const DefaultLedgerZone = "default"

// LedgerConfig controls the optional event ledger ([ledger]). It is off
// unless enabled.
type LedgerConfig struct {
	Enabled bool `json:"enabled" toml:"enabled"`
	// Source names this host's segments; "" means "av-<installation_id>".
	Source      string `json:"source,omitempty" toml:"source"`
	DefaultZone string `json:"default_zone,omitempty" toml:"default_zone"`
	// ReplicateConfidential lets confidential-tier events leave the host
	// on push (PR 14). Off by default.
	ReplicateConfidential bool               `json:"replicate_confidential,omitempty" toml:"replicate_confidential"`
	Zones                 []LedgerZoneConfig `json:"zones,omitempty" toml:"zones"`
}

// LedgerZoneConfig is one [[ledger.zones]] entry.
type LedgerZoneConfig struct {
	ID string `json:"id" toml:"id"`
	// Replicate defaults to true (jilog `spool`); false keeps the zone
	// local on push.
	Replicate *bool `json:"replicate,omitempty" toml:"replicate"`
	// ImportPath follows a jilog-format ledger directory (reads
	// <import_path>/segments).
	ImportPath     string `json:"import_path,omitempty" toml:"import_path"`
	SpoolPath      string `json:"spool_path,omitempty" toml:"spool_path"`
	SpoolAuthority bool   `json:"spool_authority,omitempty" toml:"spool_authority"`
}

// Replicates reports whether the zone is pushed (default true).
func (z LedgerZoneConfig) Replicates() bool { return z.Replicate == nil || *z.Replicate }

// ImportSegmentsDir is the segments directory the ledger-import job and
// `ledger import` read for this zone, "" when import_path is unset.
func (z LedgerZoneConfig) ImportSegmentsDir() (string, error) {
	if z.ImportPath == "" {
		return "", nil
	}
	p, err := pathutil.ExpandHome(z.ImportPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(p, "segments"), nil
}

// EffectiveSource is the local segment source: [ledger] source, or
// "av-<installation_id>" so it never collides with jilog's hostname-based
// sources on the same machine (D22).
func (c LedgerConfig) EffectiveSource(installationID string) string {
	if c.Source != "" {
		return c.Source
	}
	return "av-" + installationID
}

// EffectiveDefaultZone is default_zone, or "default".
func (c LedgerConfig) EffectiveDefaultZone() string {
	if c.DefaultZone != "" {
		return c.DefaultZone
	}
	return DefaultLedgerZone
}

// ZoneIDs lists the configured zones with the default zone first; the
// default zone exists even when no [[ledger.zones]] entry names it.
func (c LedgerConfig) ZoneIDs() []string {
	def := c.EffectiveDefaultZone()
	ids := []string{def}
	for _, z := range c.Zones {
		if z.ID != def {
			ids = append(ids, z.ID)
		}
	}
	return ids
}

// Zone returns the settings for id; the default zone gets defaults when it
// has no entry.
func (c LedgerConfig) Zone(id string) (LedgerZoneConfig, bool) {
	for _, z := range c.Zones {
		if z.ID == id {
			return z, true
		}
	}
	if id == c.EffectiveDefaultZone() {
		return LedgerZoneConfig{ID: id}, true
	}
	return LedgerZoneConfig{}, false
}

// Validate checks names that become path segments and identities.
func (c LedgerConfig) Validate() error {
	if c.Source != "" && !ledger.ValidSourceName(c.Source) {
		return fmt.Errorf("[ledger] source %q must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$", c.Source)
	}
	if c.DefaultZone != "" && !ledger.ValidSourceName(c.DefaultZone) {
		return fmt.Errorf("[ledger] default_zone %q must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$", c.DefaultZone)
	}
	seen := map[string]bool{}
	for _, z := range c.Zones {
		if !ledger.ValidSourceName(z.ID) {
			return fmt.Errorf("[ledger.zones] id must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ (got %q)", z.ID)
		}
		if seen[z.ID] {
			return fmt.Errorf("[ledger.zones] duplicate id %q", z.ID)
		}
		seen[z.ID] = true
		for _, f := range []struct{ key, path string }{
			{"import_path", z.ImportPath}, {"spool_path", z.SpoolPath},
		} {
			if f.path == "" {
				continue
			}
			expanded, err := pathutil.ExpandHome(f.path)
			if err != nil {
				return fmt.Errorf("[ledger.zones] %s: %w", f.key, err)
			}
			if !filepath.IsAbs(expanded) {
				return fmt.Errorf("[ledger.zones] %s %q must be an absolute path", f.key, f.path)
			}
		}
	}
	return nil
}

// normalized returns c with surrounding whitespace trimmed from every
// name and path.
func (c LedgerConfig) normalized() LedgerConfig {
	c.Source = strings.TrimSpace(c.Source)
	c.DefaultZone = strings.TrimSpace(c.DefaultZone)
	zones := make([]LedgerZoneConfig, len(c.Zones))
	for i, z := range c.Zones {
		z.ID = strings.TrimSpace(z.ID)
		z.ImportPath = strings.TrimSpace(z.ImportPath)
		z.SpoolPath = strings.TrimSpace(z.SpoolPath)
		zones[i] = z
	}
	c.Zones = zones
	return c
}
