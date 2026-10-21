package db

import (
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/assets"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/fileevidence"
)

func evidenceMessage() Message {
	n := int64(6)
	return Message{
		SessionID: "evidence", Ordinal: 0, Role: "assistant", Content: "write example.txt", SourceUUID: "a", FileEvidenceCoverage: fileevidence.Coverage{Version: 1, State: "complete"},
		ToolCalls:              []ToolCall{{ToolName: "write", ToolUseID: "t", Category: "Write", InputJSON: `{"path":"example.txt","content":"alpha\n"}`, ResultContent: "ok"}},
		FileEvidenceCandidates: []fileevidence.Candidate{{Record: fileevidence.Record{Position: fileevidence.Position{Entry: "a", Call: 0, ResultEntry: "b", Result: 0, Block: 0}, Kind: "write", MediaType: "text/plain", Size: &n, SHA256: "b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060", ProofMethod: fileevidence.ProofPiWriteUTF8V1, Retention: "source"}, Payload: []byte("alpha\n"), Support: "input", SupportingText: `{"path":"example.txt","content":"alpha\n"}`, Call: 0}},
	}
}

func readEvidence(t *testing.T, d *DB) fileevidence.Snapshot {
	t.Helper()
	snapshot, err := d.ReadFileEvidenceSnapshot(t.Context(), "evidence", fileevidence.Window{})
	require.NoError(t, err)
	return snapshot
}

func seedEvidence(t *testing.T, d *DB) {
	t.Helper()
	d.SetAssetsDir(t.TempDir())
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "evidence", Project: "example", Machine: "local", Agent: "pi"}))
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "evidence", []Message{evidenceMessage()}))
}

func TestFileEvidencePersistenceBindingAndNoop(t *testing.T) {
	d := testDB(t)
	seedEvidence(t, d)
	before := readEvidence(t, d)
	require.Len(t, before.Rows, 1)
	assert.Equal(t, 1, before.Total)
	assert.Equal(t, "complete", before.Coverage.State)
	assert.NotEmpty(t, before.Binding)
	ids := messageIDsByOrdinal(t, d, "evidence")
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "evidence", []Message{evidenceMessage()}))
	after, err := d.ReadFileEvidenceSnapshot(t.Context(), "evidence", fileevidence.Window{Binding: before.Binding})
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Equal(t, ids, messageIDsByOrdinal(t, d, "evidence"))
	page, err := d.ReadMessagesWithFileEvidence(t.Context(), "evidence", MessageWindow{Asc: true})
	require.NoError(t, err)
	assert.Equal(t, before.Binding, page.Binding)
	require.Len(t, page.Evidence[0], 1)
	b, err := json.Marshal(page.Messages)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "SHA256")
	assert.NotContains(t, string(b), "FileEvidence")
	m := evidenceMessage()
	m.FileEvidenceCandidates[0].Record.DisplayName = "example.txt"
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "evidence", []Message{m}))
	changed := readEvidence(t, d)
	assert.NotEqual(t, before.Revision, changed.Revision)
	require.ErrorIs(t, d.ValidateFileEvidenceBinding(t.Context(), "evidence", before.Binding), ErrFileEvidenceStale)
	require.NoError(t, d.SetDatabaseIDForTest(t.Context(), "replacement-generation"))
	assert.ErrorIs(t, d.ValidateFileEvidenceBinding(t.Context(), "evidence", changed.Binding), ErrFileEvidenceStale)
}

func TestFileEvidenceRetainedEventNoop(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "evidence", Agent: "pi"}))
	m := evidenceMessage()
	m.ToolCalls[0].ResultContent = "progress\nalpha\n"
	m.ToolCalls[0].ResultEvents = []ToolResultEvent{{Content: "progress"}, {Content: "alpha\n"}}
	c := &m.FileEvidenceCandidates[0]
	c.Support, c.SupportingText = "result", "alpha\n"
	c.Record.Position.Result = 7
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "evidence", []Message{m}))
	before := readEvidence(t, d)
	require.Len(t, before.Rows, 1)
	assert.Equal(t, new(1), before.Rows[0].Record.ResultEventIndex)
	assert.Equal(t, 7, before.Rows[0].Record.Position.Result)
	ids := messageIDsByOrdinal(t, d, "evidence")
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "evidence", []Message{m}))
	assert.Equal(t, before, readEvidence(t, d))
	assert.Equal(t, ids, messageIDsByOrdinal(t, d, "evidence"))
}

