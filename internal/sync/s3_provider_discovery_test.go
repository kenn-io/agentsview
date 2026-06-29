package sync

import (
	"context"
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
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:           parser.AgentClaude,
		Path:            path,
		Project:         "test-proj",
		Machine:         "laptop",
		SourceSize:      int64(len(content)),
		SourceMtime:     mtime,
		ProviderSource:  &source,
		ProviderProcess: true,
	})
	require.NoError(t, res.err)
	require.Len(t, res.results, 1)

	written, _, failed := e.writeBatch([]pendingWrite{{
		sess: res.results[0].Session,
		msgs: res.results[0].Messages,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)

	sess, err := database.GetSessionFull(context.Background(), "laptop~shared-id")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "laptop", sess.Machine)
	assert.Equal(t, path, derefString(sess.FilePath))
}
