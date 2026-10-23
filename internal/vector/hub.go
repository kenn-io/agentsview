package vector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
	"go.kenn.io/agentsview/internal/remotecred"
)

const (
	HubVectorVersion  = "vectors.v1"
	MaxHubLookupBytes = 32 << 20
)

var (
	ErrHubLookupInvalid     = errors.New("invalid vector lookup")
	ErrHubGenerationChanged = errors.New("generation_changed")
)

// HubGeneration describes the serving process's pinned recipe and its pushers.
// Unavailable is nonempty when compatibility cannot be established.
type HubGeneration struct {
	Fingerprint      string            `json:"fingerprint,omitempty"`
	Model            string            `json:"model,omitempty"`
	Dimension        int               `json:"dimension,omitempty"`
	Params           map[string]string `json:"params,omitempty"`
	DataVersion      int               `json:"data_version,omitempty"`
	IncludeAutomated bool              `json:"include_automated"`
	Unavailable      string            `json:"unavailable,omitempty"`
}

type HubIdentity struct {
	SessionID   string `json:"session_id"`
	UnitKind    string `json:"unit_kind"`
	SourceUUID  string `json:"source_uuid,omitempty"`
	Occurrence  int    `json:"occurrence,omitempty"`
	Ordinal     int    `json:"ordinal,omitempty"`
	ContentHash string `json:"content_hash"`
}

type HubLookupRequest struct {
	Fingerprint string        `json:"fingerprint"`
	Identities  []HubIdentity `json:"identities" maxItems:"256"`
	Cursor      string        `json:"cursor,omitempty"`
}

func (r HubLookupRequest) Validate() error {
	if r.Fingerprint == "" || len(r.Fingerprint) > 128 || len(r.Identities) == 0 || len(r.Identities) > 256 || len(r.Cursor) > 128 {
		return fmt.Errorf("%w: requires a fingerprint and 1 to 256 identities", ErrHubLookupInvalid)
	}
	for _, d := range r.Identities {
		if d.SessionID == "" || len(d.SessionID) > 2048 || len(d.SourceUUID) > 2048 || d.ContentHash == "" || len(d.ContentHash) > 256 || (d.UnitKind != "user" && d.UnitKind != "run") || d.Ordinal < 0 || (d.SourceUUID != "" && d.Occurrence < 1) {
			return fmt.Errorf("%w: invalid document identity", ErrHubLookupInvalid)
		}
	}
	return nil
}

// Index refers to an identity's position in the unchanged lookup request.
// Pages can split a document; consumers install only a complete chunk set.
type HubChunk struct {
	Index      int    `json:"index"`
	ChunkIndex int    `json:"chunk_index"`
	ChunkCount int    `json:"chunk_count"`
	Vector     string `json:"vector"`
}

type HubLookupReply struct {
	Version     string     `json:"version"`
	Fingerprint string     `json:"fingerprint"`
	Chunks      []HubChunk `json:"chunks"`
	Absent      []int      `json:"absent"`
	NextCursor  string     `json:"next_cursor,omitempty"`
}

func EncodeHubVector(values []float32) (string, error) {
	if len(values) == 0 {
		return "", errors.New("empty vector")
	}
	raw := make([]byte, len(values)*4)
	for i, v := range values {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return "", errors.New("non-finite vector")
		}
		binary.LittleEndian.PutUint32(raw[i*4:], math.Float32bits(v))
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func DecodeHubVector(encoded string, dimension int) ([]float32, error) {
	if dimension <= 0 || len(encoded) > MaxHubLookupBytes {
		return nil, errors.New("invalid vector size")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw)%4 != 0 || len(raw)/4 != dimension {
		return nil, fmt.Errorf("vector dimension mismatch: expected %d", dimension)
	}
	values, err := decodeFloat32Blob(raw)
	if err != nil {
		return nil, err
	}
	for i := range values {
		if math.IsNaN(float64(values[i])) || math.IsInf(float64(values[i]), 0) {
			return nil, errors.New("non-finite vector")
		}
	}
	return values, nil
}

// HTTPHub uses the configured remote's credential and redirect-safe client.
type HTTPHub struct {
	URL, Token string
	Client     *http.Client
}

func (h *HTTPHub) HubVectorGeneration(ctx context.Context) (out HubGeneration, err error) {
	err = h.request(ctx, http.MethodGet, "generation", nil, &out)
	return
}
func (h *HTTPHub) LookupHubVectors(ctx context.Context, in HubLookupRequest) (out HubLookupReply, err error) {
	if err = in.Validate(); err != nil {
		return
	}
	err = h.request(ctx, http.MethodPost, "lookup", in, &out)
	return
}
func (h *HTTPHub) request(ctx context.Context, method, path string, body, out any) error {
	if h.Client == nil {
		return errors.New("remote vector HTTP client unavailable")
	}
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(h.URL, "/")+"/api/v1/vectors/"+path, bytes.NewReader(raw))
	if err != nil {
		return errors.New("invalid remote vector URL")
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	req.Header.Set("Accept-Encoding", "zstd")
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err == nil && resp.StatusCode == http.StatusConflict {
		resp.Body.Close()
		return ErrHubGenerationChanged
	}
	resp, err = remotecred.ResponsePolicy(h.URL)(resp, err)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote %s vector endpoint unavailable (HTTP %d)", remotecred.Redact(h.URL), resp.StatusCode)
	}
	var reader io.Reader = resp.Body
	switch resp.Header.Get("Content-Encoding") {
	case "zstd":
		decoder, err := zstd.NewReader(resp.Body, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
		if err != nil {
			return errors.New("invalid compressed hub vectors")
		}
		defer decoder.Close()
		reader = decoder
	case "", "identity":
	default:
		return errors.New("unsupported hub vector encoding")
	}
	raw, err = io.ReadAll(io.LimitReader(reader, MaxHubLookupBytes+1))
	if err != nil {
		return errors.New("reading hub vectors failed")
	}
	if len(raw) > MaxHubLookupBytes {
		return errors.New("hub vector response exceeds 32 MiB")
	}
	if err = json.Unmarshal(raw, out); err != nil {
		return errors.New("invalid hub vector response")
	}
	return nil
}
