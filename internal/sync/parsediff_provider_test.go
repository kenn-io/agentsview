package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestParseDiffDiscoversProviderSources(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "provider-only.jsonl")
	require.NoError(t, os.WriteFile(sourcePath, []byte("{}\n"), 0o644))
	info, err := os.Stat(sourcePath)
	require.NoError(t, err)

	provider := parseDiffProvider{
		sourcePath: sourcePath,
		mtime:      info.ModTime(),
		size:       info.Size(),
	}
	engine := NewDiffEngine(dbtest.OpenTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "devbox",
		ProviderFactories: []parser.ProviderFactory{
			parseDiffProviderFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	})

	report, err := engine.ParseDiff(context.Background(), ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentClaude},
	})

	require.NoError(t, err)
	require.NotNil(t, report)
	assert.Equal(t, 1, report.FilesExamined)
	assert.Equal(t, ParseDiffTotals{NewOnDisk: 1, Examined: 0}, report.Totals)
	if assert.Len(t, report.Sessions, 1) {
		assert.Equal(t, DiffNewOnDisk, report.Sessions[0].Class)
		assert.Equal(t, "provider-discovered", report.Sessions[0].SessionID)
		assert.Equal(t, sourcePath, report.Sessions[0].FilePath)
	}
}

func TestParseDiffProviderAuthoritativeAgentsAreDiscoverable(t *testing.T) {
	engine := NewDiffEngine(dbtest.OpenTestDB(t), EngineConfig{})
	for _, agent := range []parser.AgentType{
		parser.AgentGptme,
		parser.AgentPi,
		parser.AgentOMP,
		parser.AgentWorkBuddy,
		parser.AgentCortex,
		parser.AgentKimi,
		parser.AgentQwenPaw,
		parser.AgentOpenHands,
		parser.AgentCursor,
		parser.AgentVibe,
		parser.AgentClaude,
		parser.AgentCowork,
		parser.AgentHermes,
		// DB-backed provider-authoritative agents: discoverable through
		// their providers even though FileBased is false.
		parser.AgentForge,
		parser.AgentPiebald,
		parser.AgentWarp,
	} {
		def, ok := parser.AgentByType(agent)
		require.True(t, ok, "agent %s", agent)
		assert.True(t, engine.parseDiffAgentDiscoverable(def),
			"parse-diff engine must include provider-authoritative %s", agent)
	}
}

// TestParseDiffDBBackedAgentsAreDiscoverable pins the specific contract this
// change adds: the DB-backed provider-authoritative agents are FileBased=false
// yet still admitted by the parse-diff discoverability gate, because the gate
// keys on the provider factory, not FileBased.
func TestParseDiffDBBackedAgentsAreDiscoverable(t *testing.T) {
	engine := NewDiffEngine(dbtest.OpenTestDB(t), EngineConfig{})
	for _, agent := range []parser.AgentType{
		parser.AgentForge,
		parser.AgentPiebald,
		parser.AgentWarp,
	} {
		def, ok := parser.AgentByType(agent)
		require.True(t, ok, "agent %s", agent)
		assert.False(t, def.FileBased,
			"%s is expected to be DB-backed (FileBased=false)", agent)
		assert.True(t, engine.parseDiffAgentDiscoverable(def),
			"DB-backed %s must be discoverable by parse-diff", agent)
	}

	// Import-only agents are still rejected: they are not
	// provider-authoritative and have no source to re-parse.
	for _, agent := range []parser.AgentType{
		parser.AgentClaudeAI,
		parser.AgentChatGPT,
	} {
		def, ok := parser.AgentByType(agent)
		require.True(t, ok, "agent %s", agent)
		assert.False(t, engine.parseDiffAgentDiscoverable(def),
			"import-only %s must not be discoverable by parse-diff", agent)
	}
}

func TestSyncAllDiscoversProviderSources(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "provider-only-sync.jsonl")
	require.NoError(t, os.WriteFile(sourcePath, []byte("{}\n"), 0o644))
	info, err := os.Stat(sourcePath)
	require.NoError(t, err)

	provider := parseDiffProvider{
		sourcePath: sourcePath,
		mtime:      info.ModTime(),
		size:       info.Size(),
	}
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "devbox",
		ProviderFactories: []parser.ProviderFactory{
			parseDiffProviderFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	})

	stats := engine.SyncAll(context.Background(), nil)

	assert.Equal(t, 1, stats.TotalSessions)
	assert.Equal(t, 1, stats.Synced)
	session, err := database.GetSession(context.Background(), "provider-discovered")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, sourcePath, database.GetSessionFilePath("provider-discovered"))
}

type parseDiffProviderFactory struct {
	provider parseDiffProvider
}

func (f parseDiffProviderFactory) Definition() parser.AgentDef {
	return parser.AgentDef{
		Type:        parser.AgentClaude,
		DisplayName: "Claude Code",
		FileBased:   true,
	}
}

func (f parseDiffProviderFactory) Capabilities() parser.Capabilities {
	return parser.Capabilities{
		Source: parser.SourceCapabilities{
			DiscoverSources: parser.CapabilitySupported,
			FindSource:      parser.CapabilitySupported,
		},
	}
}

func (f parseDiffProviderFactory) NewProvider(
	parser.ProviderConfig,
) parser.Provider {
	p := f.provider
	p.ProviderBase = parser.ProviderBase{
		Def:  f.Definition(),
		Caps: f.Capabilities(),
	}
	return p
}

