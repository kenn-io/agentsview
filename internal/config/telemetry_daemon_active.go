package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// telemetryDaemonActiveFilename holds "<installation ID> <UTC day>" for the
// last day a daemon_active event was accepted for sending.
const telemetryDaemonActiveFilename = "telemetry-daemon-active"

// ClaimDaemonActive calls send unless this installation already sent on the
// UTC day of now. The config lock serializes daemons sharing the data
// directory. It reserves the day in the record before calling send, so a
// record that cannot be read or written never lets restarts send again. When
// send fails, it undoes the reservation by putting back the previous record,
// which leaves the day open for a retry. If undoing also fails, the
// installation reports nothing for that day.
func (c *Config) ClaimDaemonActive(now time.Time, send func() error) error {
	day := now.UTC().Format(time.DateOnly)
	return c.withConfigLock(func() error {
		path := filepath.Join(c.DataDir, telemetryDaemonActiveFilename)
		previous, err := os.ReadFile(path)
		existed := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		id, recorded, _ := strings.Cut(strings.TrimSpace(string(previous)), " ")
		if id == c.InstallationID && recorded == day {
			return nil
		}
		if err := c.writeInstallationFile(telemetryDaemonActiveFilename, c.InstallationID+" "+day); err != nil {
			return err
		}
		if err := send(); err != nil {
			var undoErr error
			if existed {
				undoErr = c.writeInstallationFile(telemetryDaemonActiveFilename, strings.TrimSpace(string(previous)))
			} else {
				undoErr = os.Remove(path)
			}
			return errors.Join(err, undoErr)
		}
		return nil
	})
}
