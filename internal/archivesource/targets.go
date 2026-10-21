package archivesource

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"slices"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func completionTarget(source Source, cfg config.Config) (string, error) {
	content, err := config.ParseArchiveContent(string(cfg.ArchiveContent))
	if err != nil {
		return "", err
	}
	images, err := config.ParseToolResultImages(string(cfg.ToolResultImages))
	if err != nil {
		return "", err
	}
	if source.Root.Provider != "claude" && source.Root.Provider != "codex" {
		return "", errors.New("unsupported archive source provider")
	}
	type agentVersion struct {
		Agent   parser.AgentType
		Version int
	}
	agents := parser.ProviderStoredAgents(parser.AgentType(source.Root.Provider))
	slices.Sort(agents)
	versions := make([]agentVersion, 0, len(agents))
	for _, agent := range agents {
		versions = append(versions, agentVersion{agent, db.AgentDataVersion(agent)})
	}
	blocked := slices.Clone(cfg.ResultContentBlockedCategories)
	slices.Sort(blocked)
	blocked = slices.Compact(blocked)
	data, err := json.Marshal(struct {
		Manifest string
		Versions []agentVersion
		Content  config.ArchiveContent
		Images   config.ToolResultImages
		Blocked  []string
	}{source.ManifestID, versions, content, images, blocked})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}
