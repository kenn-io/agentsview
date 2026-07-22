package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestJSONLSourceSetCompanionWatchRootsStayBoundedByConfiguredRoots(t *testing.T) {
	measure := func(t *testing.T, transcriptCount int) (int, float64) {
		t.Helper()
		root := t.TempDir()
		for i := range transcriptCount {
			transcript := filepath.Join(root, fmt.Sprintf("session-%04d.jsonl", i))
			writeFile(t, transcript, "{}\n")
			writeFile(t, transcript+".meta", "meta")
		}
		set := NewJSONLSourceSet(
			AgentClaude,
			[]string{root},
			WithCompanionFiles(companionFor),
		)

		var roots []WatchRoot
		allocs := testing.AllocsPerRun(20, func() {
			var err error
			roots, err = set.WatchRoots(context.Background())
			require.NoError(t, err)
		})
		return len(roots), allocs
	}

	smallRoots, smallAllocs := measure(t, 1)
	largeRoots, largeAllocs := measure(t, 500)
	assert.Equal(t, smallRoots, largeRoots,
		"root-plan cardinality must depend on configured roots, not transcripts")
	assert.Equal(t, smallAllocs, largeAllocs,
		"root planning allocations must not scale with transcript companions")
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

// A companion event must resolve its owning transcript by inverse path
// derivation, not by scanning every discovered transcript's companion list —
// per-event work must stay bounded by the changed batch, not archive size.
func TestJSONLSourceSetCompanionChangedPathSkipsForwardScan(t *testing.T) {
	measure := func(t *testing.T, transcriptCount int) int {
		t.Helper()
		root := t.TempDir()
		for i := range transcriptCount {
			transcript := filepath.Join(root, fmt.Sprintf("session-%04d.jsonl", i))
			writeFile(t, transcript, "{}\n")
			writeFile(t, transcript+".meta", "meta")
		}
		forwardCalls := 0
		set := NewJSONLSourceSet(
			AgentClaude,
			[]string{root},
			WithCompanionFiles(func(transcriptPath string) []string {
				forwardCalls++
				return companionFor(transcriptPath)
			}),
			WithCompanionTranscript(func(companionPath string) (string, bool) {
				transcript, ok := strings.CutSuffix(companionPath, ".meta")
				return transcript, ok
			}),
		)

		companion := filepath.Join(root, "session-0000.jsonl.meta")
		changed, err := set.SourcesForChangedPath(context.Background(), ChangedPathRequest{
			Path:      companion,
			EventKind: "write",
			WatchRoot: root,
		})
		require.NoError(t, err)
		require.Len(t, changed, 1)
		src, ok := changed[0].Opaque.(JSONLSource)
		require.True(t, ok)
		assert.Equal(t, filepath.Join(root, "session-0000.jsonl"), src.Path)
		return forwardCalls
	}

	small := measure(t, 2)
	large := measure(t, 40)
	assert.Equal(t, small, large,
		"companion mapping must not fan the forward hook out over the archive")
	assert.LessOrEqual(t, small, 1,
		"the forward hook is at most a single consistency check per event")
}

// Changed-path resolution must not materialize the archive: with the default
// path-derived keys, the discovered set cannot hold a different preferred ref
// for a candidate, so per-event work stays bounded by the changed path for
// both transcript and companion events.
func TestJSONLSourceSetChangedPathSkipsArchiveDiscovery(t *testing.T) {
	measure := func(t *testing.T, transcriptCount int) (int, int) {
		t.Helper()
		root := t.TempDir()
		for i := range transcriptCount {
			transcript := filepath.Join(root, fmt.Sprintf("session-%04d.jsonl", i))
			writeFile(t, transcript, "{}\n")
			writeFile(t, transcript+".meta", "meta")
		}
		includeChecks := 0
		set := NewJSONLSourceSet(
			AgentClaude,
			[]string{root},
			WithIncludePath(func(root, path string) bool {
				includeChecks++
				return true
			}),
			WithCompanionFiles(companionFor),
			WithCompanionTranscript(func(companionPath string) (string, bool) {
				transcript, ok := strings.CutSuffix(companionPath, ".meta")
				return transcript, ok
			}),
		)

		resolve := func(path string) {
			changed, err := set.SourcesForChangedPath(
				context.Background(),
				ChangedPathRequest{Path: path, EventKind: "write", WatchRoot: root},
			)
			require.NoError(t, err)
			require.Len(t, changed, 1)
		}
		transcript := filepath.Join(root, "session-0000.jsonl")
		resolve(transcript)
		transcriptChecks := includeChecks
		includeChecks = 0
		resolve(transcript + ".meta")
		return transcriptChecks, includeChecks
	}

	smallTranscript, smallCompanion := measure(t, 2)
	largeTranscript, largeCompanion := measure(t, 40)
	assert.Equal(t, smallTranscript, largeTranscript,
		"transcript events must not scan the archive")
	assert.Equal(t, smallCompanion, largeCompanion,
		"companion events must not scan the archive")
}
