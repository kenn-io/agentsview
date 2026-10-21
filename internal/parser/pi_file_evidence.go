package parser

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tidwall/gjson"

	"go.kenn.io/agentsview/internal/fileevidence"
	"go.kenn.io/agentsview/internal/stringutil"
)

type piProofEntry struct {
	Type    string  `json:"type"`
	ID      string  `json:"id"`
	Parent  *string `json:"parentId"`
	Message struct {
		Role       string         `json:"role"`
		ToolCallID string         `json:"toolCallId"`
		ToolName   string         `json:"toolName"`
		IsError    *bool          `json:"isError"`
		Content    jsontext.Value `json:"content"`
		Details    jsontext.Value `json:"details"`
		Structured jsontext.Value `json:"structuredContent"`
	} `json:"message"`
}

// The index owns only ancestry and offsets, never transcript payloads.
type piProofLocation struct {
	Type    string  `json:"type"`
	ID      string  `json:"id"`
	Parent  *string `json:"parentId"`
	Message struct {
		Role       string `json:"role"`
		ToolCallID string `json:"toolCallId"`
	} `json:"message"`
	offset, size int64
	digest       [32]byte
}

type piProofIndex struct {
	entries map[string]piProofLocation
	order   []string
	valid   bool
}

func (p *piProofIndex) add(line string, offset, size int64) {
	var e piProofLocation
	if json.Unmarshal([]byte(line), &e) != nil {
		p.valid = false
		return
	}
	if e.ID == "" {
		return
	}
	if len(e.ID) > 256 || len(e.Type) > 256 || len(e.Message.Role) > 256 || len(e.Message.ToolCallID) > 256 || e.Parent != nil && len(*e.Parent) > 256 {
		p.valid = false
		return
	}
	if _, duplicate := p.entries[e.ID]; duplicate || !gjson.Get(line, "parentId").Exists() {
		p.valid = false
	}
	e.offset, e.size = offset, size
	e.digest = sha256.Sum256([]byte(strings.TrimSpace(line)))
	p.entries[e.ID] = e
	p.order = append(p.order, e.ID)
}

func (p *piProofIndex) load(f io.ReaderAt, id string) (piProofEntry, bool) {
	loc := p.entries[id]
	var e piProofEntry
	if loc.size <= 0 || loc.size > maxLineSize+2 {
		return e, false
	}
	raw := make([]byte, loc.size)
	if _, err := f.ReadAt(raw, loc.offset); err != nil {
		return e, false
	}
	raw = bytes.TrimSpace(raw)
	if sha256.Sum256(raw) != loc.digest {
		return e, false
	}
	err := json.Unmarshal(raw, &e)
	return e, err == nil
}

func piProofContentBlocks(raw jsontext.Value) []jsontext.Value {
	var blocks []jsontext.Value
	if len(raw) == 0 || json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	return blocks
}

