package ingest

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

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
	// SourceToolResults holds tool results archived by the sessions saved
	// from the same source, such as a CodeBuddy conversation and its rewound
	// branches. Persisted tool-result restore uses it only for an identity
	// the session's own rows lack, and only when every other session agrees.
	// It also holds the image links those sessions archived for a converted
	// CodeBuddy conversation's 4.x images, used the same way.
	SourceToolResults *SourceToolResults
	// LoadMessages optionally supplies retained rows only when a history policy
	// needs them. It takes precedence over Messages; callers own read errors
	// and any caching needed across reconciliation and later preservation.
	LoadMessages func(context.Context) ([]db.Message, error)
}

// SourceToolResults indexes the archived tool results that sessions saved
// from one source hold for persisted outputs a sync could not resolve. A
// CodeBuddy rewind moves a call from its conversation to a branch, so the
// branch's own archive lacks the output the conversation saved. Build one
// index per source and share it across that source's sessions; each lookup
// skips the session being reconciled. It indexes, the same way, the image
// links archived for messages that now show a 4.x image as unavailable.
type SourceToolResults struct {
	wanted map[toolResultEventIdentity]bool
	saved  map[toolResultEventIdentity][]savedToolResult
	// wantedImages and images key image links by message source UUID.
	wantedImages map[string]bool
	images       map[string][]savedImageLinks
}

type savedToolResult struct {
	sessionID string
	event     db.ToolResultEvent
}

type savedImageLinks struct {
	sessionID string
	links     []string
}

// Want requests the tool results that messages flag as unresolved persisted
// outputs. Call it for every session that will use the index before Add.
func (s *SourceToolResults) Want(messages []parser.ParsedMessage) {
	for key := range flaggedPersistedToolResultIdentities(messages) {
		if s.wanted == nil {
			s.wanted = make(map[toolResultEventIdentity]bool)
		}
		s.wanted[key] = true
	}
}

// WantImages requests the archived image links of the messages that show an
// image as unavailable. Call it only for a converted CodeBuddy conversation
// whose 4.x directory no longer reads in full.
func (s *SourceToolResults) WantImages(messages []parser.ParsedMessage) {
	for _, m := range messages {
		if m.SourceUUID == "" || !strings.Contains(m.Content, codebuddyImageUnavailable) {
			continue
		}
		if s.wantedImages == nil {
			s.wantedImages = make(map[string]bool)
		}
		s.wantedImages[m.SourceUUID] = true
	}
}

// HasUnavailableImage reports whether any message shows an image as
// unavailable.
func HasUnavailableImage(messages []parser.ParsedMessage) bool {
	return slices.ContainsFunc(messages, func(m parser.ParsedMessage) bool {
		return strings.Contains(m.Content, codebuddyImageUnavailable)
	})
}

// Wants reports whether any tool result or image link was requested.
func (s *SourceToolResults) Wants() bool {
	return len(s.wanted) > 0 || len(s.wantedImages) > 0
}

// Add keeps the requested tool results and image links archived in one
// session's rows. Other rows are not retained, so a caller can read sessions
// one at a time.
func (s *SourceToolResults) Add(sessionID string, messages []db.Message) {
	for _, message := range messages {
		if s.wantedImages[message.SourceUUID] &&
			strings.Contains(message.Content, codebuddyImageLink) {
			if s.images == nil {
				s.images = make(map[string][]savedImageLinks)
			}
			s.images[message.SourceUUID] = append(s.images[message.SourceUUID],
				savedImageLinks{
					sessionID: sessionID,
					links:     codebuddyImageTokenRe.FindAllString(message.Content, -1),
				})
		}
		for _, call := range message.ToolCalls {
			for _, event := range call.ResultEvents {
				key := toolResultEventIdentity{
					toolUseID: event.ToolUseID, source: event.Source,
					timestamp: event.Timestamp,
				}
				if !s.wanted[key] {
					continue
				}
				if s.saved == nil {
					s.saved = make(map[toolResultEventIdentity][]savedToolResult)
				}
				s.saved[key] = append(s.saved[key], savedToolResult{
					sessionID: sessionID, event: event,
				})
			}
		}
	}
}

// Found reports whether any requested tool result or image link was
// archived.
func (s *SourceToolResults) Found() bool {
	return s != nil && (len(s.saved) > 0 || len(s.images) > 0)
}

