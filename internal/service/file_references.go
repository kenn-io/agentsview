package service

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/fileevidence"
	"go.kenn.io/agentsview/internal/integrations/docbank"
)

const (
	FileReferencesIntentHeader = "X-AgentsView-Lookup-Intent"
	FileReferencesIntentValue  = "docbank"
	FileReferencesMaxBytes     = 2 << 20
)

var (
	ErrFileReferencesInput       = errors.New("invalid file references input")
	ErrFileReferencesNotFound    = errors.New("session or occurrence not found")
	ErrFileReferencesUnavailable = errors.New("file references unavailable")
)

// FileReferencesService is the shared explicit lookup operation.
type FileReferencesService interface {
	FileReferences(context.Context, FileReferencesInput) (*FileReferencesResult, error)
}
type FileReferencesInput struct {
	SessionID       string `json:"session_id"`
	Binding         string `json:"binding,omitempty"`
	OccurrenceID    string `json:"occurrence_id,omitempty"`
	Offset          int    `json:"offset,omitempty"`
	Limit           int    `json:"limit,omitempty"`
	ReferenceOffset int    `json:"reference_offset,omitempty"`
	FirstOrdinal    *int   `json:"first_ordinal,omitempty"`
	LastOrdinal     *int   `json:"last_ordinal,omitempty"`
	Refresh         bool   `json:"refresh,omitempty"`
}

func NormalizeFileReferencesInput(in FileReferencesInput) (FileReferencesInput, error) {
	if in.SessionID == "" || len(in.Binding) > 128 || len(in.OccurrenceID) > 4096 || in.Offset < 0 || in.Limit < 0 || in.ReferenceOffset < 0 ||
		(in.Binding == "" && (in.Offset > 0 || in.OccurrenceID != "" || in.ReferenceOffset > 0)) ||
		(in.ReferenceOffset > 0 && in.OccurrenceID == "") || (in.Refresh && in.ReferenceOffset != 0) ||
		((in.FirstOrdinal == nil) != (in.LastOrdinal == nil)) {
		return in, ErrFileReferencesInput
	}
	if in.FirstOrdinal != nil && (*in.FirstOrdinal < 0 || *in.LastOrdinal < *in.FirstOrdinal) {
		return in, ErrFileReferencesInput
	}
	if in.Limit == 0 {
		in.Limit = 20
	}
	in.Limit = min(in.Limit, 50)
	return in, nil
}

type FileReferencesResult struct {
	Binding     string                    `json:"binding"`
	Revision    string                    `json:"revision"`
	Coverage    string                    `json:"coverage"`
	Capability  string                    `json:"capability"`
	Integration string                    `json:"integration"`
	Occurrences []FileReferenceOccurrence `json:"occurrences"`
	Total       int                       `json:"total"`
	NextOffset  *int                      `json:"next_offset"`
}
type LocalFileEvidence struct {
	OccurrenceID     string `json:"occurrence_id"`
	MessageOrdinal   int    `json:"message_ordinal"`
	ToolCallIndex    *int   `json:"tool_call_index"`
	ResultEventIndex *int   `json:"result_event_index"`
	BlockIndex       int    `json:"block_index"`
	Kind             string `json:"kind"`
	MediaType        string `json:"media_type"`
	DisplayName      string `json:"display_name"`
	DisplayPath      string `json:"display_path"`
	ProofMethod      string `json:"proof_method"`
	Size             *int64 `json:"size"`
	SHA256           string `json:"sha256"`
	AssetRef         string `json:"asset_ref"`
	IneligibleReason string `json:"ineligible_reason"`
}
type MessageFileEvidence struct {
	Ordinal int                 `json:"ordinal"`
	Records []LocalFileEvidence `json:"records"`
}
type FileReferenceOccurrence struct {
	LocalFileEvidence
	State               string          `json:"state"`
	CheckedAt           *time.Time      `json:"checked_at"`
	Matches             []docbank.Match `json:"matches"`
	ReferenceTotal      *int            `json:"reference_total"`
	NextReferenceOffset *int            `json:"next_reference_offset"`
}

func localFileEvidence(ordinal int, r fileevidence.Record, binding string) LocalFileEvidence {
	slot, _ := r.Position.Slot()
	out := LocalFileEvidence{OccurrenceID: db.EncodeFileEvidenceOccurrence(ordinal, slot, binding), MessageOrdinal: ordinal, BlockIndex: r.Position.Block, Kind: r.Kind, MediaType: r.MediaType, DisplayName: r.DisplayName, DisplayPath: r.DisplayPath, ProofMethod: r.ProofMethod, Size: r.Size, SHA256: r.SHA256, AssetRef: r.AssetRef, IneligibleReason: r.IneligibleReason}
	if r.Position.Call >= 0 {
		out.ToolCallIndex = new(r.Position.Call)
	}
	out.ResultEventIndex = r.ResultEventIndex
	return out
}

func WithFileReferencesResolver(r docbank.Resolver) DirectBackendOption {
	return func(b *directBackend) { b.fileResolver = r }
}

