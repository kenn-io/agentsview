// Package fileevidence describes exact retained transcript bytes without I/O.
package fileevidence

import (
	"encoding/json/v2"
	"errors"
)

const (
	MaxAttachmentBytes   = 32 << 20
	MaxMessageBytes      = 64 << 20
	MaxOccurrences       = 64
	ProofInlinePayloadV1 = "inline_payload_v1"
	ProofPiReadUTF8V1    = "pi_native_read_utf8_v1"
	ProofPiWriteUTF8V1   = "pi_native_write_utf8_v1"
	ProofOwnedImageV1    = "owned_image_verified_v1"
)

type Position struct {
	Entry       string
	Call        int
	ResultEntry string
	Result      int
	Block       int
}

func (p Position) Slot() (string, error) {
	if len(p.Entry) > 256 || len(p.ResultEntry) > 256 || p.Call < -1 || p.Result < -1 || p.Block < -1 || p.Entry == "" {
		return "", errors.New("invalid position")
	}
	b, err := json.Marshal([]any{1, p.Entry, p.Call, p.ResultEntry, p.Result, p.Block})
	if len(b) > 1024 {
		return "", errors.New("invalid position")
	}
	return string(b), err
}

type Record struct {
	// ResultEventIndex is the retained per-call event, independent of native Position.Result.
	// The archive derives it from support metadata rather than the native slot.
	ResultEventIndex *int `json:"-"`
	Position         Position
	Kind             string
	MediaType        string
	DisplayName      string
	DisplayPath      string
	Size             *int64
	SHA256           string
	ProofMethod      string
	Retention        string
	AssetRef         string
	IneligibleReason string
}
type Coverage struct {
	Version int
	State   string
}
type Window struct {
	Binding                   string
	Offset, Limit             int
	FirstOrdinal, LastOrdinal *int
	OccurrenceID              string
}
type Snapshot struct {
	Binding, Revision string
	Coverage          Coverage
	Rows              []Occurrence
	Total             int
	NextOffset        *int
}
type Occurrence struct {
	MessageOrdinal int
	Record         Record
}

// Candidate holds transient bytes and the exact field which supports their proof.
// Support is message, input, or result; Call/Result index the retained source.
type Candidate struct {
	Record                  Record
	Payload                 []byte
	Support, SupportingText string
	ToolUseID               string
	Call, Result            int
}

// AppendCandidate enforces the same decoded budget before and after pairing.
func AppendCandidate(current []Candidate, c Candidate) []Candidate {
	if len(current) >= MaxOccurrences {
		return current
	}
	n := len(c.Payload)
	for _, prior := range current {
		n += len(prior.Payload)
	}
	if n > MaxMessageBytes {
		c.Payload = nil
		c.Record.Size = nil
		c.Record.SHA256 = ""
		c.Record.IneligibleReason = "message_limit"
	}
	return append(current, c)
}
