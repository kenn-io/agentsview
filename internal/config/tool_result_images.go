package config

import (
	"fmt"
	"strings"
)

// ToolResultImages controls supported inline tool-result image retention and
// typed image/document evidence captured from supported transcript blocks.
type ToolResultImages string

const (
	ToolResultImagesKeep    ToolResultImages = ""
	ToolResultImagesDrop    ToolResultImages = "drop"
	ToolResultImagesOffload ToolResultImages = "offload"
)

// ParseToolResultImages accepts the persisted policy values. An empty
// value means the default keep policy.
func ParseToolResultImages(value string) (ToolResultImages, error) {
	switch value = strings.ToLower(strings.TrimSpace(value)); ToolResultImages(value) {
	case "":
		return ToolResultImagesKeep, nil
	case "keep":
		return ToolResultImagesKeep, nil
	case "drop", "offload":
		return ToolResultImages(value), nil
	default:
		return "", fmt.Errorf(
			`tool_result_images must be "keep", "drop", or "offload" (got %q)`, value,
		)
	}
}
