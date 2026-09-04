package rawderive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestProviderParserUsesProviderCaptureContractAndStablePaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	localPath := filepath.Join(root, "session.jsonl")
	require.NoError(t, os.WriteFile(localPath, []byte("session"), 0o400))
	stablePath := "/canonical/claude/session.jsonl"
	manifest := parserTestManifest(t, parser.AgentClaude, stablePath, []rawsync.Entry{{
		Path: "session.jsonl", Type: "file", Length: 7,
		Objects: []rawsync.ObjectRef{objectRefForBytes(t, []byte("session"))},
	}})
	provider := &providerParserFixture{root: root, localPath: localPath}
	factory := &providerParserFixtureFactory{provider: provider}
	dispatch, err := NewProviderParser([]parser.ProviderFactory{factory}, "hosted-worker")
	require.NoError(t, err)

	parsed, err := dispatch.Parse(t.Context(), manifest, &Materialization{
		root: root, entries: map[string]string{"session.jsonl": localPath},
	})
	require.NoError(t, err)
	assert.False(t, parsed.Tombstone)
	require.Len(t, parsed.Outcome.Results, 1)
	assert.Equal(t, stablePath, parsed.Outcome.Results[0].Result.Session.File.Path)
	assert.Equal(t, "hosted-worker", factory.config.Machine)
	assert.True(t, factory.config.StableSourceSnapshots)
	assert.Equal(t, []string{root}, factory.config.Roots)
	assert.Equal(t, stablePath, factory.config.PathRewriter(localPath))
	assert.Equal(t, stablePath, provider.request.Source.Key)
	assert.Equal(t, stablePath, provider.request.Fingerprint.Key)
	resolved, ok := provider.request.StoredPathResolver(stablePath)
	assert.True(t, ok)
	assert.Equal(t, localPath, resolved)
}

func TestProviderParserRunsRegisteredClaudeParserWithoutLeakingMaterializedPath(t *testing.T) {
	t.Parallel()
	contents := []byte(
		`{"type":"user","timestamp":"2026-08-13T12:00:00Z","uuid":"u1",` +
			`"sessionId":"session-a","message":{"content":"hello"},"cwd":"/work/project"}` + "\n" +
			`{"type":"assistant","timestamp":"2026-08-13T12:00:01Z","uuid":"a1",` +
			`"parentUuid":"u1","sessionId":"session-a","message":{"content":"hi"}}` + "\n",
	)
	stablePath := "/canonical/claude/project/session.jsonl"
	// Use 100 ns precision so the assertion is portable to Windows filesystems.
	sourceModTime := time.Date(2026, 8, 13, 11, 30, 0, 246813500, time.UTC)
	object := objectRefForBytes(t, contents)
	manifest := parserTestManifest(t, parser.AgentClaude, stablePath, []rawsync.Entry{{
		Path: "project/session.jsonl", Type: "file", Length: int64(len(contents)),
		ModTimeNS: sourceModTime.UnixNano(),
		Objects:   []rawsync.ObjectRef{object},
	}})
	materialized, err := (Materializer{
		Store:         &materializerStore{objects: map[rawsync.ObjectRef][]byte{object: contents}},
		BaseDir:       t.TempDir(),
		MaxTotalBytes: 1 << 20,
	}).Materialize(t.Context(), manifest)
	require.NoError(t, err)
	defer func() { require.NoError(t, materialized.Cleanup()) }()
	dispatch, err := NewProviderParser(parser.ProviderFactories(), "hosted-worker")
	require.NoError(t, err)

	parsed, err := dispatch.Parse(t.Context(), manifest, materialized)

	require.NoError(t, err)
	require.Len(t, parsed.Outcome.Results, 1)
	assert.Equal(t, stablePath, parsed.Outcome.Results[0].Result.Session.File.Path)
	assert.NotContains(t, parsed.Outcome.Results[0].Result.Session.File.Path, materialized.Root())
	assert.Equal(t, parser.AgentClaude, parsed.Outcome.Results[0].Result.Session.Agent)
	assert.Equal(t, sourceModTime.UnixNano(), parsed.Outcome.Results[0].Result.Session.File.Mtime,
		"parser output must inherit the captured source mod time, not the worker clock")
}

// hostileServerRepo creates a real git repository outside any materialized
// tree and returns a working directory inside it whose lexical base name
// ("workdir") differs from the repository name a filesystem git-root walk
// would leak.
func hostileServerRepo(t *testing.T, repoName string) string {
	t.Helper()
	base := t.TempDir()
	repo := filepath.Join(base, repoName)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o700))
	cwd := filepath.Join(repo, "workdir")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	return cwd
}

// TestProviderParserHostedParseKeepsClaudeProjectLexicalForHostileCwd pins
// that a hostile cwd recorded in a materialized transcript cannot make the
// hosted parse walk this worker's filesystem: the attributed project stays
// the lexical cwd base name and never leaks the server-side repository name
// a git-root walk would find.
func TestProviderParserHostedParseKeepsClaudeProjectLexicalForHostileCwd(t *testing.T) {
	t.Parallel()
	hostileCwd := hostileServerRepo(t, "server-secret-claude")
	contents := []byte(
		`{"type":"user","timestamp":"2026-08-13T12:00:00Z","uuid":"u1",` +
			`"sessionId":"session-hostile","message":{"content":"hello"},"cwd":` +
			strconv.Quote(hostileCwd) + `}` + "\n" +
			`{"type":"assistant","timestamp":"2026-08-13T12:00:01Z","uuid":"a1",` +
			`"parentUuid":"u1","sessionId":"session-hostile","message":{"content":"hi"}}` + "\n",
	)
	object := objectRefForBytes(t, contents)
	stablePath := "/canonical/claude/project/session-hostile.jsonl"
	manifest := parserTestManifest(t, parser.AgentClaude, stablePath, []rawsync.Entry{{
		Path: "project/session-hostile.jsonl", Type: "file", Length: int64(len(contents)),
		Objects: []rawsync.ObjectRef{object},
	}})
	materialized, err := (Materializer{
		Store:   &materializerStore{objects: map[rawsync.ObjectRef][]byte{object: contents}},
		BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20,
	}).Materialize(t.Context(), manifest)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, materialized.Cleanup()) })
	dispatch, err := NewProviderParser(parser.ProviderFactories(), "hosted-worker")
	require.NoError(t, err)

	parsed, err := dispatch.Parse(t.Context(), manifest, materialized)

	require.NoError(t, err)
	require.Len(t, parsed.Outcome.Results, 1)
	project := parsed.Outcome.Results[0].Result.Session.Project
	assert.Equal(t, "workdir", project,
		"hosted project attribution must stay lexical for a hostile recorded cwd")
	assert.NotContains(t, project, "secret",
		"the server-side repository name must not leak through project attribution")
}

