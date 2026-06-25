package parser

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// coworkProviderForRoot constructs a cowork provider rooted at root.
func coworkProviderForRoot(t *testing.T, root, machine string) Provider {
	t.Helper()
	provider, ok := NewProvider(AgentCowork, ProviderConfig{
		Roots:   []string{root},
		Machine: machine,
	})
	require.True(t, ok)
	return provider
}

// coworkDiscoveredPaths returns the transcript paths the provider discovers
// under root.
func coworkDiscoveredPaths(t *testing.T, root string) []string {
	t.Helper()
	sources, err := coworkProviderForRoot(t, root, "").Discover(context.Background())
	require.NoError(t, err)
	paths := make([]string, len(sources))
	for i, source := range sources {
		paths[i] = source.DisplayPath
	}
	return paths
}

// coworkParseTranscript finds and parses a single cowork transcript through
// the provider, returning the parse results and any excluded session IDs.
func coworkParseTranscript(
	t *testing.T, root, transcript, machine string,
) ([]ParseResult, []string) {
	t.Helper()
	provider := coworkProviderForRoot(t, root, machine)
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: transcript,
	})
	require.NoError(t, err)
	require.True(t, ok, "find source for %s", transcript)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:  source,
		Machine: machine,
	})
	require.NoError(t, err)
	results := make([]ParseResult, len(outcome.Results))
	for i, out := range outcome.Results {
		results[i] = out.Result
	}
	return results, outcome.ExcludedSessionIDs
}

// All identifiers, titles, and content below are synthetic fixtures.

// coworkFixture describes one cowork session to materialize on disk.
type coworkFixture struct {
	org             string
	workspace       string
	sessionUUID     string // names local_<uuid>.json and local_<uuid>/
	cliSessionID    string // names the nested transcript
	encodedProject  string // the .claude/projects/<enc> directory name
	title           string
	folders         []string
	createdAt       int64
	lastActivityAt  int64
	transcriptLines []string
}

// writeCoworkSession materializes a cowork session under root and returns
// the metadata path and transcript path.
func writeCoworkSession(
	t *testing.T, root string, f coworkFixture,
) (metaPath, transcriptPath string) {
	t.Helper()

	wsDir := filepath.Join(root, f.org, f.workspace)
	sessionDirName := "local_" + f.sessionUUID
	metaPath = filepath.Join(wsDir, sessionDirName+".json")

	meta := map[string]any{
		"sessionId":           sessionDirName,
		"cliSessionId":        f.cliSessionID,
		"title":               f.title,
		"userSelectedFolders": f.folders,
		"createdAt":           f.createdAt,
		"lastActivityAt":      f.lastActivityAt,
	}
	metaBytes, err := json.Marshal(meta)
	require.NoError(t, err, "marshal meta")
	require.NoError(t, os.MkdirAll(wsDir, 0o755), "mkdir workspace")
	require.NoError(t, os.WriteFile(metaPath, metaBytes, 0o644), "write meta")

	projectDir := filepath.Join(
		wsDir, sessionDirName, ".claude", "projects", f.encodedProject,
	)
	require.NoError(t, os.MkdirAll(projectDir, 0o755), "mkdir project")
	transcriptPath = filepath.Join(projectDir, f.cliSessionID+".jsonl")
	require.NoError(t,
		os.WriteFile(
			transcriptPath,
			[]byte(strings.Join(f.transcriptLines, "\n")+"\n"),
			0o644,
		),
		"write transcript",
	)
	return metaPath, transcriptPath
}

// coworkTranscriptLines returns a small but realistic Claude Code
// transcript: an ai-title event, one user turn, and one assistant turn
// carrying token usage.
func coworkTranscriptLines(cli string) []string {
	return []string{
		`{"type":"ai-title","aiTitle":"Auto title","sessionId":"` + cli + `"}`,
		`{"type":"user","uuid":"u1","parentUuid":null,` +
			`"sessionId":"` + cli + `","cwd":"/sessions/test",` +
			`"gitBranch":"HEAD","version":"2.1.119",` +
			`"timestamp":"2026-03-01T10:00:00.000Z",` +
			`"message":{"role":"user","content":"hello there"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1",` +
			`"sessionId":"` + cli + `","requestId":"req_1",` +
			`"timestamp":"2026-03-01T10:00:05.000Z",` +
			`"message":{"role":"assistant","id":"msg_1",` +
			`"model":"claude-sonnet-4-6","stop_reason":"end_turn",` +
			`"content":[{"type":"text","text":"hi back"}],` +
			`"usage":{"input_tokens":10,"cache_read_input_tokens":2,` +
			`"output_tokens":5}}}`,
	}
}

