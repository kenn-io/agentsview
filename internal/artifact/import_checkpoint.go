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

type importCheckpointSession struct {
	GID          string
	ManifestHash string
}

const (
	importCheckpointOriginField uint8 = 1 << iota
	importCheckpointSequenceField
	importCheckpointSessionsField
	importCheckpointVersionField
	importCheckpointCurrentFields = importCheckpointOriginField |
		importCheckpointSequenceField |
		importCheckpointSessionsField |
		importCheckpointVersionField
)

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
	checkpoint, sessionsRaw, err := decodeImportCheckpointHeader(
		data, expectedOrigin, name,
	)
	if err != nil {
		return importCheckpoint{}, err
	}
	checkpoint.Sessions = make(map[string]string)
	_, err = streamImportCheckpointSessions(
		sessionsRaw, expectedOrigin, 128,
		func(page []importCheckpointSession) error {
			for _, session := range page {
				if _, exists := checkpoint.Sessions[session.GID]; exists {
					return invalidImportCheckpointf(
						"duplicate session GID %q", session.GID,
					)
				}
				checkpoint.Sessions[session.GID] = session.ManifestHash
			}
			return nil
		},
	)
	if err != nil {
		return importCheckpoint{}, err
	}
	return checkpoint, nil
}

func decodeImportCheckpointHeader(
	data []byte, expectedOrigin, name string,
) (importCheckpoint, json.RawMessage, error) {
	fields, version, future, err := decodeImportCheckpointFields(data)
	if err != nil {
		return importCheckpoint{}, nil, err
	}
	if future {
		return importCheckpoint{}, nil, &futureArtifactVersionError{
			Kind: KindCheckpoints, Version: version,
		}
	}
	for _, field := range []string{"origin", "seq", "sessions"} {
		if _, ok := fields[field]; !ok {
			return importCheckpoint{}, nil, invalidImportCheckpointf(
				"field %q is missing", field,
			)
		}
	}

	var origin string
	if err := json.Unmarshal(fields["origin"], &origin); err != nil {
		return importCheckpoint{}, nil, invalidImportCheckpointf(
			"origin is invalid: %v", err,
		)
	}
	if origin != expectedOrigin {
		return importCheckpoint{}, nil, invalidImportCheckpointf(
			"origin %q does not match %q", origin, expectedOrigin,
		)
	}
	var sequence int
	if err := json.Unmarshal(fields["seq"], &sequence); err != nil {
		return importCheckpoint{}, nil, invalidImportCheckpointf(
			"sequence is invalid: %v", err,
		)
	}
	nameSequence, err := checkpointSequence(name)
	if err != nil {
		return importCheckpoint{}, nil, invalidImportCheckpointf(
			"checkpoint name is invalid: %v", err,
		)
	}
	if sequence != nameSequence {
		return importCheckpoint{}, nil, invalidImportCheckpointf(
			"sequence %d does not match %s", sequence, name,
		)
	}
	return importCheckpoint{
		Version: version, Origin: origin, Sequence: sequence,
	}, fields["sessions"], nil
}

func decodeImportCheckpointFields(
	data []byte,
) (map[string]json.RawMessage, int, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return nil, 0, false,
			invalidImportCheckpointf("decoding object: %v", err)
	}
	if token != json.Delim('{') {
		return nil, 0, false,
			invalidImportCheckpointf("checkpoint must be an object")
	}
	fields := make(map[string]json.RawMessage, 3)
	var seen uint8
	version := 0
	versionSeen := false
	future := false
	unknownBeforeVersion := false
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, 0, false,
				invalidImportCheckpointf("decoding object key: %v", err)
		}
		key, ok := token.(string)
		if !ok {
			return nil, 0, false,
				invalidImportCheckpointf("object key is invalid")
		}
		field := importCheckpointField(key)
		if field != 0 && seen&field != 0 {
			return nil, 0, false,
				invalidImportCheckpointf("duplicate field %q", key)
		}
		if field != 0 {
			seen |= field
		}
		if key == "v" {
			if err := decoder.Decode(&version); err != nil {
				return nil, 0, false, invalidImportCheckpointf(
					"version is invalid: %v", err,
				)
			}
			versionSeen = true
			switch {
			case version > checkpointFormatVersion:
				future = true
			case version < checkpointFormatVersion:
				return nil, 0, false, invalidImportCheckpointf(
					"version %d is unsupported", version,
				)
			case unknownBeforeVersion:
				return nil, 0, false, invalidImportCheckpointf(
					"current-version fields are not exact",
				)
			}
			continue
		}
		currentField := field != 0
		if !currentField {
			if versionSeen && !future {
				return nil, 0, false, invalidImportCheckpointf(
					"current-version fields are not exact",
				)
			}
			unknownBeforeVersion = true
		}
		valueStart := skipJSONWhitespace(data, int(decoder.InputOffset()))
		if valueStart >= len(data) || data[valueStart] != ':' {
			return nil, 0, false,
				invalidImportCheckpointf("field %q has no value", key)
		}
		valueStart = skipJSONWhitespace(data, valueStart+1)
		if err := skipImportJSONValue(decoder); err != nil {
			return nil, 0, false, invalidImportCheckpointf(
				"decoding field %q: %v", key, err,
			)
		}
		valueEnd := int(decoder.InputOffset())
		if valueEnd <= valueStart || valueEnd > len(data) {
			return nil, 0, false,
				invalidImportCheckpointf("field %q is invalid", key)
		}
		if currentField {
			fields[key] = data[valueStart:valueEnd]
		}
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return nil, 0, false,
			invalidImportCheckpointf("checkpoint object is incomplete")
	}
	var trailing json.RawMessage
	err = decoder.Decode(&trailing)
	if !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, 0, false,
				invalidImportCheckpointf("checkpoint has trailing JSON")
		}
		return nil, 0, false,
			invalidImportCheckpointf("decoding trailing content: %v", err)
	}
	if !versionSeen {
		return nil, 0, false,
			invalidImportCheckpointf("version is missing")
	}
	if !future && seen != importCheckpointCurrentFields {
		return nil, 0, false, invalidImportCheckpointf(
			"current-version fields are not exact",
		)
	}
	return fields, version, future, nil
}

