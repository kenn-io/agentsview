package capture

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
)

func TestOversizedManifestDoesNotReplaceRecoverableState(t *testing.T) {
	root := t.TempDir()
	state, err := createState(filepath.Join(root, "capture"), manifest{
		OccurrenceID:      "bounded-manifest",
		Provider:          string(ProviderClaude),
		ProviderSessionID: "11111111-1111-4111-8111-111111111111",
		ProviderRoot:      filepath.Join(root, "provider"),
		ProviderWorkDir:   filepath.Join(root, "work"),
		StartedAt:         time.Now().UTC(),
		Invocation:        invocationName(ProviderClaude),
		Limits:            DefaultLimits(),
	})
	require.NoError(t, err)
	t.Cleanup(state.close)

	for i := range state.manifest.Limits.MaxSources {
		state.manifest.Sources = append(state.manifest.Sources, TranscriptSource{
			SessionID: fmt.Sprintf("agent-%03d", i),
			RawSource: artifact.RawSourceRef{
				Hash: strings.Repeat("a", 64), Size: 1,
				MediaType: "application/jsonl",
				Path: fmt.Sprintf(
					"claude/projects/project/session/subagents/%03d-%s.jsonl",
					i, strings.Repeat("x", 2048),
				),
			},
		})
	}

	err = state.saveManifest()

	require.Error(t, err)
	assert.Equal(t, ReasonSourceLimit, reasonForError(err, ReasonIngestFailed))
	recovered, readErr := readCaptureManifest(state.dir)
	require.NoError(t, readErr)
	assert.Empty(t, recovered.Sources)
}