type parseDiffProvider struct {
	parser.ProviderBase
	sourcePath string
	mtime      time.Time
	size       int64
}

func (p parseDiffProvider) Discover(context.Context) ([]parser.SourceRef, error) {
	return []parser.SourceRef{p.source()}, nil
}

func (p parseDiffProvider) FindSource(
	context.Context,
	parser.FindSourceRequest,
) (parser.SourceRef, bool, error) {
	return p.source(), true, nil
}

func (p parseDiffProvider) Fingerprint(
	context.Context,
	parser.SourceRef,
) (parser.SourceFingerprint, error) {
	return parser.SourceFingerprint{
		Key:     p.sourcePath,
		Size:    p.size,
		MTimeNS: p.mtime.UnixNano(),
		Hash:    "provider-hash",
	}, nil
}

func (p parseDiffProvider) Parse(
	context.Context,
	parser.ParseRequest,
) (parser.ParseOutcome, error) {
	return parser.ParseOutcome{
		Results: []parser.ParseResultOutcome{{
			Result: parser.ParseResult{
				Session: parser.ParsedSession{
					ID:           "provider-discovered",
					Agent:        parser.AgentClaude,
					Machine:      "devbox",
					Project:      "provider",
					StartedAt:    p.mtime,
					EndedAt:      p.mtime,
					MessageCount: 1,
					File: parser.FileInfo{
						Path:  p.sourcePath,
						Size:  p.size,
						Mtime: p.mtime.UnixNano(),
						Hash:  "provider-hash",
					},
				},
				Messages: []parser.ParsedMessage{{
					Role:      parser.RoleUser,
					Content:   "provider discovered",
					Timestamp: p.mtime,
					Ordinal:   0,
				}},
			},
			DataVersion: parser.DataVersionCurrent,
		}},
		ResultSetComplete: true,
	}, nil
}

func (p parseDiffProvider) source() parser.SourceRef {
	return parser.SourceRef{
		Provider:       parser.AgentClaude,
		Key:            p.sourcePath,
		DisplayPath:    p.sourcePath,
		FingerprintKey: p.sourcePath,
		ProjectHint:    "provider",
	}
}

func TestParseDiffProviderSourcesThreadsS3Metadata(t *testing.T) {
	const uri = "s3://bucket/host/raw/claude/proj/session.jsonl"
	engine := NewDiffEngine(dbtest.OpenTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"s3://bucket/host/raw/claude"},
		},
		Machine: "devbox",
		ProviderFactories: []parser.ProviderFactory{
			s3ParseDiffProviderFactory{uri: uri},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	})

	files, err := engine.parseDiffProviderSources(context.Background(), parser.AgentClaude)
	require.NoError(t, err)
	require.Len(t, files, 1)

	// The S3 discovery metadata must be threaded onto the DiscoveredFile so
	// parse-diff ordering uses the real mtime instead of a zero value (which
	// would treat the S3 session as oldest and skew --limit selection).
	f := files[0]
	assert.Equal(t, uri, f.Path)
	assert.Equal(t, "remote-box", f.Machine)
	assert.Equal(t, int64(4096), f.SourceSize)
	assert.Equal(t, int64(1779012030000)*1_000_000, f.SourceMtime)
	assert.Equal(t, "s3-fingerprint", f.SourceFingerprint)
	assert.Equal(t, "myproj", f.Project)
}

type s3ParseDiffProviderFactory struct {
	uri string
}

func (f s3ParseDiffProviderFactory) Definition() parser.AgentDef {
	return parser.AgentDef{
		Type:        parser.AgentClaude,
		DisplayName: "Claude Code",
		FileBased:   true,
	}
}

func (f s3ParseDiffProviderFactory) Capabilities() parser.Capabilities {
	return parser.Capabilities{
		Source: parser.SourceCapabilities{
			DiscoverSources: parser.CapabilitySupported,
		},
	}
}

func (f s3ParseDiffProviderFactory) NewProvider(
	parser.ProviderConfig,
) parser.Provider {
	return s3ParseDiffProvider{
		ProviderBase: parser.ProviderBase{
			Def:  f.Definition(),
			Caps: f.Capabilities(),
		},
		uri: f.uri,
	}
}

type s3ParseDiffProvider struct {
	parser.ProviderBase
	uri string
}

func (p s3ParseDiffProvider) Discover(
	context.Context,
) ([]parser.SourceRef, error) {
	return []parser.SourceRef{{
		Provider:    parser.AgentClaude,
		Key:         p.uri,
		DisplayPath: p.uri,
		Opaque: parser.S3DiscoveredSource{
			URI:         p.uri,
			Project:     "myproj",
			Machine:     "remote-box",
			Size:        4096,
			MtimeNS:     int64(1779012030000) * 1_000_000,
			Fingerprint: "s3-fingerprint",
		},
	}}, nil
}

func (p s3ParseDiffProvider) FindSource(
	context.Context,
	parser.FindSourceRequest,
) (parser.SourceRef, bool, error) {
	return parser.SourceRef{}, false, nil
}

func (p s3ParseDiffProvider) Fingerprint(
	context.Context,
	parser.SourceRef,
) (parser.SourceFingerprint, error) {
	return parser.SourceFingerprint{}, nil
}

func (p s3ParseDiffProvider) Parse(
	context.Context,
	parser.ParseRequest,
) (parser.ParseOutcome, error) {
	return parser.ParseOutcome{}, nil
}
