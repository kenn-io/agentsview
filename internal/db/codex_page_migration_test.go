package db

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
)

func TestCodexPageUpgradeKeepsPageWithBareRolloutFilename(t *testing.T) {
	const rollout = "22222222-2222-4222-8222-222222222222"
	const page = "codex:11111111-1111-4111-8111-111111111111_" + rollout
	ctx := t.Context()
	src := testDB(t)
	path := filepath.Join(t.TempDir(), "rollout-2026-09-01T10-00-00-"+rollout+".jsonl")
	insertSession(t, src, page, "project", func(s *Session) {
		s.Agent, s.FilePath = "codex", &path
	})
	insertMessages(t, src, Message{SessionID: page, Role: "user", Content: "Archived page question"})
	messages, err := src.GetAllMessages(ctx, page)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	_, err = src.PinMessage(ctx, page, messages[0].ID, new("Page note"))
	require.NoError(t, err)
	_, err = src.getWriter().Exec(ctx, "PRAGMA user_version=135")
	require.NoError(t, err)

	dst := testDB(t)
	insertSession(t, dst, page, "project", func(s *Session) {
		s.Agent, s.FilePath = "codex", &path
	})
	insertMessages(t, dst, Message{SessionID: page, Role: "user", Content: "Archived page question"})
	otherPath := filepath.Join(t.TempDir(), filepath.Base(path))
	insertSession(t, dst, "codex:"+rollout, "project", func(s *Session) {
		s.Agent, s.FilePath = "codex", &otherPath
	})
	insertMessages(t, dst, Message{SessionID: "codex:" + rollout, Role: "user", Content: "Other rollout question"})
	_, err = dst.CopyOrphanedDataFrom(src.Path())
	require.NoError(t, err)
	require.NoError(t, dst.CopySessionMetadataFrom(src.Path()))
	got, err := dst.GetAllMessages(ctx, page)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Archived page question", got[0].Content)
	pins, err := dst.ListPinnedMessages(ctx, page, "")
	require.NoError(t, err)
	assert.Len(t, pins, 1)
	pins, err = dst.ListPinnedMessages(ctx, "codex:"+rollout, "")
	require.NoError(t, err)
	assert.Empty(t, pins)
}

func TestCodexPageUpgradeSkipsFilenameOnlyMetadataMappings(t *testing.T) {
	for _, version := range []int{134, 135, 136} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			ctx := t.Context()
			src := testDB(t)
			path := filepath.Join(t.TempDir(), "rollout-2026-09-01T10-00-00-22222222-2222-4222-8222-222222222222.jsonl")
			insertSession(t, src, "codex:11111111-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222", "project", func(s *Session) {
				s.Agent, s.FilePath = "codex", &path
			})
			_, err := src.getWriter().Exec(ctx, "PRAGMA user_version="+strconv.Itoa(version))
			require.NoError(t, err)
			dst := testDB(t)
			insertSession(t, dst, "codex:22222222-2222-4222-8222-222222222222", "project", func(s *Session) {
				s.Agent = "codex"
				s.FilePath = new(filepath.Join(t.TempDir(), filepath.Base(path)))
			})
			conn, err := dst.getWriter().Conn(ctx)
			require.NoError(t, err)
			defer conn.Close()
			_, err = conn.ExecContext(ctx, "ATTACH DATABASE ? AS old_db", src.Path())
			require.NoError(t, err)
			_, err = prepareCodexSessionIDMap(ctx, conn, false)
			require.NoError(t, err)
			var mappings int
			require.NoError(t, conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM _codex_session_ids").Scan(&mappings))
			assert.Zero(t, mappings, "a filename alone cannot prove a thread correction")
			_, err = conn.ExecContext(ctx, dropCodexSessionIDMapSQL+"; DETACH DATABASE old_db")
			require.NoError(t, err)
		})
	}
}