func TestCoworkProviderDiscoversSessions(t *testing.T) {
	root := t.TempDir()
	cli := "c0000000-0000-4000-8000-000000000001"
	_, transcript := writeCoworkSession(t, root, coworkFixture{
		org:             "org",
		workspace:       "ws",
		sessionUUID:     "50000000-0000-4000-8000-000000000001",
		cliSessionID:    cli,
		encodedProject:  "-sessions-demo",
		title:           "Demo session",
		transcriptLines: coworkTranscriptLines(cli),
	})

	got := coworkDiscoveredPaths(t, root)
	require.Len(t, got, 1, "discovered sources")
	assert.Equal(t, transcript, got[0], "DisplayPath")
}

func TestCoworkProviderDiscoverIgnoresNoise(t *testing.T) {
	root := t.TempDir()
	wsDir := filepath.Join(root, "org", "ws")
	require.NoError(t, os.MkdirAll(wsDir, 0o755), "mkdir ws")

	// Sibling cache files must not be treated as session metadata.
	for _, name := range []string{
		"cowork_settings.json",
		"cowork-clientdata-cache.json",
		"artifacts.json",
	} {
		require.NoError(t,
			os.WriteFile(filepath.Join(wsDir, name), []byte("{}"), 0o644),
			"write %s", name,
		)
	}
	// A skills-plugin mirror must be skipped entirely.
	skillDir := filepath.Join(root, "skills-plugin", "ws", "org")
	require.NoError(t, os.MkdirAll(skillDir, 0o755), "mkdir skills")
	require.NoError(t,
		os.WriteFile(
			filepath.Join(skillDir, "local_fake.json"), []byte("{}"), 0o644,
		),
		"write skills noise",
	)
	// A metadata file with no transcript yet must be skipped.
	require.NoError(t,
		os.WriteFile(
			filepath.Join(wsDir, "local_"+
				"00000000-0000-4000-8000-0000000000ff.json"),
			[]byte(`{"cliSessionId":"00000000-0000-4000-8000-0000000000fe"}`), 0o644,
		),
		"write transcript-less meta",
	)

	assert.Empty(t, coworkDiscoveredPaths(t, root))
}

func TestCoworkProviderParsesSession(t *testing.T) {
	root := t.TempDir()
	cli := "c0000000-0000-4000-8000-000000000002"
	_, transcript := writeCoworkSession(t, root, coworkFixture{
		org:             "org",
		workspace:       "ws",
		sessionUUID:     "50000000-0000-4000-8000-000000000002",
		cliSessionID:    cli,
		encodedProject:  "-sessions-demo",
		title:           "Sample session title",
		createdAt:       1700000000000,
		lastActivityAt:  1700000100000,
		transcriptLines: coworkTranscriptLines(cli),
	})

	results, excluded := coworkParseTranscript(t, root, transcript, "host-1")
	require.Empty(t, excluded, "excluded")
	require.Len(t, results, 1, "results")

	sess := results[0].Session
	assert.Equal(t, "cowork:"+cli, sess.ID, "ID prefixed")
	assert.Equal(t, AgentCowork, sess.Agent, "Agent")
	assert.Equal(t, "cowork", sess.Project, "Project")
	assert.Equal(t, "Sample session title", sess.SessionName, "title")
	assert.Equal(t, "host-1", sess.Machine, "Machine")
	assert.Equal(t, 2, sess.MessageCount, "MessageCount")
	assert.Equal(t, 1, sess.UserMessageCount, "UserMessageCount")

	// Token usage must be counted (this is the crux of issue #639).
	hasTotal, hasPeak := sess.AggregateTokenPresence()
	assert.True(t, hasTotal, "HasTotalOutputTokens")
	assert.True(t, hasPeak, "HasPeakContextTokens")
	assert.Equal(t, 5, sess.TotalOutputTokens, "TotalOutputTokens")
	assert.Equal(t, 12, sess.PeakContextTokens, "PeakContextTokens (input+cacheRead)")
}

func TestCoworkProviderParseTitleFallsBackToAITitle(t *testing.T) {
	root := t.TempDir()
	cli := "c0000000-0000-4000-8000-000000000003"
	_, transcript := writeCoworkSession(t, root, coworkFixture{
		org:             "org",
		workspace:       "ws",
		sessionUUID:     "50000000-0000-4000-8000-000000000003",
		cliSessionID:    cli,
		encodedProject:  "-sessions-demo",
		title:           "", // no explicit title
		transcriptLines: coworkTranscriptLines(cli),
	})

	results, _ := coworkParseTranscript(t, root, transcript, "host-1")
	require.Len(t, results, 1, "results")
	assert.Equal(t, "Auto title", results[0].Session.SessionName,
		"falls back to ai-title event")
}

