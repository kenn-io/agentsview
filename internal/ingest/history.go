package ingest

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// HistoryAction describes how a candidate relates to its same-source prior
// projection.
type HistoryAction uint8

const (
	HistoryReplace HistoryAction = iota
	HistoryPreserve
	HistoryMerge
)

// PriorSession is the normalized state previously accepted from this exact
// source member. Callers own loading it and surfacing any read error.
type PriorSession struct {
	Session  db.Session
	Messages []db.Message
}

// HistoryResult contains the effective rows and the decision a storage caller
// must apply. PriorContributed tells provenance writers to retain the prior
// accepted source generation.
type HistoryResult struct {
	Candidate        Candidate
	Action           HistoryAction
	PriorContributed bool
}

// ReconcileProviderHistory applies provider-specific archive preservation to
// explicitly supplied current and same-source prior rows. It performs no
// storage or filesystem reads.
func ReconcileProviderHistory(
	ctx context.Context,
	candidate Candidate,
	prior *PriorSession,
	options ContentOptions,
) (HistoryResult, error) {
	result := HistoryResult{Candidate: candidate, Action: HistoryReplace}
	if err := ctx.Err(); err != nil || prior == nil {
		return result, err
	}
	// Apply source-owned write intent before finalization and content hashing.
	// Immutable projection rows cannot inherit these fields through an upsert.
	if candidate.Session.PreserveSessionName {
		candidate.Session.SessionName = prior.Session.SessionName
		result.Candidate = candidate
		result.PriorContributed = true
	}
	agent := candidate.Parsed.Session.Agent
	if agent == "" {
		agent = parser.AgentType(candidate.Session.Agent)
	}
	if IsOpenCodeFormatStorageAgent(agent) {
		if shouldPreserveOpenCodeFormatArchive(
			agent, candidate, *prior, options,
		) {
			result.Candidate.Session = prior.Session
			result.Candidate.Messages = prior.Messages
			result.Action = HistoryPreserve
			result.PriorContributed = true
		}
		return result, ctx.Err()
	}

	switch agent {
	case parser.AgentRooCode, parser.AgentKiloLegacy:
		if len(candidate.Messages) == 0 && len(prior.Messages) > 0 {
			result.Candidate.Session = prior.Session
			result.Candidate.Messages = prior.Messages
			result.Action = HistoryPreserve
			result.PriorContributed = true
		}
		return result, ctx.Err()
	case parser.AgentVSCopilot:
		decision := visualStudioCopilotArchiveDecision(
			candidate.Messages, prior.Messages,
		)
		if decision.preserve {
			decision.merged = prior.Messages
		}
		if decision.merged == nil {
			return result, ctx.Err()
		}
		parsedMessages := candidate.Messages
		merged, _ := db.ProjectToolResultImages(
			decision.merged, options.ToolResultImages,
		)
		candidate.Messages = merged
		applyVisualStudioCopilotArchiveSessionFields(
			&candidate.Session, &prior.Session, parsedMessages, merged,
		)
		if err := ApplySessionMessageDerivedFieldsContext(
			ctx, &candidate.Session, merged,
			candidate.Parsed.Session.CountsAuthoritative,
		); err != nil {
			return HistoryResult{}, err
		}
		if err := ApplySessionTokenTotalsFromMessagesContext(
			ctx, &candidate.Session, merged,
		); err != nil {
			return HistoryResult{}, err
		}
		result.Candidate = candidate
		result.Action = HistoryMerge
		result.PriorContributed = true
		return result, ctx.Err()
	default:
		return result, ctx.Err()
	}
}