func TestCodexMisfiledPageUpgradeRetainsTranscriptMetadata(t *testing.T) {
	const oldID = "codex:33333333-3333-4333-8333-333333333333"
	const page = "codex:11111111-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222"
	for _, version := range []int{134, 135, 136} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			ctx := t.Context()
			src := testDB(t)
			path := filepath.Join(t.TempDir(), "rollout-2026-09-01T10-00-00-"+page[len("codex:"):]+".jsonl")
			insertSession(t, src, oldID, "project", func(s *Session) {
				s.Agent, s.FilePath = "codex", &path
			})
			insertMessages(t, src, Message{SessionID: oldID, Role: "user", Content: "Page question"})
			messages, err := src.GetAllMessages(ctx, oldID)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			_, err = src.PinMessage(ctx, oldID, messages[0].ID, new("Saved page note"))
			require.NoError(t, err)
			insertVerifiedRecallSelection(t, src, "saved-page", oldID, 0, 0, nil)
			before, err := src.ExportConversationChanges(ctx, ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, before.Changes, 1)
			_, err = src.getWriter().Exec(ctx, "PRAGMA user_version="+strconv.Itoa(version))
			require.NoError(t, err)

			dst := testDB(t)
			require.NoError(t, dst.CopyArchiveIdentityFrom(src.Path()))
			insertSession(t, dst, page, "project", func(s *Session) {
				s.Agent, s.FilePath = "codex", &path
			})
			insertMessages(t, dst, Message{SessionID: page, Role: "user", Content: "Page question"})
			_, err = dst.CopyOrphanedDataFrom(src.Path())
			require.NoError(t, err)
			require.NoError(t, dst.CopyRecallEntriesFrom(src.Path()))
			require.NoError(t, dst.CopySessionMetadataFrom(src.Path()))
			pins, err := dst.ListPinnedMessages(ctx, page, "")
			require.NoError(t, err)
			assert.Len(t, pins, 1)
			recall, err := dst.GetRecallEntry(ctx, "saved-page")
			require.NoError(t, err)
			require.NotNil(t, recall)
			assert.Equal(t, page, recall.SourceSessionID)
			require.Len(t, recall.Evidence, 1)
			assert.Equal(t, page, recall.Evidence[0].SessionID)
			after, err := dst.ExportConversationChanges(ctx, ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, after.Changes, 1)
			assert.Equal(t, before.Changes[0].MessageID, after.Changes[0].MessageID)
			assert.Equal(t, page, after.Changes[0].SessionID)
			assert.False(t, after.Changes[0].Deleted)
		})
	}
}

func TestCodexPageUpgradeRetainsRecallEvidence(t *testing.T) {
	const thread = "codex:11111111-1111-4111-8111-111111111111"
	const page = thread + "_22222222-2222-4222-8222-222222222222"
	for _, headPresent := range []bool{false, true} {
		t.Run(strconv.FormatBool(headPresent), func(t *testing.T) {
			ctx := t.Context()
			src := testDB(t)
			seedRecallEvidenceWindow(t, src, thread, 10, "page", "")
			path := filepath.Join(t.TempDir(), "rollout-2026-09-01T10-00-00-"+page[len("codex:"):]+".jsonl")
			_, err := src.getWriter().Exec(ctx, `UPDATE sessions SET agent='codex', file_path=? WHERE id=?`, path, thread)
			require.NoError(t, err)
			original := insertVerifiedRecallSelection(t, src, "saved-fact", thread, 10, 11, []string{"tool-a"})
			_, err = src.EnsureExtractGeneration(ctx, ExtractGeneration{Fingerprint: "generation", Model: "model", Segmenter: "turns-v1"})
			require.NoError(t, err)
			_, err = src.UpsertExtractProgress(ctx, ExtractProgressUpsert{
				SessionID: thread, Fingerprint: "generation", ContentDigest: "page-digest", UnitsTotal: 4, StampedAt: time.Now(),
			})
			require.NoError(t, err)
			require.NoError(t, src.AdvanceExtractCursor(ctx, thread, "generation", "page-digest", 2))
			messages := shiftedRecallMessages(t, src, thread, 0)
			_, err = src.getWriter().Exec(ctx, "PRAGMA user_version=130")
			require.NoError(t, err)
			dst := testDB(t)
			insertSession(t, dst, page, "project")
			for i := range messages {
				messages[i].SessionID = page
			}
			insertMessages(t, dst, messages...)
			if headPresent {
				seedRecallEvidenceWindow(t, dst, thread, 10, "head", "changed")
			}
			require.NoError(t, dst.CopyRecallEntriesFrom(src.Path()))
			got, err := dst.GetRecallEntry(ctx, "saved-fact")
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, page, got.SourceSessionID)
			assert.True(t, got.ProvenanceOK)
			require.Len(t, got.Evidence, 1)
			assert.Equal(t, page, got.Evidence[0].SessionID)
			assert.Equal(t, original.ContentDigest, got.Evidence[0].ContentDigest)
			progress, found, err := dst.ExtractProgress(ctx, page, "generation")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, 2, progress.UnitCursor)
			_, found, err = dst.ExtractProgress(ctx, thread, "generation")
			require.NoError(t, err)
			assert.False(t, found, "the page cursor must not resume against the head")
		})
	}
}

