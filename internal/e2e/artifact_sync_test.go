//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestArtifactSyncTwoInstanceFolder(t *testing.T) {
	t.Parallel()

	nodeA := newArtifactSyncNode(t, "workstation-a", "node-a1b2c3")
	nodeB := newArtifactSyncNode(t, "workstation-b", "node-d4e5f6")
	target := filepath.Join(t.TempDir(), "shared-artifacts")
	sourcePath := filepath.Join(
		nodeA.providerRoot,
		"project",
		"session.jsonl",
	)
	initialSource := []byte(testjsonl.NewSessionBuilder().
		AddClaudeUser(
			"2026-07-01T10:00:00Z",
			"investigate the orchard index",
		).
		AddClaudeAssistant(
			"2026-07-01T10:00:01Z",
			"the orchard index is ready",
		).
		String())
	writeArtifactSyncSource(t, sourcePath, initialSource)

	require.Equal(
		t,
		1,
		nodeA.engine.SyncAll(t.Context(), nil).Synced,
	)
	published := syncArtifactNode(t, nodeA, target)
	assert.Equal(t, 1, published.ExportedSessions)
	assert.Positive(t, published.PublishedArtifacts)

	imported := syncArtifactNode(t, nodeB, target)
	assert.Equal(t, 1, imported.ImportedSessions)
	assert.Equal(t, 2, imported.ImportedMessages)
	importedID := nodeA.origin + "~session"
	assertArtifactSessionVisible(
		t,
		nodeB.database,
		importedID,
		"orchard",
		2,
	)

	updatedSource := []byte(testjsonl.NewSessionBuilder().
		AddClaudeUser(
			"2026-07-01T10:00:00Z",
			"investigate the orchard index",
		).
		AddClaudeAssistant(
			"2026-07-01T10:00:01Z",
			"the orchard index is ready",
		).
		AddClaudeUser(
			"2026-07-01T10:00:02Z",
			"also build the lighthouse index",
		).
		String())
	writeArtifactSyncSource(t, sourcePath, updatedSource)
	require.NoError(
		t,
		nodeA.engine.SyncPathsContext(
			t.Context(),
			[]string{sourcePath},
		),
	)

	republished := syncArtifactNode(t, nodeA, target)
	assert.Equal(t, 1, republished.ExportedSessions)
	reimported := syncArtifactNode(t, nodeB, target)
	assert.Equal(t, 1, reimported.ImportedSessions)
	assertArtifactSessionVisible(
		t,
		nodeB.database,
		importedID,
		"lighthouse",
		3,
	)

	nodeB.restart(t)
	replay := syncArtifactNode(t, nodeB, target)
	assert.Zero(t, replay.ImportedSessions)
	assert.Zero(t, replay.ImportedMessages)
	assertArtifactSessionVisible(
		t,
		nodeB.database,
		importedID,
		"lighthouse",
		3,
	)

	publishCompleteCorruptCheckpoint(t, target)
	afterCorruptionA := syncArtifactNode(t, nodeA, target)
	assert.Equal(t, 1, afterCorruptionA.Quarantined)
	afterCorruptionB := syncArtifactNode(t, nodeB, target)
	assert.False(t, afterCorruptionA.More)
	assert.False(t, afterCorruptionB.More)
	assertArtifactSessionVisible(
		t,
		nodeB.database,
		importedID,
		"lighthouse",
		3,
	)

	preservedSource, err := os.ReadFile(sourcePath)
	require.NoError(t, err)
	assert.Equal(t, updatedSource, preservedSource)

	controlDataDir := t.TempDir()
	controlDB, err := db.Open(filepath.Join(controlDataDir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, controlDB.ConfigureArtifactLocalMachine("control"))
	require.NoError(t, controlDB.Close())
	assert.NoDirExists(t, filepath.Join(controlDataDir, "artifacts"))
}

type artifactSyncNode struct {
	dataDir      string
	databasePath string
	providerRoot string
	machine      string
	origin       string
	database     *db.DB
	engine       *agentsync.Engine
}

