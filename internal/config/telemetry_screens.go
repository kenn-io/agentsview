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

// MigrateTelemetryScreenClaims converts legacy claims and moves unreadable claims aside.
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
		var current screenClaims
		if json.Unmarshal(data, &current) == nil && current.readable() {
			return nil
		}
		fields := strings.Fields(string(data))
		if len(fields) >= 3 && isClaimDay(fields[1]) {
			state := screenClaims{Version: 1, Days: make(map[string][]string)}
			if err := addLegacyScreenClaims(state.Days, fields); err != nil {
				return err
			}
			encoded, err := json.Marshal(state)
			if err != nil {
				return err
			}
			return c.writeInstallationFile(telemetryScreensFilename, string(encoded))
		}
		unreadablePath := path + ".unreadable"
		if err := os.Rename(path, unreadablePath); err != nil {
			return err
		}
		log.Printf("moved unreadable telemetry screen claims to %s", unreadablePath)
		return nil
	})
}

// addLegacyScreenClaims converts validated "installation-id day screen..." claims.
func addLegacyScreenClaims(days map[string][]string, fields []string) error {
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
