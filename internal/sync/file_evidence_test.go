package sync

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/fileevidence"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type memoryObservedEvidenceStage struct {
	*codexStagingSink
	baseline uint64
	peak     int64
}

func (s *memoryObservedEvidenceStage) VisitFileEvidence(ctx context.Context, visit func(db.StagedFileEvidence) error) error {
	return s.codexStagingSink.VisitFileEvidence(ctx, func(item db.StagedFileEvidence) error {
		if err := visit(item); err != nil {
			return err
		}
		runtime.GC()
		var current runtime.MemStats
		runtime.ReadMemStats(&current)
		s.peak = max(s.peak, int64(current.HeapAlloc)-int64(s.baseline))
		return nil
	})
}

func TestFileEvidenceParseDiffMetadata(t *testing.T) {
	a := db.Message{Role: "user", Content: "same"}
	b := a
	b.FileEvidence = []fileevidence.Record{{Position: fileevidence.Position{Entry: "a", Call: -1, Result: -1, Block: 0}, Kind: "image", IneligibleReason: "unsupported_payload"}}
	assert.NotEmpty(t, messageMetadataDiff(a, b))
}

func TestFileEvidenceCodexStagingMemory(t *testing.T) {
	// Native user and tool images belong in scratch. Retaining raw arrays or
	// decoded candidates makes live heap scale with count.
	measure := func(count int, nativeUser bool, policy config.ToolResultImages) int64 {
		path := filepath.Join(t.TempDir(), "rollout-2026-01-01T00-00-00-019eb791-cf7d-75c1-8439-9ed74c122c11.jsonl")
		f, err := os.Create(path)
		require.NoError(t, err)
		_, err = fmt.Fprintln(f, `{"type":"session_meta","payload":{"id":"019eb791-cf7d-75c1-8439-9ed74c122c11","cwd":"/workspace"}}`)
		require.NoError(t, err)
		for i := range count {
			encoded := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 1<<20)))
			if nativeUser {
				_, err = fmt.Fprintf(f, `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect"},{"type":"input_image","image_url":"data:image/png;base64,%s"}]}}`+"\n", encoded)
			} else {
				_, err = fmt.Fprintf(f, `{"type":"response_item","payload":{"type":"function_call","call_id":"t%d","name":"view_image","arguments":"{}"}}`+"\n", i)
				require.NoError(t, err)
				_, err = fmt.Fprintf(f, `{"type":"response_item","payload":{"type":"function_call_output","call_id":"t%d","output":[{"type":"input_image","image_url":"data:image/png;base64,%s"}]}}`+"\n", i, encoded)
			}
			require.NoError(t, err)
		}
		require.NoError(t, f.Close())
		sink, err := newCodexStagingSink(t.Context(), t.TempDir(), nil)
		require.NoError(t, err)
		defer func() { require.NoError(t, sink.Close()) }()
		sink.toolResultImages = policy
		database := openTestDB(t)
		database.SetAssetsDir(t.TempDir())
		database.SetToolResultImages(policy)
		sink.database = database
		cfg := parser.ProviderConfig{Machine: "local", Roots: []string{filepath.Dir(path)}}
		provider, ok := parser.NewProvider(parser.AgentCodex, cfg)
		require.True(t, ok)
		sources, err := provider.Discover(t.Context())
		require.NoError(t, err)
		require.Len(t, sources, 1)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		session, messages, _, _, _, _, err := parser.ParseCodexSessionStreaming(t.Context(), cfg, sources[0], sink)
		require.NoError(t, err)
		require.NoError(t, sink.Err())
		require.Len(t, messages, count)
		objects, err := os.ReadDir(database.AssetsDir())
		require.NoError(t, err)
		require.Empty(t, objects, "staging must not publish assets before session acceptance")
		runtime.GC()
		runtime.ReadMemStats(&after)
		pw := pendingWrite{sess: *session, msgs: messages}
		row := toDBSession(pw)
		require.NoError(t, database.UpsertSession(t.Context(), row))
		observed := &memoryObservedEvidenceStage{codexStagingSink: sink, baseline: before.HeapAlloc}
		require.NoError(t, database.ReplaceSessionContentStaged(t.Context(), row.ID, toDBMessages(pw, nil), observed, nil, nil))
		snapshot, err := database.ReadFileEvidenceSnapshot(t.Context(), row.ID, fileevidence.Window{Limit: 64})
		require.NoError(t, err)
		want := count
		if policy == config.ToolResultImagesDrop {
			want = 0
		}
		assert.Equal(t, want, snapshot.Total)
		runtime.KeepAlive(messages)
		runtime.KeepAlive(sink)
		return max(int64(after.HeapAlloc)-int64(before.HeapAlloc), observed.peak)
	}
	for _, nativeUser := range []bool{false, true} {
		for _, policy := range []config.ToolResultImages{config.ToolResultImagesKeep, config.ToolResultImagesDrop} {
			t.Run(fmt.Sprintf("user=%t/%s", nativeUser, policy), func(t *testing.T) {
				small, large := measure(2, nativeUser, policy), measure(20, nativeUser, policy)
				assert.Less(t, large-small, int64(8<<20), "image count must not retain decoded bytes or raw arrays")
			})
		}
	}
}

