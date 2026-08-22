//go:build !windows

package capture

import (
	"fmt"
	"os"
)

func ensureClaudeProviderRoot(providerRoot string) error {
	if err := os.MkdirAll(providerRoot, 0o700); err != nil {
		return fmt.Errorf("creating Claude provider root: %w", err)
	}
	return nil
}
