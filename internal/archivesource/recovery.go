package archivesource

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/docbank/docbank"
)

func readRecoveryMetadata(ctx context.Context, reader *docbank.BackupReader) error {
	data, err := readSmallExtra(ctx, reader, "application/inventory.json", 32<<20)
	if err != nil {
		return err
	}
	var inventory struct {
		Version int      `json:"version"`
		Files   []string `json:"files"`
	}
	if err := json.Unmarshal(data, &inventory); err != nil {
		return fmt.Errorf("reading recovery inventory: %w", err)
	}
	if inventory.Version != 1 {
		return fmt.Errorf("unsupported recovery inventory version %d", inventory.Version)
	}
	for _, required := range []string{"sessions.db", "telemetry-install-id", "recovery-settings.json"} {
		count := 0
		for _, name := range inventory.Files {
			if name == required {
				count++
			}
		}
		if count != 1 {
			return fmt.Errorf("recovery inventory must list %s exactly once", required)
		}
	}
	identity, err := readSmallExtra(ctx, reader, "application/telemetry-install-id", 1024)
	if err != nil {
		return err
	}
	if err := config.ValidateInstallationID(strings.TrimSpace(string(identity))); err != nil {
		return fmt.Errorf("invalid source installation identity: %w", err)
	}
	data, err = readSmallExtra(ctx, reader, "application/recovery-settings.json", 32<<20)
	if err != nil {
		return err
	}
	var settings struct {
		ArchiveContent   string `json:"archive_content"`
		ToolResultImages string `json:"tool_result_images"`
		LocalMachineName string `json:"local_machine_name"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return err
	}
	if strings.TrimSpace(settings.LocalMachineName) == "" {
		return errors.New("source machine label is missing")
	}
	_, contentErr := config.ParseArchiveContent(settings.ArchiveContent)
	_, imagesErr := config.ParseToolResultImages(settings.ToolResultImages)
	return errors.Join(contentErr, imagesErr)
}

func readSmallExtra(ctx context.Context, reader *docbank.BackupReader, name string, maxBytes int64) ([]byte, error) {
	object, err := reader.Extra(name)
	if err != nil {
		return nil, err
	}
	if object.Bytes > maxBytes {
		return nil, fmt.Errorf("archive metadata %s exceeds %d bytes", name, maxBytes)
	}
	var output bytes.Buffer
	if _, err := reader.CopyExtra(ctx, name, &output); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
