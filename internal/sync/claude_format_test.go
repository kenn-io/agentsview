package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestIsClaudeFormatTranscript(t *testing.T) {
	tests := []struct {
		name  string
		agent parser.AgentType
		path  string
		want  bool
	}{
		{"claude transcript", parser.AgentClaude, "/r/proj/abc.jsonl", true},
		{"icodemate cli transcript", parser.AgentIcodemate, "/r/proj/abc.jsonl", true},
		{"icodemate opencode container", parser.AgentIcodemate, "/r/icodemate.db", false},
		{"icodemate storage json", parser.AgentIcodemate, "/r/storage/session_diff/x.json", false},
		{"openclaude stays outside the set", parser.AgentOpenClaude, "/r/proj/abc.jsonl", false},
		{"codex is not claude format", parser.AgentCodex, "/r/proj/abc.jsonl", false},
		{"extension-less path", parser.AgentClaude, "/r/proj/abc", false},
		{"bare .jsonl name has no session", parser.AgentClaude, "/r/proj/.jsonl", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isClaudeFormatTranscript(tt.agent, tt.path))
		})
	}
}

func TestClaudeFormatArchiveSessionID(t *testing.T) {
	tests := []struct {
		name  string
		agent parser.AgentType
		raw   string
		want  string
	}{
		{"claude keeps raw id", parser.AgentClaude, "abc-123", "abc-123"},
		{"icodemate gains prefix", parser.AgentIcodemate, "abc-123", "icodemate:abc-123"},
		{"icodemate prefix not doubled", parser.AgentIcodemate, "icodemate:abc", "icodemate:abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, claudeFormatArchiveSessionID(tt.agent, tt.raw))
		})
	}
}

func TestUsesCompositeSidecarFreshness(t *testing.T) {
	require.True(t, usesCompositeSidecarFreshness(
		parser.AgentIcodemate, "/r/proj/abc.jsonl",
	))
	require.False(t, usesCompositeSidecarFreshness(
		parser.AgentClaude, "/r/proj/abc.jsonl",
	), "Claude must keep plain-file freshness")
	require.False(t, usesCompositeSidecarFreshness(
		parser.AgentIcodemate, "/r/icodemate.db",
	), "OpenCode container storage is not sidecar-composite")
}

// writeTranscriptFile creates a transcript of the given size with a fixed
// mtime and returns a discovered-file record for it.
func writeTranscriptFile(
	t *testing.T, dir, name string, size int, mtime time.Time,
	agent parser.AgentType,
) parser.DiscoveredFile {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, make([]byte, size), 0o644))
	require.NoError(t, os.Chtimes(path, mtime, mtime))
	return parser.DiscoveredFile{Path: path, Agent: agent}
}

// TestPreferClaudeDiscoveredFile pins the same-session duplicate policy
// documented on claudeFormatTranscriptPreference: Claude ranks append
// progress (size) ahead of recency, while ICodeMate CLI transcripts are
// rewritten in place, so recency (mtime) outranks size and a newer shortened
// rewrite beats a larger stale copy.
func TestPreferClaudeDiscoveredFile(t *testing.T) {
	dir := t.TempDir()
	older := time.Now().Add(-time.Hour).Truncate(time.Second)
	newer := older.Add(30 * time.Minute)

	tests := []struct {
		name       string
		agent      parser.AgentType
		candidate  parser.DiscoveredFile
		current    parser.DiscoveredFile
		wantPrefer bool
	}{
		{
			name:  "claude larger older copy wins",
			agent: parser.AgentClaude,
			candidate: writeTranscriptFile(
				t, dir, "c1.jsonl", 2048, older, parser.AgentClaude,
			),
			current: writeTranscriptFile(
				t, dir, "c2.jsonl", 1024, newer, parser.AgentClaude,
			),
			wantPrefer: true,
		},
		{
			name:  "claude equal size falls back to mtime",
			agent: parser.AgentClaude,
			candidate: writeTranscriptFile(
				t, dir, "c3.jsonl", 1024, newer, parser.AgentClaude,
			),
			current: writeTranscriptFile(
				t, dir, "c4.jsonl", 1024, older, parser.AgentClaude,
			),
			wantPrefer: true,
		},
		{
			name:  "icodemate newer shortened rewrite wins",
			agent: parser.AgentIcodemate,
			candidate: writeTranscriptFile(
				t, dir, "i1.jsonl", 512, newer, parser.AgentIcodemate,
			),
			current: writeTranscriptFile(
				t, dir, "i2.jsonl", 4096, older, parser.AgentIcodemate,
			),
			wantPrefer: true,
		},
		{
			name:  "icodemate equal mtime falls back to size",
			agent: parser.AgentIcodemate,
			candidate: writeTranscriptFile(
				t, dir, "i3.jsonl", 4096, older, parser.AgentIcodemate,
			),
			current: writeTranscriptFile(
				t, dir, "i4.jsonl", 512, older, parser.AgentIcodemate,
			),
			wantPrefer: true,
		},
		{
			name:  "unstatable candidate never wins",
			agent: parser.AgentClaude,
			candidate: parser.DiscoveredFile{
				Path:  filepath.Join(dir, "missing.jsonl"),
				Agent: parser.AgentClaude,
			},
			current: writeTranscriptFile(
				t, dir, "c5.jsonl", 1, older, parser.AgentClaude,
			),
			wantPrefer: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(
				t, tt.wantPrefer,
				preferClaudeDiscoveredFile(tt.candidate, tt.current),
			)
		})
	}
}

