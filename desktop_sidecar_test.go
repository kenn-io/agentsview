package agentsview_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrepareSidecarRestoresPricingSnapshotBeforeBuild(t *testing.T) {
	requireUnixShell(t)

	root := t.TempDir()
	installRepoFile(t, root, "desktop/scripts/prepare-sidecar.sh", 0o755)
	writeDesktopDevWorkspace(t, root)
	stubs := installUnixBuildStubs(t, root)

	out, err := runInWorkspace(
		t,
		root,
		stubs.env("AGENTSVIEW_VERSION=v1.2.3"),
		"bash",
		"desktop/scripts/prepare-sidecar.sh",
	)
	require.NoError(t, err, "%s", out)

	assertEventOrder(t, stubs.events(t),
		"npm run build",
		"go run ./internal/pricing/cmd/litellm-snapshot -restore",
		"go build -tags fts5",
	)
	require.FileExists(t, filepath.Join(
		root,
		"desktop",
		"src-tauri",
		"binaries",
		"agentsview-x86_64-unknown-linux-gnu",
	))
}

func TestPrepareSidecarStopsWhenPricingSnapshotRestoreFails(t *testing.T) {
	requireUnixShell(t)

	root := t.TempDir()
	installRepoFile(t, root, "desktop/scripts/prepare-sidecar.sh", 0o755)
	writeDesktopDevWorkspace(t, root)
	stubs := installUnixBuildStubs(t, root)

	out, err := runInWorkspace(
		t,
		root,
		stubs.env("AGENTSVIEW_VERSION=v1.2.3", "RESTORE_FAIL=1"),
		"bash",
		"desktop/scripts/prepare-sidecar.sh",
	)
	require.Error(t, err, "script should fail when snapshot restore fails: %s", out)

	events := stubs.events(t)
	assertEventContains(t, events,
		"go run ./internal/pricing/cmd/litellm-snapshot -restore")
	assertNoEventContains(t, events, "go build")
}
