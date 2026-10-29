package testjsonl

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
)

// SolidPNGBase64 returns a base64 PNG of one solid color, a synthetic
// stand-in for a generated image.
func SolidPNGBase64(width, height int, fill color.RGBA) string {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.SetRGBA(x, y, fill)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// CodexImageGenerationCallJSON returns a Codex image_generation_call
// response_item. result is the base64 image.
func CodexImageGenerationCallJSON(
	id, revisedPrompt, result, timestamp string,
) string {
	return mustMarshal(map[string]any{
		"type":      "response_item",
		"timestamp": timestamp,
		"payload": map[string]any{
			"type":           "image_generation_call",
			"id":             id,
			"status":         "completed",
			"revised_prompt": revisedPrompt,
			"result":         result,
		},
	})
}

// CodexImageGenerationEndJSON returns the image_generation_end event Codex
// writes after the matching response item.
func CodexImageGenerationEndJSON(
	callID, revisedPrompt, result, savedPath, timestamp string,
) string {
	return mustMarshal(map[string]any{
		"type":      "event_msg",
		"timestamp": timestamp,
		"payload": map[string]any{
			"type":           "image_generation_end",
			"call_id":        callID,
			"status":         "completed",
			"revised_prompt": revisedPrompt,
			"result":         result,
			"saved_path":     savedPath,
		},
	})
}
