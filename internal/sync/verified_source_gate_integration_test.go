package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

type verifiedSourceCountingProvider struct {
	parser.ProviderBase
	root             string
	sources          map[string]parser.SourceRef
	fingerprintCalls int
}

func (p *verifiedSourceCountingProvider) WatchPlan(
	context.Context,
) (parser.WatchPlan, error) {
	return parser.WatchPlan{Roots: []parser.WatchRoot{{
		Path: p.root, Recursive: true,
	}}}, nil
}

func (p *verifiedSourceCountingProvider) SourcesForChangedPath(
	_ context.Context,
	req parser.ChangedPathRequest,
) ([]parser.SourceRef, error) {
	if source, ok := p.sources[filepath.Clean(req.Path)]; ok {
		return []parser.SourceRef{source}, nil
	}
	return nil, nil
}

func (p *verifiedSourceCountingProvider) Fingerprint(
	_ context.Context,
	source parser.SourceRef,
) (parser.SourceFingerprint, error) {
	p.fingerprintCalls++
	return verifiedSourceFingerprint(source.DisplayPath)
}

func (p *verifiedSourceCountingProvider) Parse(
	context.Context,
	parser.ParseRequest,
) (parser.ParseOutcome, error) {
	return parser.ParseOutcome{}, fmt.Errorf(
		"unexpected parse after seeding stored source state",
	)
}

type verifiedSourceCountingFactory struct {
	provider *verifiedSourceCountingProvider
}

func (f verifiedSourceCountingFactory) Definition() parser.AgentDef {
	return f.provider.Definition()
}

func (f verifiedSourceCountingFactory) Capabilities() parser.Capabilities {
	return f.provider.Capabilities()
}

func (f verifiedSourceCountingFactory) NewProvider(
	parser.ProviderConfig,
) parser.Provider {
	return f.provider
}

func verifiedSourceFingerprint(path string) (parser.SourceFingerprint, error) {
	info, err := os.Stat(path)
	if err != nil {
		return parser.SourceFingerprint{}, err
	}
	return parser.SourceFingerprint{
		Key:     path,
		Size:    info.Size(),
		MTimeNS: parser.CodexEffectiveMtime(path, info.ModTime().UnixNano()),
		Hash:    "verified:" + filepath.Base(path),
	}, nil
}

func newVerifiedSourceArchive(
	t *testing.T,
	count int,
) (*Engine, *verifiedSourceCountingProvider, []parser.DiscoveredFile) {
	return newVerifiedSourceArchiveWithRewriter(t, count, nil)
}

func newVerifiedSourceArchiveWithRewriter(
	t *testing.T,
	count int,
	pathRewriter func(string) string,
) (*Engine, *verifiedSourceCountingProvider, []parser.DiscoveredFile) {
	t.Helper()
	root := t.TempDir()
	database := openTestDB(t)
	provider := &verifiedSourceCountingProvider{
		ProviderBase: parser.ProviderBase{
			Def: parser.AgentDef{
				Type: parser.AgentCodex, DisplayName: "Codex",
				IDPrefix: "codex:", FileBased: true,
			},
			Caps: parser.Capabilities{Source: parser.SourceCapabilities{
				WatchSources:         parser.CapabilitySupported,
				ClassifyChangedPath:  parser.CapabilitySupported,
				CompositeFingerprint: parser.CapabilitySupported,
				VerifiedLocalStat:    parser.CapabilitySupported,
			}},
		},
		root:    root,
		sources: make(map[string]parser.SourceRef, count),
	}
	factory := verifiedSourceCountingFactory{provider: provider}
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine:           "host",
		PathRewriter:      pathRewriter,
		ProviderFactories: []parser.ProviderFactory{factory},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentCodex: parser.ProviderMigrationProviderAuthoritative,
		},
	})

	files := make([]parser.DiscoveredFile, 0, count)
	for i := range count {
		uuid := fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1)
		path := filepath.Join(
			root,
			"rollout-2026-07-11T00-00-00-"+uuid+".jsonl",
		)
		require.NoError(t, os.WriteFile(path, []byte("session\n"), 0o600))
		fingerprint, err := verifiedSourceFingerprint(path)
		require.NoError(t, err)
		filePath := path
		if pathRewriter != nil {
			filePath = pathRewriter(path)
		}
		fileSize := fingerprint.Size
		fileMtime := fingerprint.MTimeNS
		fileHash := fingerprint.Hash
		require.NoError(t, database.UpsertSession(db.Session{
			ID: "codex:" + uuid, Project: "project", Machine: "host",
			Agent: string(parser.AgentCodex), FilePath: &filePath,
			FileSize: &fileSize, FileMtime: &fileMtime, FileHash: &fileHash,
		}))
		require.NoError(t, database.SetSessionDataVersion(
			"codex:"+uuid, db.CurrentDataVersion(),
		))

		source := parser.SourceRef{
			Provider: parser.AgentCodex, Key: path,
			DisplayPath: path, FingerprintKey: path,
			ProjectHint: "project",
		}
		provider.sources[filepath.Clean(path)] = source
		sourceCopy := source
		files = append(files, parser.DiscoveredFile{
			Path: path, Agent: parser.AgentCodex,
			ProviderSource: &sourceCopy, ProviderProcess: true,
		})
	}
	return engine, provider, files
}

