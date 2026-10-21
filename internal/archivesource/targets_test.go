package archivesource

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestArchiveSourceCompletionTarget(t *testing.T) {
	source := Source{ManifestID: "manifest-one", Root: db.RawArchiveRoot{Provider: "claude"}}
	policy := config.Config{ResultContentBlockedCategories: []string{"Read", "Bash"}}
	first, err := completionTarget(source, policy)
	require.NoError(t, err)
	policy.ResultContentBlockedCategories = []string{"Bash", "Read", "Bash"}
	policy.ArchiveContent = config.ArchiveContentFull
	second, err := completionTarget(source, policy)
	require.NoError(t, err)
	assert.Equal(t, first, second, "equivalent retention policy must not reread every archived source")
	policy.ArchiveContent = config.ArchiveContentUsage
	changed, err := completionTarget(source, policy)
	require.NoError(t, err)
	assert.NotEqual(t, first, changed)
	policy.ArchiveContent = config.ArchiveContentFull
	source.ManifestID = "manifest-two"
	changed, err = completionTarget(source, policy)
	require.NoError(t, err)
	assert.NotEqual(t, first, changed)
}
