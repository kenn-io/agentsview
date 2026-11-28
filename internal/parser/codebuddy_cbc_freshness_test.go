package parser

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A saved tool output rewritten in place can keep its size and modification
// time. The fingerprint still changes, so the archived text is refreshed.
func TestCodeBuddyCBCFingerprintFollowsSavedOutputContents(t *testing.T) {
	_, project := cbcHome(t)
	projects := filepath.Dir(project)
	path := filepath.Join(project, "run-1.jsonl")
	writeCBC(t, path, `{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"hi"}]}`)
	output := filepath.Join(project, "run-1", "tool-results", "call-1.txt")
	writeCBC(t, output, "first output")
	info, err := os.Stat(output)
	require.NoError(t, err)
	set := newCodeBuddyCBCSourceSet([]string{projects}, "")
	source := set.sourceRef(projects, path)
	before, err := set.Fingerprint(t.Context(), source)
	require.NoError(t, err)

	writeCBC(t, output, "other output")
	require.NoError(t, os.Chtimes(output, info.ModTime(), info.ModTime()))
	rewritten, err := os.Stat(output)
	require.NoError(t, err)
	require.Equal(t, info.Size(), rewritten.Size())
	require.Equal(t, info.ModTime(), rewritten.ModTime())

	after, err := set.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, before.Size, after.Size)
	assert.Equal(t, before.MTimeNS, after.MTimeNS)
	assert.NotEqual(t, before.Hash, after.Hash,
		"a same-size rewrite with the old modification time changes the fingerprint")
}

// The fingerprint's size and modification time are the transcript's, so a
// saved output written later does not make the transcript look newer, while
// the hash still notices the output changing.
func TestCodeBuddyCBCFingerprintKeepsTranscriptTime(t *testing.T) {
	_, project := cbcHome(t)
	projects := filepath.Dir(project)
	path := filepath.Join(project, "run-1.jsonl")
	writeCBC(t, path, `{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"hi"}]}`)
	output := filepath.Join(project, "run-1", "tool-results", "call-1.txt")
	writeCBC(t, output, "first output")
	now := time.Now()
	transcriptTime := now.Add(-time.Hour)
	require.NoError(t, os.Chtimes(path, transcriptTime, transcriptTime))
	require.NoError(t, os.Chtimes(output, now, now))
	transcript, err := os.Stat(path)
	require.NoError(t, err)
	set := newCodeBuddyCBCSourceSet([]string{projects}, "")
	source := set.sourceRef(projects, path)

	before, err := set.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, transcript.ModTime().UnixNano(), before.MTimeNS)
	assert.Equal(t, transcript.Size(), before.Size)

	writeCBC(t, output, "a longer second output")
	later := now.Add(time.Minute)
	require.NoError(t, os.Chtimes(output, later, later))
	after, err := set.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, transcript.ModTime().UnixNano(), after.MTimeNS)
	assert.Equal(t, transcript.Size(), after.Size)
	assert.NotEqual(t, before.Hash, after.Hash, "a changed saved output changes the hash")
}

// An image missing from the blob store shows a placeholder. Both freshness
// checks change when it appears, so an unchanged transcript is parsed again
// and shows the image.
func TestCodeBuddyCBCFreshnessFollowsReferencedBlobs(t *testing.T) {
	home, project := cbcHome(t)
	projects := filepath.Dir(project)
	blob := strings.Repeat("ab", 32)
	path := filepath.Join(project, "run-1.jsonl")
	writeCBC(t, path, `{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"image_blob_ref","blob_id":"`+blob+`","mime":"image/png"}]}`)
	set := newCodeBuddyCBCSourceSet([]string{projects}, "")
	source := set.sourceRef(projects, path)
	probe, ok := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy}).NewProvider(ProviderConfig{}).(*codebuddyProvider)
	require.True(t, ok)
	before, err := set.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	beforeStat := probe.ComputeMultiFileStatHash(path)
	require.NotZero(t, beforeStat)

	png, err := base64.StdEncoding.DecodeString(cbcPNG)
	require.NoError(t, err)
	stored := filepath.Join(home, "blobs", "ab", blob+".png")
	require.NoError(t, os.MkdirAll(filepath.Dir(stored), 0o755))
	require.NoError(t, os.WriteFile(stored, png, 0o600))
	after, err := set.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, after.Hash, "a restored image changes the fingerprint")
	assert.NotEqual(t, beforeStat, probe.ComputeMultiFileStatHash(path),
		"a restored image changes the stat hash")
}
