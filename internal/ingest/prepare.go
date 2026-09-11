// Package ingest converts parser output into normalized database rows without
// requiring a storage handle.
package ingest

import (
	"context"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// ContentOptions defines the content policies applied during preparation.
type ContentOptions struct {
	BlockedResultCategories map[string]bool
	ToolResultImages        config.ToolResultImages
	ArchiveContent          config.ArchiveContent
}

// Candidate is normalized parser output before caller-owned identity,
// attribution, filtering, and provider-history reconciliation.
type Candidate struct {
	Session  db.Session
	Messages []db.Message
	Parsed   parser.ParseResult
}

// PreparedSession is the complete normalized content graph ready for storage.
type PreparedSession struct {
	Session     db.Session
	Messages    []db.Message
	UsageEvents []db.UsageEvent
	Signals     db.SessionSignalUpdate
	Findings    []db.SecretFinding
	Validation  db.ValidationStats
}

// PrepareCandidate converts and pairs parser output before caller-owned
// reconciliation changes the final identity or retained transcript.
func PrepareCandidate(
	ctx context.Context, parsed parser.ParseResult, options ContentOptions,
) (Candidate, error) {
	messages, err := ConvertMessagesContext(
		ctx, parsed.Session.ID, parsed.Session.Agent, parsed.Messages,
		options.BlockedResultCategories,
	)
	if err != nil {
		return Candidate{}, err
	}
	messages, _ = db.ProjectToolResultImages(messages, options.ToolResultImages)
	session, err := ConvertSessionContext(ctx, parsed.Session, parsed.Messages)
	if err != nil {
		return Candidate{}, err
	}
	if err := ApplySessionMessageDerivedFieldsContext(
		ctx, &session, messages, parsed.Session.CountsAuthoritative,
	); err != nil {
		return Candidate{}, err
	}
	return Candidate{
		Session: session, Messages: messages, Parsed: parsed,
	}, ctx.Err()
}

// Finalize applies post-reconciliation validation, aggregate repair, storage
// projection, usage conversion, and content-derived state.
func Finalize(
	ctx context.Context, candidate Candidate, options ContentOptions,
) (PreparedSession, error) {
	return finalize(ctx, candidate, options, true)
}

// FinalizeRows performs final row normalization without derived signals or
// findings. Local staged and explicitly disabled signal paths use it when a
// later transaction-owned computation is required or derivation is disabled.
func FinalizeRows(
	ctx context.Context, candidate Candidate, options ContentOptions,
) (PreparedSession, error) {
	return finalize(ctx, candidate, options, false)
}

func finalize(
	ctx context.Context, candidate Candidate, options ContentOptions,
	derive bool,
) (PreparedSession, error) {
	session := candidate.Session
	messages := candidate.Messages
	if err := ApplySessionMessageDerivedFieldsContext(
		ctx, &session, messages,
		candidate.Parsed.Session.CountsAuthoritative,
	); err != nil {
		return PreparedSession{}, err
	}

	msgTotal, msgHasOut, msgPeak, msgHasCtx, err :=
		MessageTokenTotalsContext(ctx, messages)
	if err != nil {
		return PreparedSession{}, err
	}
	evtTotal, evtHasOut, evtPeak, evtHasCtx, err :=
		UsageEventTokenTotalsContext(ctx, candidate.Parsed.UsageEvents, false)
	if err != nil {
		return PreparedSession{}, err
	}
	totalFromMessages := session.HasTotalOutputTokens == msgHasOut &&
		session.TotalOutputTokens == msgTotal
	totalFromEvents := session.HasTotalOutputTokens == evtHasOut &&
		session.TotalOutputTokens == evtTotal
	peakFromMessages := session.HasPeakContextTokens == msgHasCtx &&
		session.PeakContextTokens == msgPeak
	peakFromEvents := session.HasPeakContextTokens == evtHasCtx &&
		session.PeakContextTokens == evtPeak

	validation, err := db.ValidateAndSanitizeContext(
		ctx, &session, messages, nil,
	)
	if err != nil {
		return PreparedSession{}, err
	}
	if totalFromMessages {
		session.TotalOutputTokens, session.HasTotalOutputTokens,
			_, _, err = MessageTokenTotalsContext(ctx, messages)
	} else if totalFromEvents {
		session.TotalOutputTokens, session.HasTotalOutputTokens,
			_, _, err = UsageEventTokenTotalsContext(
			ctx, candidate.Parsed.UsageEvents, true,
		)
	}
	if err != nil {
		return PreparedSession{}, err
	}
	if peakFromMessages {
		_, _, session.PeakContextTokens, session.HasPeakContextTokens,
			err = MessageTokenTotalsContext(ctx, messages)
	} else if peakFromEvents {
		_, _, session.PeakContextTokens, session.HasPeakContextTokens,
			err = UsageEventTokenTotalsContext(
			ctx, candidate.Parsed.UsageEvents, true,
		)
	}
	if err != nil {
		return PreparedSession{}, err
	}

	usageEvents, usageValidation, err := ConvertUsageEventsContext(
		ctx, session.ID, candidate.Parsed.UsageEvents,
	)
	if err != nil {
		return PreparedSession{}, err
	}
	validation = addValidationStats(validation, usageValidation)
	session, messages = db.ProjectSessionForStoragePolicy(
		session, messages, options.ArchiveContent,
	)
	var signalUpdate db.SessionSignalUpdate
	var findings []db.SecretFinding
	if derive {
		signalUpdate, findings = ComputeSignalsAndSecrets(session, messages)
	}
	return PreparedSession{
		Session: session, Messages: messages, UsageEvents: usageEvents,
		Signals: signalUpdate, Findings: findings, Validation: validation,
	}, ctx.Err()
}

// ApplySessionMessageDerivedFieldsContext derives filtered counts and
// automation from the reconciled transcript.
func ApplySessionMessageDerivedFieldsContext(
	ctx context.Context, session *db.Session, messages []db.Message,
	countsAuthoritative bool,
) error {
	if !countsAuthoritative {
		var user int
		for _, message := range messages {
			if err := ctx.Err(); err != nil {
				return err
			}
			if message.Role == "user" && !message.IsSystem &&
				message.SourceSubtype != parser.SourceSubtypeToolResult {
				user++
			}
		}
		session.MessageCount = len(messages)
		session.UserMessageCount = user
	}
	session.IsAutomated = db.IsAutomatedSessionMetadata(
		session.Agent, session.SessionKind,
	) || db.IsAutomatedTranscript(
		session.UserMessageCount, messages, session.FirstMessage,
	)
	return ctx.Err()
}

// MessageTokenTotalsContext returns row-derived output and context totals.
func MessageTokenTotalsContext(
	ctx context.Context, messages []db.Message,
) (totalOutput int, hasOutput bool, peakContext int, hasContext bool, err error) {
	for _, message := range messages {
		if err = ctx.Err(); err != nil {
			return
		}
		if message.HasOutputTokens {
			hasOutput = true
			totalOutput += message.OutputTokens
		}
		if message.HasContextTokens {
			hasContext = true
			if message.ContextTokens > peakContext {
				peakContext = message.ContextTokens
			}
		}
	}
	err = ctx.Err()
	return
}

// ApplySessionTokenTotalsFromMessagesContext replaces session aggregates with
// totals derived from the supplied normalized message rows.
func ApplySessionTokenTotalsFromMessagesContext(
	ctx context.Context, session *db.Session, messages []db.Message,
) error {
	totalOutput, hasOutput, peakContext, hasContext, err :=
		MessageTokenTotalsContext(ctx, messages)
	if err != nil {
		return err
	}
	session.TotalOutputTokens = totalOutput
	session.HasTotalOutputTokens = hasOutput
	session.PeakContextTokens = peakContext
	session.HasPeakContextTokens = hasContext
	return nil
}

// UsageEventTokenTotalsContext returns per-turn usage-derived totals, excluding
// authoritative session summaries.
func UsageEventTokenTotalsContext(
	ctx context.Context, events []parser.ParsedUsageEvent, clamp bool,
) (totalOutput int, hasOutput bool, peakContext int, hasContext bool, err error) {
	rolled := make([]parser.ParsedUsageEvent, 0, len(events))
	for _, event := range events {
		if err = ctx.Err(); err != nil {
			return
		}
		if event.Source == "session" {
			continue
		}
		if clamp {
			event.InputTokens = db.ClampParsedTokens(event.InputTokens)
			event.OutputTokens = db.ClampParsedTokens(event.OutputTokens)
			event.CacheCreationInputTokens = db.ClampParsedTokens(
				event.CacheCreationInputTokens,
			)
			event.CacheReadInputTokens = db.ClampParsedTokens(
				event.CacheReadInputTokens,
			)
		}
		rolled = append(rolled, event)
	}
	return parser.UsageEventTokenAggregateContext(ctx, rolled)
}

func addValidationStats(a, b db.ValidationStats) db.ValidationStats {
	a.ControlCharsStripped += b.ControlCharsStripped
	a.ModelClamped += b.ModelClamped
	a.TokensClamped += b.TokensClamped
	a.RoleCoerced += b.RoleCoerced
	a.TimestampsBlanked += b.TimestampsBlanked
	return a
}