func TestFileEvidenceCodexStagedPolicyDoesNotStoreRawBytes(t *testing.T) {
	for _, mode := range []string{"drop", "blocked", "omitted"} {
		t.Run(mode, func(t *testing.T) {
			const uuid = "019eb791-cf7d-75c1-8439-9ed74c122c11"
			root := writeCodexTranscriptRoot(t, uuid, testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(uuid, "/workspace", "codex_cli_rs", "2026-01-01T00:00:00Z"),
				`{"type":"response_item","payload":{"type":"function_call","call_id":"t","name":"exec_command","arguments":"{}"}}`,
				`{"type":"response_item","payload":{"type":"function_call_output","call_id":"t","output":[{"type":"input_image","image_url":"data:image/png;base64,YWxwaGEK"}]}}`,
			))
			database := openTestDB(t)
			database.SetAssetsDir(t.TempDir())
			if mode == "omitted" {
				database.SetArchiveContent(config.ArchiveContentTranscripts)
			}
			sink, err := newCodexStagingSink(t.Context(), t.TempDir(), map[string]bool{"Bash": mode == "blocked"})
			require.NoError(t, err)
			defer func() { require.NoError(t, sink.Close()) }()
			sink.database, sink.toolResultImages = database, config.ToolResultImagesOffload
			if mode == "drop" {
				sink.toolResultImages = config.ToolResultImagesDrop
			}
			cfg := parser.ProviderConfig{Machine: "local", Roots: []string{root}}
			provider, ok := parser.NewProvider(parser.AgentCodex, cfg)
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			_, _, _, _, _, _, err = parser.ParseCodexSessionStreaming(t.Context(), cfg, sources[0], sink)
			require.NoError(t, err)
			require.NoError(t, sink.Err())
			raw, err := os.ReadFile(sink.Path())
			require.NoError(t, err)
			assert.NotContains(t, string(raw), "data:image/png;base64,YWxwaGEK")
			objects, err := os.ReadDir(database.AssetsDir())
			require.NoError(t, err)
			assert.Empty(t, objects)
		})
	}
}

