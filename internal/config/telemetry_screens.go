package config

import (
	"encoding/json/v2"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const telemetryScreensFilename = "telemetry-screen-views"

// screenClaims is the version 1 daily claims format kit reads and writes.
type screenClaims struct {
	Version int                 `json:"version"`
	Days    map[string][]string `json:"days"`
}

// readable applies the checks kit runs before every report; kit rejects
// every screen view while the file fails them.
func (s screenClaims) readable() bool {
	if s.Version != 1 || s.Days == nil {
		return false
	}
	for _, days := range s.Days {
		for _, day := range days {
			if !isClaimDay(day) {
				return false
			}
		}
	}
	return true
}

func (c *Config) TelemetryScreenClaimsPath() string {
	return filepath.Join(c.DataDir, telemetryScreensFilename)
}

// MigrateTelemetryScreenClaims converts legacy claims and replaces claims kit
// cannot read, before kit owns the file.
func (c *Config) MigrateTelemetryScreenClaims() error {
	return c.withConfigLock(func() error {
		path := c.TelemetryScreenClaimsPath()
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		state := screenClaims{Version: 1, Days: make(map[string][]string)}
		if strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
			var current screenClaims
			if json.Unmarshal(data, &current) == nil && current.readable() {
				return nil
			}
			log.Printf("resetting unreadable telemetry screen claims in %s", path)
		} else if err := addLegacyScreenClaims(state.Days, strings.Fields(string(data))); err != nil {
			return err
		}
		encoded, err := json.Marshal(state)
		if err != nil {
			return err
		}
		return c.writeInstallationFile(telemetryScreensFilename, string(encoded))
	})
}

// addLegacyScreenClaims reads "installation-id day screen..." claims; any
// other shape carries no claims forward.
func addLegacyScreenClaims(days map[string][]string, fields []string) error {
	if len(fields) < 2 || !isClaimDay(fields[1]) {
		return nil
	}
	for _, screen := range fields[2:] {
		key, err := json.Marshal([]string{fields[0], "screen_viewed", screen})
		if err != nil {
			return err
		}
		days[string(key)] = []string{fields[1]}
	}
	return nil
}

func isClaimDay(value string) bool {
	_, err := time.Parse(time.DateOnly, value)
	return err == nil
}