func TestFileEvidencePolicyAndSanitization(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy config.ArchiveContent
		mutate func(*Message)
		want   int
		state  string
	}{
		{"transcripts", config.ArchiveContentTranscripts, func(*Message) {}, 0, "withheld"},
		{"usage", config.ArchiveContentUsage, func(*Message) {}, 0, "withheld"},
		{"changed input", config.ArchiveContentFull, func(m *Message) { m.ToolCalls[0].InputJSON = `{"path":"example.txt","content":"redacted"}` }, 0, "withheld"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			d.SetArchiveContent(tc.policy)
			require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "evidence", Agent: "pi"}))
			m := evidenceMessage()
			tc.mutate(&m)
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "evidence", []Message{m}))
			s := readEvidence(t, d)
			assert.Len(t, s.Rows, tc.want)
			assert.Equal(t, tc.state, s.Coverage.State)
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "evidence", []Message{m}))
			again := readEvidence(t, d)
			assert.Equal(t, s.Binding, again.Binding)
		})
	}
}

func TestFileEvidenceMixedCoverageIsStable(t *testing.T) {
	for _, writer := range []string{"replace", "content", "staged", "batch", "batch-append", "insert", "incremental"} {
		for _, retainedFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/retained-first=%t", writer, retainedFirst), func(t *testing.T) {
				d := testDB(t)
				require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "evidence", Agent: "pi"}))
				retained, withheld := evidenceMessage(), evidenceMessage()
				withheld.SourceUUID = "withheld"
				withheld.ToolCalls[0].InputJSON = `{"path":"example.txt","content":"redacted"}`
				messages := []Message{retained, withheld}
				if !retainedFirst {
					messages = []Message{withheld, retained}
				}
				for i := range messages {
					messages[i].Ordinal = i
				}
				write := func() {
					switch writer {
					case "replace":
						require.NoError(t, d.ReplaceSessionMessages(t.Context(), "evidence", messages))
					case "content":
						require.NoError(t, d.ReplaceSessionContent(t.Context(), "evidence", messages, SessionSignalUpdate{}, nil))
					case "staged":
						require.NoError(t, d.ReplaceSessionContentStaged(t.Context(), "evidence", messages, newScratchStagedResults(t), nil, nil))
					case "batch":
						_, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{{Session: Session{ID: "evidence", Agent: "pi"}, Messages: messages, ReplaceMessages: true, SkipSignalUpdates: true}})
						require.NoError(t, err)
					default:
						for _, m := range messages {
							switch writer {
							case "batch-append":
								_, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{{Session: Session{ID: "evidence", Agent: "pi"}, Messages: []Message{m}, SkipSignalUpdates: true}})
								require.NoError(t, err)
							case "insert":
								require.NoError(t, d.InsertMessages(t.Context(), []Message{m}))
							default:
								_, err := d.WriteSessionIncremental(t.Context(), "evidence", []Message{m}, IncrementalSessionUpdate{})
								require.NoError(t, err)
							}
						}
					}
				}
				write()
				before := readEvidence(t, d)
				assert.Equal(t, "complete", before.Coverage.State)
				require.Len(t, before.Rows, 1)
				if writer == "insert" || writer == "incremental" || writer == "batch-append" {
					require.NoError(t, d.ReplaceSessionMessages(t.Context(), "evidence", messages))
				} else {
					write()
				}
				after := readEvidence(t, d)
				assert.Equal(t, before, after)
			})
		}
	}
}

func messageAttachmentEvidence(id string) Message {
	payload := []byte("proof")
	proof := fileevidence.Proof(payload)
	message := Message{
		SessionID: id, Ordinal: 0, Role: "user", Content: "attachment",
		ContentLength:        len("attachment"),
		FileEvidenceCoverage: fileevidence.Coverage{Version: 1, State: "complete"},
	}
	for i, kind := range []string{"image", "document"} {
		mediaType := "image/png"
		if kind == "document" {
			mediaType = "text/plain"
		}
		message.FileEvidenceCandidates = append(
			message.FileEvidenceCandidates,
			fileevidence.Candidate{
				Record: fileevidence.Record{
					Position: fileevidence.Position{Entry: "user", Call: -1, Result: -1, Block: i},
					Kind:     kind, MediaType: mediaType, Size: proof.Size,
					SHA256: proof.SHA256, ProofMethod: fileevidence.ProofInlinePayloadV1,
					Retention: "source",
				},
				Payload: payload, Support: "message", SupportingText: message.Content,
				Call: -1, Result: -1,
			},
		)
	}
	return message
}

