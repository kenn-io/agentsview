package rawclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

// maxUploadOffsetConflicts bounds consecutive offset-conflict recoveries so a
// server that keeps rejecting cannot loop the client forever.
const maxUploadOffsetConflicts = 8

// MissingObjects asks the server which of objects it does not already hold
// for provider and returns that subset in request order.
func (c *Client) MissingObjects(
	ctx context.Context,
	provider parser.AgentType,
	objects []rawsync.ObjectRef,
) ([]rawsync.ObjectRef, error) {
	positions := make(map[rawsync.ObjectRef]int, len(objects))
	unique := make([]rawsync.ObjectRef, 0, len(objects))
	for _, object := range objects {
		canonical, err := rawsync.NewObjectRef(object.SHA256, object.Length)
		if err != nil || canonical != object {
			return nil, errors.New("rawclient: invalid missing object request")
		}
		if _, duplicate := positions[object]; duplicate {
			continue
		}
		positions[object] = len(unique)
		unique = append(unique, object)
	}
	response, err := c.do(ctx, func(api *apiclient.Client) (*apiclient.PostAPIV1RawSyncObjectsMissingResp, error) {
		return api.PostAPIV1RawSyncObjectsMissingWithResponse(ctx, &apiclient.PostAPIV1RawSyncObjectsMissingRequestOptions{Body: &apiclient.RawSyncMissingObjectsInputBody{Provider: string(provider), Objects: unique}})
	})
	if err != nil {
		return nil, err
	}
	if len(response.Body) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	out := response.JSON200
	lastPosition := -1
	missing := make([]rawsync.ObjectRef, 0, len(out.Missing))
	for _, object := range out.Missing {
		canonical, err := rawsync.NewObjectRef(object.SHA256, object.Length)
		position, requested := positions[object]
		if err != nil || canonical != object || !requested || position <= lastPosition {
			return nil, errors.New("rawclient: invalid missing objects response")
		}
		lastPosition = position
		missing = append(missing, object)
	}
	return missing, nil
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
	response, err := c.do(ctx, func(api *apiclient.Client) (*apiclient.PostAPIV1RawSyncUploadsResp, error) {
		return api.PostAPIV1RawSyncUploadsWithResponse(ctx, &apiclient.PostAPIV1RawSyncUploadsRequestOptions{Body: &apiclient.RawSyncUploadStartInputBody{Provider: string(provider), Object: object}})
	})
	if err != nil {
		return err
	}
	session := response.JSON200
	if err := validateUploadIdentity(*session, object, ""); err != nil {
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
			ctx, *session.UploadID, object, offset, chunk,
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
		ctx, *session.UploadID, object, offset, nil,
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
	if int64(len(chunk)) > c.chunkBytes {
		return 0, false, fmt.Errorf("rawclient: chunk of %d bytes exceeds upload chunk size %d", len(chunk), c.chunkBytes)
	}
	response, err := c.do(ctx, func(api *apiclient.Client) (*apiclient.PatchAPIV1RawSyncUploadsUploadIDResp, error) {
		return api.PatchAPIV1RawSyncUploadsUploadIDWithResponse(ctx, &apiclient.PatchAPIV1RawSyncUploadsUploadIDRequestOptions{
			PathParams: &apiclient.PatchAPIV1RawSyncUploadsUploadIDPath{UploadID: url.PathEscape(uploadID)},
		}, func(_ context.Context, req *http.Request) error {
			req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Body = io.NopCloser(bytes.NewReader(chunk))
			req.ContentLength = int64(len(chunk))
			req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(chunk)), nil }
			return nil
		})
	})
	if err != nil {
		return 0, false, err
	}
	out := response.JSON200
	if err := validateUploadIdentity(*out, object, uploadID); err != nil {
		return 0, false, err
	}
	if err := validateUploadProgress(out.Offset, out.Complete, object.Length); err != nil {
		return 0, false, err
	}
	next, complete := out.Offset, out.Complete
	if headerOffset, headerComplete, ok := uploadProgress(response.HTTPResponse.Header); ok {
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
	response apiclient.RawSyncUploadResponse,
	object rawsync.ObjectRef,
	uploadID string,
) error {
	if response.Object.SHA256 != object.SHA256 || response.Object.Length != object.Length {
		return errors.New("rawclient: upload response identifies a different object")
	}
	if uploadID != "" {
		if response.UploadID == nil || *response.UploadID != uploadID {
			return errors.New("rawclient: upload response identifies a different upload ID")
		}
		return nil
	}
	if !response.Complete && (response.UploadID == nil || *response.UploadID == "") {
		return errors.New("rawclient: incomplete upload response is missing upload ID")
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
