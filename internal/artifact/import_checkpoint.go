package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type importCheckpoint struct {
	Version  int
	Origin   string
	Sequence int
	Sessions map[string]string
}

func readVerifiedImportArtifact(
	ctx context.Context,
	store ArtifactStore,
	entry Entry,
	decodedLimit int64,
) (_ []byte, retErr error) {
	if store == nil {
		return nil, errors.New("artifact import store is required")
	}
	if err := validateStoreRef(entry.Ref); err != nil {
		return nil, err
	}
	if err := validateStoreIdentity(entry.Identity); err != nil {
		return nil, err
	}
	if err := validateRefIdentity(entry.Ref, entry.Identity); err != nil {
		return nil, err
	}
	if decodedLimit < 0 {
		return nil, errors.New("artifact import decoded limit must not be negative")
	}
	if entry.Identity.Size > decodedLimit {
		return nil, fmt.Errorf(
			"%w: %s exceeds decoded limit %d",
			ErrArtifactInvalid, entry.Ref.Name, decodedLimit,
		)
	}

	opened, reader, err := store.Open(ctx, entry.Ref)
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, errors.New("artifact import store returned no reader")
	}
	defer func() {
		retErr = errors.Join(retErr, reader.Close())
	}()
	if opened.Ref != entry.Ref || opened.Identity != entry.Identity {
		return nil, fmt.Errorf(
			"%w: opened artifact identity differs from import claim",
			ErrArtifactCorrupt,
		)
	}

	var body bytes.Buffer
	copyBuffer := make([]byte, 32<<10)
	_, err = io.CopyBuffer(
		&body, io.LimitReader(reader, decodedLimit+1), copyBuffer,
	)
	if err != nil {
		return nil, fmt.Errorf("reading artifact import content: %w", err)
	}
	if int64(body.Len()) > decodedLimit {
		return nil, fmt.Errorf(
			"%w: %s exceeds decoded limit %d",
			ErrArtifactInvalid, entry.Ref.Name, decodedLimit,
		)
	}
	if err := reader.Verify(); err != nil {
		return nil, fmt.Errorf("verifying artifact import content: %w", err)
	}
	if int64(body.Len()) != entry.Identity.Size {
		return nil, fmt.Errorf(
			"%w: artifact import size differs from catalog identity",
			ErrArtifactCorrupt,
		)
	}
	return body.Bytes(), nil
}

func decodeImportCheckpoint(
	data []byte, expectedOrigin, name string,
) (importCheckpoint, error) {
	fields, err := decodeImportJSONObject(data)
	if err != nil {
		return importCheckpoint{}, err
	}
	versionRaw, ok := fields["v"]
	if !ok {
		return importCheckpoint{}, invalidImportCheckpointf("version is missing")
	}
	var version int
	if err := json.Unmarshal(versionRaw, &version); err != nil {
		return importCheckpoint{}, invalidImportCheckpointf(
			"version is invalid: %v", err,
		)
	}
	if version > checkpointFormatVersion {
		return importCheckpoint{}, &futureArtifactVersionError{
			Kind: KindCheckpoints, Version: version,
		}
	}
	if version < checkpointFormatVersion {
		return importCheckpoint{}, invalidImportCheckpointf(
			"version %d is unsupported", version,
		)
	}
	if len(fields) != 4 {
		return importCheckpoint{}, invalidImportCheckpointf(
			"current-version fields are not exact",
		)
	}
	for _, field := range []string{"origin", "seq", "sessions", "v"} {
		if _, ok := fields[field]; !ok {
			return importCheckpoint{}, invalidImportCheckpointf(
				"field %q is missing", field,
			)
		}
	}

	var origin string
	if err := json.Unmarshal(fields["origin"], &origin); err != nil {
		return importCheckpoint{}, invalidImportCheckpointf(
			"origin is invalid: %v", err,
		)
	}
	if origin != expectedOrigin {
		return importCheckpoint{}, invalidImportCheckpointf(
			"origin %q does not match %q", origin, expectedOrigin,
		)
	}
	var sequence int
	if err := json.Unmarshal(fields["seq"], &sequence); err != nil {
		return importCheckpoint{}, invalidImportCheckpointf(
			"sequence is invalid: %v", err,
		)
	}
	nameSequence, err := checkpointSequence(name)
	if err != nil {
		return importCheckpoint{}, invalidImportCheckpointf(
			"checkpoint name is invalid: %v", err,
		)
	}
	if sequence != nameSequence {
		return importCheckpoint{}, invalidImportCheckpointf(
			"sequence %d does not match %s", sequence, name,
		)
	}
	sessions, err := decodeImportCheckpointSessions(
		fields["sessions"], expectedOrigin,
	)
	if err != nil {
		return importCheckpoint{}, err
	}
	return importCheckpoint{
		Version: version, Origin: origin, Sequence: sequence, Sessions: sessions,
	}, nil
}

func decodeImportJSONObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return nil, invalidImportCheckpointf("decoding object: %v", err)
	}
	if token != json.Delim('{') {
		return nil, invalidImportCheckpointf("checkpoint must be an object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, invalidImportCheckpointf("decoding object key: %v", err)
		}
		key, ok := token.(string)
		if !ok {
			return nil, invalidImportCheckpointf("object key is invalid")
		}
		if _, exists := fields[key]; exists {
			return nil, invalidImportCheckpointf("duplicate field %q", key)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, invalidImportCheckpointf(
				"decoding field %q: %v", key, err,
			)
		}
		fields[key] = raw
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return nil, invalidImportCheckpointf("checkpoint object is incomplete")
	}
	var trailing json.RawMessage
	err = decoder.Decode(&trailing)
	if !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, invalidImportCheckpointf("checkpoint has trailing JSON")
		}
		return nil, invalidImportCheckpointf("decoding trailing content: %v", err)
	}
	return fields, nil
}

func decodeImportCheckpointSessions(
	data json.RawMessage, origin string,
) (map[string]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, invalidImportCheckpointf("decoding sessions: %v", err)
	}
	if token != json.Delim('{') {
		return nil, invalidImportCheckpointf("sessions must be an object")
	}
	sessions := make(map[string]string)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, invalidImportCheckpointf("decoding session GID: %v", err)
		}
		gid, ok := token.(string)
		if !ok {
			return nil, invalidImportCheckpointf("session GID is invalid")
		}
		if _, exists := sessions[gid]; exists {
			return nil, invalidImportCheckpointf("duplicate session GID %q", gid)
		}
		if !strings.HasPrefix(gid, origin+"~") || len(gid) == len(origin)+1 {
			return nil, invalidImportCheckpointf("session GID %q is invalid", gid)
		}
		var manifestHash string
		if err := decoder.Decode(&manifestHash); err != nil {
			return nil, invalidImportCheckpointf(
				"manifest hash for %q is invalid: %v", gid, err,
			)
		}
		if err := validateHashHex(manifestHash); err != nil {
			return nil, invalidImportCheckpointf(
				"manifest hash for %q is invalid: %v", gid, err,
			)
		}
		sessions[gid] = manifestHash
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return nil, invalidImportCheckpointf("sessions object is incomplete")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, invalidImportCheckpointf("sessions has trailing JSON")
	}
	return sessions, nil
}

func invalidImportCheckpointf(format string, args ...any) error {
	return fmt.Errorf("%w: checkpoint %s", ErrArtifactInvalid, fmt.Sprintf(format, args...))
}