// agreedImageLinks returns the image tokens the sessions other than
// sessionID archived for the message sourceUUID, when every archived copy
// lists the same ones.
func (s *SourceToolResults) agreedImageLinks(
	sourceUUID, sessionID string,
) ([]string, bool) {
	if s == nil {
		return nil, false
	}
	var first []string
	found := false
	for _, saved := range s.images[sourceUUID] {
		if saved.sessionID == sessionID {
			continue
		}
		if !found {
			first, found = saved.links, true
			continue
		}
		if !slices.Equal(saved.links, first) {
			return nil, false
		}
	}
	return first, found
}

// agreed returns the event the sessions other than sessionID archived for
// key, when every one of them holds the same text.
func (s *SourceToolResults) agreed(
	key toolResultEventIdentity, sessionID string,
) (db.ToolResultEvent, bool) {
	if s == nil {
		return db.ToolResultEvent{}, false
	}
	var first *db.ToolResultEvent
	for i := range s.saved[key] {
		saved := &s.saved[key][i]
		// A saved placeholder holds no text to restore and does not
		// contradict a session that saved the full output.
		if saved.sessionID == sessionID || isPersistedOutputPlaceholder(saved.event.Content) {
			continue
		}
		if first == nil {
			first = &saved.event
			continue
		}
		if saved.event.Content != first.Content ||
			saved.event.ContentLength != first.ContentLength {
			return db.ToolResultEvent{}, false
		}
	}
	if first == nil {
		return db.ToolResultEvent{}, false
	}
	return *first, true
}

// isPersistedOutputPlaceholder reports whether a saved tool result is the
// producer's placeholder for an output written to a side file, rather than
// the output itself.
func isPersistedOutputPlaceholder(content string) bool {
	return strings.HasPrefix(strings.TrimSpace(content), "<persisted-output>")
}

