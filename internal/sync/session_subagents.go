package sync

import (
	"context"
	"errors"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// SyncSessionWithSubagentsContext refreshes one session and the candidate
// Claude subagent transcripts in its root tree. Child discovery still runs
// when the requested-session refresh fails so callers can retain archived data
// while ingesting any descendants that remain available.
func (e *Engine) SyncSessionWithSubagentsContext(
	ctx context.Context,
	sessionID string,
) error {
	parentErr := e.SyncSingleSessionContext(ctx, sessionID)

	sourcePath := e.db.GetSessionFilePath(sessionID)
	if sourcePath == "" {
		sourcePath = e.FindSourceFile(sessionID)
	}
	paths := parser.ClaudeSubagentTranscriptPaths(sourcePath)
	if len(paths) == 0 {
		return parentErr
	}

	parentAgent := parser.AgentClaude
	if parent, _ := e.db.GetSession(ctx, sessionID); parent != nil {
		parentAgent = parser.AgentType(parent.Agent)
	}
	var subagentErr error
	if strings.HasPrefix(sourcePath, "s3://") {
		subagentErr = e.SyncS3SubagentTranscriptsContext(
			ctx, sessionID, parentAgent, paths)
	} else {
		subagentErr = e.SyncPathsContext(ctx, paths)
	}
	return errors.Join(parentErr, subagentErr)
}
