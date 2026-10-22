package parser

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
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
func ClaudeSourceImages(ctx context.Context, path string, sessionID string, messageUUIDs map[string]int) ([]ClaudeSourceImage, error) {
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
	var entries []dagEntry
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
			UUID       string `json:"uuid"`
			ParentUUID string `json:"parentUuid"`
			Type       string `json:"type"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			sourceErr = errors.Join(sourceErr, err)
			continue
		}
		if entry.Type != "user" && entry.Type != "assistant" {
			continue
		}
		entries = append(entries, dagEntry{uuid: entry.UUID, parentUuid: entry.ParentUUID, entryType: entry.Type, line: string(line), lineIndex: len(entries)})
	}
	sourceErr = errors.Join(sourceErr, lr.Err())
	if lr.skippedOversized {
		sourceErr = errors.Join(sourceErr, os.ErrInvalid)
	}
	baseID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	entries, err = mergeClaudeAssistantMessageChunksContext(ctx, entries)
	if err != nil {
		return nil, err
	}
	branches, err := selectClaudeBranches(ctx, entries, baseID, "")
	if err != nil {
		return nil, err
	}
	if branches != nil {
		var selected []dagEntry
		for i, branch := range branches {
			id := baseID
			if i > 0 {
				id += "-" + entries[branch.indices[0]].uuid
			}
			if id == sessionID {
				for _, index := range branch.indices {
					selected = append(selected, entries[index])
				}
				break
			}
		}
		if selected == nil {
			return nil, errors.Join(sourceErr, os.ErrNotExist)
		}
		entries = selected
	}
	pending := 0
	for _, record := range entries {
		if err := ctx.Err(); err != nil {
			return images, errors.Join(sourceErr, err)
		}
		line := []byte(record.line)
		var entry struct {
			UUID    string `json:"uuid"`
			Type    string `json:"type"`
			Message struct {
				Role    string         `json:"role"`
				Content jsontext.Value `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			sourceErr = errors.Join(sourceErr, err)
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
		if len(entry.Message.Content) == 0 || entry.Message.Content[0] != '[' {
			continue
		}
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
				add(block, "", index)
				index++
			}
		}
	}
	return images, errors.Join(sourceErr, ctx.Err())
}
