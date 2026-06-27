package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCoworkProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentCowork)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(AgentCowork, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestCoworkProviderSourceMethods(t *testing.T) {
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
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"local_*.json", "*.jsonl"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.ElementsMatch(t, []string{transcript, subagentPath}, []string{
		discovered[0].DisplayPath,
		discovered[1].DisplayPath,
	})
	for _, source := range discovered {
		assert.Equal(t, AgentCowork, source.Provider)
		assert.Equal(t, "demo", source.ProjectHint)
		assert.Equal(t, source.DisplayPath, source.FingerprintKey)
	}

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "remote~cowork:" + cli,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, transcript, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "agent-worker",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, subagentPath, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: transcript,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, transcript, found.DisplayPath)

	transcriptInfo, err := os.Stat(transcript)
	require.NoError(t, err)
	newer := transcriptInfo.ModTime().Add(time.Hour)
	require.NoError(t, os.Chtimes(metaPath, newer, newer))
	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, transcript, fingerprint.Key)
	assert.Equal(t, transcriptInfo.Size(), fingerprint.Size)
	assert.Equal(t, newer.UnixNano(), fingerprint.MTimeNS)
	assert.NotEmpty(t, fingerprint.Hash)

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
				context.Background(),
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

	require.NoError(t, os.Remove(metaPath))
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: metaPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, transcript, changed[0].DisplayPath)

	require.NoError(t, os.Remove(transcript))
	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: transcript, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, transcript, changed[0].DisplayPath)

	require.NoError(t, os.Remove(subagentPath))
	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: subagentPath, EventKind: "rename", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, subagentPath, changed[0].DisplayPath)

	ignored, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "org", "ws", "cowork-clientdata-cache.json"),
			EventKind: "write",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	assert.Empty(t, ignored)

	wrongRoot, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      transcript,
			EventKind: "write",
			WatchRoot: filepath.Join(root, "..", "other-root"),
		},
	)
	require.NoError(t, err)
	assert.Empty(t, wrongRoot)
}

func TestCoworkProviderParse(t *testing.T) {
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
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.False(t, outcome.ForceReplace)
	require.Empty(t, outcome.ExcludedSessionIDs)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "cowork:"+cli, result.Result.Session.ID)
	assert.Equal(t, AgentCowork, result.Result.Session.Agent)
	assert.Equal(t, "cowork", result.Result.Session.Project)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, transcript, result.Result.Session.File.Path)
	assert.Equal(t, fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Equal(t, "Parse title", result.Result.Session.SessionName)
	assert.Equal(t, "hello there", result.Result.Session.FirstMessage)
	assert.Len(t, result.Result.Messages, 2)
}

func TestCoworkProviderMetadataRemovalRejectsAmbiguousMainTranscripts(t *testing.T) {
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
	require.True(t, ok)

	require.NoError(t, os.Remove(metaPath))
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: metaPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	assert.Empty(t, changed)
}

func TestCoworkProviderMetadataRemovalIgnoresSymlinkEscape(t *testing.T) {
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
	require.NoError(t, os.MkdirAll(outside, 0o755))
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
	require.True(t, ok)

	require.NoError(t, os.Remove(metaPath))
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: metaPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, cli+".jsonl", filepath.Base(changed[0].DisplayPath))
}

func TestCoworkProviderMetadataRemovalIgnoresBrokenSymlinkAmbiguity(t *testing.T) {
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
	require.NoError(t, os.MkdirAll(brokenDir, 0o755))
	if err := os.Symlink(
		filepath.Join(root, "missing.jsonl"),
		filepath.Join(brokenDir, "c0000000-0000-4000-8000-000000000109.jsonl"),
	); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentCowork, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	require.NoError(t, os.Remove(metaPath))
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: metaPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
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
			found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
				FullSessionID: id,
			})
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, transcript, found.DisplayPath)
		})
	}
}