func TestFileEvidenceCodexStagedPublication(t *testing.T) {
	for _, tc := range []struct {
		name    string
		images  config.ToolResultImages
		archive config.ArchiveContent
		blocked bool
		want    int
	}{
		{"keep", config.ToolResultImagesKeep, config.ArchiveContentFull, false, 3},
		{"offload", config.ToolResultImagesOffload, config.ArchiveContentFull, false, 3},
		{"drop", config.ToolResultImagesDrop, config.ArchiveContentFull, false, 0},
		{"omitted", config.ToolResultImagesOffload, config.ArchiveContentTranscripts, false, 0},
		{"blocked", config.ToolResultImagesKeep, config.ArchiveContentFull, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const uuid = "019eb791-cf7d-75c1-8439-9ed74c122c11"
			// Repeated provider call IDs must retain their distinct owning calls.
			transcript := testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(uuid, "/workspace", "codex_cli_rs", "2026-01-01T00:00:00Z"),
				`{"type":"response_item","payload":{"type":"function_call","call_id":"t","name":"exec_command","arguments":"{}"}}`,
				`{"type":"response_item","payload":{"type":"function_call_output","call_id":"t","output":[{"type":"input_image","image_url":"data:image/png;base64,YWxwaGEK"}]}}`,
				`{"type":"response_item","payload":{"type":"function_call","call_id":"t","name":"exec_command","arguments":"{}"}}`,
				`{"type":"response_item","payload":{"type":"function_call_output","call_id":"t","output":"[{\"type\":\"input_image\",\"image_url\":\"data:image/png;base64,YWxwaGEK\"}]"}}`,
				`{"type":"response_item","payload":{"type":"function_call","call_id":"t","name":"exec_command","arguments":"{}"}}`,
				`{"type":"response_item","payload":{"type":"function_call_output","call_id":"t","output":[{"type":"input_image","image_url":"https://example.test/image.png"}]}}`,
			)
			root := writeCodexTranscriptRoot(t, uuid, transcript)
			database := openTestDB(t)
			database.SetArchiveContent(tc.archive)
			var blocked []string
			if tc.blocked {
				blocked = []string{"Bash"}
			}
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local",
				StagedCodexParseMinBytes: 1, ToolResultImages: tc.images, AssetsDir: t.TempDir(), BlockedResultCategories: blocked,
			})
			t.Cleanup(engine.Close)
			stats := engine.SyncAll(t.Context(), nil)
			require.Zero(t, stats.Failed)
			require.Equal(t, 1, stats.Synced)
			snapshot, err := database.ReadFileEvidenceSnapshot(t.Context(), "codex:"+uuid, fileevidence.Window{})
			require.NoError(t, err)
			require.Len(t, snapshot.Rows, tc.want)
			objects, err := os.ReadDir(database.AssetsDir())
			require.NoError(t, err)
			if tc.want == 0 {
				assert.Empty(t, objects)
				assert.Equal(t, "withheld", snapshot.Coverage.State)
				return
			}
			assert.Equal(t, new(0), snapshot.Rows[0].Record.ResultEventIndex)
			require.Len(t, objects, 1)
			for i, row := range snapshot.Rows {
				if i == 2 {
					assert.Empty(t, row.Record.SHA256)
					assert.Equal(t, "unsupported_payload", row.Record.IneligibleReason)
					continue
				}
				assert.Equal(t, "b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060", row.Record.SHA256)
				assert.Equal(t, fmt.Sprintf("line:%d", 2+i*2), row.Record.Position.Entry)
				assert.Equal(t, fmt.Sprintf("line:%d", 3+i*2), row.Record.Position.ResultEntry)
				assert.Equal(t, 0, row.Record.Position.Call)
				body, err := os.ReadFile(filepath.Join(database.AssetsDir(), strings.TrimPrefix(row.Record.AssetRef, "asset://")))
				require.NoError(t, err)
				assert.Equal(t, "alpha\n", string(body))
			}
		})
	}
}

func TestFileEvidenceParseDiffCoverageOnly(t *testing.T) {
	e, d, pw := pdWriteToolSession(t, "evidence-diff")
	prepared, msgs, verdict := e.prepareSessionWrite(pw, nil)
	require.Equal(t, sessionWriteOK, verdict)
	stored := pdFetchStored(t, d, prepared.ID)
	diffs, err := e.compareStoredSession(t.Context(), stored, prepared, msgs, nil)
	require.NoError(t, err)
	require.Empty(t, diffs)
	prepared.FileEvidenceCoverage = fileevidence.Coverage{Version: 1, State: "withheld"}
	diffs, err = e.compareStoredSession(t.Context(), stored, prepared, msgs, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, diffs)
}

