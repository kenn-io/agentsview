package parser

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"os"
	"strings"
)

// ClaudeSourceImage is lookup evidence, separate from the ingest projection.
type ClaudeSourceImage struct {
	SourceUUID     string
	NextSourceUUID string
	ToolUseID      string
	Index          int
	MediaType      string
	Size           int64
	SHA256         string
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
func ClaudeSourceImages(ctx context.Context, path string, messageUUIDs map[string]int) ([]ClaudeSourceImage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, os.ErrInvalid
	}
	lr := newLineReaderContext(ctx, f, maxLineSize)
	defer releaseLineReader(lr)
	var images []ClaudeSourceImage
	pending := 0
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
				Role    string         `json:"role"`
				Content jsontext.Value `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
		if entry.Type != "user" && entry.Type != "assistant" {
			continue
		}
		if _, stored := messageUUIDs[entry.UUID]; stored {
			for i := pending; i < len(images); i++ {
				images[i].NextSourceUUID = entry.UUID
			}
			pending = len(images)
		}
		if entry.Type != "user" || (entry.Message.Role != "" && entry.Message.Role != "user") {
			continue
		}
		var blocks []claudeImageBlock
		if json.Unmarshal(entry.Message.Content, &blocks) != nil {
			continue
		}
		add := func(block claudeImageBlock, toolID string, index int) {
			if (block.Type != "image" && block.Type != "document") || block.Source.Type != "base64" {
				return
			}
			hash := sha256.New()
			decoder := base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(block.Source.Data))
			size, err := io.Copy(hash, &countingReader{ctx: ctx, r: decoder})
			if err == nil {
				images = append(images, ClaudeSourceImage{SourceUUID: entry.UUID, ToolUseID: toolID, Index: index, MediaType: block.Source.MediaType, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))})
			}
		}
		index := 0
		for _, block := range blocks {
			if block.Type == "tool_result" {
				var nested []claudeImageBlock
				if json.Unmarshal(block.Content, &nested) != nil {
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
				add(block, "", index)
				index++
			}
		}
	}
	if err := lr.Err(); err != nil {
		return images, err
	}
	if lr.skippedOversized {
		return images, os.ErrInvalid
	}
	return images, ctx.Err()
}
