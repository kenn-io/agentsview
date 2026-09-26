package parser

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// antigravityBrainTranscriptFixture is a four-entry plaintext brain
// transcript. Entry order in the file is deliberately not step order: the
// last two steps are written newest-first so the parse has to sort by
// step_index. step_index 2 is missing because a step that produces no entry
// leaves a gap, which real transcripts do.
const antigravityBrainTranscriptFixture = `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-05-20T21:41:49Z","content":"list the files in the project"}
{"step_index":1,"source":"SYSTEM","type":"CONVERSATION_HISTORY","status":"DONE","created_at":"2026-05-20T21:41:50Z"}
{"step_index":4,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-05-20T21:41:52Z","content":"there are three files","thinking":"the listing is short enough to answer directly"}
{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-05-20T21:41:51Z","tool_calls":[{"name":"list_dir","args":{"DirectoryPath":"/w/p","toolAction":"Listed directory","toolSummary":"p"}}]}
`

// antigravityBrainTranscriptShort is a two-entry transcript, used where a
// test needs to tell two roots' copies of one conversation apart by their
// message counts.
const antigravityBrainTranscriptShort = `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-05-20T22:10:00Z","content":"same conversation, other tree"}
{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-05-20T22:10:01Z","content":"answered"}
`

// writeAntigravityBrainTranscript writes body to the path Antigravity's agent
// brain uses for a conversation's plaintext transcript and returns it.
func writeAntigravityBrainTranscript(t *testing.T, root, id, body string) string {
	t.Helper()
	dir := filepath.Join(root, "brain", id, ".system_generated", "logs")
	mustMkdir(t, dir)
	path := filepath.Join(dir, "transcript.jsonl")
	mustWrite(t, path, []byte(body))
	return path
}

func newAntigravityProviderForRoots(t *testing.T, roots ...string) Provider {
	t.Helper()
	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{
		Roots:   roots,
		Machine: "devbox",
	})
	require.True(t, ok, "construct antigravity provider")
	return provider
}

// parseAntigravitySources parses every discovered source and returns the
// sessions, so a test can count what a resync over the tree would store.
func parseAntigravitySources(
	t *testing.T, provider Provider, sources []SourceRef,
) []ParseResult {
	t.Helper()
	var results []ParseResult
	for _, source := range sources {
		outcome, err := provider.Parse(t.Context(), ParseRequest{
			Source: source, Machine: "devbox",
		})
		require.NoError(t, err, "parse %s", source.DisplayPath)
		for _, result := range outcome.Results {
			results = append(results, result.Result)
		}
	}
	return results
}