// TestProviderParserHostedParseKeepsDBBackedProjectLexicalForHostileCwd pins
// the same contract for a provider whose project extraction runs through an
// unconditional cwd helper: the working directory recorded inside a
// materialized provider database must stay lexical metadata and must not
// walk this worker's filesystem.
func TestProviderParserHostedParseKeepsDBBackedProjectLexicalForHostileCwd(t *testing.T) {
	t.Parallel()
	hostileCwd := hostileServerRepo(t, "server-secret-forge")
	dbBytes := forgeSnapshotFixtureWithCwd(t, map[string]string{
		"conv-001": "2026-05-02 09:58:15",
	}, hostileCwd)
	dbRef := objectRefForBytes(t, dbBytes)
	manifest := parserTestManifest(t, parser.AgentForge, parser.ForgeDBFilename, []rawsync.Entry{{
		Path: parser.ForgeDBFilename, Type: "file", Length: int64(len(dbBytes)),
		Objects: []rawsync.ObjectRef{dbRef},
	}})
	materialized, err := (Materializer{
		Store:   &materializerStore{objects: map[rawsync.ObjectRef][]byte{dbRef: dbBytes}},
		BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20,
	}).Materialize(t.Context(), manifest)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, materialized.Cleanup()) })
	dispatch, err := NewProviderParser(parser.ProviderFactories(), "hosted-worker")
	require.NoError(t, err)

	parsed, err := dispatch.Parse(t.Context(), manifest, materialized)

	require.NoError(t, err)
	require.Len(t, parsed.Outcome.Results, 1)
	project := parsed.Outcome.Results[0].Result.Session.Project
	assert.Equal(t, "workdir", project,
		"db-backed hosted attribution must stay lexical for a hostile recorded cwd")
	assert.NotContains(t, project, "secret",
		"the server-side repository name must not leak through project attribution")
}

// TestProviderParserAppliesHostedProjectPolicyToDiscoveryAndParse pins that
// every context rawderive hands to a provider -- raw-capture discovery and
// the parse itself -- carries the hosted filesystem-project-discovery guard,
// by probing through the same context-honoring helper providers use.
func TestProviderParserAppliesHostedProjectPolicyToDiscoveryAndParse(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	localPath := filepath.Join(root, "session.jsonl")
	require.NoError(t, os.WriteFile(localPath, []byte("session"), 0o400))
	hostileCwd := hostileServerRepo(t, "server-secret-policy")
	manifest := parserTestManifest(t, parser.AgentClaude, "/canonical/claude/session.jsonl", []rawsync.Entry{{
		Path: "session.jsonl", Type: "file", Length: 7,
		Objects: []rawsync.ObjectRef{objectRefForBytes(t, []byte("session"))},
	}})
	fixture := &projectPolicyProbingFixture{
		root: root, localPath: localPath, hostileCwd: hostileCwd,
	}
	dispatch, err := NewProviderParser(
		[]parser.ProviderFactory{&projectPolicyProbingFactory{provider: fixture}}, "hosted-worker",
	)
	require.NoError(t, err)

	parsed, err := dispatch.Parse(t.Context(), manifest, &Materialization{
		root: root, entries: map[string]string{"session.jsonl": localPath},
	})

	require.NoError(t, err)
	require.Len(t, parsed.Outcome.Results, 1)
	for _, probe := range []struct {
		name    string
		project string
	}{{"discovery", fixture.discoveryProject}, {"parse", fixture.parseProject}} {
		assert.Equal(t, "workdir", probe.project,
			"the hosted %s context must disable filesystem project discovery", probe.name)
		assert.NotContains(t, probe.project, "secret", probe.name)
	}
}

func TestStablePathMapUsesLongestSourceKeySuffix(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	deepPath := filepath.Join(root, "x", "y", "file.jsonl")
	shallowPath := filepath.Join(root, "y", "file.jsonl")
	deepObject := objectRefForBytes(t, []byte("deep"))
	shallowObject := objectRefForBytes(t, []byte("shallow"))
	stablePath := "/canonical/x/y/file.jsonl"
	manifest := parserTestManifest(t, parser.AgentClaude, stablePath, []rawsync.Entry{
		{
			Path: "x/y/file.jsonl", Type: "file", Length: deepObject.Length,
			Objects: []rawsync.ObjectRef{deepObject},
		},
		{
			Path: "y/file.jsonl", Type: "file", Length: shallowObject.Length,
			Objects: []rawsync.ObjectRef{shallowObject},
		},
	})
	paths := newStablePathMap(manifest, &Materialization{
		root: root,
		entries: map[string]string{
			"x/y/file.jsonl": deepPath,
			"y/file.jsonl":   shallowPath,
		},
	})

	resolved, ok := paths.resolve(stablePath)
	require.True(t, ok)
	assert.Equal(t, deepPath, resolved)
	assert.Equal(t, stablePath, paths.rewrite(deepPath))
	assert.Equal(t, paths.prefix+"y/file.jsonl", paths.rewrite(shallowPath))
}

