package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const installationFilename = "installation.json"
const legacyInstallationIDFilename = "telemetry-install-id"

type installationRecord struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Config) readInstallationID() error {
	if err := c.readInstallationRecord(); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return c.readLegacyInstallationID()
}

func (c *Config) readInstallationRecord() error {
	path := filepath.Join(c.DataDir, installationFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var record installationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return installationRecoveryError(path, err)
	}
	if err := validateInstallationID(record.ID); err != nil {
		return installationRecoveryError(path, err)
	}
	if strings.TrimSpace(record.Name) == "" {
		return installationRecoveryError(path, errors.New("initial display name must be non-empty"))
	}
	c.InstallationID = record.ID
	if !c.localMachineNameConfigured {
		c.LocalMachineName = record.Name
	}
	return nil
}

func (c *Config) readLegacyInstallationID() error {
	path := filepath.Join(c.DataDir, legacyInstallationIDFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	id := strings.TrimSpace(string(data))
	if err := validateInstallationID(id); err != nil {
		return installationRecoveryError(path, err)
	}
	c.InstallationID = id
	return nil
}

func validateInstallationID(id string) error {
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 16 {
		return errors.New("installation ID must contain 32 hexadecimal characters")
	}
	return nil
}

func installationRecoveryError(path string, err error) error {
	return fmt.Errorf("invalid installation identity in %q: %w; restore the original file to preserve identity, or remove %s and %s from %q to create a new identity",
		path, err, installationFilename, legacyInstallationIDFilename, filepath.Dir(path))
}

func (c *Config) ensureInstallationID() error {
	if err := c.readInstallationRecord(); err == nil {
		return c.removeLegacyInstallationID()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return c.withConfigLock(func() error {
		if err := c.readInstallationRecord(); err == nil {
			return c.removeLegacyInstallationID()
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := c.readLegacyInstallationID(); errors.Is(err, os.ErrNotExist) {
			var random [16]byte
			if _, err := rand.Read(random[:]); err != nil {
				return fmt.Errorf("generating installation ID: %w", err)
			}
			c.InstallationID = hex.EncodeToString(random[:])
		} else if err != nil {
			return err
		}
		if strings.TrimSpace(c.LocalMachineName) == "" {
			return errors.New("initial display name must be non-empty")
		}
		data, err := json.Marshal(installationRecord{ID: c.InstallationID, Name: c.LocalMachineName})
		if err != nil {
			return fmt.Errorf("encoding installation identity: %w", err)
		}
		file, err := os.CreateTemp(c.DataDir, ".installation-*")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		if _, err := file.Write(data); err != nil {
			file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		if err := os.Rename(file.Name(), filepath.Join(c.DataDir, installationFilename)); err != nil {
			return err
		}
		if err := c.syncInstallationDir(); err != nil {
			return err
		}
		return c.removeLegacyInstallationID()
	})
}

func (c *Config) removeLegacyInstallationID() error {
	path := filepath.Join(c.DataDir, legacyInstallationIDFilename)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	// A prior process may have stopped after renaming the new record. Make it
	// durable before retiring the old ID so a later reset cannot resurrect it.
	if err := c.syncInstallationDir(); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing adopted installation identity: %w", err)
	}
	return c.syncInstallationDir()
}

func (c *Config) syncInstallationDir() error {
	dir, err := os.Open(c.DataDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil && runtime.GOOS != "windows" {
		return fmt.Errorf("syncing installation directory: %w", err)
	}
	return nil
}