func TestFileEvidenceSurvivesLosslessImageMigration(t *testing.T) {
	for _, operation := range []string{"migrate", "resync"} {
		t.Run(operation, func(t *testing.T) {
			d := testDB(t)
			dir := t.TempDir()
			d.SetAssetsDir(dir)
			d.SetToolResultImages(config.ToolResultImagesKeep)
			m := messageAttachmentEvidence("evidence")
			m.Role = "assistant"
			document := `{"type":"document","source":{"type":"base64","media_type":"text/plain","data":"cHJvb2Y="}}`
			raw := `[{"type":"input_image","image_url":"data:image/png;base64,cHJvb2Y="},` + document + `]`
			m.ToolCalls = []ToolCall{{ToolUseID: "t", ToolName: "view_image", Category: "Other", ResultContent: raw, ResultEvents: []ToolResultEvent{{ToolUseID: "t", Source: "function_call_output", Content: raw}}}}
			for i := range m.FileEvidenceCandidates {
				c := &m.FileEvidenceCandidates[i]
				c.Support, c.SupportingText, c.Call, c.Result = "result", raw, 0, 0
				c.Record.Position = fileevidence.Position{Entry: "call", Call: 0, ResultEntry: "output", Result: 0, Block: i}
			}
			require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "evidence", Agent: "codex"}))
			require.NoError(t, d.InsertMessages(t.Context(), []Message{m}))
			before := readEvidence(t, d)
			require.Len(t, before.Rows, 2)
			var err error
			if operation == "migrate" {
				_, err = d.MigrateToolImages(t.Context(), StripImagesFilter{}, realPut(dir))
			} else {
				d.SetToolResultImages(config.ToolResultImagesOffload)
				err = d.ProjectToolImagesForSessions(t.Context(), []string{"evidence"})
			}
			require.NoError(t, err)
			after := readEvidence(t, d)
			assert.Equal(t, before.Rows, after.Rows)
			assert.Equal(t, "complete", after.Coverage.State)
			messages, err := d.GetMessages(t.Context(), "evidence", 0, 0, true)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Contains(t, messages[0].ToolCalls[0].ResultEvents[0].Content, "agentsview_image")
			assert.Contains(t, messages[0].ToolCalls[0].ResultEvents[0].Content, document)
			page, err := d.ReadMessagesWithFileEvidence(t.Context(), "evidence", MessageWindow{Asc: true})
			require.NoError(t, err)
			assert.Len(t, page.Evidence[0], 2)
			// Actual source changes must still invalidate the attachment.
			_, err = d.getWriter().ExecContext(t.Context(), "UPDATE tool_result_events SET content='changed' WHERE session_id='evidence'")
			require.NoError(t, err)
			changed := readEvidence(t, d)
			assert.Empty(t, changed.Rows)
		})
	}
}

func TestToolResultImagesDropPolicyAppliesToMessageEvidenceWithOmittedToolContent(t *testing.T) {
	for _, route := range []string{"batch", "incremental"} {
		t.Run(route, func(t *testing.T) {
			d := testDB(t)
			d.SetArchiveContent(config.ArchiveContentTranscripts)
			d.SetToolResultImages(config.ToolResultImagesDrop)
			message := messageAttachmentEvidence("evidence")
			if route == "batch" {
				_, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{{
					Session:  Session{ID: "evidence", Project: "project", Agent: "claude"},
					Messages: []Message{message}, ReplaceMessages: true,
					SkipSignalUpdates: true,
				}})
				require.NoError(t, err)
			} else {
				require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "evidence", Agent: "claude"}))
				_, err := d.WriteSessionIncremental(
					t.Context(), "evidence", []Message{message}, IncrementalSessionUpdate{},
				)
				require.NoError(t, err)
			}

			snapshot, err := d.ReadFileEvidenceSnapshot(
				t.Context(), "evidence", fileevidence.Window{},
			)
			require.NoError(t, err)
			assert.Empty(t, snapshot.Rows)
			assert.Equal(t, "withheld", snapshot.Coverage.State)
		})
	}
}

func TestToolResultImagesComparisonDropPolicyAppliesToMessageEvidenceWithOmittedToolContent(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentTranscripts)
	projected, _ := d.ProjectToolResultImagesForComparison(
		[]Message{messageAttachmentEvidence("evidence")},
		config.ToolResultImagesDrop,
	)
	require.Len(t, projected, 1)
	assert.Empty(t, projected[0].FileEvidenceCandidates)
	assert.Empty(t, projected[0].FileEvidence)
	assert.Equal(t, "withheld", projected[0].FileEvidenceCoverage.State)
}