func TestStablePathMapResolvesOriginalClientPathAliases(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	transcript := filepath.Join(root, "project", "session-a.jsonl")
	sidecar := filepath.Join(root, "project", "session-a", "tool-results", "r1.txt")
	transcriptObject := objectRefForBytes(t, []byte("transcript"))
	sidecarObject := objectRefForBytes(t, []byte("sidecar"))

	for _, tc := range []struct {
		name         string
		sourceKey    string
		embeddedPath string
	}{
		{
			name:         "posix client path",
			sourceKey:    "/srv/agent/projects/project/session-a.jsonl",
			embeddedPath: "/srv/agent/projects/project/session-a/tool-results/r1.txt",
		},
		{
			name:         "windows client path parsed on another host",
			sourceKey:    `C:\ProgramData\agent\projects\project\session-a.jsonl`,
			embeddedPath: `C:\ProgramData\agent\projects\project\session-a\tool-results\r1.txt`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			manifest := parserTestManifest(t, parser.AgentClaude, tc.sourceKey, []rawsync.Entry{
				{
					Path: "project/session-a.jsonl", Type: "file", Length: transcriptObject.Length,
					Objects: []rawsync.ObjectRef{transcriptObject},
				},
				{
					Path: "project/session-a/tool-results/r1.txt", Type: "file", Length: sidecarObject.Length,
					Objects: []rawsync.ObjectRef{sidecarObject},
				},
			})
			paths := newStablePathMap(manifest, &Materialization{
				root: root,
				entries: map[string]string{
					"project/session-a.jsonl":               transcript,
					"project/session-a/tool-results/r1.txt": sidecar,
				},
			})

			resolved, ok := paths.resolve(tc.embeddedPath)
			require.True(t, ok,
				"an original client path embedded in a transcript must resolve to its materialized companion")
			assert.Equal(t, sidecar, resolved)
			resolved, ok = paths.resolve(tc.sourceKey)
			require.True(t, ok)
			assert.Equal(t, transcript, resolved)

			// Aliases are lookup keys only: they must never become rewrite
			// targets for worker-local paths.
			assert.Equal(t, tc.sourceKey, paths.rewrite(transcript))
			assert.Equal(t, tc.embeddedPath, paths.rewrite(tc.embeddedPath))
		})
	}
}

func TestStablePathMapSkipsClientAliasesWithoutEntrySuffix(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	session := filepath.Join(root, "session.jsonl")
	object := objectRefForBytes(t, []byte("session"))
	// A source key that does not end in any manifest entry carries no safe
	// client-root derivation, so no aliases may be invented for it.
	manifest := parserTestManifest(t, parser.AgentClaude, "claude:opaque-uuid", []rawsync.Entry{{
		Path: "session.jsonl", Type: "file", Length: object.Length,
		Objects: []rawsync.ObjectRef{object},
	}})
	paths := newStablePathMap(manifest, &Materialization{
		root: root, entries: map[string]string{"session.jsonl": session},
	})

	_, ok := paths.resolve("/srv/agent/projects/session.jsonl")
	assert.False(t, ok)
	assert.Equal(t, paths.prefix+"session.jsonl", paths.rewrite(session))
}

func TestProviderParserMatchesPlanEntriesAcrossEquivalentPathSpellings(t *testing.T) {
	t.Parallel()
	// Plan validation resolves symlinks while the materialization keeps its
	// original spelling, so the same regular file can carry two spellings.
	for _, tc := range []struct {
		name string
		// prepare builds one regular file reachable through an alternate
		// spelling and returns the materialization root, the manifest entry's
		// materialized path, and the plan-side path spelling.
		prepare func(t *testing.T, base string) (root, materializedPath, planPath string, ok bool)
	}{
		{
			name: "materialization root contains a symlinked directory",
			prepare: func(t *testing.T, base string) (string, string, string, bool) {
				realDir := filepath.Join(base, "real")
				if err := os.MkdirAll(realDir, 0o700); err != nil {
					return "", "", "", false
				}
				alias := filepath.Join(base, "alias")
				if err := os.Symlink(realDir, alias); err != nil {
					return "", "", "", false
				}
				materialized := filepath.Join(alias, "session.jsonl")
				if err := os.WriteFile(materialized, []byte("session"), 0o600); err != nil {
					return "", "", "", false
				}
				return alias, materialized, materialized, true
			},
		},
		{
			name: "plan entry reaches the materialized file through a symlink",
			prepare: func(t *testing.T, base string) (string, string, string, bool) {
				root := filepath.Join(base, "mat")
				if err := os.MkdirAll(root, 0o700); err != nil {
					return "", "", "", false
				}
				materialized := filepath.Join(root, "session.jsonl")
				if err := os.WriteFile(materialized, []byte("session"), 0o600); err != nil {
					return "", "", "", false
				}
				if err := os.Symlink(materialized, filepath.Join(root, "linked.jsonl")); err != nil {
					return "", "", "", false
				}
				return root, materialized, filepath.Join(root, "linked.jsonl"), true
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, materializedPath, planPath, ok := tc.prepare(t, t.TempDir())
			if !ok {
				t.Skipf("path aliasing unavailable on this platform")
			}
			stablePath := "/canonical/claude/session.jsonl"
			manifest := parserTestManifest(t, parser.AgentClaude, stablePath, []rawsync.Entry{{
				Path: "session.jsonl", Type: "file", Length: 7,
				Objects: []rawsync.ObjectRef{objectRefForBytes(t, []byte("session"))},
			}})
			provider := &providerParserFixture{
				root: root, localPath: materializedPath, planLocalPath: planPath,
			}
			dispatch, err := NewProviderParser(
				[]parser.ProviderFactory{&providerParserFixtureFactory{provider: provider}}, "hosted-worker",
			)
			require.NoError(t, err)

			parsed, err := dispatch.Parse(t.Context(), manifest, &Materialization{
				root: root, entries: map[string]string{"session.jsonl": materializedPath},
			})

			require.NoError(t, err,
				"the same regular file must match across equivalent path spellings")
			require.Len(t, parsed.Outcome.Results, 1)
			assert.Equal(t, stablePath, parsed.Outcome.Results[0].Result.Session.File.Path)
		})
	}
}
func TestProviderParserHostedParseMatchesLocalPersistedToolResults(t *testing.T) {
	t.Parallel()
	clientRoot := t.TempDir()
	projectDir := "demo-project"
	sessionID := "session-hosted"
	transcriptPath := filepath.Join(clientRoot, projectDir, sessionID+".jsonl")
	sidecarPath := filepath.Join(
		clientRoot, projectDir, sessionID, "tool-results", "r1.txt",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(sidecarPath), 0o755))
	fullOutput := "hosted parity full output line 1\nhosted parity full output line 2\n"
	require.NoError(t, os.WriteFile(sidecarPath, []byte(fullOutput), 0o644))
	persistedNotice := "<persisted-output>\nOutput too large (48B). Full output saved to: " +
		sidecarPath + "\n</persisted-output>"
	transcript := strings.Join([]string{
		`{"type":"user","timestamp":"2026-08-13T12:00:00Z","uuid":"u1","sessionId":"` + sessionID + `","cwd":"/work/project","message":{"content":"run it"}}`,
		`{"type":"assistant","timestamp":"2026-08-13T12:00:01Z","uuid":"a1","parentUuid":"u1","sessionId":"` + sessionID + `","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make"}}]}}`,
		`{"type":"user","timestamp":"2026-08-13T12:00:02Z","uuid":"u2","parentUuid":"a1","sessionId":"` + sessionID + `","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + strconv.Quote(persistedNotice) + `,"is_error":false}]},"toolUseResult":{"persistedOutputPath":` + strconv.Quote(sidecarPath) + `,"persistedOutputSize":48}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(transcriptPath, []byte(transcript), 0o644))

	localProvider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{
		Roots: []string{clientRoot}, Machine: "hosted-worker",
	})
	require.True(t, ok)
	sources, err := localProvider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	localOutcome, err := localProvider.Parse(t.Context(), parser.ParseRequest{
		Source: sources[0], Fingerprint: parser.SourceFingerprint{Key: transcriptPath},
		Machine: "hosted-worker",
	})
	require.NoError(t, err)
	require.Len(t, localOutcome.Results, 1)
	localMessages := localOutcome.Results[0].Result.Messages
	require.Len(t, localMessages, 3)
	localToolResults := localMessages[2].ToolResults
	require.Len(t, localToolResults, 1)
	assert.Equal(t, fullOutput, parser.DecodeContent(localToolResults[0].ContentRaw),
		"local parse must resolve the persisted tool result from its sidecar")

	manifest, objects := manifestFromCapturePlan(t, parser.AgentClaude, localProvider, sources[0])
	materialized, err := (Materializer{
		Store:   &materializerStore{objects: objects},
		BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20,
	}).Materialize(t.Context(), manifest)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, materialized.Cleanup()) })
	dispatch, err := NewProviderParser(parser.ProviderFactories(), "hosted-worker")
	require.NoError(t, err)

	hosted, err := dispatch.Parse(t.Context(), manifest, materialized)

	require.NoError(t, err)
	require.Len(t, hosted.Outcome.Results, 1)
	hostedMessages := hosted.Outcome.Results[0].Result.Messages
	require.Len(t, hostedMessages, 3)
	hostedToolResults := hostedMessages[2].ToolResults
	require.Len(t, hostedToolResults, 1)
	assert.Equal(t, localToolResults[0].ContentLength, hostedToolResults[0].ContentLength)
	assert.Equal(t, fullOutput, parser.DecodeContent(hostedToolResults[0].ContentRaw),
		"materialized parse must resolve persisted tool results exactly like local parse")
	assert.Equal(t,
		localOutcome.Results[0].Result.Session.File.Path,
		hosted.Outcome.Results[0].Result.Session.File.Path,
		"the hosted session must keep the captured client path")
}