func (b *directBackend) FileReferences(ctx context.Context, in FileReferencesInput) (*FileReferencesResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	in, err := NormalizeFileReferencesInput(in)
	if err != nil {
		return nil, err
	}
	// Session access precedes capability, evidence, credentials and cache work.
	session, err := b.db.GetSession(ctx, in.SessionID)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, ErrFileReferencesNotFound
	}
	out := &FileReferencesResult{Coverage: "unavailable", Capability: "unavailable", Integration: "disabled", Occurrences: []FileReferenceOccurrence{}}
	if b.fileResolver != nil {
		out.Integration = "enabled"
	}
	reader, ok := b.db.(db.FileEvidenceReader)
	if !ok {
		return out, nil
	}
	snapshot, err := reader.ReadFileEvidenceSnapshot(ctx, in.SessionID, fileevidence.Window{Binding: in.Binding, Offset: in.Offset, Limit: in.Limit, OccurrenceID: in.OccurrenceID, FirstOrdinal: in.FirstOrdinal, LastOrdinal: in.LastOrdinal})
	if err != nil {
		return nil, fileReferencesReadError(err)
	}
	if in.OccurrenceID != "" && snapshot.Total == 0 {
		return nil, ErrFileReferencesNotFound
	}
	out.Binding = snapshot.Binding
	out.Revision = snapshot.Revision
	out.Coverage = snapshot.Coverage.State
	out.Capability = "available"
	out.Total = snapshot.Total
	out.NextOffset = snapshot.NextOffset
	pages := make(map[docbank.Lookup]docbank.Page)
	keys := make([]docbank.Lookup, len(snapshot.Rows))
	// Leave time within the total budget to revalidate after bounded remote work.
	deadline, _ := ctx.Deadline()
	remoteCtx, stop := context.WithDeadline(ctx, deadline.Add(-50*time.Millisecond))
	defer stop()
	for i, row := range snapshot.Rows {
		local := localFileEvidence(row.MessageOrdinal, row.Record, snapshot.Binding)
		occurrence := FileReferenceOccurrence{LocalFileEvidence: local, State: "ineligible", Matches: []docbank.Match{}}
		if local.IneligibleReason == "" && local.SHA256 != "" && local.Size != nil {
			occurrence.State = "unavailable"
			if b.fileResolver != nil {
				key := docbank.Lookup{SHA256: local.SHA256, Size: *local.Size, ReferenceOffset: in.ReferenceOffset, Refresh: in.Refresh}
				keys[i] = key
				page, cached := pages[key]
				if !cached {
					page = docbank.Page{State: "deferred"}
					if remoteCtx.Err() == nil {
						page, err = b.fileResolver.Lookup(remoteCtx, key)
						if err != nil && page.State != "deferred" {
							page = docbank.Page{State: "unavailable"}
						}
					}
					pages[key] = page
				}
				occurrence.State = page.State
				occurrence.CheckedAt = page.CheckedAt
				occurrence.ReferenceTotal = page.ReferenceTotal
				occurrence.NextReferenceOffset = page.NextReferenceOffset
				if page.State == "unavailable" {
					out.Integration = "unavailable"
				}
			}
		}
		out.Occurrences = append(out.Occurrences, occurrence)
	}
	if err := packFileReferences(out, pages, keys, in.ReferenceOffset); err != nil {
		return nil, err
	}
	if err := reader.ValidateFileEvidenceBinding(ctx, in.SessionID, snapshot.Binding); err != nil {
		return nil, fileReferencesReadError(err)
	}
	return out, nil
}

func fileReferencesReadError(err error) error {
	if errors.Is(err, db.ErrFileEvidenceStale) {
		return ErrSourceChanged
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrFileReferencesNotFound
	}
	return ErrFileReferencesUnavailable
}

// Charge both structured JSON and its escaped MCP text copy. The SDK may send
// both; reserving 16 KiB also covers Huma's schema and JSON-RPC envelopes.
// Match the SDK's HTML/JS escaping and nil container representation with v2.
func fileReferencesWireWeight(v any) (int, error) {
	opts := []json.Options{jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true), json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true)}
	raw, err := json.Marshal(v, opts...)
	if err != nil {
		return 0, err
	}
	quoted, err := json.Marshal(string(raw), opts...)
	return len(raw) + len(quoted), err
}

func packFileReferences(out *FileReferencesResult, pages map[docbank.Lookup]docbank.Page, keys []docbank.Lookup, offset int) error {
	weight, err := fileReferencesWireWeight(out)
	if err != nil {
		return ErrFileReferencesUnavailable
	}
	const reserve = 16 << 10
	for i, key := range keys {
		page := pages[key]
		o := &out.Occurrences[i]
		for j, match := range page.Matches {
			if j >= len(page.MatchOffsets) || page.MatchOffsets[j] < offset || (j > 0 && page.MatchOffsets[j] <= page.MatchOffsets[j-1]) || page.ReferenceTotal == nil || page.MatchOffsets[j] >= *page.ReferenceTotal || page.MatchOffsets[j] >= offset+100 {
				return ErrFileReferencesUnavailable
			}
			n, err := fileReferencesWireWeight(match)
			if err != nil {
				return ErrFileReferencesUnavailable
			}
			// Commas and continuation integer growth are bounded independently.
			if weight+n+64 > FileReferencesMaxBytes-reserve {
				o.NextReferenceOffset = new(page.MatchOffsets[j])
				break
			}
			o.Matches = append(o.Matches, match)
			weight += n + 2
		}
	}
	actual, err := fileReferencesWireWeight(out)
	if err != nil || actual > FileReferencesMaxBytes-reserve {
		return ErrFileReferencesUnavailable
	}
	return nil
}
