package parser

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	musePersonalHeaderLimit = 16 << 10
	musePersonalIndexLimit  = 8 << 20
)

var (
	errMusePersonalIncompleteHeader = errors.New("incomplete Muse Personal header")
	errMusePersonalInvalidHeader    = errors.New("invalid Muse Personal header")
)

type musePersonalHeader struct {
	Type      string    `json:"type"`
	Version   int       `json:"version"`
	SessionID string    `json:"session_id"`
	AgentID   string    `json:"agent_id"`
	CreatedAt time.Time `json:"created_at"`
}

var musePersonalUUIDPattern = regexp.MustCompile("^" + codexUUIDPattern + "$")

func musePersonalUUID(id string) bool {
	return musePersonalUUIDPattern.MatchString(id)
}

func musePersonalReadHeader(r io.Reader, path string) (musePersonalHeader, error) {
	var h musePersonalHeader
	line, err := bufio.NewReaderSize(io.LimitReader(r, musePersonalHeaderLimit+1), musePersonalHeaderLimit+1).ReadBytes('\n')
	if len(line) > musePersonalHeaderLimit {
		return h, errMusePersonalInvalidHeader
	}
	if errors.Is(err, io.EOF) {
		return h, errMusePersonalIncompleteHeader
	}
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(line, &h); err != nil {
		return h, fmt.Errorf("%w: %w", errMusePersonalInvalidHeader, err)
	}
	if h.Type != "session_header" || h.Version < 1 || !musePersonalUUID(h.SessionID) || !musePersonalUUID(h.AgentID) || h.CreatedAt.IsZero() || strings.TrimSuffix(filepath.Base(path), ".jsonl") != h.SessionID {
		return h, errMusePersonalInvalidHeader
	}
	if h.Version != 1 {
		return h, fmt.Errorf("unsupported Muse Personal transcript version %d", h.Version)
	}
	return h, nil
}

type musePersonalItem struct {
	Type      string             `json:"type"`
	Role      string             `json:"role"`
	Text      string             `json:"text"`
	Thinking  string             `json:"thinking"`
	CallID    string             `json:"call_id"`
	Name      string             `json:"name"`
	Arguments string             `json:"arguments"`
	Output    string             `json:"output"`
	Success   bool               `json:"success"`
	Parts     []musePersonalPart `json:"parts"`
}

type musePersonalPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Path     string `json:"path"`
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
}

// Decode only known payload shapes. A future variant may reuse these field
// names with different types; its discriminator remains useful on its own.
func (item *musePersonalItem) UnmarshalJSON(data []byte) error {
	var kind struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &kind); err != nil {
		return err
	}
	*item = musePersonalItem{Type: kind.Type}
	switch kind.Type {
	case "message", "message_parts":
		var role struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(data, &role); err != nil {
			return err
		}
		item.Role = role.Role
		switch role.Role {
		case "user", "assistant", "developer", "system":
		default:
			return nil
		}
	case "thinking", "commentary_text", "function_call", "function_call_output":
	default:
		return nil
	}
	type known musePersonalItem
	return json.Unmarshal(data, (*known)(item))
}

func (part *musePersonalPart) UnmarshalJSON(data []byte) error {
	var kind struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &kind); err != nil {
		return err
	}
	*part = musePersonalPart{Type: kind.Type}
	if kind.Type != "text" && kind.Type != "file_ref" {
		return nil
	}
	type known musePersonalPart
	return json.Unmarshal(data, (*known)(part))
}

type musePersonalRecord struct {
	Type          string           `json:"type"`
	Seq           *int64           `json:"seq"`
	Source        string           `json:"source"`
	CreatedAt     jsontext.Value   `json:"created_at"`
	Item          musePersonalItem `json:"item"`
	Summary       string           `json:"summary"`
	CheckpointSeq int64            `json:"checkpoint_seq"`
}

