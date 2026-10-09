package config

import (
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const telemetryScreensFilename = "telemetry-screen-views"

func (c *Config) TelemetryScreenClaimsPath() string {
	return filepath.Join(c.DataDir, telemetryScreensFilename)
}

// MigrateTelemetryScreenClaims converts legacy claims before kit owns the file.
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
		if strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
			return nil
		}
		fields := strings.Fields(string(data))
		state := struct {
			Version int                 `json:"version"`
			Days    map[string][]string `json:"days"`
		}{Version: 1, Days: make(map[string][]string)}
		if len(fields) >= 2 {
			if _, err := time.Parse(time.DateOnly, fields[1]); err == nil {
				for _, screen := range fields[2:] {
					key, err := json.Marshal([]string{fields[0], "screen_viewed", screen})
					if err != nil {
						return err
					}
					state.Days[string(key)] = []string{fields[1]}
				}
			}
		}
		encoded, err := json.Marshal(state)
		if err != nil {
			return err
		}
		return c.writeInstallationFile(telemetryScreensFilename, string(encoded))
	})
}