func (p *PriorSession) loadMessages(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.LoadMessages == nil {
		return nil
	}
	messages, err := p.LoadMessages(ctx)
	if err != nil {
		return err
	}
	p.Messages = messages
	return nil
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
// explicitly supplied current and same-source prior rows. Only policies that
// inspect retained content invoke the caller's optional message loader.
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
	// Loading rows must not change the caller's history input.
	previous := *prior
	prior = &previous
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
	if RestoresPersistedToolResults(agent, candidate.Parsed.Session.File.Path) {
		result, err := reconcileClaudePersistedToolResults(
			ctx, candidate, *prior, options, result,
		)
		if err != nil || !candidate.Parsed.Session.LegacyCompanionIncomplete {
			return result, err
		}
		return keepCodeBuddyLegacyCompanionData(ctx, *prior, result)
	}
	if IsOpenCodeFormatStorageAgent(agent) {
		preserve, err := shouldPreserveOpenCodeFormatArchive(
			ctx, agent, candidate, prior, options,
		)
		if err != nil {
			return HistoryResult{}, err
		}
		if preserve {
			result.Candidate.Session = prior.Session
			result.Candidate.Messages = prior.Messages
			result.Action = HistoryPreserve
			result.PriorContributed = true
		}
		return result, ctx.Err()
	}

	switch agent {
	case parser.AgentRooCode, parser.AgentKiloLegacy, parser.AgentCline:
		if len(candidate.Messages) != 0 {
			return result, ctx.Err()
		}
		if err := prior.loadMessages(ctx); err != nil {
			return HistoryResult{}, err
		}
		if len(prior.Messages) > 0 {
			result.Candidate.Session = prior.Session
			result.Candidate.Messages = prior.Messages
			result.Action = HistoryPreserve
			result.PriorContributed = true
		}
		return result, ctx.Err()
	case parser.AgentVSCopilot:
		if err := prior.loadMessages(ctx); err != nil {
			return HistoryResult{}, err
		}
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

// keepCodeBuddyLegacyCompanionData keeps what the archived copy of a
// converted CodeBuddy conversation read from its 4.x directory, which the
// parse could not read: the request totals, under their cb4: identities,
// and the image links that the parse shows as unavailable. A request total
// the parse already carries, such as one the converter copied into the
// transcript, wins. Once the directory reads again the parser stops asking
// for this, so its totals, including zero totals, and its images win.
func keepCodeBuddyLegacyCompanionData(
	ctx context.Context, prior PriorSession, result HistoryResult,
) (HistoryResult, error) {
	if err := prior.loadMessages(ctx); err != nil {
		return HistoryResult{}, err
	}
	candidate := result.Candidate
	messages := slices.Clone(candidate.Messages)
	usage := keepCodeBuddyLegacyUsage(&messages, prior.Messages, candidate.Session.ID)
	images := keepCodeBuddyLegacyImages(messages, prior.Messages,
		prior.SourceToolResults, candidate.Session.ID)
	if !usage && !images {
		return result, ctx.Err()
	}
	candidate.Messages = messages
	if usage {
		if err := ApplySessionMessageDerivedFieldsContext(
			ctx, &candidate.Session, messages,
			candidate.Parsed.Session.CountsAuthoritative,
		); err != nil {
			return HistoryResult{}, err
		}
		if err := ApplySessionTokenTotalsFromMessagesContext(
			ctx, &candidate.Session, messages,
		); err != nil {
			return HistoryResult{}, err
		}
	}
	result.Candidate = candidate
	result.Action = HistoryMerge
	result.PriorContributed = true
	return result, ctx.Err()
}

// keepCodeBuddyLegacyUsage carries each archived 4.x request total whose
// identity the parse lacks. A total the archive stored on a visible reply
// goes back onto that reply, with its cb4: identity, as the parser puts it
// there while the 4.x directory reads, so pins and notes stay on the reply.
// A total whose reply the parse no longer holds, or that the archive stored
// as a hidden usage row, becomes a hidden usage row at its place in time, as
// the parser adds a total whose reply the transcript no longer holds, so the
// request still counts once. The further parts of a total the parser split
// above the per-request bound, <identity>#<n>, are hidden rows and come back
// as hidden rows; when the parse carries the total itself, its own parts win.
func keepCodeBuddyLegacyUsage(
	messages *[]db.Message, archived []db.Message, sessionID string,
) bool {
	present := make(map[string]bool)
	for _, m := range *messages {
		present[m.SourceUUID] = true
	}
	parsed := maps.Clone(present)
	changed, inserted := false, false
	for _, old := range archived {
		// A 4.x request total's input sums every step, so its usage JSON
		// does not mean a context size is known; the archived row's own
		// flags say whether one was. TokenPresence only decides whether
		// the row carries usage at all.
		usageContext, usageOutput := old.TokenPresence()
		hasContext, hasOutput := old.HasContextTokens, old.HasOutputTokens || usageOutput
		if !strings.HasPrefix(old.SourceUUID, "cb4:") || present[old.SourceUUID] ||
			parsed[codebuddySplitTotalIdentity(old.SourceUUID)] ||
			(!usageContext && !usageOutput) {
			continue
		}
		present[old.SourceUUID] = true
		changed = true
		if i := codebuddyLegacyUsageReply(*messages, old); i >= 0 {
			m := &(*messages)[i]
			m.TokenUsage = old.TokenUsage
			m.ContextTokens, m.HasContextTokens = old.ContextTokens, hasContext
			m.OutputTokens, m.HasOutputTokens = old.OutputTokens, hasOutput
			m.SourceUUID = old.SourceUUID
			if m.Model == "" {
				m.Model = old.Model
			}
			continue
		}
		inserted = true
		row := db.Message{
			SessionID: sessionID, Role: string(parser.RoleAssistant),
			IsSystem: true, Timestamp: old.Timestamp,
			Model: old.Model, TokenUsage: old.TokenUsage,
			ContextTokens: old.ContextTokens, HasContextTokens: hasContext,
			OutputTokens: old.OutputTokens, HasOutputTokens: hasOutput,
			SourceUUID: old.SourceUUID,
		}
		at := len(*messages)
		if ts, err := time.Parse(time.RFC3339Nano, old.Timestamp); err == nil {
			if i := slices.IndexFunc(*messages, func(m db.Message) bool {
				t, err := time.Parse(time.RFC3339Nano, m.Timestamp)
				return err == nil && t.After(ts)
			}); i >= 0 {
				at = i
			}
		}
		*messages = slices.Insert(*messages, at, row)
	}
	if inserted {
		for i := range *messages {
			(*messages)[i].Ordinal = i
		}
	}
	return changed
}

// codebuddySplitTotalIdentity returns the identity of the 4.x request total
// that id, <identity>#<n>, is a further part of, or id itself.
func codebuddySplitTotalIdentity(id string) string {
	base, n, ok := strings.Cut(id, "#")
	if !ok || n == "" || strings.Trim(n, "0123456789") != "" {
		return id
	}
	return base
}

// codebuddyLegacyUsageReply returns the index of the parsed reply that held
// the archived visible reply old's 4.x total, or -1. The reply has no usage
// or identity of its own and the same timestamp; content breaks a tie, and a
// reply that stays ambiguous matches nothing.
func codebuddyLegacyUsageReply(messages []db.Message, old db.Message) int {
	if old.IsSystem || old.Role != string(parser.RoleAssistant) {
		return -1
	}
	var same []int
	for i, m := range messages {
		hasContext, hasOutput := m.TokenPresence()
		if m.Role == old.Role && !m.IsSystem && m.SourceUUID == "" &&
			!hasContext && !hasOutput && len(m.TokenUsage) == 0 &&
			m.Timestamp == old.Timestamp {
			same = append(same, i)
		}
	}
	if len(same) > 1 {
		same = slices.DeleteFunc(same, func(i int) bool {
			return messages[i].Content != old.Content
		})
	}
	if len(same) != 1 {
		return -1
	}
	return same[0]
}

const (
	codebuddyImageLink        = "](asset://"
	codebuddyImageUnavailable = "[image unavailable: "
)

var codebuddyImageTokenRe = regexp.MustCompile(
	`!\[image\]\(asset://[^)\s]+\)|\[image unavailable: [^\]\n]*\]`,
)

// keepCodeBuddyLegacyImages puts back the archived image links of messages
// that now show an image as unavailable. Images match by message identity
// and position, and only when both copies list the same number of images.
// A message the session's own archive holds no image links for, such as one
// a rewind just moved into a new branch, uses the links the source's other
// sessions archived, when they all agree.
func keepCodeBuddyLegacyImages(
	messages []db.Message, archived []db.Message,
	source *SourceToolResults, sessionID string,
) bool {
	saved := make(map[string][]int)
	for i, m := range archived {
		if m.SourceUUID != "" && strings.Contains(m.Content, codebuddyImageLink) {
			saved[m.SourceUUID] = append(saved[m.SourceUUID], i)
		}
	}
	counts := make(map[string]int)
	for _, m := range messages {
		counts[m.SourceUUID]++
	}
	changed := false
	for i := range messages {
		m := &messages[i]
		if m.SourceUUID == "" || counts[m.SourceUUID] != 1 ||
			!strings.Contains(m.Content, codebuddyImageUnavailable) {
			continue
		}
		var links []string
		switch olds := saved[m.SourceUUID]; len(olds) {
		case 0:
			var ok bool
			if links, ok = source.agreedImageLinks(m.SourceUUID, sessionID); !ok {
				continue
			}
		case 1:
			links = codebuddyImageTokenRe.FindAllString(archived[olds[0]].Content, -1)
		default:
			continue
		}
		content, ok := restoreCodeBuddyImageLinks(m.Content, links)
		if !ok {
			continue
		}
		if m.DialogueText != nil && *m.DialogueText == m.Content {
			// A prompt's dialogue is its content, images included.
			m.DialogueText = &content
		}
		m.Content, m.ContentLength = content, len(content)
		changed = true
	}
	return changed
}

// restoreCodeBuddyImageLinks replaces each unavailable image in text with
// the archived link at the same position.
func restoreCodeBuddyImageLinks(text string, links []string) (string, bool) {
	tokens := codebuddyImageTokenRe.FindAllStringIndex(text, -1)
	if len(tokens) != len(links) {
		return text, false
	}
	var b strings.Builder
	last, changed := 0, false
	for i, loc := range tokens {
		b.WriteString(text[last:loc[0]])
		token := text[loc[0]:loc[1]]
		if strings.HasPrefix(token, codebuddyImageUnavailable) &&
			strings.HasPrefix(links[i], "![image](asset://") {
			token, changed = links[i], true
		}
		b.WriteString(token)
		last = loc[1]
	}
	b.WriteString(text[last:])
	return b.String(), changed
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
// the call's summary reflects. An identity shared by more than one current
// event is ambiguous on the current side too, so none of them is restored;
// this caps every archived occurrence to one restore and keeps a repeated
// identity from multiplying one archived payload.
func reconcileClaudePersistedToolResults(
	ctx context.Context, candidate Candidate, prior PriorSession,
	options ContentOptions, result HistoryResult,
) (HistoryResult, error) {
	flagged := flaggedPersistedToolResultIdentities(candidate.Parsed.Messages)
	if len(flagged) == 0 {
		return result, ctx.Err()
	}
	if err := prior.loadMessages(ctx); err != nil {
		return HistoryResult{}, err
	}
	archived := archivedToolResultEventsByIdentity(prior.Messages)
	currentCounts := currentToolResultIdentityCounts(candidate.Messages)
	type callPos struct{ mi, ci int }
	var touched []callPos
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
				if !flagged[key] || currentCounts[key] != 1 {
					continue
				}
				matches := archived[key]
				if len(matches) == 0 || (len(matches) == 1 &&
					isPersistedOutputPlaceholder(matches[0].Content)) {
					// The session's own archive lacks the call, or saved
					// only the placeholder: a rewind may have moved it
					// here from a sibling that saved the full text, or
					// another copy of the transcript supplied it later.
					if saved, ok := prior.SourceToolResults.agreed(
						key, candidate.Session.ID,
					); ok {
						matches = []db.ToolResultEvent{saved}
					}
				}
				if len(matches) != 1 {
					continue
				}
				pos := callPos{mi, ci}
				if restoreClaudeToolResultEvent(event, matches[0]) &&
					!slices.Contains(touched, pos) {
					touched = append(touched, pos)
				}
			}
		}
	}
	if len(touched) == 0 {
		return result, ctx.Err()
	}
	// A restored event can reintroduce image bytes the live policy drops, the
	// same risk VSCopilot's own merge above guards against, so reapply the
	// policy here too before anything downstream reads the restored content.
	candidate.Messages = dropToolResultImagesIfPolicy(candidate.Messages, options)
	// Recompute summaries only for restored calls, resolved by position
	// because the policy above may have cloned the messages. A blocked
	// call is only re-blanked: its length was set by the first pass, which
	// saw every event, and recomputing from the now-blank events would
	// overwrite it with the restored event's length.
	for _, pos := range touched {
		call := &candidate.Messages[pos.mi].ToolCalls[pos.ci]
		if options.BlockedResultCategories[call.Category] {
			call.ResultContent = ""
			for k := range call.ResultEvents {
				call.ResultEvents[k].Content = ""
			}
			continue
		}
		if err := pairToolCallResultSummaryContext(
			ctx, call, options.BlockedResultCategories,
		); err != nil {
			return HistoryResult{}, err
		}
	}
	result.Candidate = candidate
	result.Action = HistoryMerge
	result.PriorContributed = true
	return result, ctx.Err()
}

