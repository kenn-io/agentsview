package parser

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// codebuddyRewriteInPlace replaces path's contents with body, which must
// have the old size, and restores the old modification time, as an editor
// or a repair tool can.
func codebuddyRewriteInPlace(t *testing.T, path string, body []byte) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Len(t, body, int(info.Size()), "the rewrite keeps the size")
	require.NoError(t, os.WriteFile(path, body, info.Mode().Perm()))
	require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, info.Size(), after.Size())
	require.Equal(t, info.ModTime(), after.ModTime())
}

// A converted conversation reads request totals and images from its 4.x
// directory. Editing one of those files in place, keeping its size and
// modification time, still changes the fingerprint, so the conversation
// shows the corrected usage or the repaired image.
func TestCodeBuddyCBCFingerprintFollowsLegacyCompanionContents(t *testing.T) {
	for _, tc := range []struct {
		name, rel, before, after string
	}{
		{
			name:   "request totals",
			rel:    filepath.Join("messages", "a1.json"),
			before: `{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","statsSnapshot":{"inputTokens":300,"outputTokens":40}}}`,
			after:  `{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","statsSnapshot":{"inputTokens":300,"outputTokens":45}}}`,
		},
		{
			name:   "manifest",
			rel:    "index.json",
			before: `{"messages":[{"id":"a1","role":"assistant"}]}`,
			after:  `{"messages":[{"id":"a2","role":"assistant"}]}`,
		},
		{
			name:   "image",
			rel:    filepath.Join("assets", "pic.png"),
			before: "partial image bytes",
			after:  "complete image byte",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, project := cbcHome(t)
			projects := filepath.Dir(project)
			legacyRoot := t.TempDir()
			convDir := filepath.Join(legacyRoot, "history", "ws", "conv1")
			files := map[string]string{
				"index.json":                         `{"messages":[{"id":"a1","role":"assistant"}]}`,
				filepath.Join("messages", "a1.json"): `{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","statsSnapshot":{"inputTokens":300,"outputTokens":40}}}`,
				filepath.Join("assets", "pic.png"):   "partial image bytes",
			}
			files[tc.rel] = tc.before
			for rel, body := range files {
				writeCBC(t, filepath.Join(convDir, rel), body)
			}
			path := filepath.Join(project, "conv1.jsonl")
			writeCBC(t, path,
				`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}`,
				`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"old prompt"},{"type":"input_image","image":"codebuddy-asset://assets/pic.png"}]}`,
			)
			set := newCodeBuddyCBCSourceSet([]string{projects}, "")
			set.legacyRoots = []string{legacyRoot}
			source := set.sourceRef(projects, path)
			before, err := set.Fingerprint(t.Context(), source)
			require.NoError(t, err)

			codebuddyRewriteInPlace(t, filepath.Join(convDir, tc.rel), []byte(tc.after+"\n"))

			after, err := set.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			assert.NotEqual(t, before.Hash, after.Hash,
				"an in-place edit of a 4.x file the conversation reads changes the fingerprint")
		})
	}
}

// A transcript rewritten to reference another image, keeping its size and
// modification time, lists the new image, so the image arriving later
// changes the fingerprint and shows.
func TestCodeBuddyCBCBlobReferencesFollowInPlaceRewrite(t *testing.T) {
	home, project := cbcHome(t)
	projects := filepath.Dir(project)
	line := func(blob string) string {
		return `{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"image_blob_ref","blob_id":"` + blob + `","mime":"image/png"}]}`
	}
	first, second := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	path := filepath.Join(project, "run-1.jsonl")
	writeCBC(t, path, line(first))
	ids, err := codebuddyTranscriptBlobIDs(t.Context(), path)
	require.NoError(t, err)
	require.Equal(t, []string{first}, ids)

	codebuddyRewriteInPlace(t, path, []byte(line(second)+"\n"))
	ids, err = codebuddyTranscriptBlobIDs(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, []string{second}, ids, "the rewritten transcript's reference is read again")

	set := newCodeBuddyCBCSourceSet([]string{projects}, "")
	source := set.sourceRef(projects, path)
	before, err := set.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	png, err := base64.StdEncoding.DecodeString(cbcPNG)
	require.NoError(t, err)
	stored := filepath.Join(home, "blobs", "cd", second+".png")
	require.NoError(t, os.MkdirAll(filepath.Dir(stored), 0o755))
	require.NoError(t, os.WriteFile(stored, png, 0o600))
	after, err := set.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, after.Hash, "the newly referenced image arriving changes the fingerprint")
}

// An image unreadable during import shows a placeholder. Restoring its
// permissions keeps its size, yet both freshness checks change, so the
// conversation is parsed again and shows the image.
func TestCodeBuddyCBCFreshnessFollowsRestoredBlobReadability(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not deny reads on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of mode")
	}
	home, project := cbcHome(t)
	projects := filepath.Dir(project)
	blob := strings.Repeat("ab", 32)
	path := filepath.Join(project, "run-1.jsonl")
	writeCBC(t, path, `{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"image_blob_ref","blob_id":"`+blob+`","mime":"image/png"}]}`)
	png, err := base64.StdEncoding.DecodeString(cbcPNG)
	require.NoError(t, err)
	stored := filepath.Join(home, "blobs", "ab", blob+".png")
	require.NoError(t, os.MkdirAll(filepath.Dir(stored), 0o755))
	require.NoError(t, os.WriteFile(stored, png, 0o600))
	require.NoError(t, os.Chmod(stored, 0))
	t.Cleanup(func() { _ = os.Chmod(stored, 0o600) })

	set := newCodeBuddyCBCSourceSet([]string{projects}, "")
	source := set.sourceRef(projects, path)
	probe, ok := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy}).NewProvider(ProviderConfig{}).(*codebuddyProvider)
	require.True(t, ok)
	before, err := set.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	beforeStat := probe.ComputeMultiFileStatHash(path)
	require.NotZero(t, beforeStat)

	require.NoError(t, os.Chmod(stored, 0o644))
	after, err := set.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, after.Hash, "a readable image changes the fingerprint")
	assert.NotEqual(t, beforeStat, probe.ComputeMultiFileStatHash(path),
		"restoring permissions moves the change time the stat hash records")
}
