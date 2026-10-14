// Package docs embeds the public product guides so the MCP server can search
// them offline.
package docs

import "embed"

// Public holds only guides published under https://agentsview.io/docs/.
// embed_test.go keeps this list in step with the top-level guides.
//
//go:embed activity.md artifact-sync.md chat-import.md clickhouse-sync.md commands.md configuration.md conversation-export.md data.md duckdb.md filesystem-sync.md hosted-raw-sync.md index.md mcp.md one-shot-capture.md pg-sync.md quality.md quickstart.md recall.md recent-edits.md remote-access.md reporting-export.md semantic-search-internals.md semantic-search.md session-api.md session-export.md session-intelligence.md stats.md token-usage.md usage.md
var Public embed.FS
