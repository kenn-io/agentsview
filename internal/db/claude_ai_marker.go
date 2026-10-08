package db

import (
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/config"
)

// ClaudeAIMarkerVersion changes when Claude.ai parser output changes.
const ClaudeAIMarkerVersion = 1

func claudeAIMarkerPrefix() string {
	return "claude-ai:v" + strconv.Itoa(ClaudeAIMarkerVersion) + ":"
}

// ClaudeAIMarker records the leaf and effective content retention.
func ClaudeAIMarker(policy config.ArchiveContent, leaf string) string {
	if policy != config.ArchiveContentUsage {
		policy = config.ArchiveContentFull
	}
	return claudeAIMarkerPrefix() + string(policy) + ":" + leaf
}

// ParseClaudeAIMarkerVersion reads the version without depending on its payload format.
func ParseClaudeAIMarkerVersion(marker string) int {
	version, _, ok := strings.Cut(strings.TrimPrefix(marker, "claude-ai:v"), ":")
	if !ok || !strings.HasPrefix(marker, "claude-ai:v") {
		return 0
	}
	n, err := strconv.Atoi(version)
	if err != nil {
		return 0
	}
	return n
}
