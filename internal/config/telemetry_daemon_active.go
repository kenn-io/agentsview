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

const telemetrySetupFinishedFilename = "telemetry-setup-finished"

// ClaimDaemonActive calls send unless this installation already sent on the
// UTC day of now. A record-specific lock serializes daemons sharing the data
// directory. It reserves the day in the record before calling send, so a
// record that cannot be read or written never lets restarts send again. When
// send fails, it undoes the reservation by putting back the previous record,
// which leaves the day open for a retry. If undoing also fails, the
// installation reports nothing for that day.
func (c *Config) ClaimDaemonActive(now time.Time, send func() error) error {
	day := now.UTC().Format(time.DateOnly)
	return c.claimTelemetry(telemetryDaemonActiveFilename, c.InstallationID+" "+day, send)
}

// ClaimSetupFinished sends once per installation, across daemon restarts.
func (c *Config) ClaimSetupFinished(send func() error) error {
	return c.claimTelemetry(telemetrySetupFinishedFilename, c.InstallationID, send)
}

// SetupFinishedClaimed reports whether this installation already queued setup.
// It takes the claim's lock, so it waits for a pending send and its rollback.
func (c *Config) SetupFinishedClaimed() (claimed bool, err error) {
	path := filepath.Join(c.DataDir, telemetrySetupFinishedFilename)
	err = c.withFileLock(path+".lock", func() error {
		record, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		claimed = err == nil && strings.TrimSpace(string(record)) == c.InstallationID
		return err
	})
	return claimed, err
}

func (c *Config) claimTelemetry(filename, record string, send func() error) error {
	path := filepath.Join(c.DataDir, filename)
	return c.withFileLock(path+".lock", func() error {
		previous, err := os.ReadFile(path)
		existed := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if strings.TrimSpace(string(previous)) == record {
			return nil
		}
		if err := c.writeInstallationFile(filename, record); err != nil {
			return err
		}
		if err := send(); err != nil {
			var undoErr error
			if existed {
				undoErr = c.writeInstallationFile(filename, strings.TrimSpace(string(previous)))
			} else {
				undoErr = os.Remove(path)
			}
			return errors.Join(err, undoErr)
		}
		return nil
	})
}
