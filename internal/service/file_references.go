package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/integrations/docbank"
	"go.kenn.io/agentsview/internal/parser"
)

var ErrSessionNotFound = errors.New("session not found")

type FileReferenceOccurrence struct {
	MessageOrdinal int             `json:"message_ordinal"`
	ToolUseID      *string         `json:"tool_use_id"`
	ToolName       *string         `json:"tool_name"`
	EventIndex     *int            `json:"event_index"`
	ImageIndex     int             `json:"image_index"`
	MediaType      string          `json:"media_type"`
	Size           *int64          `json:"size"`
	SHA256         string          `json:"sha256"`
	State          string          `json:"state" enum:"found,not_found,unavailable"`
	Matches        []docbank.Match `json:"matches"`
	MatchTotal     int             `json:"match_total"`
}

type FileReferencesResult struct {
	Integration string                    `json:"integration" enum:"disabled,enabled"`
	Source      string                    `json:"source,omitempty" enum:"available,unavailable"`
	Occurrences []FileReferenceOccurrence `json:"occurrences"`
}

type FileReferencesProvider interface {
	FileReferences(context.Context, string) (FileReferencesResult, error)
}

func FileReferences(ctx context.Context, svc SessionService, id string) (FileReferencesResult, error) {
	if provider, ok := svc.(FileReferencesProvider); ok {
		return provider.FileReferences(ctx, id)
	}
	return FileReferencesResult{Integration: "disabled", Occurrences: []FileReferenceOccurrence{}}, nil
}
func WithDocbank(client *docbank.Client) DirectBackendOption {
	return func(b *directBackend) { b.docbank = client }
}

func (b *directBackend) FileReferences(ctx context.Context, id string) (FileReferencesResult, error) {
	out := FileReferencesResult{Integration: "disabled", Occurrences: []FileReferenceOccurrence{}}
	if b.docbank == nil {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	session, err := b.db.GetSession(ctx, id)
	if err != nil {
		return out, err
	}
	if session == nil {
		return out, ErrSessionNotFound
	}
	messages, err := b.db.GetAllMessages(ctx, id)
	if err != nil {
		return out, err
	}
	out.Integration = "enabled"
	add := func(ordinal int, toolID, toolName *string, event *int, images []db.RetainedImage) {
		for _, image := range images {
			out.Occurrences = append(out.Occurrences, FileReferenceOccurrence{MessageOrdinal: ordinal, ToolUseID: toolID, ToolName: toolName, EventIndex: event, ImageIndex: image.Index, MediaType: image.MediaType, Size: image.Size, SHA256: image.SHA256, State: "unavailable", Matches: []docbank.Match{}})
		}
	}
	byUUID := make(map[string]int)
	type toolAt struct {
		ordinal int
		name    string
	}
	tools := make(map[string]toolAt)
	for _, message := range messages {
		if message.SourceUUID != "" {
			byUUID[message.SourceUUID] = message.Ordinal
		}
		add(message.Ordinal, nil, nil, nil, db.MarkdownAssetImages(message.Content))
		for _, call := range message.ToolCalls {
			tools[call.ToolUseID] = toolAt{message.Ordinal, call.ToolName}
			add(message.Ordinal, new(call.ToolUseID), new(call.ToolName), nil, db.RetainedToolResultImages(call.ResultContent))
			for _, event := range call.ResultEvents {
				add(message.Ordinal, new(call.ToolUseID), new(call.ToolName), new(event.EventIndex), db.RetainedToolResultImages(event.Content))
			}
		}
	}
	if session.Agent == "claude" {
		out.Source = "available"
		path := ""
		full, fullErr := b.db.GetSessionFull(ctx, id)
		if fullErr == nil && full != nil && full.FilePath != nil {
			path = *full.FilePath
		}
		images, sourceErr := parser.ClaudeSourceImages(ctx, path, byUUID)
		if sourceErr != nil {
			out.Source = "unavailable"
		}
		for _, image := range images {
			ordinal, ok := byUUID[image.SourceUUID]
			if !ok {
				ordinal, ok = byUUID[image.NextSourceUUID]
			}
			if !ok {
				ordinal = -1
			}
			var toolID, toolName *string
			if image.ToolUseID != "" {
				at, exists := tools[image.ToolUseID]
				if exists {
					ordinal = at.ordinal
					toolID, toolName = new(image.ToolUseID), new(at.name)
				}
			}
			add(ordinal, toolID, toolName, nil, []db.RetainedImage{{Index: image.Index, MediaType: image.MediaType, Size: new(image.Size), SHA256: image.SHA256}})
		}
	}
	// Each worker owns a distinct result slot; occurrences are updated after joining.
	digests := []string{}
	seen := make(map[string]int)
	for _, occurrence := range out.Occurrences {
		if _, exists := seen[occurrence.SHA256]; !exists && len(digests) < 50 {
			seen[occurrence.SHA256] = len(digests)
			digests = append(digests, occurrence.SHA256)
		}
	}
	results := make([]docbank.Result, len(digests))
	resolved := make([]bool, len(digests))
	var workers sync.WaitGroup
	for worker := 0; worker < min(4, len(digests)); worker++ {
		workers.Go(func() {
			for i := worker; i < len(digests); i += 4 {
				if ctx.Err() != nil {
					return
				}
				result, err := b.docbank.Lookup(ctx, digests[i])
				if err == nil {
					results[i], resolved[i] = result, true
				}
			}
		})
	}
	workers.Wait()
	for i := range out.Occurrences {
		occurrence := &out.Occurrences[i]
		index, exists := seen[occurrence.SHA256]
		if !exists || !resolved[index] {
			continue
		}
		result := results[index]
		occurrence.State = "not_found"
		if result.Total > 0 {
			occurrence.State = "found"
		}
		occurrence.Matches, occurrence.MatchTotal = result.Matches, result.Total
	}
	return out, nil
}