// manifestFromCapturePlan builds a canonical manifest from the provider-owned
// capture plan of one source, mirroring how the client producer materializes a
// generation: every planned file becomes one entry carrying its digest, size,
// and captured mod time.
func manifestFromCapturePlan(
	t *testing.T,
	agent parser.AgentType,
	provider parser.Provider,
	source parser.SourceRef,
) (rawsync.CanonicalManifest, map[rawsync.ObjectRef][]byte) {
	t.Helper()
	plan, supported, err := parser.ResolveRawCapturePlan(t.Context(), provider, source)
	require.NoError(t, err)
	require.True(t, supported)
	entries := make([]rawsync.Entry, 0, len(plan.Entries))
	objects := make(map[rawsync.ObjectRef][]byte, len(plan.Entries))
	for _, entry := range plan.Entries {
		contents, err := os.ReadFile(entry.LocalPath)
		require.NoError(t, err)
		info, err := os.Stat(entry.LocalPath)
		require.NoError(t, err)
		object := objectRefForBytes(t, contents)
		objects[object] = contents
		entries = append(entries, rawsync.Entry{
			Path: entry.Path, Type: "file", Length: info.Size(),
			ModTimeNS: info.ModTime().UnixNano(),
			Objects:   []rawsync.ObjectRef{object},
		})
	}
	return parserTestManifest(t, agent, plan.SourceKey, entries), objects
}