// restoreClaudeToolResultEvent writes archived's content onto event when the
// archive held real text. When the archive held only a length without text
// (an ArchiveContentTranscripts/ArchiveContentUsage policy, or a blocked
// category), event is left alone: restoring a bare length would reproduce
// the same "blank content, kept length" shape the read side already uses to
// mean a deduped summary, and there is no way to tell the two apart later.
// Accepted limit: once the side file for such an event ages out, its
// archived length cannot be recovered on a later reparse; event keeps
// whatever length the current placeholder text itself resolves to, not the
// archived length.
func restoreClaudeToolResultEvent(
	event *db.ToolResultEvent, archived db.ToolResultEvent,
) bool {
	if archived.Content == "" {
		return false
	}
	if event.Content == archived.Content &&
		event.ContentLength == archived.ContentLength {
		return false
	}
	event.Content = archived.Content
	event.ContentLength = archived.ContentLength
	event.RawContentDigest = archived.RawContentDigest
	return true
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

// currentToolResultIdentityCounts tallies how many current tool_result
// events share each identity.
func currentToolResultIdentityCounts(
	messages []db.Message,
) map[toolResultEventIdentity]int {
	counts := make(map[toolResultEventIdentity]int)
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			for _, event := range call.ResultEvents {
				counts[toolResultEventIdentity{
					toolUseID: event.ToolUseID, source: event.Source,
					timestamp: event.Timestamp,
				}]++
			}
		}
	}
	return counts
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
