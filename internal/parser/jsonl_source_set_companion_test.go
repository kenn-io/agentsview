package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// companionFor maps a transcript path to its sibling ".meta" companion. It is
// the shape a provider passes to WithCompanionFiles.
func companionFor(transcriptPath string) []string {
	return []string{transcriptPath + ".meta"}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o644))
}

func TestJSONLSourceSetCompanionFingerprintReflectsCompanionChange(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	transcript := filepath.Join(root, "session.jsonl")
	companion := transcript + ".meta"
	writeFile(t, transcript, `{"line":1}`+"\n")
	writeFile(t, companion, "v1")

	set := NewJSONLSourceSet(
		AgentClaude,
		[]string{root},
		WithCompanionFiles(companionFor),
	)

	sources, err := set.Discover(ctx)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	source := sources[0]

	before, err := set.Fingerprint(ctx, source)
	require.NoError(t, err)

	// Changing only the companion must change the source fingerprint, since the
	// companion size/mtime are folded into the transcript's freshness identity.
	writeFile(t, companion, "v2-larger-contents")
	after, err := set.Fingerprint(ctx, source)
	require.NoError(t, err)

	assert.NotEqual(t, before.Size, after.Size,
		"companion size should be folded into the fingerprint size")
	assert.NotEqual(t, before, after,
		"a companion change must alter the source fingerprint")
}

func TestJSONLSourceSetCompanionFingerprintHashChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	transcript := filepath.Join(root, "session.jsonl")
	companion := transcript + ".meta"
	writeFile(t, transcript, `{"line":1}`+"\n")
	writeFile(t, companion, "v1")

	set := NewJSONLSourceSet(
		AgentClaude,
		[]string{root},
		WithContentHashing(),
		WithCompanionFiles(companionFor),
	)
	sources, err := set.Discover(ctx)
	require.NoError(t, err)
	require.Len(t, sources, 1)

	before, err := set.Fingerprint(ctx, sources[0])
	require.NoError(t, err)
	require.NotEmpty(t, before.Hash)

	// Rewrite the companion to the same length so size and mtime resolution are
	// not the only signals; the content hash must still change.
	writeFile(t, companion, "v2")
	after, err := set.Fingerprint(ctx, sources[0])
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, after.Hash,
		"companion content must be mixed into the fingerprint hash")
}

func TestJSONLSourceSetCompanionChangedPathMapsToTranscript(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	transcript := filepath.Join(root, "session.jsonl")
	companion := transcript + ".meta"
	writeFile(t, transcript, `{"line":1}`+"\n")
	writeFile(t, companion, "meta")

	set := NewJSONLSourceSet(
		AgentClaude,
		[]string{root},
		WithCompanionFiles(companionFor),
	)

	changed, err := set.SourcesForChangedPath(ctx, ChangedPathRequest{
		Path:      companion,
		EventKind: "write",
		WatchRoot: root,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1,
		"a companion change must map back to its owning transcript")

	src, ok := changed[0].Opaque.(JSONLSource)
	require.True(t, ok)
	assert.Equal(t, transcript, src.Path)
}

func TestJSONLSourceSetCompanionWatchPlanIncludesCompanionGlob(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	transcript := filepath.Join(root, "session.jsonl")
	writeFile(t, transcript, `{"line":1}`+"\n")
	writeFile(t, transcript+".meta", "meta")

	set := NewJSONLSourceSet(
		AgentClaude,
		[]string{root},
		WithCompanionFiles(companionFor),
	)

	plan, err := set.WatchPlan(ctx)
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Contains(t, plan.Roots[0].IncludeGlobs, "session.jsonl.meta")
	assert.Contains(t, plan.Roots[0].IncludeGlobs, "*.jsonl")
}

func TestJSONLSourceSetWithoutCompanionsUnaffected(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	transcript := filepath.Join(root, "session.jsonl")
	writeFile(t, transcript, `{"line":1}`+"\n")

	set := NewJSONLSourceSet(AgentClaude, []string{root})
	sources, err := set.Discover(ctx)
	require.NoError(t, err)
	require.Len(t, sources, 1)

	fp, err := set.Fingerprint(ctx, sources[0])
	require.NoError(t, err)

	info, err := os.Stat(transcript)
	require.NoError(t, err)
	// Without a companion hook the fingerprint size is exactly the transcript
	// size, confirming the companion folding is inert when unconfigured.
	assert.Equal(t, info.Size(), fp.Size)
}
