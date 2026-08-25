package rawclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

// maxUploadOffsetConflicts bounds consecutive offset-conflict recoveries so a
// server that keeps rejecting cannot loop the client forever.
const maxUploadOffsetConflicts = 8

type uploadStartResponse struct {
	UploadID string            `json:"upload_id,omitempty"`
	Object   rawsync.ObjectRef `json:"object"`
	Offset   int64             `json:"offset"`
	Complete bool              `json:"complete"`
}

type uploadPatchResponse struct {
	uploadStartResponse
}

// MissingObjects asks the server which of objects it does not already hold
// for provider and returns that subset in request order.
func (c *Client) MissingObjects(
	ctx context.Context,
	provider parser.AgentType,
	objects []rawsync.ObjectRef,
) ([]rawsync.ObjectRef, error) {
	body := struct {
		Provider parser.AgentType    `json:"provider"`
		Objects  []rawsync.ObjectRef `json:"objects"`
	}{Provider: provider, Objects: objects}
	resp, err := c.do(ctx, http.MethodPost, "/api/v1/raw-sync/objects/missing", nil, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Missing []rawsync.ObjectRef `json:"missing"`
	}
	if err := jsonDecode(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("rawclient: decode missing objects: %w", err)
	}
	return out.Missing, nil
}

// UploadObject transfers one immutable object through a resumable upload
// session. Content must supply exactly object.Length bytes; chunk boundaries
// are storage boundaries only. Offset conflicts adopt the server's
// authoritative offset; checksum mismatch after finalization is terminal and
// the caller must re-capture the source. When the confirmed offset reaches
// the full length before the server reports completion, one empty PATCH at
// that offset triggers finalization — success is never reported before the
// server confirms the session complete.
func (c *Client) UploadObject(
	ctx context.Context,
	provider parser.AgentType,
	object rawsync.ObjectRef,
	content io.ReaderAt,
) error {
	body := struct {
		Provider parser.AgentType  `json:"provider"`
		Object   rawsync.ObjectRef `json:"object"`
	}{Provider: provider, Object: object}
	resp, err := c.do(ctx, http.MethodPost, "/api/v1/raw-sync/uploads", nil, body)
	if err != nil {
		return err
	}
	var session uploadStartResponse
	if err := jsonDecode(resp.Body, &session); err != nil {
		resp.Body.Close()
		return fmt.Errorf("rawclient: decode upload session: %w", err)
	}
	resp.Body.Close()
	if err := validateUploadIdentity(session, object, ""); err != nil {
		return err
	}
	if err := validateUploadProgress(session.Offset, session.Complete, object.Length); err != nil {
		return err
	}
	if session.Complete {
		return nil
	}
	offset := session.Offset
	buf := make([]byte, c.chunkBytes)
	conflicts := 0
	for offset < object.Length {
		chunk := buf
		if remain := object.Length - offset; remain < int64(len(chunk)) {
			chunk = chunk[:remain]
		}
		if n, err := content.ReadAt(chunk, offset); err != nil &&
			(!errors.Is(err, io.EOF) || n != len(chunk)) {
			return fmt.Errorf("rawclient: read object bytes at %d: %w", offset, err)
		}
		next, complete, err := c.appendChunk(
			ctx, session.UploadID, object, offset, chunk,
		)
		if err != nil {
			if apiErr, ok := errors.AsType[*APIError](err); ok &&
				apiErr.Code == CodeUploadOffset &&
				apiErr.CurrentUploadOffset != nil {
				adopted := *apiErr.CurrentUploadOffset
				if adopted < 0 || adopted > object.Length {
					return fmt.Errorf("rawclient: server upload offset %d out of range", adopted)
				}
				if adopted == offset {
					return fmt.Errorf("rawclient: upload offset conflict repeats offset %d", offset)
				}
				offset = adopted
				conflicts++
				if conflicts > maxUploadOffsetConflicts {
					return fmt.Errorf("rawclient: upload offset conflicted more than %d times",
						maxUploadOffsetConflicts)
				}
				continue
			}
			return err
		}
		conflicts = 0
		if next <= offset && !complete {
			return fmt.Errorf("rawclient: upload made no progress at offset %d", offset)
		}
		offset = next
		if complete {
			return nil
		}
	}
	// The session holds every byte, but no response has reported completion —
	// the start response and the last chunk can both land here. One empty
	// PATCH at the confirmed offset asks the server to finalize; custody is
	// not accepted until it answers complete.
	next, complete, err := c.appendChunk(
		ctx, session.UploadID, object, offset, nil,
	)
	if err != nil {
		return err
	}
	if next != object.Length || !complete {
		return fmt.Errorf(
			"rawclient: upload session not finalized at offset %d", next)
	}
	return nil
}

