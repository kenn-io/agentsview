//go:build windows

package capture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func ensureClaudeProviderRoot(providerRoot string) error {
	parent := filepath.Dir(filepath.Clean(providerRoot))
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("creating Claude provider parent: %w", err)
	}
	if err := verifyCaptureParentSafety(providerRoot); err != nil {
		return fmt.Errorf("validating Claude provider parent: %w", err)
	}
	err := createSecureCaptureDirectory(providerRoot)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("creating Claude provider root: %w", err)
	}
	if err := verifyExistingClaudeProviderRoot(providerRoot); err != nil {
		return fmt.Errorf("validating Claude provider root: %w", err)
	}
	return nil
}
