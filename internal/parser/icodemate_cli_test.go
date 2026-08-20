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

// TestIcodemateCLIDefaultDirs asserts the terminal CLI projects root is a
// default alongside the VSCode-extension OpenCode storage root, so both
// families are collected out of the box.
func TestIcodemateCLIDefaultDirs(t *testing.T) {
	def, ok := AgentByType(AgentIcodemate)
	require.True(t, ok, "AgentIcodemate missing from Registry")
	assert.Contains(t, def.DefaultDirs, ".local/share/icodemate")
	assert.Contains(t, def.DefaultDirs, ".icodemate/cli/projects")
}

// TestIcodemateCLIDiscoverParseAndFindSource builds a Claude-format projects
// transcript under an IcodeMate CLI root and asserts the CLI source set
// discovers it, parses it onto the icodemate agent with the icodemate: ID
// prefix, and resolves it back through FindSource.
func TestIcodemateCLIDiscoverParseAndFindSource(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "my-project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	path := filepath.Join(projectDir, "session-cli.jsonl")
	content := strings.Join([]string{
		buildMetadataLine(map[string]any{
			"type": "user", "timestamp": tsEarly, "uuid": "u1", "parentUuid": "",
			"cwd": "/workspace/my-project", "gitBranch": "main",
			"message": map[string]any{"role": "user", "content": "hello cli"},
		}),
		buildMetadataLine(map[string]any{
			"type": "assistant", "timestamp": tsEarlyS1, "uuid": "u2", "parentUuid": "u1",
			"message": map[string]any{
				"role": "assistant", "stop_reason": "end_turn",
				"usage": map[string]any{
					"input_tokens":                12,
					"cache_creation_input_tokens": 3,
					"cache_read_input_tokens":     2,
					"output_tokens":               7,
				},
				"content": []map[string]any{{"type": "text", "text": "cli reply"}},
			},
		}),
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	provider, ok := NewProvider(AgentIcodemate, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, path, discovered[0].Key)
	assert.Equal(t, "my-project", discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "icodemate:session-cli",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, path, found.Key)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      discovered[0],
		Fingerprint: SourceFingerprint{Hash: "hash-cli"},
		Machine:     "devbox",
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.True(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)

	sess := outcome.Results[0].Result.Session
	msgs := outcome.Results[0].Result.Messages
	assert.Equal(t, AgentIcodemate, sess.Agent)
	assert.Equal(t, "icodemate:session-cli", sess.ID)
	assert.Equal(t, "my_project", sess.Project)
	assert.Equal(t, "/workspace/my-project", sess.Cwd)
	assert.Equal(t, "main", sess.GitBranch)
	assert.Equal(t, "hello cli", sess.FirstMessage)
	assert.Equal(t, "hash-cli", sess.File.Hash)
	require.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, 7, msgs[1].OutputTokens)
	assert.Equal(t, 17, msgs[1].ContextTokens)
}

// TestIcodemateProviderMergesOpenCodeAndCLIRoots exercises the hybrid
// provider over a single configured Roots list containing one OpenCode-format
// root (VSCode extension) and one Claude-format CLI root. Each family's
// session must be discovered and parsed through its own layout without
// cross-contamination, both relabeled onto the icodemate agent.
func TestIcodemateProviderMergesOpenCodeAndCLIRoots(t *testing.T) {
	opencodeRoot := t.TempDir()
	sessionPath := filepath.Join(
		opencodeRoot, "storage", "session_diff", "global", "ses_vscode.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_vscode",
		"parentID":  "",
		"directory": "/home/user/code/vscodeapp",
		"title":     "VSCode Session",
		"time":      map[string]any{"created": 1700000000000, "updated": 1700000060000},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		opencodeRoot, "storage", "message", "ses_vscode", "msg_1.json",
	), map[string]any{
		"id": "msg_1", "sessionID": "ses_vscode", "role": "user",
		"time": map[string]any{"created": 1700000000000},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		opencodeRoot, "storage", "part", "msg_1", "prt_1.json",
	), map[string]any{
		"id": "prt_1", "sessionID": "ses_vscode", "messageID": "msg_1",
		"type": "text", "text": "Hello from vscode",
		"time": map[string]any{"created": 1700000000000},
	})

	cliRoot := t.TempDir()
	projectDir := filepath.Join(cliRoot, "my-project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	cliPath := filepath.Join(projectDir, "session-cli.jsonl")
	cliContent := strings.Join([]string{
		buildMetadataLine(map[string]any{
			"type": "user", "timestamp": tsEarly, "uuid": "cu1", "parentUuid": "",
			"message": map[string]any{"role": "user", "content": "hello cli"},
		}),
		buildMetadataLine(map[string]any{
			"type": "assistant", "timestamp": tsEarlyS1, "uuid": "cu2", "parentUuid": "cu1",
			"message": map[string]any{
				"role": "assistant", "stop_reason": "end_turn",
				"content": []map[string]any{{"type": "text", "text": "cli reply"}},
			},
		}),
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(cliPath, []byte(cliContent), 0o644))

	provider, ok := NewProvider(AgentIcodemate, ProviderConfig{
		Roots:   []string{opencodeRoot, cliRoot},
		Machine: "testmachine",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)

	sourcesByKey := make(map[string]SourceRef, len(discovered))
	for _, src := range discovered {
		sourcesByKey[src.Key] = src
	}
	require.Contains(t, sourcesByKey, sessionPath)
	require.Contains(t, sourcesByKey, cliPath)

	for key, src := range sourcesByKey {
		outcome, err := provider.Parse(context.Background(), ParseRequest{
			Source: src, Machine: "testmachine",
		})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		parsed := outcome.Results[0].Result.Session
		assert.Equal(t, AgentIcodemate, parsed.Agent)
		switch key {
		case sessionPath:
			assert.Equal(t, "icodemate:ses_vscode", parsed.ID)
			assert.Equal(t, "Hello from vscode", outcome.Results[0].Result.Messages[0].Content)
		case cliPath:
			assert.Equal(t, "icodemate:session-cli", parsed.ID)
			assert.Equal(t, "hello cli", parsed.FirstMessage)
		}
	}
}

// TestIcodemateCLIProviderDiscoversS3Sessions verifies the CLI source set
// scans s3:// projects roots with the icodemate provider segment and agent
// label: discovered sources carry Provider=AgentIcodemate, machine metadata
// derived from the .../<machine>/raw/icodemate layout, and the folded
// tool-result sidecar freshness identity -- never Claude's session or machine
// namespace.
func TestIcodemateCLIProviderDiscoversS3Sessions(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	root := "s3://bucket/laptop/raw/icodemate"
	sessionURI := root + "/proj/session.jsonl"
	sessionMtime := time.Unix(100, 0)
	sidecarMtime := time.Unix(200, 0)
	listS3Objects = func(got string) ([]S3Object, error) {
		require.Equal(t, root, got)
		return []S3Object{
			{
				URI:          sessionURI,
				Size:         11,
				LastModified: sessionMtime,
				Fingerprint:  "s3-meta:session",
			},
			{
				URI:          root + "/proj/session/tool-results/out.txt",
				Size:         22,
				LastModified: sidecarMtime,
				Fingerprint:  "s3-meta:sidecar",
			},
		}, nil
	}

	provider, ok := NewProvider(AgentIcodemate, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	src := sources[0]
	assert.Equal(t, AgentIcodemate, src.Provider)
	assert.Equal(t, sessionURI, src.DisplayPath)
	assert.Equal(t, sessionURI, src.FingerprintKey)
	assert.Equal(t, "proj", src.ProjectHint)

	s3, ok := src.Opaque.(S3DiscoveredSource)
	require.True(t, ok, "s3 source carries S3DiscoveredSource opaque")
	assert.Equal(t, sessionURI, s3.URI)
	assert.Equal(t, "laptop", s3.Machine)
	assert.Equal(t, "proj", s3.Project)
	assert.Equal(t, int64(33), s3.Size)
	assert.Equal(t, sidecarMtime.UnixNano(), s3.MtimeNS)
	assert.Contains(t, s3.Fingerprint, "session")
	assert.Contains(t, s3.Fingerprint, "sidecar")
}

// TestIcodemateCLIProviderParsesMaterializedSource covers the engine's S3
// materialization path: an s3:// session fetched to a local temp file is
// re-parsed through the hybrid provider with a MaterializedFileSource opaque.
// The CLI source set must accept it so a materialized transcript parses onto
// the icodemate agent and fingerprints through the same path.
func TestIcodemateCLIProviderParsesMaterializedSource(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "my-project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, "session-mat.jsonl")
	writeSourceFile(t, path, claudeProviderFixture("hello materialized"))

	provider, ok := NewProvider(AgentIcodemate, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	materialized := SourceRef{
		Provider:       AgentIcodemate,
		Key:            "s3://bucket/laptop/raw/icodemate/proj/session-mat.jsonl",
		DisplayPath:    "s3://bucket/laptop/raw/icodemate/proj/session-mat.jsonl",
		FingerprintKey: "s3://bucket/laptop/raw/icodemate/proj/session-mat.jsonl",
		ProjectHint:    "my-project",
		Opaque:         MaterializedFileSource{Path: path},
	}

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: materialized, Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	assert.Equal(t, AgentIcodemate, sess.Agent)
	assert.Equal(t, "icodemate:session-mat", sess.ID)
	assert.Equal(t, "hello materialized", sess.FirstMessage)

	fingerprint, err := provider.Fingerprint(context.Background(), materialized)
	require.NoError(t, err)
	// Key is the canonical metadata identity, not the materialized read path.
	assert.Equal(t, materialized.Key, fingerprint.Key)
	assert.Positive(t, fingerprint.Size)
	assert.NotEmpty(t, fingerprint.Hash)
}

// TestIcodemateCLISourceMethods exercises the watch, fingerprint, and
// changed-path surfaces of the terminal CLI projects source set directly.
// A pure-CLI provider (no OpenCode root) must plan a recursive *.jsonl watch
// over the projects root, discover the Claude-format transcript, resolve it
// back through FindSource, fingerprint it, and map a watched path change back
// to exactly the owning source — while ignoring files outside the root.
func TestIcodemateCLISourceMethods(t *testing.T) {
	root := t.TempDir()
	projectDir := "-Users-dev-code-demo"
	sessionID := "cli-main"
	sourcePath := filepath.Join(root, projectDir, sessionID+".jsonl")
	wrongRoot := t.TempDir()
	outsidePath := filepath.Join(wrongRoot, "other-project", "outside.jsonl")
	writeSourceFile(t, sourcePath, claudeProviderFixture("hello cli source methods"))
	writeSourceFile(t, outsidePath, claudeProviderFixture("outside"))

	provider, ok := NewProvider(AgentIcodemate, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*.jsonl"}, plan.Roots[0].IncludeGlobs)
	assert.Equal(t, "icodemate:cli-projects:"+root, plan.Roots[0].DebounceKey)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].Key)
	assert.Equal(t, projectDir, discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: sessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, sourcePath, fingerprint.Key)
	assert.Positive(t, fingerprint.Size)
	assert.Positive(t, fingerprint.MTimeNS)
	assert.NotEmpty(t, fingerprint.Hash)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)

	require.NoError(t, os.Remove(sourcePath))
	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)

	ignored, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: outsidePath, EventKind: "write", WatchRoot: wrongRoot},
	)
	require.NoError(t, err)
	assert.Empty(t, ignored)
}
