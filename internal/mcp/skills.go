package mcp

import (
	"fmt"

	"go.kenn.io/agentsview/internal/skills"
	"go.kenn.io/kit/mcpskills"
)

const ToolSkills = "agentsview.skills"

var bundledSkills = mustBundledSkills()

func mustBundledSkills() *mcpskills.Catalog {
	markdown, err := skills.MCPGuide()
	if err != nil {
		panic(err)
	}
	catalog, err := mcpskills.New(mcpskills.Config{
		ToolName: ToolSkills,
		Guides: []mcpskills.Guide{{
			Entry: mcpskills.Entry{
				Name:    "agentsview-finding-history",
				Title:   "Finding conversation history",
				Summary: "Search prior conversations and read source messages before drawing conclusions.",
			},
			Markdown: markdown,
		}},
	})
	if err != nil {
		panic(fmt.Errorf("MCP skills catalog: %w", err))
	}
	return catalog
}
