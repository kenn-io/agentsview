package config

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/atomicfile"
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
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return err
	}
	path := c.TelemetryScreenClaimsPath()
	lock := flock.New(path + ".lock")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := lock.TryLockContext(ctx, 10*time.Millisecond); err != nil {
		return fmt.Errorf("locking telemetry screen claims: %w", err)
	}
	defer func() { _ = lock.Unlock() }()
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
	backup, err := os.CreateTemp(c.DataDir, telemetryScreensFilename+".unreadable-*")
	if err != nil {
		return err
	}
	if err := backup.Close(); err != nil {
		_ = os.Remove(backup.Name())
		return err
	}
	if err := atomicfile.Replace(path, backup.Name()); err != nil {
		_ = os.Remove(backup.Name())
		return err
	}
	log.Printf("moved unreadable telemetry screen claims to %s", backup.Name())
	fields := strings.Fields(string(data))
	if len(fields) >= 3 && fields[0] == c.InstallationID && isClaimDay(fields[1]) {
		state := screenClaims{Version: 1, Days: make(map[string][]string)}
		if !addLegacyScreenClaims(state.Days, fields) {
			return nil
		}
		encoded, err := json.Marshal(state)
		if err != nil {
			log.Printf("skipping legacy telemetry screen claims: encoding claims: %v", err)
			return nil
		}
		return c.writeInstallationFile(telemetryScreensFilename, string(encoded))
	}
	return nil
}

// addLegacyScreenClaims converts validated "installation-id day screen..." claims.
func addLegacyScreenClaims(days map[string][]string, fields []string) bool {
	for _, screen := range fields[2:] {
		key, err := json.Marshal([]string{fields[0], "screen_viewed", screen})
		if err != nil {
			log.Printf("skipping legacy telemetry screen claims: encoding claim key: %v", err)
			return false
		}
		days[string(key)] = []string{fields[1]}
	}
	return true
}

func isClaimDay(value string) bool {
	_, err := time.Parse(time.DateOnly, value)
	return err == nil
}
