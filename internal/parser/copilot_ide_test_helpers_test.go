package parser

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// newVSCodeCopilotTestProvider builds a concrete vscodeCopilotProvider for the
// given roots so package tests can exercise the folded parse, discovery, and
// source-lookup behavior directly through provider-owned methods.
func newVSCodeCopilotTestProvider(
	t *testing.T, roots ...string,
) *vscodeCopilotProvider {
	t.Helper()
	provider, ok := NewProvider(AgentVSCodeCopilot, ProviderConfig{
		Roots:   roots,
		Machine: "local",
	})
	require.True(t, ok)
	p, ok := provider.(*vscodeCopilotProvider)
	require.True(t, ok)
	return p
}

// parseVSCodeCopilotTestSession parses a VSCode Copilot session file through
// the provider-owned parse method, replacing the removed package-level
// ParseVSCodeCopilotSession entrypoint.
func parseVSCodeCopilotTestSession(
	t *testing.T, path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	t.Helper()
	return newVSCodeCopilotTestProvider(t).parseSession(path, project, machine)
}

// discoverVSCodeCopilotTestSessions discovers VSCode Copilot session files
// under root through the provider source set, returning the legacy
// DiscoveredFile shape the tests assert against.
func discoverVSCodeCopilotTestSessions(
	t *testing.T, root string,
) []DiscoveredFile {
	t.Helper()
	return newVSCodeCopilotTestProvider(t, root).sources.discoverSessionFiles(root)
}

// findVSCodeCopilotTestSourceFile resolves a raw VSCode Copilot session ID to a
// session file through the provider source set, replacing the removed
// FindVSCodeCopilotSourceFile.
func findVSCodeCopilotTestSourceFile(
	t *testing.T, root, rawID string,
) string {
	t.Helper()
	return newVSCodeCopilotTestProvider(t, root).sources.findSourceFile(root, rawID)
}

// parseVisualStudioCopilotTestConversation parses one Visual Studio Copilot
// conversation through the folded free function, replacing the removed
// package-level ParseVisualStudioCopilotConversation entrypoint.
func parseVisualStudioCopilotTestConversation(
	t *testing.T, tracePath, conversationID, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	t.Helper()
	return parseVisualStudioCopilotConversation(
		tracePath, conversationID, project, machine,
	)
}

// parseVisualStudioCopilotTestSession reproduces the removed package-level
// ParseVisualStudioCopilotSession entrypoint. The path may be a real trace file
// or a <traceFile>#<conversationID> virtual path; a real trace file resolves to
// its first conversation.
func parseVisualStudioCopilotTestSession(
	t *testing.T, path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	t.Helper()
	if tracePath, conversationID, ok :=
		splitVisualStudioCopilotVirtualPath(path); ok {
		return parseVisualStudioCopilotConversation(
			tracePath, conversationID, project, machine,
		)
	}
	if !IsVisualStudioCopilotTraceFile(path) {
		return nil, nil, nil
	}
	ids, err := VisualStudioCopilotFileConversationIDs(path)
	if err != nil {
		return nil, nil, err
	}
	if len(ids) == 0 {
		return nil, nil, nil
	}
	return parseVisualStudioCopilotConversation(path, ids[0], project, machine)
}

// discoverVisualStudioCopilotTestSessions discovers Visual Studio Copilot
// session work items under root, replacing the removed
// DiscoverVisualStudioCopilotSessions.
func discoverVisualStudioCopilotTestSessions(
	t *testing.T, root string,
) []DiscoveredFile {
	t.Helper()
	return discoverVisualStudioCopilotSessionFilesUnderRoot(root)
}

// findVisualStudioCopilotTestSourceFile resolves a raw Visual Studio Copilot
// conversation ID to a conversation-scoped virtual path, replacing the removed
// FindVisualStudioCopilotSourceFile.
func findVisualStudioCopilotTestSourceFile(
	t *testing.T, root, rawID string,
) string {
	t.Helper()
	return findVisualStudioCopilotSourceFile(root, rawID)
}