func newArtifactSyncNode(
	t *testing.T,
	machine string,
	origin string,
) *artifactSyncNode {
	t.Helper()
	node := &artifactSyncNode{
		dataDir:      t.TempDir(),
		providerRoot: filepath.Join(t.TempDir(), "claude"),
		machine:      machine,
		origin:       origin,
	}
	node.databasePath = filepath.Join(node.dataDir, "sessions.db")
	node.open(t)
	t.Cleanup(func() {
		node.close(t)
	})
	return node
}

func (n *artifactSyncNode) open(t *testing.T) {
	t.Helper()
	database, err := db.Open(n.databasePath)
	require.NoError(t, err)
	require.NoError(t, database.ConfigureArtifactLocalMachine(n.machine))
	n.database = database
	n.engine = agentsync.NewEngine(database, agentsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {n.providerRoot},
		},
		Machine: n.machine,
	})
}

func (n *artifactSyncNode) close(t *testing.T) {
	t.Helper()
	if n.engine != nil {
		n.engine.Close()
		n.engine = nil
	}
	if n.database != nil {
		require.NoError(t, n.database.Close())
		n.database = nil
	}
}

func (n *artifactSyncNode) restart(t *testing.T) {
	t.Helper()
	n.close(t)
	n.open(t)
}

func syncArtifactNode(
	t *testing.T,
	node *artifactSyncNode,
	target string,
) artifact.SyncResult {
	t.Helper()
	result, err := artifact.Sync(
		t.Context(),
		node.database,
		artifact.SyncOptions{
			DataDir:        node.dataDir,
			Target:         target,
			Origin:         node.origin,
			ForbiddenRoots: []string{node.dataDir, node.providerRoot},
		},
	)
	require.NoError(t, err)
	return result
}

func assertArtifactSessionVisible(
	t *testing.T,
	database *db.DB,
	sessionID string,
	query string,
	messageCount int,
) {
	t.Helper()
	page, err := database.ListSessions(
		t.Context(),
		db.SessionFilter{Limit: 10},
	)
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, sessionID, page.Sessions[0].ID)

	session, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, messageCount, session.MessageCount)

	messages, err := database.GetMessages(
		t.Context(),
		sessionID,
		0,
		10,
		true,
	)
	require.NoError(t, err)
	require.Len(t, messages, messageCount)

	results, err := database.Search(
		t.Context(),
		db.SearchFilter{Query: query, Limit: 10},
	)
	require.NoError(t, err)
	require.Len(t, results.Results, 1)
	assert.Equal(t, sessionID, results.Results[0].SessionID)
}

func writeArtifactSyncSource(
	t *testing.T,
	path string,
	content []byte,
) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, content, 0o600))
}

func publishCompleteCorruptCheckpoint(
	t *testing.T,
	target string,
) {
	t.Helper()
	origin := "broken-a7b8c9"
	body := []byte(`{"v":1}`)
	ref, err := artifact.NewRef(
		origin,
		artifact.KindCheckpoints,
		"cp-0000000001.json",
	)
	require.NoError(t, err)
	sum := sha256.Sum256(body)
	identity, err := artifact.NewIdentity(
		hex.EncodeToString(sum[:]),
		int64(len(body)),
	)
	require.NoError(t, err)
	repository, err := artifact.OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	_, err = repository.Content().Create(
		t.Context(),
		ref,
		identity,
		"application/json",
		bytes.NewReader(body),
	)
	require.NoError(t, err)

	transport, err := artifact.OpenFolderTransport(
		target,
		artifact.FolderTransportOptions{},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })
	result, err := transport.Exchange(
		t.Context(),
		artifactSyncTransportStore{ArtifactStore: repository.Content()},
		origin,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Published)
	assert.False(t, result.More)
}

type artifactSyncTransportStore struct {
	artifact.ArtifactStore
}

func (artifactSyncTransportStore) RecordTransportChanged(
	context.Context,
	artifact.Entry,
) error {
	return nil
}
