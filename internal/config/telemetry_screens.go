package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const telemetryScreensFilename = "telemetry-screen-views"

// ClaimScreenView serializes enqueueing and persists only accepted events.
func (c *Config) ClaimScreenView(screen string, send func() error) (day string, claimed bool, err error) {
	sendAttempted := false
	err = c.withConfigLock(func() error {
		day = time.Now().UTC().Format(time.DateOnly)
		data, err := os.ReadFile(filepath.Join(c.DataDir, telemetryScreensFilename))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		fields := strings.Fields(string(data))
		if len(fields) < 2 || fields[0] != c.InstallationID || fields[1] != day {
			fields = []string{c.InstallationID, day}
		}
		if slices.Contains(fields[2:], screen) {
			return nil
		}
		sendAttempted = true
		if err := send(); err != nil {
			return err
		}
		claimed = true
		if err := c.writeInstallationFile(telemetryScreensFilename, strings.Join(append(fields, screen), " ")); err != nil {
			return err
		}
		return nil
	})
	if err != nil && !sendAttempted {
		day = time.Now().UTC().Format(time.DateOnly)
		if sendErr := send(); sendErr != nil {
			return day, false, sendErr
		}
		return day, true, err
	}
	return day, claimed, err
}