func runVerifiedSourcePass(
	t *testing.T,
	engine *Engine,
	files []parser.DiscoveredFile,
) {
	t.Helper()
	pass := engine.beginVerifiedSourcePass()
	for _, file := range files {
		res := engine.processFile(context.Background(), file)
		require.NoError(t, res.err)
		assert.True(t, res.skip)
	}
	engine.finishVerifiedSourcePass(pass, true)
}

func TestVerifiedSourceGateWarmFingerprintWorkIsCardinalityIndependent(
	t *testing.T,
) {
	for _, count := range []int{3, 40} {
		t.Run(fmt.Sprintf("sources=%d", count), func(t *testing.T) {
			engine, provider, files := newVerifiedSourceArchive(t, count)

			runVerifiedSourcePass(t, engine, files)
			assert.Equal(t, count, provider.fingerprintCalls,
				"cold pass must deep-verify every source")

			runVerifiedSourcePass(t, engine, files)
			assert.Equal(t, count, provider.fingerprintCalls,
				"warm pass must perform zero content fingerprints")
			assert.Len(t, engine.verifiedSources, count)
		})
	}
}

func TestVerifiedSourceGateWarmTrustDoesNotMaskDatabaseRepair(t *testing.T) {
	const sessionID = "codex:00000000-0000-0000-0000-000000000001"
	tests := []struct {
		name   string
		mutate func(*testing.T, *db.DB)
	}{
		{
			name: "missing row",
			mutate: func(t *testing.T, database *db.DB) {
				t.Helper()
				require.NoError(t, database.DeleteSession(sessionID))
			},
		},
		{
			name: "stale data version",
			mutate: func(t *testing.T, database *db.DB) {
				t.Helper()
				require.NoError(t, database.SetSessionDataVersion(
					sessionID, db.CurrentDataVersion()-1,
				))
			},
		},
		{
			name: "project requires reparse",
			mutate: func(t *testing.T, database *db.DB) {
				t.Helper()
				session, err := database.GetSessionFull(
					context.Background(), sessionID,
				)
				require.NoError(t, err)
				require.NotNil(t, session)
				session.Project = "_tmp_workspace"
				require.NoError(t, database.UpsertSession(*session))
			},
		},
		{
			name: "file mtimes reset",
			mutate: func(t *testing.T, database *db.DB) {
				t.Helper()
				require.NoError(t, database.ResetAllMtimes())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine, provider, files := newVerifiedSourceArchive(t, 1)
			runVerifiedSourcePass(t, engine, files)
			runVerifiedSourcePass(t, engine, files)
			require.Equal(t, 1, provider.fingerprintCalls)

			tt.mutate(t, engine.db)
			res := engine.processFile(context.Background(), files[0])

			require.ErrorContains(t, res.err,
				"unexpected parse after seeding stored source state")
			assert.Equal(t, 2, provider.fingerprintCalls,
				"persisted state requiring repair must bypass warm trust")
		})
	}
}

