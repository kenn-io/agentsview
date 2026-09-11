package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const installationIDFilename = "telemetry-install-id"

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
	return nil
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
		file, err := os.CreateTemp(c.DataDir, ".installation-*")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		if _, err := fmt.Fprintln(file, id); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		if err := os.Rename(file.Name(), filepath.Join(c.DataDir, installationIDFilename)); err != nil {
			return err
		}
		c.InstallationID = id
		return nil
	})
}