// TestAntigravityBrainTranscriptIsItsOwnSession pins the recovery this change
// exists for: a conversation whose conversations/<id>.pb stream is encrypted
// still has a plaintext transcript under brain/, and that transcript is the
// session when no conversations/<id>.db exists for the same id.
func TestAntigravityBrainTranscriptIsItsOwnSession(t *testing.T) {
	root := t.TempDir()
	id := "04385df3-99c4-4772-81a9-24fb6508046a"
	path := writeAntigravityBrainTranscript(
		t, root, id, antigravityBrainTranscriptFixture,
	)
	provider := newAntigravityProviderForRoots(t, root)

	sources, err := provider.Discover(t.Context())
	require.NoError(t, err, "Discover")
	require.Len(t, sources, 1, "one source for the transcript")
	assert.Equal(t, path, sources[0].DisplayPath, "source path")

	results := parseAntigravitySources(t, provider, sources)
	require.Len(t, results, 1, "one session")
	sess := results[0].Session
	msgs := results[0].Messages

	assert.Equal(t, path, sess.File.Path, "session file path")
	assert.Equal(t, 4, sess.MessageCount,
		"one message per transcript entry")
	require.Len(t, msgs, 4, "messages")
	assert.Equal(t, 1, sess.UserMessageCount, "user messages")
	assert.Contains(t, sess.ID, id,
		"the session id names the conversation")

	// Ordered by step_index, not by the order the lines appear in the file.
	assert.Equal(t,
		[]RoleType{RoleUser, RoleSystem, RoleAssistant, RoleAssistant},
		[]RoleType{msgs[0].Role, msgs[1].Role, msgs[2].Role, msgs[3].Role},
		"roles in step order")
	assert.Equal(t, []int{0, 1, 2, 3},
		[]int{msgs[0].Ordinal, msgs[1].Ordinal, msgs[2].Ordinal, msgs[3].Ordinal},
		"ordinals")
	assert.True(t, msgs[1].IsSystem, "the SYSTEM entry is a system message")

	assert.True(t, msgs[2].HasToolUse, "step 3 calls a tool")
	require.Len(t, msgs[2].ToolCalls, 1, "tool calls on step 3")
	assert.Equal(t, "list_dir", msgs[2].ToolCalls[0].ToolName, "tool name")
	assert.Contains(t, msgs[2].ToolCalls[0].InputJSON, "DirectoryPath",
		"tool arguments are kept")

	assert.True(t, msgs[3].HasThinking, "step 4 carries reasoning")
	assert.Equal(t, "the listing is short enough to answer directly",
		msgs[3].ThinkingText, "thinking text")
	assert.Equal(t, "there are three files", msgs[3].Content, "content")

	assert.Equal(t, "2026-05-20T21:41:49Z",
		sess.StartedAt.UTC().Format("2006-01-02T15:04:05Z"), "started_at")
	assert.Equal(t, "2026-05-20T21:41:52Z",
		sess.EndedAt.UTC().Format("2006-01-02T15:04:05Z"), "ended_at")

	// The stored session resolves back to its own file, so a later sync pass
	// re-parses the transcript instead of losing it.
	raw := strings.TrimPrefix(sess.ID, antigravityIDPrefix)
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: raw,
	})
	require.NoError(t, err, "FindSource")
	require.True(t, ok, "FindSource found the transcript")
	assert.Equal(t, path, found.DisplayPath, "round-trip path")
}

// TestAntigravityBrainTranscriptFoldsIntoItsDBSession pins that a transcript
// belonging to a conversation the IDE database already holds is folded into
// that session rather than stored beside it as a second session for the same
// conversation.
func TestAntigravityBrainTranscriptFoldsIntoItsDBSession(t *testing.T) {
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	dbOnly := t.TempDir()
	writeAntigravityIDEProviderFixture(t, dbOnly, id)
	dbOnlyProvider := newAntigravityProviderForRoots(t, dbOnly)
	baseline := parseAntigravitySources(t, dbOnlyProvider,
		mustDiscoverAntigravity(t, dbOnlyProvider),
	)
	require.Len(t, baseline, 1, "one session from the database alone")

	both := t.TempDir()
	writeAntigravityIDEProviderFixture(t, both, id)
	writeAntigravityBrainTranscript(
		t, both, id, antigravityBrainTranscriptFixture,
	)
	provider := newAntigravityProviderForRoots(t, both)

	sources, err := provider.Discover(t.Context())
	require.NoError(t, err, "Discover")
	require.Len(t, sources, 1, "the database is the only source")
	assert.Equal(t, filepath.Join(both, "conversations", id+".db"),
		sources[0].DisplayPath, "source path")

	results := parseAntigravitySources(t, provider, sources)
	require.Len(t, results, 1, "one session, not two")
	assert.Equal(t, baseline[0].Session.MessageCount+4,
		results[0].Session.MessageCount,
		"the transcript's entries join the database session")
	assert.Equal(t, baseline[0].Session.ID, results[0].Session.ID,
		"the session keeps its conversation id")
}

func mustDiscoverAntigravity(t *testing.T, provider Provider) []SourceRef {
	t.Helper()
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err, "Discover")
	return sources
}

