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
// UTC day of now, and records the day only when send succeeds. The config
// lock serializes daemons sharing the data directory. When the lock or the
// record cannot be read, send is not called, so a broken data directory
// cannot turn restarts into repeated events.
func (c *Config) ClaimDaemonActive(now time.Time, send func() error) (claimed bool, err error) {
	day := now.UTC().Format(time.DateOnly)
	err = c.withConfigLock(func() error {
		data, err := os.ReadFile(filepath.Join(c.DataDir, telemetryDaemonActiveFilename))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		id, recorded, _ := strings.Cut(strings.TrimSpace(string(data)), " ")
		if id == c.InstallationID && recorded == day {
			return nil
		}
		if err := send(); err != nil {
			return err
		}
		claimed = true
		return c.writeInstallationFile(telemetryDaemonActiveFilename, c.InstallationID+" "+day)
	})
	return claimed, err
}
