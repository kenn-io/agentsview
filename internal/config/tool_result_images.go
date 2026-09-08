package config

import (
	"fmt"
	"strings"
)

// ToolResultImages controls whether supported inline image blocks remain in
// stored tool results.
type ToolResultImages string

const (
	ToolResultImagesKeep ToolResultImages = ""
	ToolResultImagesDrop ToolResultImages = "drop"
)

// ParseToolResultImages accepts the two persisted policy values. An empty
// value means the default keep policy.
func ParseToolResultImages(value string) (ToolResultImages, error) {
	switch value = strings.ToLower(strings.TrimSpace(value)); ToolResultImages(value) {
	case "":
		return ToolResultImagesKeep, nil
	case "keep":
		return ToolResultImagesKeep, nil
	case "drop":
		return ToolResultImagesDrop, nil
	default:
		return "", fmt.Errorf(
			`tool_result_images must be "keep" or "drop" (got %q)`, value,
		)
	}
}