func TestCoworkProviderParseProjectFromSelectedFolder(t *testing.T) {
	root := t.TempDir()
	cli := "c0000000-0000-4000-8000-000000000004"
	_, transcript := writeCoworkSession(t, root, coworkFixture{
		org:             "org",
		workspace:       "ws",
		sessionUUID:     "50000000-0000-4000-8000-000000000004",
		cliSessionID:    cli,
		encodedProject:  "-sessions-demo",
		title:           "With folder",
		folders:         []string{"/home/user/code/my-app"},
		transcriptLines: coworkTranscriptLines(cli),
	})

	results, _ := coworkParseTranscript(t, root, transcript, "host-1")
	require.Len(t, results, 1, "results")
	assert.Equal(t, "my_app", results[0].Session.Project,
		"project derived from userSelectedFolders")
}

func TestCoworkProviderFindsSourceFile(t *testing.T) {
	root := t.TempDir()
	cli := "c0000000-0000-4000-8000-000000000005"
	_, transcript := writeCoworkSession(t, root, coworkFixture{
		org:             "org",
		workspace:       "ws",
		sessionUUID:     "50000000-0000-4000-8000-000000000005",
		cliSessionID:    cli,
		encodedProject:  "-sessions-demo",
		title:           "x",
		transcriptLines: coworkTranscriptLines(cli),
	})

	provider := coworkProviderForRoot(t, root, "")
	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: cli,
	})
	require.NoError(t, err)
	require.True(t, ok, "found")
	assert.Equal(t, transcript, found.DisplayPath)

	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "nonexistent-id",
	})
	require.NoError(t, err)
	assert.False(t, ok, "missing")
}

func TestCoworkProviderClassifiesChangedPath(t *testing.T) {
	root := t.TempDir()
	cli := "c0000000-0000-4000-8000-000000000006"
	metaPath, transcript := writeCoworkSession(t, root, coworkFixture{
		org:             "org",
		workspace:       "ws",
		sessionUUID:     "50000000-0000-4000-8000-000000000006",
		cliSessionID:    cli,
		encodedProject:  "-sessions-demo",
		title:           "x",
		transcriptLines: coworkTranscriptLines(cli),
	})

	provider := coworkProviderForRoot(t, root, "")
	classify := func(path string) (string, bool) {
		sources, err := provider.SourcesForChangedPath(
			context.Background(),
			ChangedPathRequest{Path: path, EventKind: "write", WatchRoot: root},
		)
		require.NoError(t, err)
		if len(sources) == 0 {
			return "", false
		}
		require.Len(t, sources, 1)
		return sources[0].DisplayPath, true
	}

	// A transcript change classifies to itself.
	got, ok := classify(transcript)
	require.True(t, ok, "transcript classified")
	assert.Equal(t, transcript, got, "transcript path")

	// A metadata change resolves to the session's transcript.
	got, ok = classify(metaPath)
	require.True(t, ok, "metadata classified")
	assert.Equal(t, transcript, got, "metadata resolves to transcript")

	// Unrelated and outside-root paths are ignored.
	_, ok = classify(filepath.Join(root, "org", "ws", "artifacts.json"))
	assert.False(t, ok, "cache file ignored")
	_, ok = classify("/some/other/place.jsonl")
	assert.False(t, ok, "outside root ignored")
}

func TestCoworkSessionMtime(t *testing.T) {
	root := t.TempDir()
	cli := "c0000000-0000-4000-8000-000000000007"
	metaPath, transcript := writeCoworkSession(t, root, coworkFixture{
		org:             "org",
		workspace:       "ws",
		sessionUUID:     "50000000-0000-4000-8000-000000000007",
		cliSessionID:    cli,
		encodedProject:  "-sessions-demo",
		title:           "Before",
		transcriptLines: coworkTranscriptLines(cli),
	})

	tInfo, err := os.Stat(transcript)
	require.NoError(t, err, "stat transcript")
	tMtime := tInfo.ModTime().UnixNano()

	// Baseline: with metadata older than the transcript, the transcript
	// mtime wins.
	older := tInfo.ModTime().Add(-time.Hour)
	require.NoError(t, os.Chtimes(metaPath, older, older), "age metadata")
	assert.Equal(t, tMtime, CoworkSessionMtime(transcript, tMtime),
		"transcript mtime wins when metadata is older")

	// A title rename bumps only the metadata file's mtime; the composite
	// must reflect it so the session is re-parsed instead of skipped.
	newer := tInfo.ModTime().Add(time.Hour)
	require.NoError(t, os.Chtimes(metaPath, newer, newer), "touch metadata")
	assert.Equal(t, newer.UnixNano(), CoworkSessionMtime(transcript, tMtime),
		"metadata mtime folded into composite")

	// No metadata file -> transcript mtime.
	require.NoError(t, os.Remove(metaPath), "remove metadata")
	assert.Equal(t, tMtime, CoworkSessionMtime(transcript, tMtime),
		"transcript mtime when metadata missing")
}

