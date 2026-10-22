package parser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"strings"
)

// ClaudeSourceImage is lookup evidence, separate from the ingest projection.
type ClaudeSourceImage struct {
	SourceUUID string
	ToolUseID  string
	Index      int
	MediaType  string
	Size       int64
	SHA256     string
}

type claudeImageBlock struct {
	Type      string         `json:"type"`
	ToolUseID string         `json:"tool_use_id"`
	Content   jsontext.Value `json:"content"`
	Source    struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
}

// ClaudeSourceImages reads bounded JSONL records without changing archived messages.
// The caller supplies the request deadline. Missing or incomplete evidence is explicit.
func ClaudeSourceImages(ctx context.Context, path string, archivedUUIDs map[string]int, archivedToolUseIDs map[string]int) ([]ClaudeSourceImage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, os.ErrInvalid
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	lr := newLineReaderContext(ctx, f, maxLineSize)
	defer releaseLineReader(lr)
	var images []ClaudeSourceImage
	seen := make(map[string]bool, len(archivedUUIDs))
	var sourceErr error
	for {
		if err := ctx.Err(); err != nil {
			return images, err
		}
		line, ok := lr.nextBytes()
		if !ok {
			break
		}
		var entry struct {
			UUID    string `json:"uuid"`
			Type    string `json:"type"`
			Message struct {
				Content jsontext.Value `json:"content"`
			} `json:"message"`
		}
		// Every archived UUID must survive in the source. Decode just its identity
		// on lines without base64; image payloads are decoded only when present.
		if !bytes.Contains(line, []byte("base64")) {
			var identity struct {
				UUID string `json:"uuid"`
			}
			if json.Unmarshal(line, &identity) == nil {
				if _, ok := archivedUUIDs[identity.UUID]; ok {
					seen[identity.UUID] = true
				}
			}
			continue
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			sourceErr = errors.Join(sourceErr, err)
			continue
		}
		if _, ok := archivedUUIDs[entry.UUID]; ok {
			seen[entry.UUID] = true
		}
		if entry.Type != "user" || len(entry.Message.Content) == 0 || entry.Message.Content[0] != '[' {
			continue
		}
		var blocks []claudeImageBlock
		if err := json.Unmarshal(entry.Message.Content, &blocks); err != nil {
			sourceErr = errors.Join(sourceErr, err)
			continue
		}
		add := func(block claudeImageBlock, toolID string, index int) {
			if (block.Type != "image" && block.Type != "document") || block.Source.Type != "base64" {
				return
			}
			hash := sha256.New()
			decoder := base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(block.Source.Data))
			size, err := io.Copy(hash, &countingReader{ctx: ctx, r: decoder})
			if err != nil {
				sourceErr = errors.Join(sourceErr, err)
			} else {
				images = append(images, ClaudeSourceImage{SourceUUID: entry.UUID, ToolUseID: toolID, Index: index, MediaType: block.Source.MediaType, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))})
			}
		}
		index := 0
		for _, block := range blocks {
			if block.Type == "tool_result" {
				if _, ok := archivedToolUseIDs[block.ToolUseID]; !ok {
					continue
				}
				var nested []claudeImageBlock
				if len(block.Content) == 0 || block.Content[0] != '[' {
					continue
				}
				if err := json.Unmarshal(block.Content, &nested); err != nil {
					sourceErr = errors.Join(sourceErr, err)
					continue
				}
				imageIndex := 0
				for _, child := range nested {
					if child.Type == "image" || child.Type == "document" {
						add(child, block.ToolUseID, imageIndex)
						imageIndex++
					}
				}
			} else if block.Type == "image" || block.Type == "document" {
				if _, ok := archivedUUIDs[entry.UUID]; ok {
					add(block, "", index)
				}
				index++
			}
		}
	}
	for uuid := range archivedUUIDs {
		if !seen[uuid] {
			sourceErr = errors.Join(sourceErr, os.ErrInvalid)
			break
		}
	}
	if lr.skippedOversized {
		sourceErr = errors.Join(sourceErr, os.ErrInvalid)
	}
	return images, errors.Join(sourceErr, lr.Err(), ctx.Err())
}