func TestFileEvidenceCopyStripRedactionAndCascade(t *testing.T) {
	src := testDB(t)
	seedEvidence(t, src)
	dst := testDB(t)
	// Force different destination row IDs, then copy without the native source.
	require.NoError(t, dst.UpsertSession(t.Context(), Session{ID: "other", Agent: "pi"}))
	require.NoError(t, dst.ReplaceSessionMessages(t.Context(), "other", []Message{{SessionID: "other", Role: "user", Content: "hello"}}))
	_, err := dst.CopyOrphanedDataFrom(src.Path())
	require.NoError(t, err)
	s := readEvidence(t, dst)
	require.Len(t, s.Rows, 1)
	require.NoError(t, dst.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), "UPDATE tool_calls SET input_json='{}' WHERE session_id='evidence'")
		return err
	}))
	s = readEvidence(t, dst)
	assert.Empty(t, s.Rows)
	require.NoError(t, src.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), "DELETE FROM sessions WHERE id='evidence'")
		return err
	}))
	var count int
	require.NoError(t, src.getReader().QueryRow(t.Context(), "SELECT count(*) FROM message_file_evidence").Scan(&count))
	assert.Zero(t, count)
}

func TestFileEvidenceAttachmentPolicyAndStrip(t *testing.T) {
	for _, kind := range []string{"image", "document"} {
		for _, drop := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/drop=%v", kind, drop), func(t *testing.T) {
				d := testDB(t)
				d.SetAssetsDir(t.TempDir())
				if drop {
					d.SetToolResultImages(config.ToolResultImagesDrop)
				}
				require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "evidence", Agent: "claude"}))
				m := evidenceMessage()
				m.ToolCalls = nil
				c := &m.FileEvidenceCandidates[0]
				c.Record.Kind = kind
				c.Record.MediaType = "image/png"
				c.Support = "message"
				c.SupportingText = m.Content
				c.Call = -1
				require.NoError(t, d.ReplaceSessionMessages(t.Context(), "evidence", []Message{m}))
				s := readEvidence(t, d)
				if drop {
					assert.Empty(t, s.Rows)
					assert.Equal(t, "withheld", s.Coverage.State)
					return
				}
				require.Len(t, s.Rows, 1)
				if kind == "image" {
					assert.Equal(t, "owned_image", s.Rows[0].Record.Retention)
					assert.NotEmpty(t, s.Rows[0].Record.AssetRef)
				} else {
					assert.Equal(t, "document_metadata", s.Rows[0].Record.Retention)
					assert.Empty(t, s.Rows[0].Record.AssetRef)
				}
				report, err := d.StripToolImages(t.Context(), StripImagesFilter{})
				require.NoError(t, err)
				assert.Equal(t, 1, report.Changed)
				stripped := readEvidence(t, d)
				assert.Empty(t, stripped.Rows)
				assert.NotEqual(t, s.Binding, stripped.Binding)
			})
		}
	}
}

func TestFileEvidenceZeroMessageCoverageAndStaged(t *testing.T) {
	d := testDB(t)
	write := SessionBatchWrite{Session: Session{ID: "empty", Agent: "pi", FileEvidenceCoverage: fileevidence.Coverage{Version: 1, State: "complete"}}, ReplaceMessages: true, SkipSignalUpdates: true}
	_, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{write})
	require.NoError(t, err)
	s, err := d.ReadFileEvidenceSnapshot(t.Context(), "empty", fileevidence.Window{})
	require.NoError(t, err)
	assert.Equal(t, "complete", s.Coverage.State)
	write.Session.FileEvidenceCoverage.State = "withheld"
	_, err = d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{write})
	require.NoError(t, err)
	after, err := d.ReadFileEvidenceSnapshot(t.Context(), "empty", fileevidence.Window{})
	require.NoError(t, err)
	assert.NotEqual(t, s.Binding, after.Binding)
	seedEvidence(t, d)
	before := readEvidence(t, d)
	m := evidenceMessage()
	m.FileEvidenceCandidates[0].Record.DisplayName = "changed.txt"
	require.NoError(t, d.ReplaceSessionContentStaged(t.Context(), "evidence", []Message{m}, newScratchStagedResults(t), nil, nil))
	after = readEvidence(t, d)
	require.Len(t, after.Rows, 1)
	assert.Equal(t, "changed.txt", after.Rows[0].Record.DisplayName)
	assert.NotEqual(t, before.Binding, after.Binding)
}