// TestAntigravityBrainChangedPathRouting pins where a write under brain/
// sends the sync. A brain/<id>/<file> artifact keeps routing to its database
// session exactly as before; a transcript routes to the database session when
// one exists and to itself when none does.
func TestAntigravityBrainChangedPathRouting(t *testing.T) {
	t.Run("artifact and transcript route to the database session", func(t *testing.T) {
		root := t.TempDir()
		id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		writeAntigravityIDEProviderFixture(t, root, id)
		transcript := writeAntigravityBrainTranscript(
			t, root, id, antigravityBrainTranscriptFixture,
		)
		dbPath := filepath.Join(root, "conversations", id+".db")
		provider := newAntigravityProviderForRoots(t, root)

		for _, changed := range []string{
			filepath.Join(root, "brain", id, "plan.md"),
			transcript,
		} {
			sources, err := provider.SourcesForChangedPath(t.Context(),
				ChangedPathRequest{Path: changed, EventKind: "write"})
			require.NoError(t, err, "SourcesForChangedPath %s", changed)
			require.Len(t, sources, 1, "one source for %s", changed)
			assert.Equal(t, dbPath, sources[0].DisplayPath,
				"routing for %s", changed)
		}
	})

	t.Run("a transcript with no database routes to itself", func(t *testing.T) {
		root := t.TempDir()
		id := "68b6d305-c8a9-45b4-96c8-fbaff15fa2f3"
		transcript := writeAntigravityBrainTranscript(
			t, root, id, antigravityBrainTranscriptFixture,
		)
		provider := newAntigravityProviderForRoots(t, root)

		sources, err := provider.SourcesForChangedPath(t.Context(),
			ChangedPathRequest{Path: transcript, EventKind: "write"})
		require.NoError(t, err, "SourcesForChangedPath")
		require.Len(t, sources, 1, "one source")
		assert.Equal(t, transcript, sources[0].DisplayPath, "routing")
	})

	t.Run("the brain watch plan includes the transcript", func(t *testing.T) {
		root := t.TempDir()
		provider := newAntigravityProviderForRoots(t, root)
		plan, err := provider.WatchPlan(t.Context())
		require.NoError(t, err, "WatchPlan")
		var globs []string
		for _, watch := range plan.Roots {
			if watch.Path == filepath.Join(root, "brain") {
				globs = watch.IncludeGlobs
			}
		}
		assert.Contains(t, globs, "transcript.jsonl",
			"the brain watch must see transcript writes")
	})
}

// TestAntigravityTwoRootsKeepBothCopiesOfOneConversation pins that the second
// default root cannot cost a session. The same conversation id under two
// configured roots yields two sessions with distinct ids and distinct file
// paths, and the outcome does not depend on which root the walk reaches
// first.
func TestAntigravityTwoRootsKeepBothCopiesOfOneConversation(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	id := "68b6d305-c8a9-45b4-96c8-fbaff15fa2f3"
	firstPath := writeAntigravityBrainTranscript(
		t, first, id, antigravityBrainTranscriptFixture,
	)
	secondPath := writeAntigravityBrainTranscript(
		t, second, id, antigravityBrainTranscriptShort,
	)

	byPath := func(results []ParseResult) map[string]ParsedSession {
		out := make(map[string]ParsedSession, len(results))
		for _, result := range results {
			out[result.Session.File.Path] = result.Session
		}
		return out
	}

	provider := newAntigravityProviderForRoots(t, first, second)
	sources := mustDiscoverAntigravity(t, provider)
	require.Len(t, sources, 2, "one source per root")
	sessions := byPath(parseAntigravitySources(t, provider, sources))
	require.Len(t, sessions, 2, "one session per root")
	require.Contains(t, sessions, firstPath, "first root's session")
	require.Contains(t, sessions, secondPath, "second root's session")
	assert.NotEqual(t, sessions[firstPath].ID, sessions[secondPath].ID,
		"session ids collide")
	assert.Equal(t, 4, sessions[firstPath].MessageCount, "first root messages")
	assert.Equal(t, 2, sessions[secondPath].MessageCount, "second root messages")

	// Same two sessions with the roots configured the other way round.
	reversed := newAntigravityProviderForRoots(t, second, first)
	reversedSessions := byPath(parseAntigravitySources(
		t, reversed, mustDiscoverAntigravity(t, reversed),
	))
	require.Len(t, reversedSessions, 2, "one session per root, reversed")
	assert.Equal(t, sessions[firstPath].ID, reversedSessions[firstPath].ID,
		"first root's session id depends on the walk order")
	assert.Equal(t, sessions[secondPath].ID, reversedSessions[secondPath].ID,
		"second root's session id depends on the walk order")
}

