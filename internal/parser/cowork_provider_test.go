package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCoworkProviderSourceMethods(t *testing.T) {
	parentAssert := assert.New(t)
	parentRequire := require.New(t)

	root := t.TempDir()
	cli := "c0000000-0000-4000-8000-000000000101"
	metaPath, transcript := writeCoworkSession(t, root, coworkFixture{
		org:             "org",
		workspace:       "ws",
		sessionUUID:     "50000000-0000-4000-8000-000000000101",
		cliSessionID:    cli,
		encodedProject:  "-Users-dev-code-demo",
		title:           "Provider title",
		folders:         []string{"/Users/dev/code/demo"},
		transcriptLines: coworkTranscriptLines(cli),
	})
	subagentPath := filepath.Join(
		filepath.Dir(transcript),
		cli,
		"subagents",
		"tasks",
		"agent-worker.jsonl",
	)
	writeSourceFile(t, subagentPath, strings.Join(coworkTranscriptLines(cli), "\n")+"\n")
	writeSourceFile(
		t,
		filepath.Join(filepath.Dir(transcript), cli, "subagents", "not-agent.jsonl"),
		strings.Join(coworkTranscriptLines(cli), "\n")+"\n",
	)
	writeSourceFile(
		t,
		filepath.Join(root, "org", "ws", "cowork-clientdata-cache.json"),
		"{}\n",
	)

	provider, ok := NewProvider(AgentCowork, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	parentRequire.True(ok)

	plan, err := provider.WatchPlan(t.Context())
	parentRequire.NoError(err)
	parentRequire.Len(plan.Roots, 1)
	parentAssert.Equal(root, plan.Roots[0].Path)
	parentAssert.True(plan.Roots[0].Recursive)
	parentAssert.Equal([]string{"local_*.json", "*.jsonl"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(t.Context())
	parentRequire.NoError(err)
	parentRequire.Len(discovered, 2)
	parentAssert.ElementsMatch([]string{transcript, subagentPath}, []string{
		discovered[0].DisplayPath,
		discovered[1].DisplayPath,
	})
	for _, source := range discovered {
		parentAssert.Equal(AgentCowork, source.Provider)
		parentAssert.Equal("demo", source.ProjectHint)
		parentAssert.Equal(source.DisplayPath, source.FingerprintKey)
	}

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "remote~cowork:" + cli,
	})
	parentRequire.NoError(err)
	parentRequire.True(ok)
	parentAssert.Equal(transcript, found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "agent-worker",
	})
	parentRequire.NoError(err)
	parentRequire.True(ok)
	parentAssert.Equal(subagentPath, found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: transcript,
	})
	parentRequire.NoError(err)
	parentRequire.True(ok)
	parentAssert.Equal(transcript, found.DisplayPath)

	transcriptInfo, err := os.Stat(transcript)
	parentRequire.NoError(err)
	newer := transcriptInfo.ModTime().Add(time.Hour)
	parentRequire.NoError(os.Chtimes(metaPath, newer, newer))
	fingerprint, err := provider.Fingerprint(t.Context(), found)
	parentRequire.NoError(err)
	parentAssert.Equal(transcript, fingerprint.Key)
	parentAssert.Equal(transcriptInfo.Size(), fingerprint.Size)
	parentAssert.Equal(newer.UnixNano(), fingerprint.MTimeNS)
	parentAssert.NotEmpty(fingerprint.Hash)

	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{name: "main transcript", path: transcript, want: transcript},
		{name: "subagent transcript", path: subagentPath, want: subagentPath},
		{name: "metadata", path: metaPath, want: transcript},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, err := provider.SourcesForChangedPath(
				t.Context(),
				ChangedPathRequest{
					Path:      tc.path,
					EventKind: "write",
					WatchRoot: root,
				},
			)
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, tc.want, changed[0].DisplayPath)
		})
	}

	parentRequire.NoError(os.Remove(metaPath))
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: metaPath, EventKind: "remove", WatchRoot: root},
	)
	parentRequire.NoError(err)
	parentRequire.Len(changed, 1)
	parentAssert.Equal(transcript, changed[0].DisplayPath)

	parentRequire.NoError(os.Remove(transcript))
	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: transcript, EventKind: "remove", WatchRoot: root},
	)
	parentRequire.NoError(err)
	parentRequire.Len(changed, 1)
	parentAssert.Equal(transcript, changed[0].DisplayPath)

	parentRequire.NoError(os.Remove(subagentPath))
	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: subagentPath, EventKind: "rename", WatchRoot: root},
	)
	parentRequire.NoError(err)
	parentRequire.Len(changed, 1)
	parentAssert.Equal(subagentPath, changed[0].DisplayPath)

	ignored, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "org", "ws", "cowork-clientdata-cache.json"),
			EventKind: "write",
			WatchRoot: root,
		},
	)
	parentRequire.NoError(err)
	parentAssert.Empty(ignored)

	wrongRoot, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      transcript,
			EventKind: "write",
			WatchRoot: filepath.Join(root, "..", "other-root"),
		},
	)
	parentRequire.NoError(err)
	parentAssert.Empty(wrongRoot)
}

func TestCoworkProviderParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	cli := "c0000000-0000-4000-8000-000000000102"
	_, transcript := writeCoworkSession(t, root, coworkFixture{
		org:             "org",
		workspace:       "ws",
		sessionUUID:     "50000000-0000-4000-8000-000000000102",
		cliSessionID:    cli,
		encodedProject:  "-sessions-demo",
		title:           "Parse title",
		transcriptLines: coworkTranscriptLines(cli),
	})

	provider, ok := NewProvider(AgentCowork, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.False(outcome.ForceReplace)
	require.Empty(outcome.ExcludedSessionIDs)
	require.Len(outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(DataVersionCurrent, result.DataVersion)
	assert.Equal("cowork:"+cli, result.Result.Session.ID)
	assert.Equal(AgentCowork, result.Result.Session.Agent)
	assert.Equal("cowork", result.Result.Session.Project)
	assert.Equal("devbox", result.Result.Session.Machine)
	assert.Equal(transcript, result.Result.Session.File.Path)
	assert.Equal(fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Equal("Parse title", result.Result.Session.SessionName)
	assert.Equal("hello there", result.Result.Session.FirstMessage)
	assert.Len(result.Result.Messages, 2)
}

func TestCoworkProviderMetadataRemovalRejectsAmbiguousMainTranscripts(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	cli := "c0000000-0000-4000-8000-000000000104"
	metaPath, transcript := writeCoworkSession(t, root, coworkFixture{
		org:             "org",
		workspace:       "ws",
		sessionUUID:     "50000000-0000-4000-8000-000000000104",
		cliSessionID:    cli,
		encodedProject:  "-sessions-demo",
		transcriptLines: coworkTranscriptLines(cli),
	})
	otherPath := filepath.Join(
		filepath.Dir(filepath.Dir(transcript)),
		"-sessions-other",
		"c0000000-0000-4000-8000-000000000105.jsonl",
	)
	writeSourceFile(
		t,
		otherPath,
		strings.Join(coworkTranscriptLines("c0000000-0000-4000-8000-000000000105"), "\n")+"\n",
	)

	provider, ok := NewProvider(AgentCowork, ProviderConfig{
		Roots: []string{root},
	})
	require.True(ok)

	require.NoError(os.Remove(metaPath))
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: metaPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(err)
	assert.Empty(t, changed)
}

func TestCoworkProviderMetadataRemovalIgnoresSymlinkEscape(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	cli := "c0000000-0000-4000-8000-000000000106"
	metaPath, _ := writeCoworkSession(t, root, coworkFixture{
		org:             "org",
		workspace:       "ws",
		sessionUUID:     "50000000-0000-4000-8000-000000000106",
		cliSessionID:    cli,
		encodedProject:  "-sessions-demo",
		transcriptLines: coworkTranscriptLines(cli),
	})
	sessionDir := strings.TrimSuffix(metaPath, ".json")
	projectsDir := filepath.Join(sessionDir, ".claude", "projects")
	outside := filepath.Join(root, "outside")
	require.NoError(os.MkdirAll(outside, 0o755))
	writeSourceFile(
		t,
		filepath.Join(outside, "c0000000-0000-4000-8000-000000000107.jsonl"),
		strings.Join(coworkTranscriptLines("c0000000-0000-4000-8000-000000000107"), "\n")+"\n",
	)
	if err := os.Symlink(outside, filepath.Join(projectsDir, "-sessions-escape")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentCowork, ProviderConfig{
		Roots: []string{root},
	})
	require.True(ok)

	require.NoError(os.Remove(metaPath))
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: metaPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(t, cli+".jsonl", filepath.Base(changed[0].DisplayPath))
}

func TestCoworkProviderMetadataRemovalIgnoresBrokenSymlinkAmbiguity(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	cli := "c0000000-0000-4000-8000-000000000108"
	metaPath, _ := writeCoworkSession(t, root, coworkFixture{
		org:             "org",
		workspace:       "ws",
		sessionUUID:     "50000000-0000-4000-8000-000000000108",
		cliSessionID:    cli,
		encodedProject:  "-sessions-demo",
		transcriptLines: coworkTranscriptLines(cli),
	})
	sessionDir := strings.TrimSuffix(metaPath, ".json")
	projectsDir := filepath.Join(sessionDir, ".claude", "projects")
	brokenDir := filepath.Join(projectsDir, "-sessions-broken")
	require.NoError(os.MkdirAll(brokenDir, 0o755))
	if err := os.Symlink(
		filepath.Join(root, "missing.jsonl"),
		filepath.Join(brokenDir, "c0000000-0000-4000-8000-000000000109.jsonl"),
	); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentCowork, ProviderConfig{
		Roots: []string{root},
	})
	require.True(ok)

	require.NoError(os.Remove(metaPath))
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: metaPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(t, cli+".jsonl", filepath.Base(changed[0].DisplayPath))
}

func TestCoworkProviderFullSessionIDPrefixLookup(t *testing.T) {
	root := t.TempDir()
	cli := "c0000000-0000-4000-8000-000000000103"
	_, transcript := writeCoworkSession(t, root, coworkFixture{
		org:             "org",
		workspace:       "ws",
		sessionUUID:     "50000000-0000-4000-8000-000000000103",
		cliSessionID:    cli,
		encodedProject:  "-sessions-demo",
		transcriptLines: coworkTranscriptLines(cli),
	})

	provider, ok := NewProvider(AgentCowork, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	for _, id := range []string{"cowork:" + cli, "remote~cowork:" + cli} {
		t.Run(strings.ReplaceAll(id, ":", "_"), func(t *testing.T) {
			found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
				FullSessionID: id,
			})
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, transcript, found.DisplayPath)
		})
	}
}