func TestFileEvidenceBatchNoopAndCoverageReset(t *testing.T) {
	d := testDB(t)
	m := evidenceMessage()
	write := SessionBatchWrite{Session: Session{ID: "evidence", Agent: "pi", FileEvidenceCoverage: fileevidence.Coverage{Version: 1, State: "complete"}}, Messages: []Message{m}, ReplaceMessages: true, SkipSignalUpdates: true}
	_, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{write})
	require.NoError(t, err)
	ids := messageIDsByOrdinal(t, d, "evidence")
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "unrelated", Agent: "pi"}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "unrelated", Role: "user", Content: "keep"}}))
	_, err = d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{write})
	require.NoError(t, err)
	assert.Equal(t, ids, messageIDsByOrdinal(t, d, "evidence"))
	write.Messages[0].FileEvidenceCandidates = nil
	write.Session.FileEvidenceCoverage.State = "withheld"
	_, err = d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{write})
	require.NoError(t, err)
	write.Messages[0] = evidenceMessage()
	write.Session.FileEvidenceCoverage.State = "complete"
	_, err = d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{write})
	require.NoError(t, err)
	s := readEvidence(t, d)
	assert.Equal(t, "complete", s.Coverage.State)
}

func TestFileEvidenceSanitizedReadAndEscapedWrite(t *testing.T) {
	d := testDB(t)
	seedEvidence(t, d)
	m := evidenceMessage()
	m.ToolCalls[0].ResultContent = "alpha\x00beta\n"
	c := &m.FileEvidenceCandidates[0]
	c.Support = "result"
	c.SupportingText = m.ToolCalls[0].ResultContent
	c.Record.Kind = "read"
	c.Record.ProofMethod = fileevidence.ProofPiReadUTF8V1
	c.Record.Size = new(int64(11))
	c.Record.SHA256 = "c81629f4fe0c966f796498fec41567683bc52e4d23832f193ea954161c630403"
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "evidence", []Message{m}))
	s := readEvidence(t, d)
	assert.Empty(t, s.Rows)
	m = evidenceMessage()
	m.ToolCalls[0].InputJSON = `{"path":"example.txt","content":"alpha\u0000beta\n"}`
	c = &m.FileEvidenceCandidates[0]
	c.SupportingText = m.ToolCalls[0].InputJSON
	c.Record.Size = new(int64(11))
	c.Record.SHA256 = "c81629f4fe0c966f796498fec41567683bc52e4d23832f193ea954161c630403"
	c.Payload = []byte("alpha\x00beta\n")
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "evidence", []Message{m}))
	s = readEvidence(t, d)
	require.Len(t, s.Rows, 1)
	assert.Equal(t, "c81629f4fe0c966f796498fec41567683bc52e4d23832f193ea954161c630403", s.Rows[0].Record.SHA256)
}

func TestFileEvidenceSourceLessCopyTightenedPolicy(t *testing.T) {
	src := testDB(t)
	seedEvidence(t, src)
	m := evidenceMessage()
	m.FileEvidenceCandidates[0].Record.Kind = "document"
	m.FileEvidenceCandidates[0].Record.MediaType = "application/pdf"
	m.FileEvidenceCandidates[0].Support = "message"
	m.FileEvidenceCandidates[0].SupportingText = m.Content
	require.NoError(t, src.ReplaceSessionMessages(t.Context(), "evidence", []Message{m}))
	for _, drop := range []bool{false, true} {
		dst := testDB(t)
		if drop {
			dst.SetToolResultImages(config.ToolResultImagesDrop)
		}
		_, err := dst.CopyOrphanedDataFrom(src.Path())
		require.NoError(t, err)
		s := readEvidence(t, dst)
		if drop {
			assert.Empty(t, s.Rows)
			assert.Equal(t, "withheld", s.Coverage.State)
		} else {
			require.Len(t, s.Rows, 1)
			assert.Equal(t, "document_metadata", s.Rows[0].Record.Retention)
		}
	}
}

func TestFileEvidenceReplacementCoverage(t *testing.T) {
	d := testDB(t)
	seedEvidence(t, d)
	complete := fileevidence.Coverage{Version: 1, State: "complete"}
	withheld := fileevidence.Coverage{Version: 1, State: "withheld"}
	require.NoError(t, d.ReplaceSessionMessagesWithToolResultImages(t.Context(), "evidence", nil, config.ToolResultImagesKeep, withheld))
	a := readEvidence(t, d)
	assert.Equal(t, "withheld", a.Coverage.State)
	require.NoError(t, d.ReplaceSessionContentStagedWithCheckpoint(t.Context(), "evidence", nil, newScratchStagedResults(t), nil, nil, nil, nil, complete))
	b := readEvidence(t, d)
	assert.Equal(t, "complete", b.Coverage.State)
	assert.NotEqual(t, a.Binding, b.Binding)
	require.NoError(t, d.ReplaceSessionMessagesWithToolResultImages(t.Context(), "evidence", nil, config.ToolResultImagesKeep, complete))
	c := readEvidence(t, d)
	assert.Equal(t, b.Binding, c.Binding)
}