func importCheckpointField(key string) uint8 {
	switch key {
	case "origin":
		return importCheckpointOriginField
	case "seq":
		return importCheckpointSequenceField
	case "sessions":
		return importCheckpointSessionsField
	case "v":
		return importCheckpointVersionField
	default:
		return 0
	}
}

func skipImportJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		for decoder.More() {
			if _, err := decoder.Token(); err != nil {
				return err
			}
			if err := skipImportJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := skipImportJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return errors.New("JSON value starts with a closing delimiter")
	}
	_, err = decoder.Token()
	return err
}

func streamImportCheckpointSessions(
	data json.RawMessage,
	origin string,
	pageSize int,
	consume func([]importCheckpointSession) error,
) (int, error) {
	if pageSize < 1 {
		return 0, errors.New("checkpoint session page size must be positive")
	}
	if consume == nil {
		return 0, errors.New("checkpoint session page consumer is required")
	}
	var offset int64
	count := 0
	for {
		page, nextOffset, done, err := decodeImportCheckpointSessionPage(
			data, origin, offset, pageSize,
		)
		if err != nil {
			return 0, err
		}
		if err := consume(page); err != nil {
			return 0, err
		}
		count += len(page)
		offset = nextOffset
		if done {
			return count, nil
		}
	}
}

func decodeImportCheckpointSessionPage(
	data json.RawMessage,
	origin string,
	offset int64,
	limit int,
) ([]importCheckpointSession, int64, bool, error) {
	if limit < 1 {
		return nil, 0, false,
			errors.New("checkpoint session page limit must be positive")
	}
	if offset < 0 || offset >= int64(len(data)) {
		return nil, 0, false,
			invalidImportCheckpointf("sessions decode cursor is invalid")
	}

	input := []byte(data)
	var inputReader io.Reader = bytes.NewReader(input)
	base := int64(0)
	if offset > 0 {
		next := skipJSONWhitespace(input, int(offset))
		if next >= len(input) {
			return nil, 0, false,
				invalidImportCheckpointf("sessions object is incomplete")
		}
		if input[next] == '}' {
			if !onlyJSONWhitespace(input[next+1:]) {
				return nil, 0, false,
					invalidImportCheckpointf("sessions has trailing JSON")
			}
			return nil, int64(len(input)), true, nil
		}
		if input[next] != ',' {
			return nil, 0, false,
				invalidImportCheckpointf("sessions decode cursor is invalid")
		}
		base = int64(next + 1)
		inputReader = io.MultiReader(
			strings.NewReader("{"), bytes.NewReader(input[next+1:]),
		)
	}

	decoder := json.NewDecoder(inputReader)
	token, err := decoder.Token()
	if err != nil {
		return nil, 0, false,
			invalidImportCheckpointf("decoding sessions: %v", err)
	}
	if token != json.Delim('{') {
		return nil, 0, false,
			invalidImportCheckpointf("sessions must be an object")
	}
	page := make([]importCheckpointSession, 0, limit)
	for len(page) < limit && decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, 0, false,
				invalidImportCheckpointf("decoding session GID: %v", err)
		}
		gid, ok := token.(string)
		if !ok {
			return nil, 0, false,
				invalidImportCheckpointf("session GID is invalid")
		}
		if !strings.HasPrefix(gid, origin+"~") || len(gid) == len(origin)+1 {
			return nil, 0, false,
				invalidImportCheckpointf("session GID %q is invalid", gid)
		}
		var manifestHash string
		if err := decoder.Decode(&manifestHash); err != nil {
			return nil, 0, false, invalidImportCheckpointf(
				"manifest hash for %q is invalid: %v", gid, err,
			)
		}
		if err := validateHashHex(manifestHash); err != nil {
			return nil, 0, false, invalidImportCheckpointf(
				"manifest hash for %q is invalid: %v", gid, err,
			)
		}
		page = append(page, importCheckpointSession{
			GID: gid, ManifestHash: manifestHash,
		})
	}
	relativeOffset := decoder.InputOffset()
	nextOffset := relativeOffset
	if offset > 0 {
		nextOffset = base + relativeOffset - 1
	}
	next := skipJSONWhitespace(data, int(nextOffset))
	if next < len(data) && data[next] == ',' {
		return page, nextOffset, false, nil
	}
	if next >= len(data) || data[next] != '}' ||
		!onlyJSONWhitespace(data[next+1:]) {
		return nil, 0, false,
			invalidImportCheckpointf("sessions object is incomplete")
	}
	return page, int64(len(data)), true, nil
}

func skipJSONWhitespace(data []byte, start int) int {
	for start < len(data) {
		switch data[start] {
		case ' ', '\t', '\r', '\n':
			start++
		default:
			return start
		}
	}
	return start
}

func onlyJSONWhitespace(data []byte) bool {
	return skipJSONWhitespace(data, 0) == len(data)
}

func invalidImportCheckpointf(format string, args ...any) error {
	return fmt.Errorf("%w: checkpoint %s", ErrArtifactInvalid, fmt.Sprintf(format, args...))
}
