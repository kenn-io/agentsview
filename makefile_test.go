package agentsview_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMakeBuildRestoresPricingSnapshot(t *testing.T) {
	contents, err := os.ReadFile("Makefile")
	require.NoError(t, err)

	deps := makefileTargetDeps(t, string(contents), "build")
	assert.Contains(t, deps, "pricing-snapshot")
	assert.Contains(t, deps, "frontend")
}

func TestMakeTestTargetsRestorePricingSnapshot(t *testing.T) {
	contents, err := os.ReadFile("Makefile")
	require.NoError(t, err)

	for _, target := range []string{
		"test",
		"test-short",
		"test-postgres",
		"test-postgres-ci",
		"test-ssh",
		"test-ssh-ci",
	} {
		deps := makefileTargetDeps(t, string(contents), target)
		assert.Contains(t, deps, "pricing-snapshot", "%s", target)
	}
}

func TestMakePricingSnapshotDelegatesToSnapshotTool(t *testing.T) {
	contents, err := os.ReadFile("Makefile")
	require.NoError(t, err)

	recipe := makefileTargetRecipe(t, string(contents), "pricing-snapshot")
	assert.Contains(t, recipe, "go run ./internal/pricing/cmd/litellm-snapshot -restore")
}

func TestMakeDevRestoresPricingSnapshot(t *testing.T) {
	contents, err := os.ReadFile("Makefile")
	require.NoError(t, err)

	deps := makefileTargetDeps(t, string(contents), "dev")
	assert.Contains(t, deps, "pricing-snapshot")
}

func TestMakeTidyRestoresPricingSnapshot(t *testing.T) {
	contents, err := os.ReadFile("Makefile")
	require.NoError(t, err)

	deps := makefileTargetDeps(t, string(contents), "tidy")
	assert.Contains(t, deps, "pricing-snapshot")
}

func makefileTargetDeps(t *testing.T, contents, target string) []string {
	t.Helper()

	prefix := target + ":"
	for line := range strings.SplitSeq(contents, "\n") {
		if after, ok := strings.CutPrefix(line, prefix); ok {
			return strings.Fields(strings.TrimSpace(after))
		}
	}

	require.Failf(t, "missing make target", "target %q was not found", target)
	return nil
}

func makefileTargetRecipe(t *testing.T, contents, target string) string {
	t.Helper()

	lines := strings.Split(contents, "\n")
	var out []string
	inTarget := false
	prefix := target + ":"
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			inTarget = true
			continue
		}
		if !inTarget {
			continue
		}
		if line == "" {
			out = append(out, line)
			continue
		}
		if !strings.HasPrefix(line, "\t") {
			break
		}
		out = append(out, line)
	}

	require.NotEmpty(t, out, "target %q recipe not found", target)
	return strings.Join(out, "\n")
}
