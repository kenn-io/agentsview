package parser

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/fileevidence"
	"go.kenn.io/agentsview/internal/stringutil"
)

func isFileEvidenceAttachment(kind string) bool {
	return kind == "image" || kind == "document" || kind == "input_image" || kind == "agentsview_image"
}

// Detection shares the capture allowlist and never decodes attachment bytes.
func hasFileEvidenceAttachment(raw string) bool {
	found := false
	blocks := gjson.Parse(raw)
	if blocks.IsArray() {
		blocks.ForEach(func(_, block gjson.Result) bool {
			kind := block.Get("type").Str
			found = isFileEvidenceAttachment(kind) || kind == "tool_result" && hasFileEvidenceAttachment(block.Get("content").Raw)
			return !found
		})
	}
	return found
}

// captureFileEvidence sees the native block array before presentation removes
// binary content. It never reads a path or trusts a producer's claimed digest.
func captureFileEvidence(m *ParsedMessage, raw, entry string) {
	var blocks []jsontext.Value
	if json.Unmarshal([]byte(raw), &blocks) != nil {
		return
	}
	callIndex := 0
	for i, raw := range blocks {
		b := gjson.ParseBytes(raw)
		kind := b.Get("type").Str
		if kind == "tool_use" {
			name := b.Get("name").Str
			if name == "Read" || name == "Write" || name == "Edit" {
				r := fileevidence.Record{Position: fileevidence.Position{Entry: entry, Call: callIndex, Result: -1, Block: i}, Kind: strings.ToLower(name), Retention: "source", IneligibleReason: "unsupported_native_contract"}
				if name == "Edit" {
					r.Kind = "write"
				}
				if _, err := r.Position.Slot(); err == nil {
					appendFileEvidence(m, fileevidence.Candidate{Record: r, Support: "input", SupportingText: b.Get("input").Raw, Call: callIndex})
				}
			}
			callIndex++
			continue
		}
		if kind == "tool_result" {
			before := len(m.FileEvidence)
			captureFileEvidence(m, b.Get("content").Raw, entry)
			for j := before; j < len(m.FileEvidence); j++ {
				c := &m.FileEvidence[j]
				c.ToolUseID = b.Get("tool_use_id").Str
				c.Support = "result"
				c.SupportingText = DecodeContent(b.Get("content").Raw)
				c.Record.Position.ResultEntry = entry
				c.Record.Position.Result = i
			}
			continue
		}
		if !isFileEvidenceAttachment(kind) {
			continue
		}
		r := fileevidence.Record{Kind: kind, ProofMethod: fileevidence.ProofInlinePayloadV1, Retention: "source", IneligibleReason: "unsupported_payload", Position: fileevidence.Position{Entry: entry, Call: -1, Result: -1, Block: i}}
		if kind == "input_image" {
			r.Kind = "image"
		}
		if _, err := r.Position.Slot(); err != nil {
			continue
		}
		var payload []byte
		var err error
		if kind == "agentsview_image" {
			r.Kind = "image"
			r.ProofMethod = ""
			r.IneligibleReason = "unverified_asset"
			var size int64
			if json.Unmarshal([]byte(b.Get("byte_size").Raw), &size) == nil && size >= 0 && size <= fileevidence.MaxAttachmentBytes && len(b.Get("image_ref").Str) <= 128 {
				r.Size = &size
				r.AssetRef = b.Get("image_ref").Str
				r.MediaType = stringutil.SafeTruncate(b.Get("media_type").Str, 128)
			}
		} else if kind == "input_image" {
			r.MediaType, payload, err = fileevidence.DecodeImageURI(b.Get("image_url").Str)
		} else if b.Get("source.type").Str == "base64" {
			r.MediaType = b.Get("source.media_type").Str
			payload, err = fileevidence.DecodeBase64(b.Get("source.data").Str)
		} else if b.Get("data").Type == gjson.String {
			r.MediaType = b.Get("mimeType").Str
			payload, err = fileevidence.DecodeBase64(b.Get("data").Str)
		}
		if payload != nil && err == nil && ((r.Kind == "image" && strings.HasPrefix(r.MediaType, "image/")) || (r.Kind == "document" && r.MediaType != "")) {
			proof := fileevidence.Proof(payload)
			r.Size = proof.Size
			r.SHA256 = proof.SHA256
			r.IneligibleReason = ""
		}
		r.DisplayName = stringutil.SafeTruncate(filepath.Base(b.Get("name").Str), 512)
		if r.DisplayName == "." {
			r.DisplayName = ""
		}
		appendFileEvidence(m, fileevidence.Candidate{Record: r, Payload: payload, Support: "message", SupportingText: m.Content, Call: -1, Result: -1})
	}
}

func appendFileEvidence(m *ParsedMessage, c fileevidence.Candidate) {
	m.FileEvidence = fileevidence.AppendCandidate(m.FileEvidence, c)
}

type nativeFileEvidenceKey struct{}

func nativeFileEvidenceContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, nativeFileEvidenceKey{}, true)
}

func nativeFileEvidence(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, _ := ctx.Value(nativeFileEvidenceKey{}).(bool)
	return enabled
}

// CodexFileEvidenceStager keeps native attachment arrays in scratch until the
// session is accepted, avoiding decoded payloads in the parser's EOF state.
type CodexFileEvidenceStager interface {
	StageMessageFileEvidence(context.Context, string, string, string)
	StageToolFileEvidence(context.Context, string, *ParsedToolCallPosition, string, string, string)
}

// CaptureCodexMessageFileEvidence decodes one retained native message at publication.
func CaptureCodexMessageFileEvidence(raw, entry string) []fileevidence.Candidate {
	m := ParsedMessage{}
	captureFileEvidence(&m, raw, entry)
	return m.FileEvidence
}

// CaptureCodexToolFileEvidence decodes one retained native result at publication.
func CaptureCodexToolFileEvidence(raw, entry, resultEntry string) []fileevidence.Candidate {
	m := ParsedMessage{}
	captureFileEvidence(&m, raw, resultEntry)
	for i := range m.FileEvidence {
		c := &m.FileEvidence[i]
		c.Support = "result"
		c.SupportingText = raw
		c.Record.Position.Entry = entry
		c.Record.Position.ResultEntry = resultEntry
		c.Record.Position.Result = 0
	}
	return m.FileEvidence
}

func (b *codexSessionBuilder) captureToolFileEvidence(ctx context.Context, callID, raw string) {
	if b.agent != AgentCodex || b.skipFileEvidence {
		return
	}
	position, ok := b.toolCallPosition(callID)
	if !ok || position == nil {
		return
	}
	entry := b.evidenceEntries[*position]
	if entry == "" {
		return
	}
	resultEntry := fmt.Sprintf("line:%d", b.evidenceLine)
	if staged, ok := b.sink.(CodexFileEvidenceStager); ok {
		staged.StageToolFileEvidence(ctx, callID, position, entry, resultEntry, raw)
		return
	}
	if b.evidenceResults == nil {
		b.evidenceResults = map[string][]fileevidence.Candidate{}
	}
	b.evidenceResults[entry] = append(b.evidenceResults[entry], CaptureCodexToolFileEvidence(raw, entry, resultEntry)...)
}
