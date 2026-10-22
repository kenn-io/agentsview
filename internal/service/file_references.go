package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/integrations/docbank"
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
	State          string          `json:"state" enum:"found,not_found,unavailable,limit_exceeded"`
	Matches        []docbank.Match `json:"matches"`
	MatchTotal     int             `json:"match_total"`
}

type FileReferencesResult struct {
	TranscriptRevision string                    `json:"transcript_revision"`
	Integration        string                    `json:"integration" enum:"disabled,enabled"`
	LookupFailed       bool                      `json:"lookup_failed,omitempty" doc:"At least one Docbank lookup failed; retry may resolve unavailable occurrences."`
	Occurrences        []FileReferenceOccurrence `json:"occurrences"`
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

// SupportsFileReferences reports whether svc can look up session files, letting a backend that knows its Docbank configuration opt out.
func SupportsFileReferences(svc SessionService) bool {
	_, provider := svc.(FileReferencesProvider)
	capability, ok := svc.(interface{ SupportsFileReferences() bool })
	return provider && (!ok || capability.SupportsFileReferences())
}

func (b *directBackend) SupportsFileReferences() bool { return b.docbank != nil }

func WithDocbank(client *docbank.Client) DirectBackendOption {
	return func(b *directBackend) { b.docbank = client }
}

// WithConfiguredDocbank applies the same integration configuration to every local backend.
func WithConfiguredDocbank(cfg config.Config) (DirectBackendOption, error) {
	if err := cfg.Integrations.Docbank.Validate(); err != nil {
		return nil, err
	}
	client := docbank.New(cfg.Integrations.Docbank)
	if !cfg.Integrations.Docbank.Configured() {
		client = nil
	}
	return WithDocbank(client), nil
}

func (b *directBackend) FileReferences(ctx context.Context, id string) (FileReferencesResult, error) {
	out := FileReferencesResult{Integration: "disabled", Occurrences: []FileReferenceOccurrence{}}
	if b.docbank == nil {
		return out, nil
	}
	readCtx, cancelRead := context.WithTimeout(ctx, 5*time.Second)
	defer cancelRead()
	messages, revision, err := b.fileReferenceMessages(readCtx, id)
	cancelRead()
	if err != nil {
		return out, err
	}
	out.TranscriptRevision = revision
	out.Integration = "enabled"
	add := func(ordinal int, toolID, toolName *string, event *int, images []db.RetainedImage) {
		for _, image := range images {
			out.Occurrences = append(out.Occurrences, FileReferenceOccurrence{MessageOrdinal: ordinal, ToolUseID: toolID, ToolName: toolName, EventIndex: event, ImageIndex: image.Index, MediaType: image.MediaType, Size: image.Size, SHA256: image.SHA256, State: "unavailable", Matches: []docbank.Match{}})
		}
	}
	for _, message := range messages {
		add(message.Ordinal, nil, nil, nil, db.MarkdownAssetImages(message.Content))
		for _, call := range message.ToolCalls {
			add(message.Ordinal, new(call.ToolUseID), new(call.ToolName), nil, db.RetainedToolResultImages(call.ResultContent))
			for _, event := range call.ResultEvents {
				add(message.Ordinal, new(call.ToolUseID), new(call.ToolName), new(event.EventIndex), db.RetainedToolResultImages(event.Content))
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
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
	for worker := range min(4, len(digests)) {
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
		if !exists {
			occurrence.State = "limit_exceeded"
			continue
		}
		if !resolved[index] {
			out.LookupFailed = true
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

// fileReferenceMessages uses the same observed and expected revisions as Messages,
// including across pages, so a lookup never combines different transcripts.
func (b *directBackend) fileReferenceMessages(ctx context.Context, id string) ([]db.Message, string, error) {
	session, err := b.db.GetSession(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if session == nil {
		return nil, "", ErrSessionNotFound
	}
	if session.TranscriptRevision == nil || *session.TranscriptRevision == "" {
		return nil, "", ErrRevisionBoundReadUnavailable
	}
	revision := *session.TranscriptRevision
	var messages []db.Message
	for from := 0; ; {
		page, err := b.Messages(ctx, id, MessageFilter{From: new(from), Limit: db.MaxMessageLimit, ExpectedRevision: revision})
		if err != nil {
			return nil, "", err
		}
		messages = append(messages, page.Messages...)
		if len(page.Messages) < db.MaxMessageLimit {
			return messages, revision, nil
		}
		from = *page.LastOrdinal + 1
	}
}