func TestProviderParserHostedParseMatchesLocalBackgroundForkLineage(t *testing.T) {
	t.Parallel()
	clientRoot := t.TempDir()
	projectDir := filepath.Join(clientRoot, "demo-project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	origLine := func(uuid, parent, ts, sessionID, kind, content string) string {
		parentJSON := "null"
		if parent != "" {
			parentJSON = strconv.Quote(parent)
		}
		kindField := ""
		if kind != "" {
			kindField = `"sessionKind":` + strconv.Quote(kind) + `,`
		}
		return `{"type":"user","uuid":` + strconv.Quote(uuid) + `,"parentUuid":` + parentJSON +
			`,"timestamp":"` + ts + `","sessionId":"` + sessionID + `",` + kindField +
			`"cwd":"/work/project","message":{"content":` + strconv.Quote(content) + `}}`
	}
	assistantLine := func(uuid, parent, ts, sessionID, kind, text string) string {
		kindField := ""
		if kind != "" {
			kindField = `"sessionKind":` + strconv.Quote(kind) + `,`
		}
		return `{"type":"assistant","uuid":` + strconv.Quote(uuid) + `,"parentUuid":` + strconv.Quote(parent) +
			`,"timestamp":"` + ts + `","sessionId":"` + sessionID + `",` + kindField +
			`"message":{"id":"msg_` + uuid + `","content":[{"type":"text","text":` + strconv.Quote(text) + `}]}}`
	}
	origContent := strings.Join([]string{
		origLine("u1", "", "2026-01-01T10:00:00Z", "orig-1111", "", "first question"),
		assistantLine("a1", "u1", "2026-01-01T10:00:05Z", "orig-1111", "", "first answer"),
	}, "\n") + "\n"
	forkContent := strings.Join([]string{
		origLine("u1", "", "2026-01-01T10:00:00Z", "fork-2222", "bg", "first question"),
		assistantLine("a1", "u1", "2026-01-01T10:00:05Z", "fork-2222", "bg", "first answer"),
		origLine("u2", "a1", "2026-01-01T11:00:00Z", "fork-2222", "bg", "continued question"),
		assistantLine("a2", "u2", "2026-01-01T11:00:05Z", "fork-2222", "bg", "continued answer"),
	}, "\n") + "\n"
	origPath := filepath.Join(projectDir, "orig-1111.jsonl")
	forkPath := filepath.Join(projectDir, "fork-2222.jsonl")
	require.NoError(t, os.WriteFile(origPath, []byte(origContent), 0o644))
	require.NoError(t, os.WriteFile(forkPath, []byte(forkContent), 0o644))

	localProvider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{
		Roots: []string{clientRoot}, Machine: "hosted-worker",
	})
	require.True(t, ok)
	sources, err := localProvider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 2)
	var forkSource parser.SourceRef
	for _, source := range sources {
		if strings.HasSuffix(source.Key, "fork-2222.jsonl") {
			forkSource = source
		}
	}
	require.NotEmpty(t, forkSource.Key)
	localOutcome, err := localProvider.Parse(t.Context(), parser.ParseRequest{
		Source: forkSource, Fingerprint: parser.SourceFingerprint{Key: forkPath},
		Machine: "hosted-worker",
	})
	require.NoError(t, err)
	require.Len(t, localOutcome.Results, 1)
	localSession := localOutcome.Results[0].Result.Session
	require.Equal(t, "orig-1111", localSession.ParentSessionID,
		"local parse must link the background fork to its parent")
	require.Len(t, localOutcome.Results[0].Result.Messages, 2,
		"local parse must trim the replayed prefix")

	manifest, objects := manifestFromCapturePlan(t, parser.AgentClaude, localProvider, forkSource)
	require.Len(t, manifest.Manifest.Entries, 2,
		"the fork generation must carry its lineage sibling transcript")
	materialized, err := (Materializer{
		Store:   &materializerStore{objects: objects},
		BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20,
	}).Materialize(t.Context(), manifest)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, materialized.Cleanup()) })
	dispatch, err := NewProviderParser(parser.ProviderFactories(), "hosted-worker")
	require.NoError(t, err)

	hosted, err := dispatch.Parse(t.Context(), manifest, materialized)

	require.NoError(t, err)
	require.Len(t, hosted.Outcome.Results, 1)
	hostedResult := hosted.Outcome.Results[0].Result
	hostedSession := hostedResult.Session
	assert.Equal(t, localSession.ParentSessionID, hostedSession.ParentSessionID,
		"materialized parse must reproduce background-fork parent linkage")
	assert.Equal(t, localSession.RelationshipType, hostedSession.RelationshipType)
	assert.Equal(t, localSession.MessageCount, hostedSession.MessageCount)
	require.Len(t, hostedResult.Messages, 2,
		"materialized parse must trim the replayed prefix like local parse")
	for i := range hostedResult.Messages {
		assert.Equal(t,
			localOutcome.Results[0].Result.Messages[i].Content,
			hostedResult.Messages[i].Content)
		assert.Equal(t,
			localOutcome.Results[0].Result.Messages[i].Role,
			hostedResult.Messages[i].Role)
		assert.Equal(t,
			localOutcome.Results[0].Result.Messages[i].Timestamp,
			hostedResult.Messages[i].Timestamp)
	}
	assert.Equal(t, forkPath, hostedSession.File.Path,
		"the hosted session must keep the captured client path")
}

func TestProviderParserMatchSelectsPrimarySourceWhenSiblingPlansShareEntries(t *testing.T) {
	t.Parallel()
	clientRoot := t.TempDir()
	projectDir := filepath.Join(clientRoot, "demo-project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	line := func(kind, uuid, parent, ts, sessionID, mark, content string) string {
		parentJSON := "null"
		if parent != "" {
			parentJSON = strconv.Quote(parent)
		}
		markField := ""
		if mark != "" {
			markField = `"sessionKind":` + strconv.Quote(mark) + `,`
		}
		role := "user"
		body := `"message":{"content":` + strconv.Quote(content) + `}`
		if kind == "assistant" {
			role = "assistant"
			body = `"message":{"id":"msg_` + uuid + `","content":[{"type":"text","text":` +
				strconv.Quote(content) + `}]}`
		}
		return `{"type":"` + role + `","uuid":` + strconv.Quote(uuid) + `,"parentUuid":` + parentJSON +
			`,"timestamp":"` + ts + `","sessionId":"` + sessionID + `",` + markField + body + `}`
	}
	chain := func(sessionID string, ownQuestion, ownAnswer string) string {
		return strings.Join([]string{
			line("user", "u1", "", "2026-01-01T10:00:00Z", sessionID, "bg", "first question"),
			line("assistant", "a1", "u1", "2026-01-01T10:00:05Z", sessionID, "bg", "first answer"),
			line("user", "u2", "a1", "2026-01-01T11:00:00Z", sessionID, "bg", ownQuestion),
			line("assistant", "a2", "u2", "2026-01-01T11:00:05Z", sessionID, "bg", ownAnswer),
		}, "\n") + "\n"
	}
	// A chained background fork: fork-2222 and fork-3333 are both bg-marked
	// and share one root uuid, so each one's capture plan carries the other as
	// an immutable lineage input and both plans cover the same entry set.
	require.NoError(t, os.WriteFile(
		filepath.Join(projectDir, "fork-2222.jsonl"),
		[]byte(chain("fork-2222", "continued question", "continued answer")), 0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(projectDir, "fork-3333.jsonl"),
		[]byte(chain("fork-3333", "later question", "later answer")), 0o644,
	))

	localProvider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{
		Roots: []string{clientRoot}, Machine: "hosted-worker",
	})
	require.True(t, ok)
	sources, err := localProvider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 2)
	var primary parser.SourceRef
	for _, source := range sources {
		if strings.HasSuffix(source.Key, "fork-2222.jsonl") {
			primary = source
		}
	}
	require.NotEmpty(t, primary.Key)

	manifest, objects := manifestFromCapturePlan(t, parser.AgentClaude, localProvider, primary)
	require.Len(t, manifest.Manifest.Entries, 2)
	materialized, err := (Materializer{
		Store:   &materializerStore{objects: objects},
		BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20,
	}).Materialize(t.Context(), manifest)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, materialized.Cleanup()) })
	dispatch, err := NewProviderParser(parser.ProviderFactories(), "hosted-worker")
	require.NoError(t, err)

	hosted, err := dispatch.Parse(t.Context(), manifest, materialized)

	require.NoError(t, err,
		"shared sibling entries must not make source matching ambiguous")
	require.Len(t, hosted.Outcome.Results, 1)
	assert.True(t, strings.HasSuffix(
		hosted.Outcome.Results[0].Result.Session.File.Path, "fork-2222.jsonl"),
		"the manifest's primary transcript must be the parsed source")
	assert.Equal(t, "fork-2222", hosted.Outcome.Results[0].Result.Session.ID)
}

