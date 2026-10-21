package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"go.kenn.io/agentsview/internal/assets"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/fileevidence"
	"go.kenn.io/agentsview/internal/stringutil"
)

type evidenceSupport struct {
	Kind, Digest string
	Call, Result int
}

func (s evidenceSupport) resultEventIndex() *int {
	if s.Kind == "event" && s.Result >= 0 {
		return new(s.Result)
	}
	return nil
}

func evidenceHash(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

func evidenceText(m Message, s evidenceSupport) (string, bool) {
	if s.Kind == "message" {
		return m.Content, true
	}
	if s.Call < 0 || s.Call >= len(m.ToolCalls) {
		return "", false
	}
	c := m.ToolCalls[s.Call]
	if s.Kind == "input" {
		return c.InputJSON, true
	}
	if s.Kind == "result" {
		return c.ResultContent, true
	}
	if s.Kind == "event" && s.Result >= 0 && s.Result < len(c.ResultEvents) {
		return c.ResultEvents[s.Result].Content, true
	}
	return "", false
}

// normalizeFileEvidence projects descriptors and references without publishing assets.
func normalizeFileEvidence(messages []Message, policy config.ArchiveContent, images config.ToolResultImages, dir string) []Message {
	out, _ := prepareFileEvidence(messages, policy, images, dir, false)
	return out
}

func (db *DB) publishFileEvidence(messages []Message, images config.ToolResultImages) ([]Message, error) {
	return prepareFileEvidence(messages, db.ArchiveContent(), images, db.AssetsDir(), true)
}

func prepareFileEvidence(messages []Message, policy config.ArchiveContent, images config.ToolResultImages, dir string, put bool) ([]Message, error) {
	out := slices.Clone(messages)
	for i := range out {
		m := &out[i]
		records := slices.Clone(m.FileEvidence)
		supports := slices.Clone(m.evidenceSupport)
		var pending []fileevidence.Candidate
		payloadBytes := 0
		for _, c := range m.FileEvidenceCandidates {
			payloadBytes += len(c.Payload)
		}
		for _, c := range m.FileEvidenceCandidates {
			r := c.Record
			s := evidenceSupport{c.Support, evidenceHash(c.SupportingText), c.Call, c.Result}
			// Prefer the exact retained terminal event when available, never a display
			// summary that happened to repeat it.
			if s.Kind == "result" && c.Call >= 0 && c.Call < len(m.ToolCalls) {
				for j, e := range m.ToolCalls[c.Call].ResultEvents {
					if e.Content == c.SupportingText {
						s.Kind = "event"
						s.Result = j
						break
					}
				}
			}
			if r.Kind == "image" || r.Kind == "document" {
				if images == config.ToolResultImagesDrop || policy.UsageOnly() || policy.OmitsToolContent() && c.Support != "message" {
					m.FileEvidenceCoverage = fileevidence.Coverage{Version: 1, State: "withheld"}
					continue
				}
				if r.Size != nil && r.Kind == "document" {
					r.Retention = "document_metadata"
				}
				if r.AssetRef != "" && r.Kind == "image" && r.SHA256 == "" {
					// Typed references have no decoded payload until verification.
					// Reserve room alongside inline candidates before reading the asset.
					if r.Size != nil && *r.Size > int64(fileevidence.MaxMessageBytes-payloadBytes) {
						r.Size = nil
						r.AssetRef = ""
						r.IneligibleReason = "message_limit"
					} else if body, ok := verifiedEvidenceAsset(dir, r); ok {
						payloadBytes += len(body)
						p := fileevidence.Proof(body)
						r.Size = p.Size
						r.SHA256 = p.SHA256
						r.IneligibleReason = ""
						c.Payload = body
					} else {
						r.Size = nil
						if !put {
							// Keep the declared size for a later real-directory verification.
							pending = append(pending, c)
						}
					}
				}
				if r.Size != nil && r.Kind == "image" && r.SHA256 != "" {
					// Unsupported formats are ineligible evidence, not transient storage failures.
					if _, ok := assets.ExtForMediaType(r.MediaType); !ok {
						m.FileEvidenceCoverage = fileevidence.Coverage{Version: 1, State: "withheld"}
						continue
					}
					var ref string
					var err error
					if put {
						ref, _, err = assets.Put(dir, r.MediaType, c.Payload)
					} else {
						ref, err = assets.Reference(r.MediaType, c.Payload)
					}
					if err != nil {
						if put {
							return nil, fmt.Errorf("publishing file evidence asset: %w", err)
						}
						m.FileEvidenceCoverage = fileevidence.Coverage{Version: 1, State: "withheld"}
						continue
					}
					r.Retention = "owned_image"
					r.AssetRef = ref
					r.ProofMethod = fileevidence.ProofOwnedImageV1
					if !put {
						c.Record = r
						pending = append(pending, c)
					}
				}
			}
			records = append(records, r)
			supports = append(supports, s)
		}
		m.FileEvidenceCandidates = pending
		m.FileEvidence = nil
		m.evidenceSupport = nil
		// Native order and persisted slot order must compare identically.
		type entry struct {
			record  fileevidence.Record
			support evidenceSupport
			slot    string
		}
		ordered := map[string]entry{}
		for j, r := range records {
			if j >= len(supports) {
				m.FileEvidenceCoverage.State = "withheld"
				continue
			}
			s := supports[j]
			r.ResultEventIndex = s.resultEventIndex()
			keep := !policy.UsageOnly() && (!policy.OmitsToolContent() || s.Kind == "message")
			if (r.Kind == "image" || r.Kind == "document") && images == config.ToolResultImagesDrop {
				keep = false
			}
			if r.Retention != "owned_image" && r.Retention != "document_metadata" {
				text, ok := evidenceText(*m, s)
				keep = keep && ok && evidenceHash(text) == s.Digest
			}
			if _, err := r.Position.Slot(); err != nil {
				keep = false
			}
			if !keep {
				m.FileEvidenceCoverage = fileevidence.Coverage{Version: 1, State: "withheld"}
				continue
			}
			// Parser substrings can share a backing buffer with a whole image
			// array. Retain only these compact descriptor strings after publish.
			r.MediaType = strings.Clone(r.MediaType)
			r.DisplayName = strings.Clone(stringutil.SafeTruncate(r.DisplayName, 512))
			r.DisplayPath = strings.Clone(stringutil.SafeTruncate(r.DisplayPath, 512))
			r.Position.Entry = strings.Clone(r.Position.Entry)
			r.Position.ResultEntry = strings.Clone(r.Position.ResultEntry)
			slot, _ := r.Position.Slot()
			ordered[slot] = entry{r, s, slot}
		}
		keys := make([]string, 0, len(ordered))
		for key := range ordered {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			e := ordered[key]
			m.FileEvidence = append(m.FileEvidence, e.record)
			m.evidenceSupport = append(m.evidenceSupport, e.support)
		}
		if len(m.FileEvidence) > 0 && m.FileEvidenceCoverage.Version > 0 {
			m.FileEvidenceCoverage.State = "complete"
		}
	}
	return out, nil
}

func fileEvidenceEqual(a, b Message) bool {
	return reflect.DeepEqual(a.FileEvidence, b.FileEvidence) && reflect.DeepEqual(a.evidenceSupport, b.evidenceSupport)
}

// preserveLosslessAttachmentEvidenceTx snapshots owned images and document
// metadata for lossless image offload. The normal source triggers still run;
// only the caller that retains every payload byte may restore these rows.
func preserveLosslessAttachmentEvidenceTx(ctx context.Context, tx *sql.Tx, id string) (func() error, error) {
	type owned struct {
		message int64
		record  fileevidence.Record
		support evidenceSupport
	}
	rows, err := tx.QueryContext(ctx, "SELECT message_id,"+evidenceCols+" FROM message_file_evidence WHERE retention IN ('owned_image','document_metadata') AND support_kind IN ('result','event') AND message_id IN (SELECT id FROM messages WHERE session_id=?)", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var saved []owned
	for rows.Next() {
		var item owned
		item.record, item.support, err = scanEvidence(rows, &item.message)
		if err != nil {
			return nil, err
		}
		saved = append(saved, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return func() error {
		for _, item := range saved {
			r, s := item.record, item.support
			slot, err := r.Position.Slot()
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO message_file_evidence(message_id,"+evidenceCols+") VALUES ("+placeholderList(16)+")", item.message, slot, r.Kind, r.MediaType, r.DisplayName, r.DisplayPath, r.Size, r.SHA256, r.ProofMethod, r.Retention, r.AssetRef, r.IneligibleReason, s.Kind, s.Digest, s.Call, s.Result); err != nil {
				return err
			}
		}
		if len(saved) > 0 {
			_, err := tx.ExecContext(ctx, "UPDATE sessions SET file_evidence_coverage='complete' WHERE id=?", id)
			return err
		}
		return nil
	}, nil
}

func stripFileAttachmentsTx(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	result, err := tx.ExecContext(ctx, "DELETE FROM message_file_evidence WHERE kind IN ('image','document') AND message_id IN(SELECT id FROM messages WHERE session_id=?)", id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE sessions SET file_evidence_coverage=CASE WHEN EXISTS(SELECT 1 FROM messages m JOIN message_file_evidence e ON e.message_id=m.id WHERE m.session_id=?) THEN 'complete' ELSE 'withheld' END WHERE id=?", id, id)
	return true, err
}

func (db *DB) addFileAttachmentStats(ctx context.Context, id string, stats ToolImageStats) (ToolImageStats, error) {
	var n, size int64
	err := db.getReader().QueryRowContext(ctx, "SELECT count(*),COALESCE(sum(e.size),0) FROM message_file_evidence e JOIN messages m ON m.id=e.message_id WHERE m.session_id=? AND e.kind IN ('image','document')", id).Scan(&n, &size)
	stats.Payloads += n
	stats.DecodedBytes += size
	return stats, err
}

// Stored-source rewrites invalidate their dependent proof in the same SQLite
// transaction, including callers which bypass normal parser replacement.
func installFileEvidenceTriggersTx(ctx context.Context, tx *sql.Tx) error {
	for _, source := range []struct{ name, table, changed, where, session string }{
		{"message", "messages", "OLD.content IS NOT NEW.content", "message_id=OLD.id AND support_kind='message' AND retention='source'", "OLD.session_id"},
		{"input", "tool_calls", "OLD.input_json IS NOT NEW.input_json", "message_id=OLD.message_id AND call_index=OLD.call_index AND support_kind='input'", "OLD.session_id"},
		{"result", "tool_calls", "OLD.result_content IS NOT NEW.result_content", "message_id=OLD.message_id AND call_index=OLD.call_index AND support_kind='result'", "OLD.session_id"},
		{"event", "tool_result_events", "OLD.content IS NOT NEW.content", "message_id IN(SELECT id FROM messages WHERE session_id=OLD.session_id AND ordinal=OLD.tool_call_message_ordinal) AND call_index=OLD.call_index AND result_index=OLD.event_index AND support_kind='event'", "OLD.session_id"},
	} {
		for _, operation := range []string{"UPDATE", "DELETE"} {
			condition := ""
			if operation == "UPDATE" {
				condition = "(" + source.changed + ") AND "
			}
			query := "CREATE TRIGGER IF NOT EXISTS file_evidence_" + source.name + "_" + operation + " AFTER " + operation + " ON " + source.table + " WHEN " + condition + "EXISTS(SELECT 1 FROM message_file_evidence WHERE " + source.where + ") BEGIN UPDATE sessions SET transcript_revision=CAST(CAST(transcript_revision AS INTEGER)+1 AS TEXT),local_modified_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=" + source.session + "; DELETE FROM message_file_evidence WHERE " + source.where + "; UPDATE sessions SET file_evidence_coverage=CASE WHEN EXISTS(SELECT 1 FROM messages m JOIN message_file_evidence e ON e.message_id=m.id WHERE m.session_id=" + source.session + ") THEN 'complete' ELSE 'withheld' END WHERE id=" + source.session + "; END"
			if _, err := tx.ExecContext(ctx, query); err != nil {
				return err
			}
		}
	}
	return nil
}

// evidenceCoverage aggregates the retained occurrence set before storage policy
// can remove message rows. An explicit marker also represents examined empty sessions.
func evidenceCoverage(messages []Message, marker ...fileevidence.Coverage) fileevidence.Coverage {
	var c fileevidence.Coverage
	if len(marker) > 0 {
		c = marker[0]
	}
	retained, withheld := false, c.State == "withheld"
	for _, m := range messages {
		if m.FileEvidenceCoverage.Version > c.Version {
			c = m.FileEvidenceCoverage
		}
		withheld = withheld || m.FileEvidenceCoverage.State == "withheld"
		retained = retained || len(m.FileEvidence) > 0 || len(m.FileEvidenceCandidates) > 0
	}
	if c.Version > 0 {
		c.State = "complete"
		if withheld && !retained {
			c.State = "withheld"
		}
	}
	return c
}

// FileEvidenceEqual compares the retained local descriptor and source identity.
func FileEvidenceEqual(a, b Message) bool { return fileEvidenceEqual(a, b) }

// ProjectFileEvidenceForComparison computes owned image references without I/O.
func (db *DB) ProjectFileEvidenceForComparison(messages []Message) []Message {
	return normalizeFileEvidence(messages, db.ArchiveContent(), db.ToolResultImages(), db.AssetsDir())
}

// Only typed provenance reaches this bounded normal-extraction read. Lookup
// reads persisted descriptors and never opens an asset or a transcript path.
func verifiedEvidenceAsset(dir string, r fileevidence.Record) ([]byte, bool) {
	if r.Size == nil || *r.Size < 0 || *r.Size > fileevidence.MaxAttachmentBytes {
		return nil, false
	}
	ext, ok := assets.ExtForMediaType(r.MediaType)
	if !ok {
		return nil, false
	}
	name := strings.TrimPrefix(r.AssetRef, "asset://")
	if r.AssetRef == name || filepath.Base(name) != name || len(name) != 64+len(ext) || !strings.HasSuffix(name, ext) {
		return nil, false
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, false
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() != *r.Size {
		return nil, false
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, *r.Size+1))
	if err != nil || len(body) != int(*r.Size) {
		return nil, false
	}
	return body, evidenceHash(string(body)) == strings.TrimSuffix(name, ext)
}
