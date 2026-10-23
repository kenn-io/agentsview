package config

import "errors"

// RawSyncConfig selects workstation behavior while raw capture uploads sources.
type RawSyncConfig struct {
	Hosted bool `toml:"hosted" json:"hosted"`
}

// ErrHostedRawMode identifies local work refused by the hosted raw setting.
var ErrHostedRawMode = errors.New("local parsing, extraction, embedding and replica pushes are disabled by [raw_sync] hosted; use raw-sync watch or backfill")

// HostedRawMode disables local derivation without changing archive custody.
func (c Config) HostedRawMode() bool { return c.RawSync.Hosted }