func TestFileEvidenceTypedOwnedAsset(t *testing.T) {
	for _, tc := range []struct{ valid, batch bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("valid=%t/batch=%t", tc.valid, tc.batch), func(t *testing.T) {
			d := testDB(t)
			seedEvidence(t, d)
			ref, _, err := assets.Put(d.AssetsDir(), "image/png", []byte("alpha\n"))
			require.NoError(t, err)
			if !tc.valid {
				require.NoError(t, os.WriteFile(filepath.Join(d.AssetsDir(), strings.TrimPrefix(ref, "asset://")), []byte("other!"), 0o600))
			}
			m := evidenceMessage()
			c := &m.FileEvidenceCandidates[0]
			c.Payload = nil
			c.Record.Kind = "image"
			c.Record.MediaType = "image/png"
			c.Record.AssetRef = ref
			c.Record.SHA256 = ""
			c.Record.IneligibleReason = "unverified_asset"
			if tc.batch {
				session, messages := ProjectSessionForStoragePolicy(Session{ID: "evidence", Agent: "pi"}, []Message{m}, config.ArchiveContentFull)
				_, err = d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{{Session: session, Messages: messages, ReplaceMessages: true, SkipSignalUpdates: true}})
			} else {
				err = d.ReplaceSessionMessages(t.Context(), "evidence", []Message{m})
			}
			require.NoError(t, err)
			s := readEvidence(t, d)
			require.Len(t, s.Rows, 1)
			if tc.valid {
				assert.Equal(t, "owned_image", s.Rows[0].Record.Retention)
				require.NotNil(t, s.Rows[0].Record.Size)
				assert.Equal(t, int64(6), *s.Rows[0].Record.Size)
				assert.Equal(t, "b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060", s.Rows[0].Record.SHA256)
			} else {
				assert.Empty(t, s.Rows[0].Record.SHA256)
			}
		})
	}
}

func TestFileEvidenceOwnedAssetMessageBudget(t *testing.T) {
	const digest = "83ee47245398adee79bd9c0a8bc57b821e92aba10f5f9ade8a5d1fae4d8c4302"
	payload := make([]byte, 32<<20)
	for _, inlineFirst := range []bool{false, true} {
		t.Run("inline-first="+strconv.FormatBool(inlineFirst), func(t *testing.T) {
			d := testDB(t)
			d.SetAssetsDir(t.TempDir())
			ref, _, err := assets.Put(d.AssetsDir(), "image/png", payload)
			require.NoError(t, err)
			assert.Equal(t, "asset://"+digest+".png", ref)
			m := Message{SessionID: "evidence", Role: "user", Content: "images", FileEvidenceCoverage: fileevidence.Coverage{Version: 1, State: "complete"}}
			for block := range 3 {
				c := fileevidence.Candidate{
					Record: fileevidence.Record{
						Position: fileevidence.Position{Entry: "user", Call: -1, Result: -1, Block: block},
						Kind:     "image", MediaType: "image/png", Size: new(int64(32 << 20)), AssetRef: ref, IneligibleReason: "unverified_asset",
					}, Support: "message", SupportingText: "images",
				}
				if inlineFirst && block == 0 {
					c.Record.AssetRef = ""
					c.Record.SHA256 = digest
					c.Record.IneligibleReason = ""
					c.Payload = payload
				}
				m.FileEvidenceCandidates = fileevidence.AppendCandidate(m.FileEvidenceCandidates, c)
			}
			session, prepared := d.ProjectSessionForStorage(Session{ID: "evidence", Agent: "claude"}, []Message{m})
			require.Len(t, prepared, 1)
			retained := 0
			for _, c := range prepared[0].FileEvidenceCandidates {
				retained += len(c.Payload)
			}
			assert.Equal(t, 64<<20, retained, "owned and inline payloads share one message budget")
			require.Len(t, prepared[0].FileEvidence, 3)
			assert.Nil(t, prepared[0].FileEvidence[2].Size)
			assert.Empty(t, prepared[0].FileEvidence[2].SHA256)
			assert.Equal(t, "message_limit", prepared[0].FileEvidence[2].IneligibleReason)
			_, err = d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{{Session: session, Messages: prepared, ReplaceMessages: true, SkipSignalUpdates: true}})
			require.NoError(t, err)
			snapshot := readEvidence(t, d)
			require.Len(t, snapshot.Rows, 3)
			for _, row := range snapshot.Rows[:2] {
				assert.Equal(t, digest, row.Record.SHA256)
				require.NotNil(t, row.Record.Size)
				assert.Equal(t, int64(32<<20), *row.Record.Size)
			}
			assert.Nil(t, snapshot.Rows[2].Record.Size)
			assert.Empty(t, snapshot.Rows[2].Record.SHA256)
			assert.Equal(t, "message_limit", snapshot.Rows[2].Record.IneligibleReason)
		})
	}
}

