package agentsview_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDockerfileRequiresPricingSnapshotBeforeBuild(t *testing.T) {
	contents, err := os.ReadFile("Dockerfile")
	require.NoError(t, err)

	dockerfile := string(contents)
	restoreIndex := strings.Index(dockerfile,
		"go run ./internal/pricing/cmd/litellm-snapshot -restore")
	buildIndex := strings.Index(dockerfile, "go build -tags fts5")
	require.NotEqual(t, -1, restoreIndex,
		"Dockerfile must restore the pricing snapshot")
	require.NotEqual(t, -1, buildIndex,
		"Dockerfile must build agentsview")
	assert.Less(t, restoreIndex, buildIndex,
		"Dockerfile must restore the pricing snapshot before compiling")
}
