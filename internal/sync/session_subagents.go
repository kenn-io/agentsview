package sync

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

var subagentTranscriptPaths = parser.ClaudeSubagentTranscriptPaths

// SyncSessionWithSubagentsContext refreshes one session and the candidate
// Claude subagent transcripts in its root tree. Child discovery still runs
// when the requested-session refresh fails so callers can retain archived data
// while ingesting any descendants that remain available.
func (e *Engine) SyncSessionWithSubagentsContext(
	ctx context.Context,
	sessionID string,
) error {
	parentErr := e.SyncSingleSessionContext(ctx, sessionID)

	parentAgent := parser.AgentClaude
	if def, ok := parser.AgentByPrefix(sessionID); ok {
		parentAgent = def.Type
	}
	parent, lookupErr := e.db.GetSessionFull(ctx, sessionID)
	if lookupErr != nil {
		return errors.Join(parentErr, fmt.Errorf(
			"load parent session for subagent sync: %w", lookupErr,
		))
	}
	sourcePath := ""
	if parent != nil {
		parentAgent = parser.AgentType(parent.Agent)
		if parent.FilePath != nil {
			sourcePath = *parent.FilePath
		}
	}
	if sourcePath == "" {
		sourcePath = e.FindSourceFile(sessionID)
	}
	paths := subagentTranscriptPaths(sourcePath)
	if len(paths) == 0 {
		return parentErr
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
