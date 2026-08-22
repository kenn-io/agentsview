//go:build darwin

package capture

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCapturePreflightRejectsCaseAliasedDarwinResultPath(t *testing.T) {
	root := t.TempDir()
	captureDir := filepath.Join(root, "Capture-State")
	resultPath := filepath.Join(root, "capture-state", "manifest.json")

	_, err := validateResultPath(captureDir, resultPath)

	require.ErrorContains(t, err, "outside the capture directory")
}
