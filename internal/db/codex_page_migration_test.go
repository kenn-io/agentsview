package db

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
)

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

func TestCodexThreadIdentityUpgradePreservesTranscriptMetadata(t *testing.T) {
	const original = "codex:11111111-1111-7111-8111-111111111111"
	const corrected = "codex:22222222-2222-7222-8222-222222222222"
	for _, tc := range []struct {
		name                                                                                                     string
		moved, missing, originalAbsent, trash, deleted, headerOwnsID, compressed, unrelatedTarget, oldCompressed bool
	}{
		{name: "both_present"},
		{name: "relocated", moved: true},
		{name: "relocated_compressed", moved: true, compressed: true},
		{name: "relocated_uncompressed", moved: true, oldCompressed: true},
		{name: "unrelated_target", originalAbsent: true, unrelatedTarget: true},
		{name: "missing", missing: true},
		{name: "original_absent", originalAbsent: true},
		{name: "both_missing", originalAbsent: true, missing: true},
		{name: "trash", trash: true},
		{name: "trash_original_absent", trash: true, originalAbsent: true},
		{name: "permanent_deletion", deleted: true},
		{name: "header_owns_id", headerOwnsID: true},
		{name: "header_owns_id_relocated", headerOwnsID: true, moved: true},
		{name: "header_owns_id_relocated_compressed", headerOwnsID: true, moved: true, compressed: true},
		{name: "header_owns_id_relocated_uncompressed", headerOwnsID: true, moved: true, oldCompressed: true},
		{name: "header_owns_id_missing", headerOwnsID: true, missing: true},
		{name: "header_owns_id_compressed", headerOwnsID: true, compressed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			src := testDB(t)
			seedRecallEvidenceWindow(t, src, original, 0, "saved", "")
			path := filepath.Join(t.TempDir(), "rollout-2025-10-20T12-09-52-"+corrected[len("codex:"):]+".jsonl")
			if tc.oldCompressed {
				path += ".zst"
			}
			_, err := src.getWriter().Exec(ctx, `UPDATE sessions SET agent='codex', file_path=?, data_version=135 WHERE id=?`, path, original)
			require.NoError(t, err)
			selection := insertVerifiedRecallSelection(t, src, "saved-fact", original, 0, 1, []string{"tool-a"})
			messages, err := src.GetAllMessages(ctx, original)
			require.NoError(t, err)
			pinID, err := src.PinMessage(ctx, original, messages[0].ID, new("saved note"))
			require.NoError(t, err)
			require.Positive(t, pinID)
			require.NoError(t, src.RenameSession(ctx, original, new("saved name")))
			_, err = src.StarSession(ctx, original)
			require.NoError(t, err)
			before, err := src.ExportConversationChanges(ctx, ConversationExportOptions{})
			require.NoError(t, err)
			require.NotEmpty(t, before.Changes)
			if tc.trash {
				require.NoError(t, src.SoftDeleteSession(ctx, original))
			}
			if tc.deleted {
				require.NoError(t, src.DeleteSession(ctx, original))
			}
			_, err = src.getWriter().Exec(ctx, "PRAGMA user_version=135")
			require.NoError(t, err)

			dst := testDB(t)
			require.NoError(t, dst.CopyArchiveIdentityFrom(src.Path()))
			require.NoError(t, dst.CopyExcludedSessionsFrom(src.Path()))
			_, err = dst.CopyTrashedDataFrom(src.Path())
			require.NoError(t, err)
			// Model the fresh parse after exclusions and trash have been copied.
			if !tc.deleted && !tc.trash && !tc.headerOwnsID && !tc.originalAbsent {
				seedRecallEvidenceWindow(t, dst, original, 0, "head", "changed")
				_, err = dst.getWriter().Exec(ctx, `UPDATE sessions SET agent='codex', file_path=? WHERE id=?`, filepath.Join(filepath.Dir(path), "rollout-2025-10-20T12-00-00-"+original[len("codex:"):]+".jsonl"), original)
				require.NoError(t, err)
			}
			if !tc.missing && !tc.trash && !tc.deleted {
				target := corrected
				if tc.headerOwnsID {
					target = original
				}
				parsedPath := strings.TrimSuffix(path, ".zst")
				if tc.moved {
					parsedPath = filepath.Join(t.TempDir(), filepath.Base(parsedPath))
				}
				if tc.unrelatedTarget {
					parsedPath = filepath.Join(t.TempDir(), "rollout-2025-10-21T12-09-52-"+corrected[len("codex:"):]+".jsonl")
				}
				if tc.compressed {
					parsedPath += ".zst"
				}
				insertSession(t, dst, target, "agentsview", func(s *Session) {
					s.Agent = "codex"
					s.FilePath = &parsedPath
				})
				for i := range messages {
					messages[i].SessionID = target
				}
				insertMessages(t, dst, messages...)
			}
			_, err = dst.CopyOrphanedDataFrom(src.Path())
			require.NoError(t, err)
			require.NoError(t, dst.CopyRecallEntriesFrom(src.Path()))
			require.NoError(t, dst.CopySessionMetadataFrom(src.Path()))
			if tc.deleted {
				assert.True(t, dst.IsSessionExcluded(ctx, corrected))
				got, err := dst.GetSession(ctx, corrected)
				require.NoError(t, err)
				assert.Nil(t, got)
				return
			}
			wantID := corrected
			if tc.headerOwnsID || (tc.originalAbsent && tc.missing) || tc.unrelatedTarget {
				wantID = original
				duplicate, err := dst.GetSession(ctx, corrected)
				require.NoError(t, err)
				if !tc.unrelatedTarget {
					assert.Nil(t, duplicate)
				}
			}
			got, err := dst.GetSessionFull(ctx, wantID)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Nil(t, got.ParentSessionID)
			assert.Nil(t, got.ParserParentSessionID)
			assert.Empty(t, got.RelationshipType)
			assert.Equal(t, tc.trash, dst.IsSessionTrashed(ctx, wantID))
			pins, err := dst.ListPinnedMessages(ctx, wantID, "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, wantID, pins[0].SessionID)
			assert.Equal(t, new("saved note"), pins[0].Note)
			recall, err := dst.GetRecallEntry(ctx, "saved-fact")
			require.NoError(t, err)
			require.NotNil(t, recall)
			assert.Equal(t, wantID, recall.SourceSessionID)
			require.Len(t, recall.Evidence, 1)
			assert.Equal(t, wantID, recall.Evidence[0].SessionID)
			assert.Equal(t, selection.ContentDigest, recall.Evidence[0].ContentDigest)
			assert.True(t, recall.ProvenanceOK)
			after, err := dst.ExportConversationChanges(ctx, ConversationExportOptions{})
			require.NoError(t, err)
			byID := make(map[string]ConversationChange)
			for _, change := range after.Changes {
				byID[change.MessageID] = change
			}
			for _, old := range before.Changes {
				if old.MessageID == "" {
					continue
				}
				change, found := byID[old.MessageID]
				assert.True(t, found, "export message ID must survive")
				assert.Equal(t, wantID, change.SessionID)
			}
			if tc.trash {
				assert.True(t, dst.IsSessionTrashed(ctx, original), "the original thread must stay hidden when its own rollout returns")
				if tc.originalAbsent {
					require.ErrorIs(t, dst.UpsertSession(ctx, Session{ID: original, Agent: "codex", Project: "agentsview", Machine: "local"}), ErrSessionTrashed)
					assert.True(t, dst.IsSessionTrashed(ctx, original))
					returning, err := dst.GetAllMessages(ctx, original)
					require.NoError(t, err)
					assert.Empty(t, returning)
				}
				return
			}

			nameOwner := wantID
			if !tc.originalAbsent && !tc.headerOwnsID {
				nameOwner = original
				assert.Nil(t, got.DisplayName)
			}
			owner, err := dst.GetSessionFull(ctx, nameOwner)
			require.NoError(t, err)
			require.NotNil(t, owner)
			assert.Equal(t, new("saved name"), owner.DisplayName)
			stars, err := dst.ListStarredSessionIDs(ctx)
			require.NoError(t, err)
			assert.Equal(t, []string{nameOwner}, stars)
		})
	}
}