// TestClaudeCandidateHasAppendProgress pins the committed-copy escape hatch:
// only strict transcript growth counts as append progress, so an equal-size
// or shorter copy can never displace the committed source.
func TestClaudeCandidateHasAppendProgress(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().Truncate(time.Second)
	small := writeTranscriptFile(t, dir, "a.jsonl", 100, at, parser.AgentIcodemate)
	equal := writeTranscriptFile(t, dir, "b.jsonl", 100, at, parser.AgentIcodemate)
	large := writeTranscriptFile(t, dir, "c.jsonl", 200, at, parser.AgentIcodemate)

	require.True(t, claudeCandidateHasAppendProgress(large, small))
	require.False(t, claudeCandidateHasAppendProgress(equal, small))
	require.False(t, claudeCandidateHasAppendProgress(small, large))
}

// TestProtectedFileSessionCountDisabledIcodemate pins the resync
// empty-discovery safety accounting: with the ICodeMate provider enabled,
// its CLI JSONL rows stay protected while container rows are excluded; with
// the provider disabled, nothing is discovered and every ICodeMate row must
// leave the protected count so a rebuild can complete while preserving the
// disabled provider's sessions. The scoped variant covers contributor-backed
// rebuild accounting.
func TestProtectedFileSessionCountDisabledIcodemate(t *testing.T) {
	database := openTestDB(t)
	containerPath := "/data/icodemate/icodemate.db"
	cliPath := "/data/icodemate/cli/projects/proj/abc.jsonl"
	claudePath := "/data/claude/projects/proj/def.jsonl"
	for _, sess := range []db.Session{
		{
			ID: "icodemate:container-1", Project: "proj", Machine: "local",
			Agent: "icodemate", FilePath: &containerPath, MessageCount: 1,
		},
		{
			ID: "icodemate:cli-1", Project: "proj", Machine: "local",
			Agent: "icodemate", FilePath: &cliPath, MessageCount: 1,
		},
		{
			ID: "claude-1", Project: "proj", Machine: "local",
			Agent: "claude", FilePath: &claudePath, MessageCount: 1,
		},
	} {
		require.NoError(t, database.UpsertSession(sess))
	}

	tests := []struct {
		name           string
		preserveAgents []parser.AgentType
		scoped         bool
		want           int
	}{
		{
			name: "enabled provider protects CLI transcript rows",
			want: 2,
		},
		{
			name:           "disabled provider releases every icodemate row",
			preserveAgents: []parser.AgentType{parser.AgentIcodemate},
			want:           1,
		},
		{
			name:           "contributor-scoped disabled provider releases icodemate rows",
			preserveAgents: []parser.AgentType{parser.AgentIcodemate},
			scoped:         true,
			want:           1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			count, err := protectedFileSessionCount(
				database, "local", "", tt.scoped, tt.preserveAgents,
			)
			require.NoError(t, err)
			assert.Equal(t, tt.want, count)
		})
	}
}
