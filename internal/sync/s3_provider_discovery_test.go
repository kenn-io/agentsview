package sync

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// TestProcessFileS3ProviderDiscoveredRoutesToS3Path verifies that an s3://
// DiscoveredFile shaped exactly as discoverProviderSources now emits it -- a
// provider-authoritative agent, ProviderProcess set, and a ProviderSource
// carrying the S3DiscoveredSource opaque -- still routes through the S3 sync
// path. processProviderFile must let the s3:// guard win over the provider
// parse path (providers read local files), and the threaded Machine/size/mtime
// must drive the same namespaced result as direct S3 discovery.
func TestProcessFileS3ProviderDiscoveredRoutesToS3Path(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/shared-id.jsonl"
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "Hello").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "Hi.").
		String()

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		if got != path {
			return nil, missingS3ObjectError()
		}
		return io.NopCloser(strings.NewReader(content)), nil
	}

	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()
	e := &Engine{
		db:      database,
		machine: "central",
		providerMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	}
	source := parser.SourceRef{
		Provider:       parser.AgentClaude,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		ProjectHint:    "test-proj",
		Opaque: parser.S3DiscoveredSource{
			URI:     path,
			Project: "test-proj",
			Machine: "laptop",
			Size:    int64(len(content)),
			MtimeNS: mtime,
		},
	}
	res := e.processFile(t.Context(), parser.DiscoveredFile{
		Agent:           parser.AgentClaude,
		Path:            path,
		Project:         "test-proj",
		Machine:         "laptop",
		SourceSize:      int64(len(content)),
		SourceMtime:     mtime,
		ProviderSource:  &source,
		ProviderProcess: true,
	})
	require.NoError(res.err)
	require.Len(res.results, 1)

	written, _, failed, _ := e.writeBatch([]pendingWrite{{
		sess: res.results[0].Session,
		msgs: res.results[0].Messages,
	}}, syncWriteDefault, false)
	require.Equal(1, written)
	require.Equal(0, failed)

	sess, err := database.GetSessionFull(t.Context(), "laptop~shared-id")
	require.NoError(err)
	require.NotNil(sess)
	assert.Equal("laptop", sess.Machine)
	assert.Equal(path, derefString(sess.FilePath))
}

func TestDiscoverProviderSourcesThreadsS3TranscriptMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const root = "s3://bucket/remote-box/raw/claude"
	const uri = root + "/proj/session.jsonl"
	e := NewEngine(openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "central",
		ProviderFactories: []parser.ProviderFactory{
			s3ParseDiffProviderFactory{uri: uri},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(e.Close)

	files, failures := e.discoverProviderSources(t.Context(), nil, nil)
	require.Zero(failures)
	require.Len(files, 1)
	assert.Equal(int64(2048), files[0].TranscriptSize)
	assert.Equal(int64(1779012020000)*1_000_000, files[0].TranscriptMtime)
}