func TestFileEvidenceCodexStagedAssetFailureDoesNotAdvance(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122c11"
	transcript := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(uuid, "/workspace", "codex_cli_rs", "2026-01-01T00:00:00Z"),
		`{"type":"response_item","payload":{"type":"function_call","call_id":"t","name":"exec_command","arguments":"{}"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"t","output":[{"type":"input_image","image_url":"data:image/png;base64,YWxwaGEK"}]}}`,
	)
	root := writeCodexTranscriptRoot(t, uuid, transcript)
	database := openTestDB(t)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocked, []byte("occupied"), 0o600))
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local", StagedCodexParseMinBytes: 1, AssetsDir: blocked})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 1, stats.Failed)
	assert.Zero(t, stats.Synced)
	checkpoint, found, err := database.GetParserCheckpoint(t.Context(), "codex:"+uuid)
	require.NoError(t, err)
	assert.Nil(t, checkpoint)
	assert.False(t, found)
	rows, err := database.GetMessages(t.Context(), "codex:"+uuid, 0, 100, true)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestFileEvidenceCodexStagedUnsupportedImageDoesNotFail(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122c11"
	transcript := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(uuid, "/workspace", "codex_cli_rs", "2026-01-01T00:00:00Z"),
		`{"type":"response_item","payload":{"type":"function_call","call_id":"t","name":"exec_command","arguments":"{}"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"t","output":[{"type":"input_image","image_url":"data:image/avif;base64,YWxwaGEK"}]}}`,
	)
	root := writeCodexTranscriptRoot(t, uuid, transcript)
	database := openTestDB(t)
	dir := t.TempDir()
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local", StagedCodexParseMinBytes: 1, AssetsDir: dir})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	assert.Equal(t, 1, stats.Synced)
	snapshot, err := database.ReadFileEvidenceSnapshot(t.Context(), "codex:"+uuid, fileevidence.Window{})
	require.NoError(t, err)
	assert.Empty(t, snapshot.Rows)
	assert.Equal(t, "withheld", snapshot.Coverage.State)
	rows, err := database.GetMessages(t.Context(), "codex:"+uuid, 0, 100, true)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestFileEvidenceCodexStagedUserAttachments(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  config.ToolResultImages
		archive config.ArchiveContent
		want    int
	}{
		{"keep", config.ToolResultImagesKeep, config.ArchiveContentFull, 2},
		{"offload", config.ToolResultImagesOffload, config.ArchiveContentFull, 2},
		{"drop", config.ToolResultImagesDrop, config.ArchiveContentFull, 0},
		{"transcripts", config.ToolResultImagesKeep, config.ArchiveContentTranscripts, 2},
		{"usage", config.ToolResultImagesKeep, config.ArchiveContentUsage, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const uuid = "019eb791-cf7d-75c1-8439-9ed74c122c11"
			root := writeCodexTranscriptRoot(t, uuid, testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(uuid, "/workspace", "codex_cli_rs", "2026-01-01T00:00:00Z"),
				`{"type":"response_item","payload":{"id":"u","type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,YWxwaGEK"}]}}`,
				`{"type":"response_item","payload":{"id":"u","type":"message","role":"user","content":[{"type":"input_text","text":"inspect"},{"type":"input_image","image_url":"data:image/png;base64,YmV0YQo="}]}}`,
				`{"type":"compacted","payload":{"replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"retained"},{"type":"input_image","image_url":"data:image/png;base64,YmV0YQo="}]}]}}`,
			))
			database := openTestDB(t)
			database.SetArchiveContent(tc.archive)
			sink, err := newCodexStagingSink(t.Context(), t.TempDir(), nil)
			require.NoError(t, err)
			defer func() { require.NoError(t, sink.Close()) }()
			sink.database, sink.toolResultImages = database, tc.policy
			database.SetToolResultImages(tc.policy)
			database.SetAssetsDir(t.TempDir())
			cfg := parser.ProviderConfig{Machine: "local", Roots: []string{root}}
			provider, ok := parser.NewProvider(parser.AgentCodex, cfg)
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			session, messages, _, _, _, _, err := parser.ParseCodexSessionStreaming(t.Context(), cfg, sources[0], sink)
			require.NoError(t, err)
			require.NoError(t, sink.Err())
			for _, message := range messages {
				assert.Empty(t, message.FileEvidence, "staged parser must not retain decoded attachments")
			}
			raw, err := os.ReadFile(sink.Path())
			require.NoError(t, err)
			if tc.want == 0 {
				assert.NotContains(t, string(raw), "data:image/png;base64,")
			}
			pw := pendingWrite{sess: *session, msgs: messages}
			row := toDBSession(pw)
			require.NoError(t, database.UpsertSession(t.Context(), row))
			require.NoError(t, database.ReplaceSessionContentStaged(t.Context(), row.ID, toDBMessages(pw, nil), sink, nil, nil))
			snapshot, err := database.ReadFileEvidenceSnapshot(t.Context(), row.ID, fileevidence.Window{})
			require.NoError(t, err)
			require.Len(t, snapshot.Rows, tc.want)
			objects, err := os.ReadDir(database.AssetsDir())
			require.NoError(t, err)
			require.Len(t, objects, tc.want)
			for i, evidence := range snapshot.Rows {
				assert.Equal(t, i, evidence.MessageOrdinal)
				assert.Equal(t, "u", evidence.Record.Position.Entry)
				body, err := os.ReadFile(filepath.Join(database.AssetsDir(), strings.TrimPrefix(evidence.Record.AssetRef, "asset://")))
				require.NoError(t, err)
				assert.Equal(t, []string{"alpha\n", "beta\n"}[i], string(body))
			}
		})
	}
}