// piFileEvidence runs before generic pairing, so empty result carriers can vanish
// from display without losing the proof attached to their actual call.
func piFileEvidence(f io.ReaderAt, index piProofIndex, messages []ParsedMessage) {
	// Keep recognized file operations visible even when the native contract
	// cannot establish bytes. Successful proofs replace these descriptors.
	for i := range messages {
		m := &messages[i]
		for j, tc := range m.ToolCalls {
			if tc.ToolName != "read" && tc.ToolName != "write" {
				continue
			}
			r := fileevidence.Record{Position: fileevidence.Position{Entry: m.SourceUUID, Call: j, Result: -1, Block: j}, Kind: tc.ToolName, Retention: "source", IneligibleReason: "unsupported_native_contract"}
			if _, err := r.Position.Slot(); err == nil {
				appendFileEvidence(m, fileevidence.Candidate{Record: r, Support: "input", SupportingText: tc.InputJSON, Call: j})
			}
		}
	}
	entries, order := index.entries, index.order
	if !index.valid || len(order) == 0 {
		return
	}
	active := map[string]bool{}
	id := order[len(order)-1]
	for id != "" {
		e, ok := entries[id]
		if !ok || active[id] || e.Type == "context_edit" {
			return
		}
		active[id] = true
		if e.Parent == nil {
			break
		}
		id = *e.Parent
	}
	type call struct {
		entry    string
		index    int
		sequence int
	}
	calls := map[string][]call{}
	results := map[string][]string{}
	seq := map[string]int{}
	for n, id := range order {
		if !active[id] {
			continue
		}
		e := entries[id]
		seq[id] = n
		if e.Message.Role == "toolResult" {
			results[e.Message.ToolCallID] = append(results[e.Message.ToolCallID], id)
		}
	}
	for _, m := range messages {
		if !active[m.SourceUUID] || entries[m.SourceUUID].Message.Role != "assistant" {
			continue
		}
		for ci, tc := range m.ToolCalls {
			calls[tc.ToolUseID] = append(calls[tc.ToolUseID], call{m.SourceUUID, ci, seq[m.SourceUUID]})
		}
	}
	for mi := range messages {
		m := &messages[mi]
		for ti := range m.ToolCalls {
			tc := &m.ToolCalls[ti]
			cs, rs := calls[tc.ToolUseID], results[tc.ToolUseID]
			if len(cs) != 1 || len(rs) != 1 || cs[0].entry != m.SourceUUID || cs[0].index != ti || seq[rs[0]] <= cs[0].sequence {
				continue
			}
			c := cs[0]
			if tc.ToolName != "read" && tc.ToolName != "write" {
				continue
			}
			nativeCall, ok := index.load(f, c.entry)
			if !ok {
				continue
			}
			var callBlock jsontext.Value
			ci := 0
			for _, raw := range piProofContentBlocks(nativeCall.Message.Content) {
				if gjson.GetBytes(raw, "type").Str != "toolCall" {
					continue
				}
				if ci == c.index {
					callBlock = raw
					break
				}
				ci++
			}
			var fields map[string]jsontext.Value
			if json.Unmarshal(callBlock, &fields) != nil {
				continue
			}
			unknown := false
			for key := range fields {
				if key != "type" && key != "id" && key != "name" && key != "arguments" {
					unknown = true
				}
			}
			if unknown {
				continue
			}
			e, ok := index.load(f, rs[0])
			if !ok {
				continue
			}
			var block struct {
				ID        string                    `json:"id"`
				Type      string                    `json:"type"`
				Name      string                    `json:"name"`
				Arguments map[string]jsontext.Value `json:"arguments"`
			}
			if json.Unmarshal(callBlock, &block) != nil || block.ID != tc.ToolUseID || block.Type != "toolCall" || (block.Name != "read" && block.Name != "write") || e.Message.IsError == nil || *e.Message.IsError || e.Message.ToolName != block.Name {
				continue
			}
			want := 1
			if block.Name == "write" {
				want = 2
			}
			if len(block.Arguments) != want {
				continue
			}
			var path string
			if json.Unmarshal(block.Arguments["path"], &path) != nil || path == "" {
				continue
			}
			resultContent := piProofContentBlocks(e.Message.Content)
			if len(resultContent) != 1 || (len(e.Message.Details) > 0 && string(e.Message.Details) != "{}" && string(e.Message.Details) != "null") {
				continue
			}
			var text struct {
				Type string         `json:"type"`
				Text jsontext.Value `json:"text"`
			}
			if json.Unmarshal(resultContent[0], &text) != nil || text.Type != "text" {
				continue
			}
			output, err := fileevidence.DecodeText(text.Text)
			if err != nil {
				continue
			}
			if len(e.Message.Structured) > 0 {
				other, err := fileevidence.DecodeText(e.Message.Structured)
				if err != nil || string(other) != string(output) {
					continue
				}
			}
			// These notices are emitted by native Read instead of complete file bytes.
			if block.Name == "read" && (strings.Contains(string(output), "Use offset=") || strings.Contains(string(output), "Showing lines ") || strings.Contains(string(output), "exceeds ") || strings.Contains(string(output), "image processing failed") || strings.HasPrefix(string(output), "Read image file [")) {
				continue
			}
			payload := output
			support := "result"
			method := fileevidence.ProofPiReadUTF8V1
			if block.Name == "write" {
				payload, err = fileevidence.DecodeText(block.Arguments["content"])
				support = "input"
				method = fileevidence.ProofPiWriteUTF8V1
			}
			if err != nil {
				continue
			}
			r := fileevidence.Proof(payload)
			r.Position = fileevidence.Position{Entry: c.entry, Call: ti, ResultEntry: rs[0], Result: 0, Block: 0}
			if _, err := r.Position.Slot(); err != nil {
				continue
			}
			r.Kind = block.Name
			r.MediaType = "text/plain"
			r.DisplayPath = stringutil.SafeTruncate(path, 512)
			r.DisplayName = stringutil.SafeTruncate(filepath.Base(path), 512)
			r.ProofMethod = method
			r.Retention = "source"
			supporting := string(output)
			if support == "input" {
				supporting = tc.InputJSON
			}
			m.FileEvidence = slices.DeleteFunc(m.FileEvidence, func(c fileevidence.Candidate) bool {
				return c.Record.Kind == block.Name && c.Record.Position.Call == ti && c.Record.IneligibleReason == "unsupported_native_contract"
			})
			appendFileEvidence(m, fileevidence.Candidate{Record: r, Payload: payload, Support: support, SupportingText: supporting, Call: ti, Result: 0})
		}
	}
}
