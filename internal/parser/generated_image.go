package parser

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"

	"go.kenn.io/agentsview/internal/assets"
)

// generatedImageCallName is the tool name for an image a Codex model
// generated, in local rollouts and Codex Cloud alike.
const generatedImageCallName = "image_generation"

// generatedImageResult turns a base64 image payload into a tool result
// holding one input_image block, the shape the archive's image policy and
// the session viewer read. It reports false unless the payload is strict
// base64 of a PNG, JPEG, WebP, or GIF image.
func generatedImageResult(payload string) (string, bool) {
	decoder := base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(payload))
	head := make([]byte, 512)
	n, err := io.ReadFull(decoder, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", false
	}
	mediaType := http.DetectContentType(head[:n])
	if _, ok := assets.ExtForMediaType(mediaType); !ok {
		return "", false
	}
	if _, err := io.Copy(io.Discard, decoder); err != nil {
		return "", false
	}
	block, err := json.Marshal([]map[string]string{{
		"type":      "input_image",
		"image_url": "data:" + mediaType + ";base64," + payload,
	}}, json.Deterministic(true))
	if err != nil {
		return "", false
	}
	return string(block), true
}

// jsonWithoutField returns a JSON object without one top-level field, so a
// call's input can keep every field except a payload moved to its result.
func jsonWithoutField(raw jsontext.Value, field string) (string, bool) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return "", false
	}
	delete(fields, field)
	trimmed, err := json.Marshal(fields, json.Deterministic(true))
	if err != nil {
		return "", false
	}
	return string(trimmed), true
}
