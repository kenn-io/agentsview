package ingest

import (
	"bytes"
	"context"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/timeutil"
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
	if candidate.Parsed.Session.KeepStoredGroupKey {
		candidate.Session.GroupKey = prior.Session.GroupKey
		result.Candidate = candidate
		result.PriorContributed = true
	}
	agent := candidate.Parsed.Session.Agent
	if agent == "" {
		agent = parser.AgentType(candidate.Session.Agent)
	}
	// Icodemate uses OpenCode storage for its VSCode-extension path and the
	// shared Claude transcript layout for its CLI path, so an icodemate
	// candidate can match both predicates; the file path (not the agent
	// alone) decides which family this candidate belongs to.
	if IsClaudeFormatTranscript(agent, candidate.Parsed.Session.File.Path) {
		return reconcileClaudePersistedToolResults(
			ctx, candidate, *prior, options, result,
		)
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
	case parser.AgentRooCode, parser.AgentKiloLegacy, parser.AgentCline:
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
		merged := dropToolResultImagesIfPolicy(decision.merged, options)
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

// toolResultEventIdentity ties a current tool_result event to its archived
// counterpart. ToolUseID alone can repeat across a resumed session's
// retries and branch reorders, so Source and Timestamp must agree too.
type toolResultEventIdentity struct {
	toolUseID string
	source    string
	timestamp string
}

// reconcileClaudePersistedToolResults restores, from the archived session,
// any current tool_result event whose persisted-output side file could not
// be resolved while parsing (parser.ParsedToolResult.PersistedOutputUnresolved).
// A flagged current event is matched to its one archived counterpart by
// ToolUseID, Source, and Timestamp all agreeing — never by array position —
// so an incremental dedup collapse or a retry/branch reorder that shifts
// position can never pair the wrong occurrence; no unique match leaves the
// event untouched. A later, unflagged occurrence of the same tool_use_id is
// never touched, so a correction already in the transcript stays the one
// the call's summary reflects.
func reconcileClaudePersistedToolResults(
	ctx context.Context, candidate Candidate, prior PriorSession,
	options ContentOptions, result HistoryResult,
) (HistoryResult, error) {
	flagged := flaggedPersistedToolResultIdentities(candidate.Parsed.Messages)
	if len(flagged) == 0 {
		return result, ctx.Err()
	}
	archived := archivedToolResultEventsByIdentity(prior.Messages)
	changed := false
	for mi := range candidate.Messages {
		for ci := range candidate.Messages[mi].ToolCalls {
			call := &candidate.Messages[mi].ToolCalls[ci]
			for ei := range call.ResultEvents {
				if err := ctx.Err(); err != nil {
					return HistoryResult{}, err
				}
				event := &call.ResultEvents[ei]
				key := toolResultEventIdentity{
					toolUseID: event.ToolUseID, source: event.Source,
					timestamp: event.Timestamp,
				}
				if !flagged[key] {
					continue
				}
				matches := archived[key]
				if len(matches) != 1 {
					continue
				}
				if restoreClaudeToolResultEvent(event, matches[0]) {
					changed = true
				}
			}
		}
	}
	if !changed {
		return result, ctx.Err()
	}
	// A restored event can reintroduce image bytes the live policy drops, the
	// same risk VSCopilot's own merge above guards against, so reapply the
	// policy here too before anything downstream reads the restored content.
	candidate.Messages = dropToolResultImagesIfPolicy(candidate.Messages, options)
	if err := PairToolResultEventSummariesContext(
		ctx, candidate.Messages, options.BlockedResultCategories,
	); err != nil {
		return HistoryResult{}, err
	}
	result.Candidate = candidate
	result.Action = HistoryMerge
	result.PriorContributed = true
	return result, ctx.Err()
}

// restoreClaudeToolResultEvent writes archived's content onto event when
// archived held real text, or only its length (clearing event's own
// placeholder content first) when archived held a length without text, so a
// text-dropping archive policy's own later blanking can't disagree with what
// is restored. It reports whether anything changed.
func restoreClaudeToolResultEvent(
	event *db.ToolResultEvent, archived db.ToolResultEvent,
) bool {
	if archived.Content != "" {
		if event.Content == archived.Content &&
			event.ContentLength == archived.ContentLength {
			return false
		}
		event.Content = archived.Content
		event.ContentLength = archived.ContentLength
		event.RawContentDigest = archived.RawContentDigest
		return true
	}
	if archived.ContentLength <= 0 {
		return false
	}
	participatesBefore := event.SummaryParticipates != nil && *event.SummaryParticipates
	changed := event.Content != "" || event.ContentLength != archived.ContentLength ||
		!bytes.Equal(event.RawContentDigest, archived.RawContentDigest) ||
		!participatesBefore
	event.Content = ""
	event.ContentLength = archived.ContentLength
	event.RawContentDigest = archived.RawContentDigest
	// Mark this event as the authoritative latest state explicitly, even
	// though it carries no text: every consumer that decides which event
	// drives a call's summary checks this field, not the shape of Content,
	// so a withheld correction is never mistaken for an ordinary empty
	// event or confused with an unrelated event's own dedup eligibility.
	event.SummaryParticipates = new(true)
	return changed
}

// flaggedPersistedToolResultIdentities returns the identity of every
// current tool_result whose parser flag is set.
func flaggedPersistedToolResultIdentities(
	messages []parser.ParsedMessage,
) map[toolResultEventIdentity]bool {
	var flagged map[toolResultEventIdentity]bool
	for _, message := range messages {
		for _, toolResult := range message.ToolResults {
			if !toolResult.PersistedOutputUnresolved {
				continue
			}
			if flagged == nil {
				flagged = make(map[toolResultEventIdentity]bool)
			}
			flagged[toolResultEventIdentity{
				toolUseID: toolResult.ToolUseID, source: toolResult.Source,
				timestamp: timeutil.Format(message.Timestamp),
			}] = true
		}
	}
	return flagged
}

// archivedToolResultEventsByIdentity indexes every archived tool_result
// event by identity. A repeated identity lists every match, so a caller can
// require the match to be unique before restoring from it.
func archivedToolResultEventsByIdentity(
	messages []db.Message,
) map[toolResultEventIdentity][]db.ToolResultEvent {
	var archived map[toolResultEventIdentity][]db.ToolResultEvent
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			for _, event := range call.ResultEvents {
				if archived == nil {
					archived = make(map[toolResultEventIdentity][]db.ToolResultEvent)
				}
				key := toolResultEventIdentity{
					toolUseID: event.ToolUseID, source: event.Source,
					timestamp: event.Timestamp,
				}
				archived[key] = append(archived[key], event)
			}
		}
	}
	return archived
}
