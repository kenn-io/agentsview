package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/kit/atomicfile"
)

const (
	installationIDFilename = "telemetry-install-id"
	// installationCreatedFilename holds "<id> <RFC 3339 time>" for IDs created
	// by this version. File mtimes are unusable: copying the data directory
	// keeps the ID but usually resets them.
	installationCreatedFilename = "telemetry-install-created"
)

func (c *Config) readInstallationID() error {
	path := filepath.Join(c.DataDir, installationIDFilename)
	data, err := readInstallationIDFile(path)
	if err != nil {
		return err
	}
	id := strings.TrimSpace(string(data))
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 16 {
		return fmt.Errorf("invalid installation identity in %q: installation ID must contain 32 hexadecimal characters; restore the original file to preserve identity, or remove it to create a new identity", path)
	}
	c.InstallationID = id
	c.InstallationCreatedAt = readInstallationCreatedAt(c.DataDir, id)
	return nil
}

// readInstallationCreatedAt returns zero, meaning an unknown install age, when
// the record is missing, malformed, or belongs to a different ID.
func readInstallationCreatedAt(dataDir, id string) time.Time {
	data, err := readInstallationIDFile(filepath.Join(dataDir, installationCreatedFilename))
	if err != nil {
		return time.Time{}
	}
	recordedID, value, ok := strings.Cut(strings.TrimSpace(string(data)), " ")
	if !ok || recordedID != id {
		return time.Time{}
	}
	createdAt, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return createdAt
}

func (c *Config) ensureInstallationID() error {
	if err := c.readInstallationID(); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return c.withConfigLock(func() error {
		if err := c.readInstallationID(); !errors.Is(err, os.ErrNotExist) {
			return err
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return fmt.Errorf("generating installation ID: %w", err)
		}
		id := hex.EncodeToString(random[:])
		createdAt := time.Now().UTC()
		// Record the time before publishing the ID so a published ID always has one.
		if err := c.writeInstallationFile(installationCreatedFilename, id+" "+createdAt.Format(time.RFC3339Nano)); err != nil {
			return err
		}
		if err := c.writeInstallationFile(installationIDFilename, id); err != nil {
			return err
		}
		c.InstallationID = id
		c.InstallationCreatedAt = createdAt
		return nil
	})
}

func (c *Config) writeInstallationFile(name, content string) error {
	file, err := os.CreateTemp(c.DataDir, ".installation-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := fmt.Fprintln(file, content); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return atomicfile.Replace(file.Name(), filepath.Join(c.DataDir, name))
}