func TestCoworkProviderDiscoverIncludesSubagents(t *testing.T) {
	root := t.TempDir()
	cli := "c0000000-0000-4000-8000-000000000008"
	enc := "-sessions-demo"
	_, transcript := writeCoworkSession(t, root, coworkFixture{
		org:             "org",
		workspace:       "ws",
		sessionUUID:     "50000000-0000-4000-8000-000000000008",
		cliSessionID:    cli,
		encodedProject:  enc,
		title:           "Subagent demo",
		transcriptLines: coworkTranscriptLines(cli),
	})

	// Write a subagent transcript alongside the main one, mirroring
	// Claude Code's layout: <enc>/<cli>/subagents/agent-<id>.jsonl.
	subDir := filepath.Join(filepath.Dir(transcript), cli, "subagents")
	require.NoError(t, os.MkdirAll(subDir, 0o755), "mkdir subagents")
	subPath := filepath.Join(subDir, "agent-0000000000000001.jsonl")
	subLines := []string{
		`{"type":"user","uuid":"su1","parentUuid":null,"sessionId":"` + cli + `",` +
			`"cwd":"/sessions/test","timestamp":"2026-03-01T10:01:00.000Z",` +
			`"message":{"role":"user","content":"sub task"}}`,
		`{"type":"assistant","uuid":"sa1","parentUuid":"su1","sessionId":"` + cli + `",` +
			`"timestamp":"2026-03-01T10:01:05.000Z","message":{"role":"assistant",` +
			`"id":"m2","model":"claude-sonnet-4-6","stop_reason":"end_turn",` +
			`"content":[{"type":"text","text":"done"}],` +
			`"usage":{"input_tokens":3,"output_tokens":2}}}`,
	}
	require.NoError(t,
		os.WriteFile(subPath, []byte(strings.Join(subLines, "\n")+"\n"), 0o644),
		"write subagent",
	)

	paths := coworkDiscoveredPaths(t, root)
	assert.Contains(t, paths, transcript, "main transcript discovered")
	assert.Contains(t, paths, subPath, "subagent transcript discovered")

	// The subagent parses into a cowork-namespaced subagent session whose
	// parent is the main session.
	results, _ := coworkParseTranscript(t, root, subPath, "host-1")
	require.Len(t, results, 1, "results")
	sub := results[0].Session
	assert.Equal(t, "cowork:agent-0000000000000001", sub.ID, "subagent ID")
	assert.Equal(t, "cowork:"+cli, sub.ParentSessionID, "parent prefixed")
	assert.Equal(t, RelSubagent, sub.RelationshipType, "RelSubagent")

	// The provider resolves the subagent by its raw ID too.
	provider := coworkProviderForRoot(t, root, "")
	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "agent-0000000000000001",
	})
	require.NoError(t, err)
	require.True(t, ok, "find subagent source")
	assert.Equal(t, subPath, found.DisplayPath)
}

func TestResolveCoworkSessionRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	root := t.TempDir()
	cli := "c0000000-0000-4000-8000-000000000009"
	sessionDir := filepath.Join(root, "org", "ws", "local_session")
	projectsDir := filepath.Join(sessionDir, ".claude", "projects")
	require.NoError(t, os.MkdirAll(projectsDir, 0o755), "mkdir projects")

	// Plant the real transcript OUTSIDE the session dir, then expose it
	// inside projects/ via a directory symlink. resolveCoworkSession must
	// refuse to follow the escape.
	outside := filepath.Join(root, "outside")
	require.NoError(t, os.MkdirAll(outside, 0o755), "mkdir outside")
	require.NoError(t,
		os.WriteFile(filepath.Join(outside, cli+".jsonl"), []byte("{}\n"), 0o644),
		"write escaped transcript",
	)
	require.NoError(t,
		os.Symlink(outside, filepath.Join(projectsDir, "-evil")),
		"symlink enc dir",
	)

	main, encDir := resolveCoworkSession(sessionDir, cli)
	assert.Empty(t, main, "symlinked escape rejected")
	assert.Empty(t, encDir, "no enc dir for escape")
}

func TestCoworkDefaultDirs(t *testing.T) {
	dirs := coworkDefaultDirs()
	require.Len(t, dirs, 4, "macOS, Linux, Windows MSIX, Windows Roaming")
	assert.Contains(t, dirs,
		"AppData/Local/Packages/Claude_pzs8sxrjxfjjc/"+
			"LocalCache/Roaming/Claude/local-agent-mode-sessions",
		"Windows MSIX package-local path")
	assert.Contains(t, dirs,
		"AppData/Roaming/Claude/local-agent-mode-sessions",
		"Windows Roaming fallback")
	for _, d := range dirs {
		assert.True(t,
			strings.HasSuffix(d, "local-agent-mode-sessions"),
			"dir %q targets local-agent-mode-sessions", d,
		)
	}
}