// appendChunk PATCHes one chunk at offset and returns the server-confirmed
// next offset and completion, preferring the response headers over the body.
// A successful response must acknowledge exactly the bytes in this request.
func (c *Client) appendChunk(
	ctx context.Context,
	uploadID string,
	object rawsync.ObjectRef,
	offset int64,
	chunk []byte,
) (int64, bool, error) {
	resp, err := c.doOctet(ctx, http.MethodPatch,
		"/api/v1/raw-sync/uploads/"+url.PathEscape(uploadID), offset, chunk)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	var out uploadPatchResponse
	if err := jsonDecode(resp.Body, &out); err != nil {
		return 0, false, fmt.Errorf("rawclient: decode upload append: %w", err)
	}
	if err := validateUploadIdentity(out.uploadStartResponse, object, uploadID); err != nil {
		return 0, false, err
	}
	if err := validateUploadProgress(out.Offset, out.Complete, object.Length); err != nil {
		return 0, false, err
	}
	next, complete := out.Offset, out.Complete
	if headerOffset, headerComplete, ok := uploadProgress(resp.Header); ok {
		next, complete = headerOffset, headerComplete
	}
	if err := validateUploadProgress(next, complete, object.Length); err != nil {
		return 0, false, err
	}
	expectedNext := offset + int64(len(chunk))
	if next != expectedNext {
		return 0, false, fmt.Errorf(
			"rawclient: upload response confirmed offset %d; expected offset %d after %d-byte chunk",
			next, expectedNext, len(chunk))
	}
	return next, complete, nil
}

func validateUploadIdentity(
	response uploadStartResponse,
	object rawsync.ObjectRef,
	uploadID string,
) error {
	if response.Object != object {
		return fmt.Errorf("rawclient: upload response identifies a different object")
	}
	if uploadID != "" {
		if response.UploadID != uploadID {
			return fmt.Errorf("rawclient: upload response identifies a different upload ID")
		}
		return nil
	}
	if !response.Complete && response.UploadID == "" {
		return fmt.Errorf("rawclient: incomplete upload response is missing upload ID")
	}
	return nil
}

func validateUploadProgress(offset int64, complete bool, length int64) error {
	if offset < 0 || offset > length {
		return fmt.Errorf("rawclient: upload response offset %d out of range", offset)
	}
	if complete && offset != length {
		return fmt.Errorf(
			"rawclient: upload response complete at offset %d, want %d", offset, length,
		)
	}
	return nil
}

// uploadProgress reads the authoritative post-PATCH progress headers. It
// reports ok only when both headers parse; callers then prefer these values
// over the response body.
func uploadProgress(h http.Header) (offset int64, complete bool, ok bool) {
	rawOffset := h.Get("Upload-Offset")
	rawComplete := h.Get("Upload-Complete")
	if rawOffset == "" || rawComplete == "" {
		return 0, false, false
	}
	offset, err := strconv.ParseInt(rawOffset, 10, 64)
	if err != nil {
		return 0, false, false
	}
	complete, err = strconv.ParseBool(rawComplete)
	if err != nil {
		return 0, false, false
	}
	return offset, complete, true
}

// doOctet sends one authenticated octet-stream request carrying the
// Upload-Offset header, sharing do's unauthorized-retry logic. The chunk must
// not exceed the client's chunk size, which itself never exceeds
// rawsync.DefaultUploadChunkBytes.
func (c *Client) doOctet(
	ctx context.Context,
	method string,
	path string,
	offset int64,
	chunk []byte,
) (*http.Response, error) {
	if int64(len(chunk)) > c.chunkBytes {
		return nil, fmt.Errorf("rawclient: chunk of %d bytes exceeds upload chunk size %d",
			len(chunk), c.chunkBytes)
	}
	header := http.Header{}
	header.Set("Content-Type", "application/octet-stream")
	header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	return c.do(ctx, method, path, header, octetBody{data: chunk})
}