func TestFileEvidenceLegacyReadOnlyAndRollback(t *testing.T) {
	d := testDB(t)
	seedEvidence(t, d)
	before := readEvidence(t, d)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, e := tx.ExecContext(t.Context(), "CREATE TRIGGER reject_evidence BEFORE INSERT ON message_file_evidence BEGIN SELECT RAISE(ABORT,'fixture rejection'); END")
		return e
	}))
	m := evidenceMessage()
	m.Content = "changed"
	m.FileEvidenceCandidates[0].Record.Position.Entry = "line:99"
	require.Error(t, d.ReplaceSessionMessages(t.Context(), "evidence", []Message{m}))
	after := readEvidence(t, d)
	assert.Equal(t, before, after)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		for _, source := range []string{"message", "input", "result", "event"} {
			for _, op := range []string{"UPDATE", "DELETE"} {
				if _, e := tx.ExecContext(t.Context(), "DROP TRIGGER file_evidence_"+source+"_"+op); e != nil {
					return e
				}
			}
		}
		_, e := tx.ExecContext(t.Context(), "DROP TABLE message_file_evidence")
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(t.Context(), "ALTER TABLE sessions DROP COLUMN file_evidence_version")
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(t.Context(), "ALTER TABLE sessions DROP COLUMN file_evidence_coverage")
		return e
	}))
	ro, err := OpenReadOnly(t.Context(), d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ro.Close()) })
	legacy := readEvidence(t, ro)
	assert.Equal(t, "unavailable", legacy.Coverage.State)
	assert.Empty(t, legacy.Rows)
}

func TestFileEvidenceRevisionWorkIsBounded(t *testing.T) {
	for _, n := range []int{3, 1000} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			d := testDB(t)
			seedEvidence(t, d)
			messages := make([]Message, n)
			require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "unaffected", Agent: "pi"}))
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "unaffected", Role: "user", Content: "preserve"}}))
			unaffected := messageIDsByOrdinal(t, d, "unaffected")
			for i := range messages {
				messages[i] = evidenceMessage()
				messages[i].Ordinal = i
				messages[i].Content = "中文 example"
			}
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "evidence", messages))
			ids := messageIDsByOrdinal(t, d, "evidence")
			before := readEvidence(t, d)
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), "CREATE TABLE evidence_audit(ordinal INTEGER); CREATE TRIGGER audit_message_write AFTER UPDATE ON messages BEGIN INSERT INTO evidence_audit VALUES(NEW.ordinal); END")
				return err
			}))
			messages[1].FileEvidenceCandidates[0].Record.DisplayName = "changed.txt"
			started := time.Now()
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "evidence", messages))
			elapsed := time.Since(started)
			var written int
			require.NoError(t, d.getReader().QueryRow(t.Context(), "SELECT count(*) FROM evidence_audit").Scan(&written))
			assert.Equal(t, 1, written)
			assert.Equal(t, ids, messageIDsByOrdinal(t, d, "evidence"))
			assert.Equal(t, unaffected, messageIDsByOrdinal(t, d, "unaffected"))
			after := readEvidence(t, d)
			assert.NotEqual(t, before.Binding, after.Binding)
			t.Logf("examined=%d written=%d elapsed=%s cjk_index=%v", n, written, elapsed, d.HasCJKFTS(t.Context()))
		})
	}
}

func TestFileEvidenceCopyPartialPolicy(t *testing.T) {
	src := testDB(t)
	seedEvidence(t, src)
	doc := evidenceMessage()
	doc.Ordinal = 1
	c := &doc.FileEvidenceCandidates[0]
	c.Record.Kind = "document"
	c.Record.MediaType = "application/pdf"
	c.Support = "message"
	c.SupportingText = doc.Content
	require.NoError(t, src.ReplaceSessionMessages(t.Context(), "evidence", []Message{evidenceMessage(), doc}))
	dst := testDB(t)
	dst.SetArchiveContent(config.ArchiveContentTranscripts)
	_, err := dst.CopyOrphanedDataFrom(src.Path())
	require.NoError(t, err)
	s := readEvidence(t, dst)
	require.Len(t, s.Rows, 1)
	assert.Equal(t, "complete", s.Coverage.State)
}