// TestProviderParserRejectsProviderWithoutRawCaptureSupportBeforeDiscovery
// pins that a registered factory which does not advertise
// RawCapture.Support is rejected before the provider is constructed or its
// discovery runs: unsupported providers would otherwise fall back to their
// ordinary normalized discovery over untrusted materialized data.
func TestProviderParserRejectsProviderWithoutRawCaptureSupportBeforeDiscovery(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	localPath := filepath.Join(root, "session.jsonl")
	require.NoError(t, os.WriteFile(localPath, []byte("session"), 0o400))
	manifest := parserTestManifest(t, parser.AgentClaude, "/canonical/claude/session.jsonl", []rawsync.Entry{{
		Path: "session.jsonl", Type: "file", Length: 7,
		Objects: []rawsync.ObjectRef{objectRefForBytes(t, []byte("session"))},
	}})
	factory := &unsupportedRawCaptureFactory{provider: &unsupportedRawCaptureProvider{}}
	dispatch, err := NewProviderParser([]parser.ProviderFactory{factory}, "hosted-worker")
	require.NoError(t, err)

	parsed, err := dispatch.Parse(t.Context(), manifest, &Materialization{
		root: root, entries: map[string]string{"session.jsonl": localPath},
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, rawsync.ErrInvalid)
	assert.Contains(t, err.Error(), parser.ProviderFeatureRawCapture)
	assert.False(t, parsed.Tombstone)
	assert.Empty(t, parsed.Outcome.Results)
	assert.False(t, factory.constructed,
		"an unsupported provider must be rejected before construction")
	assert.False(t, factory.provider.discoverInvoked,
		"an unsupported provider must be rejected before discovery runs")
}

func TestProviderParserHandlesTombstoneWithoutInvokingProvider(t *testing.T) {
	t.Parallel()
	identity, err := rawsync.NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	manifest, err := rawsync.ValidateAndCanonicalize(identity, rawsync.Manifest{
		SchemaVersion:    rawsync.ManifestSchemaVersion,
		Provider:         parser.AgentClaude,
		ConfiguredRootID: "root-a",
		SourceKey:        "/canonical/claude/session.jsonl",
		CaptureID:        "capture-a",
		CapturedAt:       time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC),
		Kind:             rawsync.ManifestTombstone,
	}, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	dispatch, err := NewProviderParser(parser.ProviderFactories(), "hosted-worker")
	require.NoError(t, err)

	parsed, err := dispatch.Parse(t.Context(), manifest, nil)
	require.NoError(t, err)
	assert.True(t, parsed.Tombstone)
	assert.Empty(t, parsed.Outcome.Results)
}

type providerParserFixtureFactory struct {
	provider *providerParserFixture
	config   parser.ProviderConfig
}

func (f *providerParserFixtureFactory) Definition() parser.AgentDef {
	return parser.AgentDef{Type: parser.AgentClaude}
}

func (f *providerParserFixtureFactory) Capabilities() parser.Capabilities {
	return providerParserCapabilities()
}

func (f *providerParserFixtureFactory) NewProvider(config parser.ProviderConfig) parser.Provider {
	f.config = config
	return f.provider
}

type providerParserFixture struct {
	parser.Provider
	root          string
	localPath     string
	planLocalPath string
	request       parser.ParseRequest
}

func (p *providerParserFixture) Definition() parser.AgentDef {
	return parser.AgentDef{Type: parser.AgentClaude}
}

func (p *providerParserFixture) Capabilities() parser.Capabilities {
	return providerParserCapabilities()
}

func (p *providerParserFixture) Discover(context.Context) ([]parser.SourceRef, error) {
	return []parser.SourceRef{{
		Provider:       parser.AgentClaude,
		ConfiguredRoot: p.root,
		Key:            p.localPath,
		DisplayPath:    p.localPath,
		FingerprintKey: p.localPath,
	}}, nil
}

func (p *providerParserFixture) DiscoverRawCaptureSourcesEach(
	ctx context.Context,
	yield func(parser.SourceRef) error,
) (bool, error) {
	sources, err := p.Discover(ctx)
	if err != nil {
		return false, err
	}
	for _, source := range sources {
		if err := yield(source); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (p *providerParserFixture) PlanRawCapture(
	_ context.Context,
	_ parser.SourceRef,
) (parser.RawCapturePlan, error) {
	planPath := p.planLocalPath
	if planPath == "" {
		planPath = p.localPath
	}
	return parser.RawCapturePlan{
		ConfiguredRoot: p.root,
		CaptureRoot:    p.root,
		SourceKey:      p.localPath,
		Entries: []parser.RawCaptureEntry{{
			Path: "session.jsonl", LocalPath: planPath, Appendable: true,
		}},
	}, nil
}

func (p *providerParserFixture) Fingerprint(
	_ context.Context,
	source parser.SourceRef,
) (parser.SourceFingerprint, error) {
	return parser.SourceFingerprint{Key: source.FingerprintKey, Size: 7}, nil
}

func (p *providerParserFixture) Parse(
	_ context.Context,
	request parser.ParseRequest,
) (parser.ParseOutcome, error) {
	p.request = request
	return parser.ParseOutcome{
		Results: []parser.ParseResultOutcome{{Result: parser.ParseResult{
			Session: parser.ParsedSession{File: parser.FileInfo{Path: p.localPath}},
		}}},
		ResultSetComplete: true,
	}, nil
}

func providerParserCapabilities() parser.Capabilities {
	return parser.Capabilities{RawCapture: parser.RawCaptureCapabilities{
		Support:  parser.CapabilitySupported,
		Shape:    parser.RawCaptureShapeFiles,
		Append:   parser.RawCaptureAppendOne,
		Snapshot: parser.RawCaptureSnapshotNone,
	}}
}

// unsupportedRawCaptureFactory is registered for Claude but advertises no
// raw-capture support, recording whether construction ever happened.
type unsupportedRawCaptureFactory struct {
	provider    *unsupportedRawCaptureProvider
	constructed bool
}

func (f *unsupportedRawCaptureFactory) Definition() parser.AgentDef {
	return parser.AgentDef{Type: parser.AgentClaude}
}

func (f *unsupportedRawCaptureFactory) Capabilities() parser.Capabilities {
	capabilities := providerParserCapabilities()
	capabilities.RawCapture.Support = parser.CapabilityUnsupported
	return capabilities
}

func (f *unsupportedRawCaptureFactory) NewProvider(parser.ProviderConfig) parser.Provider {
	f.constructed = true
	return f.provider
}

// unsupportedRawCaptureProvider records any discovery invocation and fails
// if its unsupported discovery surface is ever reached.
type unsupportedRawCaptureProvider struct {
	parser.Provider
	discoverInvoked bool
}

func (p *unsupportedRawCaptureProvider) Definition() parser.AgentDef {
	return parser.AgentDef{Type: parser.AgentClaude}
}

func (p *unsupportedRawCaptureProvider) Capabilities() parser.Capabilities {
	return parser.Capabilities{}
}

func (p *unsupportedRawCaptureProvider) DiscoverRawCaptureSourcesEach(
	context.Context, func(parser.SourceRef) error,
) (bool, error) {
	p.discoverInvoked = true
	return false, errors.New("unsupported raw-capture discovery must never run")
}

type projectPolicyProbingFactory struct {
	provider *projectPolicyProbingFixture
}

func (f *projectPolicyProbingFactory) Definition() parser.AgentDef {
	return parser.AgentDef{Type: parser.AgentClaude}
}

func (f *projectPolicyProbingFactory) Capabilities() parser.Capabilities {
	return providerParserCapabilities()
}

func (f *projectPolicyProbingFactory) NewProvider(parser.ProviderConfig) parser.Provider {
	return f.provider
}

// projectPolicyProbingFixture answers project attribution through the same
// context-honoring helper providers use, at both hosted seams: raw-capture
// discovery and the parse itself.
type projectPolicyProbingFixture struct {
	parser.Provider
	root             string
	localPath        string
	hostileCwd       string
	discoveryProject string
	parseProject     string
}

func (p *projectPolicyProbingFixture) Definition() parser.AgentDef {
	return parser.AgentDef{Type: parser.AgentClaude}
}

func (p *projectPolicyProbingFixture) Capabilities() parser.Capabilities {
	return providerParserCapabilities()
}

func (p *projectPolicyProbingFixture) DiscoverRawCaptureSourcesEach(
	ctx context.Context,
	yield func(parser.SourceRef) error,
) (bool, error) {
	p.discoveryProject = parser.ExtractProjectFromCwdWithBranchContext(
		ctx, p.hostileCwd, "",
	)
	return true, yield(parser.SourceRef{
		Provider:       parser.AgentClaude,
		ConfiguredRoot: p.root,
		Key:            p.localPath,
		DisplayPath:    p.localPath,
		FingerprintKey: p.localPath,
	})
}

func (p *projectPolicyProbingFixture) PlanRawCapture(
	_ context.Context,
	_ parser.SourceRef,
) (parser.RawCapturePlan, error) {
	return parser.RawCapturePlan{
		ConfiguredRoot: p.root,
		CaptureRoot:    p.root,
		SourceKey:      p.localPath,
		Entries: []parser.RawCaptureEntry{{
			Path: "session.jsonl", LocalPath: p.localPath, Appendable: true,
		}},
	}, nil
}

func (p *projectPolicyProbingFixture) Fingerprint(
	_ context.Context,
	source parser.SourceRef,
) (parser.SourceFingerprint, error) {
	return parser.SourceFingerprint{Key: source.FingerprintKey, Size: 7}, nil
}

func (p *projectPolicyProbingFixture) Parse(
	ctx context.Context,
	_ parser.ParseRequest,
) (parser.ParseOutcome, error) {
	p.parseProject = parser.ExtractProjectFromCwdWithBranchContext(
		ctx, p.hostileCwd, "",
	)
	return parser.ParseOutcome{
		Results: []parser.ParseResultOutcome{{Result: parser.ParseResult{
			Session: parser.ParsedSession{
				File:    parser.FileInfo{Path: p.localPath},
				Project: p.parseProject,
			},
		}}},
		ResultSetComplete: true,
	}, nil
}

func parserTestManifest(
	t *testing.T,
	provider parser.AgentType,
	sourceKey string,
	entries []rawsync.Entry,
) rawsync.CanonicalManifest {
	t.Helper()
	identity, err := rawsync.NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	manifest, err := rawsync.ValidateAndCanonicalize(identity, rawsync.Manifest{
		SchemaVersion:    rawsync.ManifestSchemaVersion,
		Provider:         provider,
		ConfiguredRootID: "root-a",
		SourceKey:        sourceKey,
		CaptureID:        "capture-a",
		CapturedAt:       time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC),
		Kind:             rawsync.ManifestSnapshot,
		Entries:          entries,
	}, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	return manifest
}

func TestProviderParserParsesEveryForgeSessionFromMaterializedSnapshot(t *testing.T) {
	t.Parallel()
	dbBytes := forgeSnapshotFixture(t, map[string]string{
		"conv-001": "2026-05-02 09:58:15",
		"conv-002": "2026-05-03 09:58:15",
	})
	dbRef := objectRefForBytes(t, dbBytes)
	sourceModTime := time.Date(2026, 8, 13, 11, 0, 0, 987654321, time.UTC)
	manifest := parserTestManifest(t, parser.AgentForge, parser.ForgeDBFilename, []rawsync.Entry{{
		Path:      parser.ForgeDBFilename,
		Type:      "file",
		Length:    int64(len(dbBytes)),
		ModTimeNS: sourceModTime.UnixNano(),
		Objects:   []rawsync.ObjectRef{dbRef},
	}})
	store := &materializerStore{objects: map[rawsync.ObjectRef][]byte{dbRef: dbBytes}}
	materialized, err := (Materializer{
		Store: store, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20,
	}).Materialize(t.Context(), manifest)
	require.NoError(t, err)
	defer func() { require.NoError(t, materialized.Cleanup()) }()
	dispatch, err := NewProviderParser(parser.ProviderFactories(), "hosted-worker")
	require.NoError(t, err)

	parsed, err := dispatch.Parse(t.Context(), manifest, materialized)

	require.NoError(t, err)
	assert.False(t, parsed.Tombstone)
	require.Len(t, parsed.Outcome.Results, 2)
	assert.True(t, parsed.Outcome.ResultSetComplete)
	assert.True(t, parsed.Outcome.ForceReplace)
	assert.Empty(t, parsed.Outcome.SourceErrors)
	first := parsed.Outcome.Results[0].Result
	second := parsed.Outcome.Results[1].Result
	assert.Equal(t, "forge:conv-001", first.Session.ID)
	assert.Equal(t, "forge:conv-002", second.Session.ID)
	assert.Equal(t, parser.AgentForge, first.Session.Agent)
	assert.Equal(t, parser.ForgeDBFilename+"#conv-001", first.Session.File.Path,
		"each logical session must keep a stable per-session virtual identity")
	assert.Equal(t, parser.ForgeDBFilename+"#conv-002", second.Session.File.Path)
	assert.NotContains(t, first.Session.File.Path, materialized.Root())
	assert.Equal(t, int(2), first.Session.MessageCount)
	assert.Equal(t, int(2), second.Session.MessageCount)
	assert.Equal(t,
		time.Date(2026, 5, 2, 10, 0, 16, 848497543, time.UTC).UnixNano(),
		first.Session.File.Mtime,
		"database-derived session timestamps must stay source-derived")
	assert.Equal(t,
		time.Date(2026, 5, 3, 10, 00, 16, 848497543, time.UTC).UnixNano(),
		second.Session.File.Mtime)
	assert.NotEqual(t, sourceModTime.UnixNano(), first.Session.File.Mtime)
}

func TestProviderParserHandlesEmptyForgeSnapshot(t *testing.T) {
	t.Parallel()
	dbBytes := forgeSnapshotFixture(t, nil)
	dbRef := objectRefForBytes(t, dbBytes)
	manifest := parserTestManifest(t, parser.AgentForge, parser.ForgeDBFilename, []rawsync.Entry{{
		Path:    parser.ForgeDBFilename,
		Type:    "file",
		Length:  int64(len(dbBytes)),
		Objects: []rawsync.ObjectRef{dbRef},
	}})
	store := &materializerStore{objects: map[rawsync.ObjectRef][]byte{dbRef: dbBytes}}
	materialized, err := (Materializer{
		Store: store, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20,
	}).Materialize(t.Context(), manifest)
	require.NoError(t, err)
	defer func() { require.NoError(t, materialized.Cleanup()) }()
	dispatch, err := NewProviderParser(parser.ProviderFactories(), "hosted-worker")
	require.NoError(t, err)

	parsed, err := dispatch.Parse(t.Context(), manifest, materialized)

	require.NoError(t, err)
	assert.False(t, parsed.Tombstone)
	assert.Empty(t, parsed.Outcome.Results)
	assert.True(t, parsed.Outcome.ResultSetComplete)
	assert.True(t, parsed.Outcome.ForceReplace)
	assert.Equal(t, parser.SkipNoSession, parsed.Outcome.SkipReason)
}

// forgeSnapshotFixture writes a real Forge SQLite store carrying one
// conversation per supplied session ID mapped to its created_at timestamp.
func forgeSnapshotFixture(t *testing.T, conversations map[string]string) []byte {
	t.Helper()
	return forgeSnapshotFixtureWithCwd(t, conversations, "/srv/agent/project")
}

// forgeSnapshotFixtureWithCwd writes a real Forge SQLite store whose recorded
// system working directory is cwd.
func forgeSnapshotFixtureWithCwd(
	t *testing.T, conversations map[string]string, cwd string,
) []byte {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), parser.ForgeDBFilename)
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE conversations (
		conversation_id TEXT PRIMARY KEY NOT NULL,
		title TEXT,
		workspace_id BIGINT NOT NULL,
		context TEXT,
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP,
		metrics TEXT
	)`)
	require.NoError(t, err)
	ids := make([]string, 0, len(conversations))
	for id := range conversations {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		updatedAt := strings.Replace(conversations[id], "09:58:15", "10:00:16.848497543", 1)
		systemInformation := "<system_information>\n<current_working_directory>" +
			cwd + "</current_working_directory>\n</system_information>"
		context := fmt.Sprintf(`{
			"conversation_id": %q,
			"messages": [
				{
					"message": {"text": {
						"role": "System",
						"content": %s,
						"model": "gpt-5.4",
						"timestamp": "2026-05-02T09:58:15.741021507Z"
					}}
				},
				{
					"message": {"text": {
						"role": "User",
						"content": "Summarize the repository.",
						"model": "gpt-5.4",
						"timestamp": "2026-05-02T09:58:16.000000000Z"
					}}
				},
				{
					"message": {"text": {
						"role": "Assistant",
						"content": "It syncs agent sessions.",
						"model": "gpt-5.4",
						"timestamp": "2026-05-02T09:58:18.000000000Z"
					}}
				}
			]
		}`, id, strconv.Quote(systemInformation))
		_, err = db.Exec(
			`INSERT INTO conversations
			 (conversation_id, title, workspace_id, context, created_at, updated_at, metrics)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, "Forge Session", int64(1), context,
			conversations[id], updatedAt, "",
		)
		require.NoError(t, err)
	}
	require.NoError(t, db.Close())
	contents, err := os.ReadFile(dbPath)
	require.NoError(t, err)
	return contents
}