// TestAntigravityEncryptedStreamsStayUnstored is the guard on the recovery
// above. An AES-encrypted conversations/<id>.pb or implicit/<id>.pb with no
// key yields a session with no messages when a provider is pointed at it,
// and a session row is all the archive needs to call the file stored. So the
// count of Antigravity sessions over a tree of encrypted streams must not
// rise: only the plaintext transcript becomes a session.
func TestAntigravityEncryptedStreamsStayUnstored(t *testing.T) {
	t.Setenv("ANTIGRAVITY_KEY", "")
	root := t.TempDir()
	conversationID := "082b3e07-5c1b-4f4e-9a3e-7c5d2f1e8a90"
	implicitID := "e3afabb0-3561-4c2a-8b7f-1d9e4a6c2b83"
	transcriptID := "68b6d305-c8a9-45b4-96c8-fbaff15fa2f3"

	mustMkdir(t, filepath.Join(root, "conversations"))
	mustMkdir(t, filepath.Join(root, "implicit"))
	mustWrite(t, filepath.Join(root, "conversations", conversationID+".pb"),
		[]byte("encrypted-conversation-stream"))
	mustWrite(t, filepath.Join(root, "implicit", implicitID+".pb"),
		[]byte("encrypted-implicit-conversation"))
	transcript := writeAntigravityBrainTranscript(
		t, root, transcriptID, antigravityBrainTranscriptFixture,
	)

	provider := newAntigravityProviderForRoots(t, root)
	sources := mustDiscoverAntigravity(t, provider)
	for _, source := range sources {
		assert.NotContains(t, source.DisplayPath, ".pb",
			"an encrypted stream was discovered as a source")
	}
	require.Len(t, sources, 1, "only the plaintext transcript is a source")
	assert.Equal(t, transcript, sources[0].DisplayPath, "source path")

	results := parseAntigravitySources(t, provider, sources)
	require.Len(t, results, 1, "one session, for the transcript only")
	assert.Equal(t, transcript, results[0].Session.File.Path, "session file")
	assert.Positive(t, results[0].Session.MessageCount,
		"the stored session has messages")

	// The changed-path route must not make a source of one either: a watch
	// event on an encrypted stream has nothing for this provider to store.
	for _, encrypted := range []string{
		filepath.Join(root, "conversations", conversationID+".pb"),
		filepath.Join(root, "implicit", implicitID+".pb"),
	} {
		changed, err := provider.SourcesForChangedPath(t.Context(),
			ChangedPathRequest{Path: encrypted, EventKind: "write"})
		require.NoError(t, err, "SourcesForChangedPath %s", encrypted)
		assert.Empty(t, changed, "encrypted stream routed to a source")
	}
}

// TestAntigravityDefaultRootsCoverTheIDETree pins the configuration half of
// the same guard. The IDE variant writes to its own directory, which is a
// default root, and neither Antigravity provider is pointed at the other's
// tree: the CLI provider is the one that reads .pb streams, and pointing it
// at these trees would store an encrypted conversation as an empty session.
func TestAntigravityDefaultRootsCoverTheIDETree(t *testing.T) {
	ide, ok := AgentByType(AgentAntigravity)
	require.True(t, ok, "AgentAntigravity missing from Registry")
	assert.Contains(t, ide.DefaultDirs, ".gemini/antigravity",
		"the original Antigravity root")
	assert.Contains(t, ide.DefaultDirs, ".gemini/antigravity-ide",
		"the IDE variant's own root")
	assert.NotContains(t, ide.DefaultDirs, ".gemini/antigravity-backup",
		"a backup copy would duplicate every session it holds")

	cli, ok := AgentByType(AgentAntigravityCLI)
	require.True(t, ok, "AgentAntigravityCLI missing from Registry")
	for _, dir := range []string{
		".gemini/antigravity", ".gemini/antigravity-ide",
	} {
		assert.NotContains(t, cli.DefaultDirs, dir,
			"the CLI reader must not be pointed at %s", dir)
	}
}
