package parser

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The same converted conversation and 4.x folder mirrored under two roots,
// as each remote sync extracts into a new temporary directory, fingerprint
// alike, so an unchanged import skips the second write. Extraction keeps the
// archived modification times, so the copies share them.
func TestCodeBuddyCBCFingerprintIgnoresLegacyRootLocation(t *testing.T) {
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fingerprint := func() string {
		t.Helper()
		_, project := cbcHome(t)
		projects := filepath.Dir(project)
		legacyRoot := t.TempDir()
		convDir := filepath.Join(legacyRoot, "history", "ws", "conv1")
		writeCBC(t, filepath.Join(convDir, "index.json"),
			`{"messages":[{"id":"a1","role":"assistant"}]}`)
		writeCBC(t, filepath.Join(convDir, "messages", "a1.json"),
			`{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","statsSnapshot":{"inputTokens":300,"outputTokens":40}}}`)
		path := filepath.Join(project, "conv1.jsonl")
		writeCBC(t, path,
			`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}`,
			`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"go"}]}`,
		)
		for _, root := range []string{legacyRoot, projects} {
			require.NoError(t, filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				return os.Chtimes(p, stamp, stamp)
			}))
		}
		set := newCodeBuddyCBCSourceSet([]string{projects}, "")
		set.legacyRoots = []string{legacyRoot}
		got, err := set.Fingerprint(t.Context(), set.sourceRef(projects, path))
		require.NoError(t, err)
		return got.Hash
	}
	assert.Equal(t, fingerprint(), fingerprint())
}

// A remote sync's extraction keeps file times but not directory times, so the
// same 4.x folder extracted twice differs only in its directories' times. The
// fingerprint then stays the same, and the unchanged conversation is skipped.
func TestCodeBuddyCBCFingerprintIgnoresLegacyDirectoryTimes(t *testing.T) {
	_, project := cbcHome(t)
	projects := filepath.Dir(project)
	legacyRoot := t.TempDir()
	convDir := filepath.Join(legacyRoot, "history", "ws", "conv1")
	writeCBC(t, filepath.Join(convDir, "index.json"),
		`{"messages":[{"id":"a1","role":"assistant"}]}`)
	writeCBC(t, filepath.Join(convDir, "messages", "a1.json"),
		`{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","statsSnapshot":{"inputTokens":300,"outputTokens":40}}}`)
	writeCBC(t, filepath.Join(convDir, "assets", "pic.png"), "image bytes")
	path := filepath.Join(project, "conv1.jsonl")
	writeCBC(t, path,
		`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}`,
		`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"go"}]}`,
	)
	set := newCodeBuddyCBCSourceSet([]string{projects}, "")
	set.legacyRoots = []string{legacyRoot}
	source := set.sourceRef(projects, path)
	before, err := set.Fingerprint(t.Context(), source)
	require.NoError(t, err)

	later := time.Date(2027, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, dir := range []string{filepath.Join(convDir, "messages"), filepath.Join(convDir, "assets")} {
		require.NoError(t, os.Chtimes(dir, later, later))
	}
	after, err := set.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, before.Hash, after.Hash)
}

// A converted reply that carries no text but reports its 4.x request total
// as zero keeps a hidden usage row under its cb4: identity. Without that row,
// recovery of a partly unreadable 4.x folder would restore an older total.
func TestCodeBuddyConvertedEmptyReplyKeepsZeroTotal(t *testing.T) {
	home, project := cbcHome(t)
	writeCBC(t, filepath.Join(project, "conv1.jsonl"),
		`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/34"}}`,
		`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"go"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[],"providerData":{"agent":"codebuddy-ide-4.x","conversationRequestId":"req1","messageId":"a1","model":"glm-x","rawUsage":{"prompt_tokens":0,"completion_tokens":0}}}`,
		`{"id":"mu","parentId":"a1","type":"model-usage","timestamp":1767225603000,"providerData":{"agent":"codebuddy-ide-4.x","conversationRequestId":"req2","messageId":"a2","model":"glm-x","rawUsage":{"prompt_tokens":0,"completion_tokens":0}}}`,
	)
	main := cbcSession(t, parseCBCHome(t, home), "codebuddy:conv1")
	byID := map[string]ParsedMessage{}
	for _, m := range main.Messages {
		byID[m.SourceUUID] = m
	}
	for _, id := range []string{"cb4:req1:a1", "cb4:req2:a2"} {
		row, ok := byID[id]
		require.True(t, ok, "%s keeps its identity", id)
		assert.True(t, row.IsSystem, id)
		assert.Zero(t, row.OutputTokens, id)
		assert.True(t, row.HasOutputTokens, "%s reports a known zero", id)
	}
}

// Each remote sync extracts into a new temporary root. The recorded-id index
// cache keeps a bounded number of roots however many are scanned.
func TestCodeBuddyRecordedIDIndexesStayBounded(t *testing.T) {
	codebuddyRecordedIDIndexes.Lock()
	clear(codebuddyRecordedIDIndexes.byRoot)
	codebuddyRecordedIDIndexes.Unlock()
	t.Cleanup(func() {
		codebuddyRecordedIDIndexes.Lock()
		clear(codebuddyRecordedIDIndexes.byRoot)
		codebuddyRecordedIDIndexes.Unlock()
	})
	for range codebuddyRecordedIDIndexRoots * 3 {
		_, project := cbcHome(t)
		require.NoError(t, os.WriteFile(filepath.Join(project, "run.jsonl"),
			[]byte(`{"type":"session-meta","id":"m","sessionId":"run-x"}`+"\n"), 0o600))
		_, err := codebuddyCBCTranscriptsRecording(t.Context(), filepath.Dir(project), "run-x")
		require.NoError(t, err)
	}
	codebuddyRecordedIDIndexes.Lock()
	defer codebuddyRecordedIDIndexes.Unlock()
	assert.LessOrEqual(t, len(codebuddyRecordedIDIndexes.byRoot), codebuddyRecordedIDIndexRoots)
}