func TestFileEvidencePiLateResult(t *testing.T) {
	for _, atomicWrite := range []bool{false, true} {
		t.Run(strconv.FormatBool(atomicWrite), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "evidence.jsonl")
			initial := `{"type":"session","version":3,"id":"evidence","cwd":"/workspace"}` + "\n" +
				`{"type":"message","id":"a","parentId":null,"message":{"role":"assistant","content":[{"type":"toolCall","id":"t","name":"write","arguments":{"path":"example.txt","content":"alpha\n"}}]}}` + "\n"
			require.NoError(t, os.WriteFile(path, []byte(initial), 0o600))
			database := openTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentPi: {root}}, Machine: "local",
				DiscardPendingWritesOnCancel: atomicWrite,
			})
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			before, err := database.ReadFileEvidenceSnapshot(t.Context(), "pi:evidence", fileevidence.Window{})
			require.NoError(t, err)
			require.Len(t, before.Rows, 1)
			assert.Empty(t, before.Rows[0].Record.SHA256)
			result := `{"type":"message","id":"b","parentId":"a","message":{"role":"toolResult","toolCallId":"t","toolName":"write","content":[{"type":"text","text":"Successfully wrote example.txt"}],"isError":false}}` + "\n"
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			require.NoError(t, err)
			_, err = f.WriteString(result)
			require.NoError(t, err)
			require.NoError(t, f.Close())
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			after, err := database.ReadFileEvidenceSnapshot(t.Context(), "pi:evidence", fileevidence.Window{})
			require.NoError(t, err)
			require.Len(t, after.Rows, 1)
			assert.Equal(t, 0, after.Rows[0].MessageOrdinal)
			assert.Equal(t, "b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060", after.Rows[0].Record.SHA256)
			assert.Equal(t, "pi_native_write_utf8_v1", after.Rows[0].Record.ProofMethod)
		})
	}
}