func TestVerifiedSourceGateRechecksAfterStatAndWatcherInvalidation(t *testing.T) {
	engine, provider, files := newVerifiedSourceArchive(t, 1)
	file := files[0]
	runVerifiedSourcePass(t, engine, files)
	runVerifiedSourcePass(t, engine, files)
	require.Equal(t, 1, provider.fingerprintCalls)

	info, err := os.Stat(file.Path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file.Path, []byte("changed\n"), 0o600))
	require.NoError(t, os.Chtimes(file.Path, info.ModTime(), info.ModTime()))
	runVerifiedSourcePass(t, engine, files)
	assert.Equal(t, 2, provider.fingerprintCalls,
		"same-size rewrite with restored mtime must deep-verify")

	classified := engine.classifyPaths([]string{file.Path})
	require.Len(t, classified, 1)
	res := engine.processFile(context.Background(), classified[0])
	require.NoError(t, res.err)
	assert.True(t, res.skip)
	assert.Equal(t, 3, provider.fingerprintCalls,
		"a watcher-classified source must invalidate warm trust")

	engine.clearWatcherOverflowCaches()
	res = engine.processFile(context.Background(), file)
	require.NoError(t, res.err)
	assert.True(t, res.skip)
	assert.Equal(t, 4, provider.fingerprintCalls,
		"watcher overflow must clear every verified-source trust record")
}

func TestVerifiedSourceGateBypassesPathRewrittenSources(t *testing.T) {
	rewriter := func(path string) string { return "remote:" + path }
	engine, provider, files := newVerifiedSourceArchiveWithRewriter(
		t, 1, rewriter,
	)

	runVerifiedSourcePass(t, engine, files)
	runVerifiedSourcePass(t, engine, files)
	assert.Equal(t, 2, provider.fingerprintCalls,
		"remote materializations must deep-verify on every pass")
	assert.Empty(t, engine.verifiedSources)
}

func TestVerifiedSourceGateLegacyClaudeRowMustEstablishFingerprint(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "legacy-session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("session\n"), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)

	database := openTestDB(t)
	fileSize := info.Size()
	fileMtime := info.ModTime().UnixNano()
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "claude:legacy-session", Project: "project", Machine: "host",
		Agent: string(parser.AgentClaude), FilePath: &path,
		FileSize: &fileSize, FileMtime: &fileMtime,
	}))
	require.NoError(t, database.SetSessionDataVersion(
		"claude:legacy-session", db.CurrentDataVersion(),
	))

	provider := &verifiedSourceCountingProvider{
		ProviderBase: parser.ProviderBase{
			Def: parser.AgentDef{
				Type: parser.AgentClaude, DisplayName: "Claude",
				IDPrefix: "claude:", FileBased: true,
			},
			Caps: parser.Capabilities{Source: parser.SourceCapabilities{
				IncrementalAppend: parser.CapabilitySupported,
				VerifiedLocalStat: parser.CapabilitySupported,
			}},
		},
		root:    root,
		sources: make(map[string]parser.SourceRef),
	}
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine:           "host",
		ProviderFactories: []parser.ProviderFactory{verifiedSourceCountingFactory{provider}},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	source := parser.SourceRef{
		Provider: parser.AgentClaude, Key: path,
		DisplayPath: path, FingerprintKey: path,
		ProjectHint: "project",
	}
	res := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path: path, Agent: parser.AgentClaude,
		ProviderSource: &source, ProviderProcess: true,
	})

	require.ErrorContains(t, res.err,
		"unexpected parse after seeding stored source state")
	assert.Equal(t, 1, provider.fingerprintCalls,
		"a legacy row without file_hash must not keep taking the stat-only skip")
	record, ok := engine.verifiedSources[path]
	require.True(t, ok)
	assert.False(t, record.trusted,
		"failed parsing must not promote source trust")
}