func TestFileEvidenceSelector(t *testing.T) {
	d := testDB(t)
	seedEvidence(t, d)
	before := readEvidence(t, d)
	require.Len(t, before.Rows, 1)
	slot, err := before.Rows[0].Record.Position.Slot()
	require.NoError(t, err)
	selector := func(slot string) string {
		b, e := json.Marshal([]any{0, slot, before.Binding})
		require.NoError(t, e)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	selected, err := d.ReadFileEvidenceSnapshot(t.Context(), "evidence", fileevidence.Window{Binding: before.Binding, OccurrenceID: selector(slot)})
	require.NoError(t, err)
	assert.Len(t, selected.Rows, 1)
	_, err = d.ReadFileEvidenceSnapshot(t.Context(), "evidence", fileevidence.Window{Binding: before.Binding, OccurrenceID: selector(strings.Replace(slot, "[1,", "[2,", 1))})
	require.ErrorIs(t, err, ErrFileEvidenceStale)
	_, err = d.ReadFileEvidenceSnapshot(t.Context(), "evidence", fileevidence.Window{Offset: 1})
	require.ErrorIs(t, err, ErrFileEvidenceStale)
	m := evidenceMessage()
	m.FileEvidenceCandidates[0].Record.Position.Entry = "line:22"
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "evidence", []Message{m}))
	_, err = d.ReadFileEvidenceSnapshot(t.Context(), "evidence", fileevidence.Window{Binding: before.Binding, OccurrenceID: selector(slot)})
	require.ErrorIs(t, err, ErrFileEvidenceStale)
}

func TestFileEvidencePreparedAssetFailureAbortsWrite(t *testing.T) {
	for _, mode := range []string{"insert", "replace", "incremental", "atomic"} {
		t.Run(mode, func(t *testing.T) {
			d := testDB(t)
			require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "evidence", Agent: "codex"}))
			m := messageAttachmentEvidence("evidence")
			_, prepared := ProjectSessionForStoragePolicy(Session{ID: "evidence"}, []Message{m}, config.ArchiveContentFull)
			blocked := filepath.Join(t.TempDir(), "not-a-directory")
			require.NoError(t, os.WriteFile(blocked, []byte("occupied"), 0o600))
			d.SetAssetsDir(blocked)
			var err error
			switch mode {
			case "insert":
				err = d.InsertMessages(t.Context(), prepared)
			case "replace":
				err = d.ReplaceSessionMessages(t.Context(), "evidence", prepared)
			case "incremental":
				_, err = d.WriteSessionIncremental(t.Context(), "evidence", prepared, IncrementalSessionUpdate{})
			case "atomic":
				_, err = d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{{Session: Session{ID: "evidence", Agent: "codex"}, Messages: prepared, ReplaceMessages: true}})
			}
			require.ErrorContains(t, err, "asset")
			snapshot := readEvidence(t, d)
			assert.Empty(t, snapshot.Rows)
			messages, err := d.GetMessages(t.Context(), "evidence", 0, 100, true)
			require.NoError(t, err)
			assert.Empty(t, messages)
		})
	}
}

func TestFileEvidenceBatchPublicationFailureIsSessionLocal(t *testing.T) {
	d := testDB(t)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocked, []byte("occupied"), 0o600))
	d.SetAssetsDir(blocked)
	writes := []SessionBatchWrite{}
	for _, id := range []string{"before", "broken", "after"} {
		m := Message{SessionID: id, Ordinal: 0, Role: "user", Content: id}
		if id == "broken" {
			m = messageAttachmentEvidence(id)
		}
		writes = append(writes, SessionBatchWrite{Session: Session{ID: id, Agent: "codex"}, Messages: []Message{m}, ReplaceMessages: true})
	}
	result, err := d.WriteSessionBatchContext(t.Context(), writes)
	require.NoError(t, err)
	assert.Equal(t, 2, result.WrittenSessions)
	assert.Equal(t, []int{0, 2}, result.WrittenIndexes)
	assert.Equal(t, 1, result.FailedSessions)
	assert.Equal(t, []string{"broken"}, result.FailedIDs)
	require.Len(t, result.Errors, 1)
	require.ErrorContains(t, result.Errors[0], "asset")
	for _, id := range []string{"before", "after"} {
		messages, err := d.GetMessages(t.Context(), id, 0, 100, true)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		assert.Equal(t, id, messages[0].Content)
	}
	messages, err := d.GetMessages(t.Context(), "broken", 0, 100, true)
	require.NoError(t, err)
	assert.Empty(t, messages)
}