func (record *musePersonalRecord) UnmarshalJSON(data []byte) error {
	var envelope struct {
		Type      string         `json:"type"`
		Source    string         `json:"source"`
		CreatedAt jsontext.Value `json:"created_at"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	*record = musePersonalRecord{Type: envelope.Type, Source: envelope.Source, CreatedAt: envelope.CreatedAt}
	if envelope.Type != "item" && envelope.Type != "compaction_checkpoint" {
		return nil
	}
	type known musePersonalRecord
	return json.Unmarshal(data, (*known)(record))
}

// parseMusePersonalSession streams original records into the supported normalized
// projection. Opaque metadata stays in raw custody, never arbitrary system text.
func parseMusePersonalSession(ctx context.Context, path, machine string) (*ParsedSession, []ParsedMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	h, err := musePersonalReadHeader(f, path)
	if err != nil {
		return nil, nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	sess := &ParsedSession{ID: "muse-personal:" + h.SessionID, SourceSessionID: h.SessionID, SourceVersion: "1", Agent: AgentMusePersonal, Machine: machine, StartedAt: h.CreatedAt, EndedAt: h.CreatedAt, TranscriptFidelity: "full", File: FileInfo{Path: path, Size: info.Size(), Mtime: info.ModTime().UnixNano()}}
	// Freeze the observed length so concurrent appends are retried as one full
	// source. Check framing separately because the shared reader discards the
	// delimiter when it skips an oversized line.
	if info.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], info.Size()-1); err != nil {
			return nil, nil, err
		}
		sess.IsTruncated = last[0] != '\n'
	}
	lr := newLineReaderContext(ctx, io.LimitReader(f, info.Size()), maxLineSize)
	defer releaseLineReader(lr)
	messages := make([]ParsedMessage, 0)
	// Retain compact identities rather than copies of raw records.
	seen := make(map[int64]map[[32]byte]bool)
	occurrences := make(map[string]int)
	type entry struct {
		record    musePersonalRecord
		timestamp time.Time
		sourceID  string
		order     int64
	}
	entries := make([]entry, 0)
	highWater := int64(-1)
	pending := make(map[string][]int)
	for lineNo := 0; ; lineNo++ {
		lr.skippedOversized = false
		line, readErr := lr.readLineBytes()
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return nil, nil, readErr
			}
			break
		}
		if !lr.terminated && !lr.skippedOversized {
			sess.IsTruncated = true
		}
		if lr.skippedOversized {
			sess.MalformedLines++
			continue
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if lineNo == 0 {
			continue
		}
		if !lr.terminated {
			break
		}
		var rec musePersonalRecord
		if json.Unmarshal(line, &rec) != nil || bytes.Equal(bytes.TrimSpace(rec.CreatedAt), []byte("null")) {
			sess.MalformedLines++
			continue
		}
		var identity string
		var ts time.Time
		if rec.Type == "compaction_checkpoint" {
			var epoch int64
			if json.Unmarshal(rec.CreatedAt, &epoch) != nil {
				sess.MalformedLines++
				continue
			}
			ts = time.Unix(epoch, 0).UTC()
			identity = "checkpoint:" + strconv.FormatInt(rec.CheckpointSeq, 10)
		} else {
			if json.Unmarshal(rec.CreatedAt, &ts) != nil {
				var epoch int64
				if rec.Type == "item" || json.Unmarshal(rec.CreatedAt, &epoch) != nil {
					sess.MalformedLines++
					continue
				}
				ts = time.Unix(epoch, 0).UTC()
			}
			if rec.Type == "item" {
				if rec.Seq == nil || *rec.Seq < 0 {
					sess.MalformedLines++
					continue
				}
				digest := sha256.Sum256(line)
				seqSeen := seen[*rec.Seq]
				if seqSeen == nil {
					seqSeen = make(map[[32]byte]bool)
					seen[*rec.Seq] = seqSeen
				}
				if seqSeen[digest] {
					continue
				}
				seqSeen[digest] = true
				identity = "item:" + strconv.FormatInt(*rec.Seq, 10)
			} else {
				identity = "record"
			}
		}
		occurrences[identity]++
		sourceID := h.SessionID + ":" + identity + ":" + strconv.Itoa(occurrences[identity])
		order := highWater
		if rec.Type == "item" {
			order = *rec.Seq
			if order > highWater {
				highWater = order
			}
		}
		entries = append(entries, entry{record: rec, timestamp: ts, sourceID: sourceID, order: order})
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	// Real transcripts can write item records out of sequence. Sort only the
	// decoded projection; raw lines and opaque payloads are never retained.
	// Unsequenced boundaries stay after their preceding sequence high-water mark.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].order < entries[j].order })
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		rec, ts, sourceID := entry.record, entry.timestamp, entry.sourceID
		if ts.After(sess.EndedAt) {
			sess.EndedAt = ts
		}
		empty := ""
		msg := ParsedMessage{Ordinal: len(messages), Role: RoleAssistant, Timestamp: ts, SourceType: rec.Source, SourceSubtype: rec.Item.Type, SourceUUID: sourceID, DialogueText: &empty}
		item := rec.Item
		switch {
		case rec.Type == "compaction_checkpoint":
			msg.Role = RoleSystem
			msg.IsSystem = true
			msg.IsCompactBoundary = true
			msg.SourceSubtype = "compaction_checkpoint"
			msg.Content = rec.Summary
		case rec.Type != "item":
			msg.Role = RoleSystem
			msg.IsSystem = true
			msg.SourceSubtype = musePersonalUnsupportedSubtype("record", rec.Type)
			msg.Content = musePersonalUnsupportedContent
		default:
			switch item.Type {
			case "message", "message_parts":
				switch item.Role {
				case "user":
					msg.Role = RoleUser
				case "assistant":
					msg.Role = RoleAssistant
				case "developer", "system":
					msg.Role = RoleSystem
					msg.IsSystem = true
				default:
					msg.Role = RoleSystem
					msg.IsSystem = true
					msg.SourceSubtype = musePersonalUnsupportedSubtype("role", item.Role)
					msg.Content = musePersonalUnsupportedContent
				}
				if !strings.HasPrefix(msg.SourceSubtype, "unsupported:") {
					msg.Content = item.Text
					if item.Type == "message_parts" {
						var parts []string
						for _, part := range item.Parts {
							switch part.Type {
							case "text":
								parts = append(parts, part.Text)
							case "file_ref":
								parts = append(parts, fmt.Sprintf("[File: %s (%s), path: %s]", part.Filename, part.MimeType, part.Path))
							default:
								parts = append(parts, musePersonalUnsupportedContent)
								msg.SourceSubtype = musePersonalUnsupportedSubtype("part", part.Type)
							}
						}
						msg.Content = strings.Join(parts, "\n")
					}
					visible := msg.Content
					msg.DialogueText = &visible
				}
			case "thinking":
				msg.Content = "[Thinking]\n" + item.Thinking + "\n[/Thinking]"
				msg.ThinkingText = item.Thinking
				msg.HasThinking = true
			case "commentary_text":
				msg.Content, msg.SourceSubtype, msg.DialogueText = item.Text, "commentary", &item.Text
			case "function_call":
				msg.HasToolUse = true
				msg.ToolCalls = []ParsedToolCall{{ToolUseID: item.CallID, ToolName: item.Name, Category: NormalizeToolCategory(item.Name), InputJSON: item.Arguments}}
				if item.CallID != "" {
					pending[item.CallID] = append(pending[item.CallID], len(messages))
				}
			case "function_call_output":
				candidates := pending[item.CallID]
				if item.CallID != "" && len(candidates) == 1 {
					status := "error"
					if item.Success {
						status = "completed"
					}
					call := &messages[candidates[0]].ToolCalls[0]
					call.ResultEvents = append(call.ResultEvents, ParsedToolResultEvent{ToolUseID: item.CallID, Source: rec.Source, Status: status, Content: item.Output, Timestamp: ts})
					delete(pending, item.CallID)
					continue
				}
				msg.Role = RoleTool
				msg.SourceSubtype = "tool_result"
				msg.Content = item.Output
			default:
				msg.Role = RoleSystem
				msg.IsSystem = true
				msg.SourceSubtype = musePersonalUnsupportedSubtype("item", item.Type)
				msg.Content = musePersonalUnsupportedContent
			}
		}
		msg.ContentLength = len(msg.Content)
		if msg.Role == RoleUser {
			sess.UserMessageCount++
			if sess.FirstMessage == "" {
				sess.FirstMessage = msg.Content
			}
		}
		messages = append(messages, msg)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	sess.MessageCount = len(messages)
	updates, err := musePersonalIndexUpdates(ctx, path, h)
	if err != nil {
		return nil, nil, err
	}
	for _, updated := range updates {
		if !updated.Before(sess.EndedAt) {
			sess.EndedAt = updated
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return sess, messages, nil
}

// Index paths and opaque fields are never authorities. Only the full identity
// and ordered update instants participate in enrichment and freshness.
func musePersonalIndexIdentity(h musePersonalHeader) string {
	return h.SessionID + ":" + h.AgentID + ":" + h.CreatedAt.UTC().Format(time.RFC3339Nano)
}

func musePersonalIndexUpdates(ctx context.Context, path string, h musePersonalHeader) ([]time.Time, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	index := filepath.Join(filepath.Dir(path), "sessions.json")
	info, err := siblingMetadataFileInfoStrict(index)
	if err != nil || info == nil {
		return nil, err
	}
	generation := fileStatTupleDigest(0xD6, index)
	identity := musePersonalIndexIdentity(h)
	// Without reliable ctime/inode evidence, never reuse a shared snapshot.
	prefix := fmt.Sprintf("muse-personal-index:%s:%x:", index, generation)
	var entries map[string][]time.Time
	load := func() error {
		var err error
		entries, err = musePersonalLoadIndex(ctx, index, info, generation)
		if err != nil {
			return err
		}
		if reconciliationCacheAvailable(ctx) && generation != 0 {
			for key, updates := range entries {
				value, err := json.Marshal(updates)
				if err != nil {
					return err
				}
				if err := reconciliationCachePut(ctx, prefix+key, string(value)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if reconciliationCacheAvailable(ctx) && generation != 0 {
		if err := reconciliationCacheOnce(ctx, prefix, load); err != nil {
			return nil, err
		}
		value, found, err := reconciliationCacheGet(ctx, prefix+identity)
		if err != nil {
			return nil, err
		}
		var updates []time.Time
		if found {
			if err := json.Unmarshal([]byte(value), &updates); err != nil {
				return nil, err
			}
		}
		// A cached generation must still describe the current physical file. In
		// particular, an old snapshot cannot certify a newer engine stat digest.
		if err := musePersonalVerifyIndex(ctx, index, info, generation); err != nil {
			return nil, err
		}
		return updates, nil
	}
	if err := load(); err != nil {
		return nil, err
	}
	return entries[identity], nil
}

func musePersonalVerifyIndex(ctx context.Context, path string, before os.FileInfo, generation uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) ||
		before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) ||
		generation != fileStatTupleDigest(0xD6, path) {
		return errors.New("Muse Personal index changed during read")
	}
	return nil
}

func musePersonalLoadIndex(ctx context.Context, path string, info os.FileInfo, generation uint64) (map[string][]time.Time, error) {
	observeSharedContainerScan(ctx)
	if info.Size() > musePersonalIndexLimit {
		return nil, musePersonalVerifyIndex(ctx, path, info, generation)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.New("Muse Personal index replaced before read")
	}
	raw, err := io.ReadAll(io.LimitReader(&countingReader{ctx: ctx, r: f}, musePersonalIndexLimit+1))
	if err != nil {
		return nil, err
	}
	if err := musePersonalVerifyIndex(ctx, path, info, generation); err != nil {
		return nil, err
	}
	var data struct {
		Version  int `json:"version"`
		Sessions []struct {
			SessionID string    `json:"session_id"`
			AgentID   string    `json:"agent_id"`
			CreatedAt time.Time `json:"created_at"`
			UpdatedAt time.Time `json:"updated_at"`
		} `json:"sessions"`
	}
	// Unmarshal validates the entire document, including EOF, before any cache
	// writes. Malformed/unsupported advisory metadata keeps transcript semantics.
	if len(raw) > musePersonalIndexLimit || json.Unmarshal(raw, &data) != nil || data.Version != 1 {
		return nil, musePersonalVerifyIndex(ctx, path, info, generation)
	}
	entries := make(map[string][]time.Time)
	for _, entry := range data.Sessions {
		key := musePersonalIndexIdentity(musePersonalHeader{SessionID: entry.SessionID, AgentID: entry.AgentID, CreatedAt: entry.CreatedAt})
		entries[key] = append(entries[key], entry.UpdatedAt)
	}
	return entries, musePersonalVerifyIndex(ctx, path, info, generation)
}

const musePersonalUnsupportedContent = "[Unsupported Muse Personal content]"

// musePersonalUnsupportedSubtype retains only a bounded type discriminator
// in message metadata. Payload fields remain available in raw custody rather
// than rendered content.
func musePersonalUnsupportedSubtype(scope, kind string) string {
	var token strings.Builder
	for _, r := range kind {
		if token.Len() == 64 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			token.WriteByte(byte(r))
		} else {
			token.WriteByte('_')
		}
	}
	return "unsupported:" + scope + ":" + token.String()
}
