package sync

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"

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
	require.NoError(t, res.err)
	require.Len(t, res.results, 1)

	written, _, failed, _ := e.writeBatch([]pendingWrite{{
		sess: res.results[0].Session,
		msgs: res.results[0].Messages,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)

	sess, err := database.GetSessionFull(t.Context(), "laptop~shared-id")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "laptop", sess.Machine)
	assert.Equal(t, path, derefString(sess.FilePath))
}

func TestDiscoverProviderSourcesThreadsS3TranscriptMetadata(t *testing.T) {
	const root = "s3://bucket/remote-box/raw/claude"
	const uri = root + "/proj/session.jsonl"
	e := NewEngine(t.Context(), openTestDB(t), EngineConfig{
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
	require.Zero(t, failures)
	require.Len(t, files, 1)
	assert.Equal(t, int64(2048), files[0].TranscriptSize)
	assert.Equal(t, int64(1779012020000)*1_000_000, files[0].TranscriptMtime)
}

func TestS3FailedRootKeepsLocalImports(t *testing.T) {
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>Access denied</Message></Error>`)
	}))
	t.Cleanup(s3.Close)
	t.Setenv("AWS_S3_ENDPOINT", s3.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
	t.Setenv("AWS_REGION", "us-east-1")
	const id = "11111111-1111-4111-8111-111111111111"
	for _, agent := range []parser.AgentType{parser.AgentClaude, parser.AgentIcodemate, parser.AgentCodex, parser.AgentCursor} {
		for _, remoteFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/remote-first=%t", agent, remoteFirst), func(t *testing.T) {
				root := t.TempDir()
				name := "project/" + id + ".jsonl"
				content := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "healthy local transcript", id).String()
				storedID := id
				if agent == parser.AgentCodex {
					name = "rollout-" + id + ".jsonl"
					storedID = "codex:" + id
					content = testjsonl.NewSessionBuilder().AddCodexMeta("2026-01-01T00:00:00Z", id, "/demo", "codex").AddCodexMessage("2026-01-01T00:00:01Z", "user", "healthy local transcript").String()
				}
				if agent == parser.AgentIcodemate {
					storedID = "icodemate:" + id
				}
				if agent == parser.AgentCursor {
					name = "project/agent-transcripts/" + id + ".txt"
					storedID = "cursor:" + id
					content = "user:\nhealthy local transcript\nassistant:\nHello.\n"
				}
				file := filepath.Join(root, filepath.FromSlash(name))
				require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
				require.NoError(t, os.WriteFile(file, []byte(content), 0o644))
				remote := "s3://bucket/machine/raw/" + string(agent)
				roots := []string{root, remote}
				if remoteFirst {
					roots = []string{remote, root}
				}
				database := openTestDB(t)
				engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: roots}, Machine: "local"})
				t.Cleanup(engine.Close)
				stats := engine.SyncAll(t.Context(), nil)
				assert.False(t, stats.ProcessingComplete(), "failed S3 discovery stays incomplete")
				session, err := database.GetSessionFull(t.Context(), storedID)
				require.NoError(t, err)
				require.NotNil(t, session, "healthy local discovery survives the remote error")
				assert.Equal(t, file, derefString(session.FilePath))
			})
		}
	}
}

func TestDeferredSourceLookupIncludesFreebuff(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "archive.db") + "#member"
	dbtest.SeedSession(t, database, "freebuff:member", "project", func(s *db.Session) { s.Agent = "freebuff"; s.FilePath = &path })
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodebuff: {root}}})
	t.Cleanup(engine.Close)
	found, err := engine.HasActiveSessionSourceInRoots(t.Context(), map[string]struct{}{filepath.Join(root, "archive.db"): {}})
	require.NoError(t, err)
	assert.True(t, found)
}
