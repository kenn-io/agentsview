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

func uploadIdentityTranscript(sessionID, marker, secondID string) string {
	markerField := ""
	if marker != "" {
		markerField = `,"isSidechain":` + marker
	}
	if secondID == "" {
		secondID = sessionID
	}
	return fmt.Sprintf(
		`{"type":"user","uuid":"u1","timestamp":"2026-01-01T00:00:00Z","sessionId":%q%s,"message":{"content":"hello"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-01-01T00:00:01Z","sessionId":%q%s,"message":{"content":[{"type":"text","text":"hi"}]}}`+"\n",
		sessionID, markerField, secondID, markerField,
	)
}

func parseClaudeUploadIdentityFixture(t *testing.T, filename, content string) []ParseResult {
	t.Helper()
	path := filepath.Join(t.TempDir(), filename+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	results, _, err := claudeParseFile(path, "project", "remote", claudeParseOptions{uploadIdentity: true})
	require.NoError(t, err)
	return results
}

func TestClaudeUploadIdentityAdoptsExplicitRootAndDerivesFork(t *testing.T) {
	lines := []string{
		`{"type":"user","uuid":"u1","timestamp":"2026-01-01T00:00:00Z","sessionId":"root-stable","isSidechain":false,"message":{"content":"root"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-01-01T00:00:01Z","sessionId":"root-stable","isSidechain":false,"message":{"content":[{"type":"text","text":"root answer"}]}}`,
	}
	for _, branch := range []string{"b", "c"} {
		parent := "a1"
		for i := 1; i <= 4; i++ {
			uuid := branch + fmt.Sprint(i)
			lines = append(lines, fmt.Sprintf(`{"type":"user","uuid":%q,"parentUuid":%q,"timestamp":"2026-01-01T00:00:%02dZ","sessionId":"root-stable","isSidechain":false,"message":{"content":"%s"}}`, uuid, parent, i+1, uuid))
			assistant := branch + "a" + fmt.Sprint(i)
			lines = append(lines, fmt.Sprintf(`{"type":"assistant","uuid":%q,"parentUuid":%q,"timestamp":"2026-01-01T00:00:%02dZ","sessionId":"root-stable","isSidechain":false,"message":{"content":[{"type":"text","text":"answer"}]}}`, assistant, uuid, i+1))
			parent = assistant
		}
	}
	results := parseClaudeUploadIdentityFixture(t, "transport-name", strings.Join(lines, "\n")+"\n")
	require.Len(t, results, 2)
	assert.Equal(t, "root-stable", results[0].Session.ID)
	assert.Empty(t, results[0].Session.ParentSessionID)
	assert.Equal(t, "root-stable-c1", results[1].Session.ID)
	assert.Equal(t, "root-stable", results[1].Session.ParentSessionID)
	assert.Equal(t, RelFork, results[1].Session.RelationshipType)
}

func TestClaudeUploadIdentityRequiresStableStrictRootEvidence(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"sidechain", uploadIdentityTranscript("parent", "true", "parent")},
		{"markerless", uploadIdentityTranscript("root", "", "root")},
		{"mixed IDs", uploadIdentityTranscript("root", "false", "other")},
		{"invalid ID", uploadIdentityTranscript("bad/id", "false", "bad/id")},
		{"missing ID", strings.Replace(uploadIdentityTranscript("root", "false", "root"), `,"sessionId":"root","isSidechain"`, `,"isSidechain"`, 1)},
		{"string marker", uploadIdentityTranscript("root", `"false"`, "root")},
		{"null marker", uploadIdentityTranscript("root", "null", "root")},
		{"reserved device", uploadIdentityTranscript("CON", "false", "CON")},
		{"reserved COM1", uploadIdentityTranscript("COM1", "false", "COM1")},
		{"reserved LPT9", uploadIdentityTranscript("LPT9", "false", "LPT9")},
		{"too long", uploadIdentityTranscript(strings.Repeat("a", 250), "false", strings.Repeat("a", 250))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := parseClaudeUploadIdentityFixture(t, "transport-name", tt.content)
			require.Len(t, results, 1)
			assert.Equal(t, "transport-name", results[0].Session.ID)
		})
	}
}

func TestClaudeUploadIdentityAllows249ByteID(t *testing.T) {
	id := strings.Repeat("a", 249)
	results := parseClaudeUploadIdentityFixture(t, "transport-name", uploadIdentityTranscript(id, "false", id))
	require.Len(t, results, 1)
	assert.Equal(t, id, results[0].Session.ID)
}

func TestClaudeUploadIdentityIgnoresStagedCompanionPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "project", "subagents")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "agent-root.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(uploadIdentityTranscript("root-stable", "false", "root-stable")), 0o644))
	results, _, err := claudeParseFile(path, "project", "remote", claudeParseOptions{uploadIdentity: true})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Session.ParentSessionID)
	assert.Equal(t, RelNone, results[0].Session.RelationshipType)
}

func TestClaudeUploadIdentityAllowsNonReservedDevicePrefixes(t *testing.T) {
	for _, id := range []string{"COM0", "COMA", "LPT0", "LPTA"} {
		results := parseClaudeUploadIdentityFixture(t, "transport-name", uploadIdentityTranscript(id, "false", id))
		require.Len(t, results, 1)
		assert.Equal(t, id, results[0].Session.ID)
	}
}

func TestClaudeLocalProviderPreservesFilenameIdentity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "-Users-dev-demo", "transport-name.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(uploadIdentityTranscript("embedded", "false", "embedded")), 0o644))
	provider, ok := NewProvider(AgentClaude, ProviderConfig{Roots: []string{root}, Machine: "local"})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "transport-name", outcome.Results[0].Result.Session.ID)
}
