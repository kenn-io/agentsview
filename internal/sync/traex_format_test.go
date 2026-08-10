package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestIsCodexFormatAgent(t *testing.T) {
	tests := []struct {
		agent parser.AgentType
		want  bool
	}{
		{parser.AgentCodex, true},
		{parser.AgentTraeX, true},
		{parser.AgentClaude, false},
		{parser.AgentOpenCode, false},
		// The Trae IDE agent reads VS Code state, not rollout JSONL.
		{parser.AgentTrae, false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.agent), func(t *testing.T) {
			assert.Equal(t, tt.want, isCodexFormatAgent(tt.agent))
		})
	}
}

// TestDedupeTraeXPrefersDatedLayout covers the format-shaped duplicate
// resolution: one UUID with a live dated copy and a flat archived copy folds
// to the dated one, exactly as it does for Codex.
func TestDedupeTraeXPrefersDatedLayout(t *testing.T) {
	const uuid = "019fbcca-9fd4-7d20-83dc-0762b2f839b3"
	name := "rollout-2026-08-01T18-07-03-" + uuid + ".jsonl"
	files := []parser.DiscoveredFile{
		{
			Agent: parser.AgentTraeX,
			Path: filepath.Join(
				"/home/user/.trae/cli/archived_sessions", name,
			),
		},
		{
			Agent: parser.AgentTraeX,
			Path: filepath.Join(
				"/home/user/.trae/cli/sessions/2026/08/01", name,
			),
		},
	}

	got := dedupeDiscoveredFiles(files)

	require.Len(t, got, 1)
	assert.Equal(t, files[1].Path, got[0].Path)
}

// TestDedupeKeepsTraeXAndCodexSeparate guards the namespace split: the two
// agents share the rollout filename shape, so a UUID that exists under both
// must survive as two files rather than collapsing into one.
func TestDedupeKeepsTraeXAndCodexSeparate(t *testing.T) {
	const uuid = "019fbcca-9fd4-7d20-83dc-0762b2f839b3"
	name := "rollout-2026-08-01T18-07-03-" + uuid + ".jsonl"
	files := []parser.DiscoveredFile{
		{
			Agent: parser.AgentCodex,
			Path: filepath.Join(
				"/home/user/.codex/sessions/2026/08/01", name,
			),
		},
		{
			Agent: parser.AgentTraeX,
			Path: filepath.Join(
				"/home/user/.trae/cli/sessions/2026/08/01", name,
			),
		},
	}

	got := dedupeDiscoveredFiles(files)

	require.Len(t, got, 2)
	assert.ElementsMatch(
		t,
		[]string{files[0].Path, files[1].Path},
		[]string{got[0].Path, got[1].Path},
	)
}

func TestTraeXUsesIncrementalAppend(t *testing.T) {
	assert.True(t, usesIncrementalAppend(string(parser.AgentTraeX)))
	assert.True(t, usesIncrementalAppend(string(parser.AgentCodex)))
	assert.False(t, usesIncrementalAppend(string(parser.AgentGemini)))
}

// TestParseDiffLiveMtimeTraeXUsesTranscriptStat pins TraeX to the same
// transcript-only live mtime Codex uses for the parse-diff raced guard.
func TestParseDiffLiveMtimeTraeXUsesTranscriptStat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-2026-08-01T18-07-03-x.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)

	got, err := parseDiffLiveMtime(parser.AgentTraeX, path)
	require.NoError(t, err)
	assert.Equal(t, info.ModTime().UnixNano(), got)
}

// TestShouldSkipProviderSourceByDBIgnoresNonCodexFormat keeps the DB-aware
// skip scoped to the Codex format family.
func TestShouldSkipProviderSourceByDBIgnoresNonCodexFormat(t *testing.T) {
	e := &Engine{}
	assert.False(t, e.shouldSkipProviderSourceByDB(
		parser.DiscoveredFile{Agent: parser.AgentGemini, Path: "/tmp/x.jsonl"},
		parser.SourceFingerprint{},
		parser.ProviderSyncSemantics{},
	))
}

func TestShouldSkipProviderSourceByDBScopesCodexFormatAgent(t *testing.T) {
	database := openTestDB(t)
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	require.NoError(t, database.UpsertSession(db.Session{
		ID:        "codex:shared",
		Agent:     string(parser.AgentCodex),
		FilePath:  strPtr(path),
		FileSize:  int64Ptr(128),
		FileMtime: int64Ptr(456),
	}))
	require.NoError(t, database.SetSessionDataVersion(
		"codex:shared", db.CurrentDataVersion(),
	))

	e := &Engine{db: database}
	assert.False(t, e.shouldSkipProviderSourceByDB(
		parser.DiscoveredFile{Agent: parser.AgentTraeX, Path: path},
		parser.SourceFingerprint{Size: 128, MTimeNS: 456},
		parser.ProviderSyncSemantics{},
	), "Codex row must not satisfy TraeX database freshness")
}
