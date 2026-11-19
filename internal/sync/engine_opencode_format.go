package sync

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

func isOpenCodeFormatStorageAgent(agent parser.AgentType) bool {
	return ingest.IsOpenCodeFormatStorageAgent(agent)
}

func openCodeFormatDBName(agent parser.AgentType) string {
	return ingest.OpenCodeFormatDBName(agent)
}

func resolveOpenCodeFormatSource(
	agent parser.AgentType, dir string,
) parser.OpenCodeSource {
	switch agent {
	case parser.AgentOpenCode:
		return parser.ResolveOpenCodeSource(dir)
	case parser.AgentKilo:
		return parser.ResolveKiloSource(dir)
	case parser.AgentMiMoCode:
		return parser.ResolveMiMoCodeSource(dir)
	case parser.AgentIcodemate:
		return parser.ResolveIcodemateSource(dir)
	default:
		return parser.OpenCodeSource{}
	}
}

func openCodeFormatSourceMtime(ctx context.Context,
	agent parser.AgentType, path string,
) (int64, error) {
	switch agent {
	case parser.AgentOpenCode:
		return parser.OpenCodeSourceMtime(ctx, path)
	case parser.AgentKilo:
		return parser.KiloSourceMtime(ctx, path)
	case parser.AgentMiMoCode:
		return parser.MiMoCodeSourceMtime(ctx, path)
	case parser.AgentIcodemate:
		return parser.IcodemateSourceMtime(ctx, path)
	default:
		return 0, fmt.Errorf("unknown OpenCode-format agent: %s", agent)
	}
}

func isOpenCodeFormatStoragePath(
	agent parser.AgentType, path string,
) bool {
	return ingest.IsOpenCodeFormatStoragePath(agent, path)
}

func isOpenCodeFormatContainerSource(
	agent parser.AgentType, path string,
) bool {
	return ingest.IsOpenCodeFormatContainerSource(agent, path)
}

func isOpenCodeFormatSQLiteVirtualPath(
	agent parser.AgentType, path string,
) bool {
	return ingest.IsOpenCodeFormatSQLiteVirtualPath(agent, path)
}

func openCodeLegacyArchiveLooksIncomplete(
	parsed, stored []db.Message,
) bool {
	return ingest.OpenCodeLegacyArchiveLooksIncomplete(parsed, stored)
}

func openCodeUsageOnlyArchiveLooksIncomplete(
	parsed, stored []db.Message,
) bool {
	return ingest.OpenCodeUsageOnlyArchiveLooksIncomplete(parsed, stored)
}