func TestCodexPageUpgradeRetainsColdConversationState(t *testing.T) {
	const thread = "codex:11111111-1111-4111-8111-111111111111"
	const page = thread + "_22222222-2222-4222-8222-222222222222"
	ctx := t.Context()
	src := testDB(t)
	src.SetArchiveContent(config.ArchiveContentUsage)
	insertSession(t, src, thread, "project", func(s *Session) {
		s.Agent = "codex"
		s.FilePath = new(filepath.Join(t.TempDir(), "rollout-2026-09-01T10-00-00-"+page[len("codex:"):]+".jsonl"))
	})
	insertMessages(t, src, Message{SessionID: thread, Role: "assistant", Content: "Not retained"})
	_, err := src.getWriter().Exec(ctx, "PRAGMA user_version=130")
	require.NoError(t, err)
	dst := testDB(t)
	require.NoError(t, dst.CopyArchiveIdentityFrom(src.Path()))
	insertSession(t, dst, page, "project")
	insertSession(t, dst, thread, "project")
	insertMessages(t, dst, Message{SessionID: thread, Role: "assistant", Content: "Head answer", SourceUUID: "head-answer"})
	_, err = dst.CopyOrphanedDataFrom(src.Path())
	require.NoError(t, err)
	rows, active := conversationProjectionState(t, dst)
	assert.Zero(t, rows)
	assert.False(t, active)
	got, err := dst.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	var pageState *ConversationChange
	for _, change := range got.Changes {
		if change.Type == "session" && change.SessionID == page {
			pageState = &change
		}
		if change.SessionID == thread {
			assert.Empty(t, change.Gap, "the head must not inherit the page's content policy")
		}
	}
	require.NotNil(t, pageState)
	assert.Equal(t, "archive_content_excluded", pageState.Gap)
}

func TestCodexPageUpgradeRetainsReparsedConversationIDs(t *testing.T) {
	const thread = "codex:11111111-1111-4111-8111-111111111111"
	const page = thread + "_22222222-2222-4222-8222-222222222222"
	for _, headPresent := range []bool{false, true} {
		t.Run(strconv.FormatBool(headPresent), func(t *testing.T) {
			ctx := t.Context()
			src := testDB(t)
			insertSession(t, src, thread, "project", func(s *Session) {
				s.Agent = "codex"
				s.FilePath = new(filepath.Join(t.TempDir(), "rollout-2026-09-01T10-00-00-"+page[len("codex:"):]+".jsonl"))
			})
			messages := []Message{
				{SessionID: thread, Ordinal: 0, Role: "assistant", Content: "Page answer", SourceUUID: "kept"},
				{SessionID: thread, Ordinal: 1, Role: "assistant", Content: "Removed answer", SourceUUID: "removed"},
			}
			insertMessages(t, src, messages...)
			original, err := src.ExportConversationChanges(ctx, ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, original.Changes, 2)
			require.NoError(t, src.ReplaceSessionMessages(ctx, thread, messages[:1]))
			_, err = src.getWriter().Exec(ctx, "PRAGMA user_version=130")
			require.NoError(t, err)
			dst := testDB(t)
			require.NoError(t, dst.CopyArchiveIdentityFrom(src.Path()))
			insertSession(t, dst, page, "project")
			insertMessages(t, dst, Message{SessionID: page, Role: "assistant", Content: "Page answer", SourceUUID: "kept"})
			if headPresent {
				insertSession(t, dst, thread, "project")
				insertMessages(t, dst, Message{SessionID: thread, Role: "assistant", Content: "Head answer", SourceUUID: "kept"})
			}
			_, err = dst.CopyOrphanedDataFrom(src.Path())
			require.NoError(t, err)
			rebuilt, err := dst.ExportConversationChanges(ctx, ConversationExportOptions{})
			require.NoError(t, err)
			byID := make(map[string]ConversationChange)
			for _, change := range rebuilt.Changes {
				byID[change.MessageID] = change
			}
			for i, old := range original.Changes {
				got, found := byID[old.MessageID]
				require.True(t, found, "the exported ID must survive the upgrade")
				assert.Equal(t, page, got.SessionID)
				assert.Equal(t, i == 1, got.Deleted)
			}
		})
	}
}
