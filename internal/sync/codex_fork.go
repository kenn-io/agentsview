package sync

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"maps"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/stringutil"
)

// Keep local reconciliation metadata out of immutable hosted ingest payloads.
type preparedSessionWrite struct {
	ingest.PreparedSession
	codexForkState *db.CodexForkState
	stagedToolKeys map[string]string
}

const codexForkUnresolvedWarning = "Fork parent unavailable. Messages remain visible; unverified history may include parent replay, and its token usage is withheld until reconciliation."

// reconcileCodexFork keeps available content while the replay parent is absent.
// Source origins and replay evidence commit with the resulting transcript. A
// restart therefore never mistakes a fallback for previously resolved history.
func (e *Engine) reconcileCodexFork(ctx context.Context, c ingest.Candidate, staged *codexStagingSink) (ingest.Candidate, *db.CodexForkState, map[string]string, error) {
	info := c.Parsed.Session.CodexFork
	if info == nil {
		return c, nil, nil, nil
	}
	store := e.historyStore()
	reader := e.db
	if old, ok := e.archiveStore.(*db.DB); ok {
		reader = old
	}
	priorID := c.Session.ID
	previous, err := reader.GetCodexForkState(ctx, priorID)
	if err != nil {
		return c, nil, nil, err
	}
	// Resolved full parses are authoritative unless an earlier retry retained
	// history. In that common case, never hydrate archived tool payloads.
	retainMessages := !info.Resolved || (previous != nil && previous.PreserveHistory)
	retainedEvents, err := reader.GetUsageEvents(ctx, priorID)
	if err != nil {
		return c, nil, nil, err
	}
	needsHistory := retainMessages
	for _, event := range retainedEvents {
		needsHistory = needsHistory || event.MessageOrdinal != nil
	}
	loadHistory := store.GetAllMessages
	if staged != nil || !retainMessages {
		loadHistory = reader.GetCodexForkMessageMetadata
	}
	var prior []db.Message
	if needsHistory {
		prior, err = loadHistory(ctx, priorID)
		if err != nil {
			return c, nil, nil, err
		}
	}
	if needsHistory && len(prior) == 0 && c.Session.FilePath != nil {
		// Before version 40 a child could occupy its parent's ID. The exact
		// source path, agent and legacy version identify that shipped mapping.
		ids, err := reader.ListSessionIDsByFilePath(ctx, *c.Session.FilePath, c.Session.Agent)
		if err != nil {
			return c, nil, nil, err
		}
		for _, id := range ids {
			if c.Session.ParentSessionID == nil || id != *c.Session.ParentSessionID || reader.GetSessionDataVersion(ctx, id) >= 40 {
				continue
			}
			prior, err = loadHistory(ctx, id)
			if err != nil {
				return c, nil, nil, err
			}
			priorID = id
			retainedEvents, err = reader.GetUsageEvents(ctx, priorID)
			if err != nil {
				return c, nil, nil, err
			}
			previous, err = reader.GetCodexForkState(ctx, priorID)
			if err != nil {
				return c, nil, nil, err
			}
			break
		}
	}
	state := &db.CodexForkState{ParentID: info.ParentID, ParentKnown: info.Resolved, ParentTurns: maps.Clone(info.ParentTurns)}
	if previous != nil && previous.ParentID == info.ParentID && previous.ParentKnown {
		state.ParentKnown = true
		if state.ParentTurns == nil {
			state.ParentTurns = make(map[string]struct{})
		}
		maps.Copy(state.ParentTurns, previous.ParentTurns)
	}

	state.PreserveHistory = !info.Resolved

	// Capture original scratch occurrence identities before replay removal or
	// insertion of archived-only messages changes the final ordinal stream.
	sourceToolKeys := make([][]string, len(c.Messages))
	occurrences := make(map[string]int)
	for i, m := range c.Messages {
		for _, call := range m.ToolCalls {
			key := db.StagedToolCallKey(call.ToolUseID, occurrences[call.ToolUseID])
			occurrences[call.ToolUseID]++
			sourceToolKeys[i] = append(sourceToolKeys[i], key)
		}
	}
	type entry struct {
		message    db.Message
		provenance db.CodexForkMessage
		source     int
		stagedKeys []string
	}
	incoming := make([]entry, 0, len(c.Messages))
	// Saved membership can be more complete than the currently readable
	// parent pages. An idless turn_context still opens the child boundary;
	// distinguish it from pre-turn replay records by explicit turn presence.
	replaying := state.ParentKnown
	for i, m := range c.Messages {
		origin := c.Parsed.Messages[i].CodexOrigin
		if replaying {
			if _, inherited := state.ParentTurns[origin.TurnID]; inherited || (origin.TurnID == "" && !origin.InTurn) {
				continue
			}
			replaying = false
		}
		provisional := !state.ParentKnown
		var pendingUsage *db.CodexForkUsage
		if provisional {
			pendingUsage = &db.CodexForkUsage{TokenUsage: m.TokenUsage, ContextTokens: m.ContextTokens, OutputTokens: m.OutputTokens, HasContextTokens: m.HasContextTokens, HasOutputTokens: m.HasOutputTokens}
			m.TokenUsage = nil
			m.ContextTokens, m.OutputTokens = 0, 0
			m.HasContextTokens, m.HasOutputTokens = false, false
		}
		incoming = append(incoming, entry{message: m, provenance: db.CodexForkMessage{Origin: origin, Provisional: provisional, PendingUsage: pendingUsage}, source: i})
	}

	// Occurrence queues preserve repeated identical messages. Legacy rows lack
	// source origins; pair them by content, timestamp and tool identities once,
	// then persist their proven source positions with this write.
	byOrigin := make(map[parser.CodexMessageOrigin][]int)
	byContent := make(map[[32]byte][]int)
	contentKeys := make([][32]byte, len(incoming))
	for i, item := range incoming {
		byOrigin[item.provenance.Origin] = append(byOrigin[item.provenance.Origin], i)
		_, projected := e.db.ProjectSessionForStorage(c.Session, []db.Message{item.message})
		if len(projected) == 0 {
			continue
		}
		db.SanitizeMessage(&projected[0])
		key, err := db.CodexForkMessageKey(projected[0])
		if err != nil {
			return c, nil, nil, err
		}
		byContent[key] = append(byContent[key], i)
		contentKeys[i] = key
	}
	used := make([]bool, len(incoming))
	merged := make([]entry, 0, len(prior)+len(incoming))
	next := 0
	priorOrdinals := make(map[int]int)
	sourceOrdinals := make(map[int]int)
	priorKeys := make([][32]byte, len(prior))
	priorOrigins := make([]db.CodexForkMessage, len(prior))
	inheritedPrior := make([]bool, len(prior))
	legacyCounts := make(map[[32]byte]int)
	reserved := make([]bool, len(incoming))
	priorMatches := make(map[int]int)
	archivedReplaying := state.ParentKnown
	for i, m := range prior {
		if err := ctx.Err(); err != nil {
			return c, nil, nil, err
		}
		key, err := db.CodexForkMessageKey(m)
		if err != nil {
			return c, nil, nil, err
		}
		var old db.CodexForkMessage
		if previous != nil && m.Ordinal < len(previous.Messages) {
			if key == previous.Messages[m.Ordinal].Digest {
				old = previous.Messages[m.Ordinal]
			}
		}
		// Classify saved replay before reserving source occurrences. Pre-turn
		// replay and idless child turns share an empty turn ID.
		if archivedReplaying && old.Origin.Line > 0 {
			_, inherited := state.ParentTurns[old.Origin.TurnID]
			if inherited || (old.Origin.TurnID == "" && !old.Origin.InTurn) {
				inheritedPrior[i] = true
				continue
			}
			archivedReplaying = false
		}
		_, projected := e.db.ProjectSessionForStorage(c.Session, []db.Message{m})
		if len(projected) > 0 {
			key, err = db.CodexForkMessageKey(projected[0])
			if err != nil {
				return c, nil, nil, err
			}
		}
		priorKeys[i], priorOrigins[i] = key, old
		if old.Origin.Line == 0 {
			legacyCounts[key]++
			continue
		}
		for _, index := range byOrigin[old.Origin] {
			if !reserved[index] && key == contentKeys[index] {
				reserved[index] = true
				priorMatches[i] = index
				break
			}
		}
	}
	for i, old := range priorOrigins {
		if _, matched := priorMatches[i]; matched || inheritedPrior[i] || old.Origin.Line == 0 {
			continue
		}
		for _, index := range byContent[priorKeys[i]] {
			if !reserved[index] && old.Origin.TurnID == incoming[index].provenance.Origin.TurnID {
				reserved[index], priorMatches[i] = true, index
				break
			}
		}
	}
	// Existing source occurrences own their matches before legacy rows can
	// claim them. Fewer returning legacy occurrences cannot identify which
	// saved usage belongs to a child rather than copied parent history.
	legacyIncoming := make(map[[32]byte]int)
	for i, key := range contentKeys {
		if !reserved[i] {
			legacyIncoming[key]++
		}
	}
	for i, m := range prior {
		if err := ctx.Err(); err != nil {
			return c, nil, nil, err
		}
		if inheritedPrior[i] {
			continue
		}
		key, old := priorKeys[i], priorOrigins[i]
		ambiguousLegacy := old.Origin.Line == 0 && legacyCounts[key] != legacyIncoming[key]
		match := -1
		if index, matched := priorMatches[i]; matched {
			match = index
		}
		if match < 0 && old.Origin.Line == 0 && !ambiguousLegacy {
			for _, index := range byContent[key] {
				if !used[index] && !reserved[index] {
					match = index
					break
				}
			}
		}
		if staged != nil && retainMessages && len(m.ToolCalls) > 0 {
			loaded, err := store.GetMessages(ctx, priorID, m.Ordinal, 1, true)
			if err != nil {
				return c, nil, nil, err
			}
			if len(loaded) != 1 || loaded[0].Ordinal != m.Ordinal {
				return c, nil, nil, errors.New("archived fork message changed during reconciliation")
			}
			m.ToolCalls = loaded[0].ToolCalls
		}
		if match >= 0 {
			current := &incoming[match]
			if retainMessages {
				previousUsage := m
				if old.PendingUsage != nil {
					usage := old.PendingUsage
					previousUsage.TokenUsage, previousUsage.ContextTokens, previousUsage.OutputTokens = usage.TokenUsage, usage.ContextTokens, usage.OutputTokens
					previousUsage.HasContextTokens, previousUsage.HasOutputTokens = usage.HasContextTokens, usage.HasOutputTokens
				}
				if current.provenance.Provisional {
					usage := current.provenance.PendingUsage
					if len(usage.TokenUsage) == 0 && !usage.HasContextTokens && !usage.HasOutputTokens {
						current.provenance.PendingUsage = &db.CodexForkUsage{TokenUsage: previousUsage.TokenUsage, ContextTokens: previousUsage.ContextTokens, OutputTokens: previousUsage.OutputTokens, HasContextTokens: previousUsage.HasContextTokens, HasOutputTokens: previousUsage.HasOutputTokens}
					}
				} else if len(current.message.TokenUsage) == 0 && !current.message.HasContextTokens && !current.message.HasOutputTokens {
					current.message.TokenUsage, current.message.ContextTokens, current.message.OutputTokens = previousUsage.TokenUsage, previousUsage.ContextTokens, previousUsage.OutputTokens
					current.message.HasContextTokens, current.message.HasOutputTokens = previousUsage.HasContextTokens, previousUsage.HasOutputTokens
					if len(previousUsage.TokenUsage) > 0 || previousUsage.HasContextTokens || previousUsage.HasOutputTokens {
						state.PreserveHistory = true
					}
				}
				// Results are updates, not a completeness count. Equal-sized partial
				// snapshots can carry different events for the same archived call.
				for j := range current.message.ToolCalls {
					if j >= len(m.ToolCalls) {
						break
					}
					call, saved := &current.message.ToolCalls[j], m.ToolCalls[j]
					if len(saved.ResultEvents) == 0 && (saved.ResultContent != "" || saved.ResultContentLength > 0) &&
						len(call.ResultEvents) == 0 && call.ResultContent == "" && call.ResultContentLength == 0 {
						call.ResultContent, call.ResultContentLength = saved.ResultContent, saved.ResultContentLength
						state.PreserveHistory = true
						if staged != nil {
							if err := staged.stageForkCall(ctx, sourceToolKeys[current.source][j], call); err != nil {
								return c, nil, nil, err
							}
						}
					}
					if call.SubagentSessionID == "" && saved.SubagentSessionID != "" {
						call.SubagentSessionID = saved.SubagentSessionID
						state.PreserveHistory = true
					}
					// The message key already compared call inputs after the current
					// storage projection, which may deliberately omit arguments.
					if call.ToolUseID != saved.ToolUseID || len(saved.ResultEvents) == 0 {
						continue
					}
					if staged != nil {
						call.ResultEvents, err = staged.forkResultEvents(ctx, sourceToolKeys[current.source][j])
						if err != nil {
							return c, nil, nil, err
						}
					}
					var retained bool
					call.ResultEvents, retained = mergeCodexForkResultEvents(saved.ResultEvents, call.ResultEvents)
					if retained {
						state.PreserveHistory = true
					}
					if staged != nil || retained {
						if !e.blockedResultCategories[call.Category] && !e.db.ArchiveContent().OmitsToolContent() {
							call.ResultContent, err = ingest.SummarizeToolResultEventsContext(ctx, call.ResultEvents)
							if err != nil {
								return c, nil, nil, err
							}
							call.ResultContentLength = len(call.ResultContent)
						} else {
							call.ResultContent = ""
							call.ResultContentLength = codexForkResultSummaryLength(call.ResultEvents)
						}
					}
					if staged != nil {
						if err := staged.stageForkCall(ctx, sourceToolKeys[current.source][j], call); err != nil {
							return c, nil, nil, err
						}
					}
				}
			}
			for next <= match {
				if !used[next] {
					sourceOrdinals[incoming[next].source] = len(merged)
					merged = append(merged, incoming[next])
					used[next] = true
				}
				next++
			}
			priorOrdinals[m.Ordinal] = sourceOrdinals[current.source]
			continue
		}
		if !retainMessages {
			continue
		}
		// Remove only records positively identified as inherited replay. Source
		// truncation alone never erases archived child messages or tool results.
		if state.ParentKnown && old.Origin.Line > 0 {
			if old.Provisional && (old.Origin.TurnID != "" || old.Origin.InTurn) && old.PendingUsage != nil {
				usage := old.PendingUsage
				m.TokenUsage, m.ContextTokens, m.OutputTokens = usage.TokenUsage, usage.ContextTokens, usage.OutputTokens
				m.HasContextTokens, m.HasOutputTokens = usage.HasContextTokens, usage.HasOutputTokens
				old.Provisional, old.PendingUsage = false, nil
			}
		}
		// A pre-provenance row missing from this source cannot prove whether
		// its usage is child work or replay. Retain the figures without counting
		// them until returning source records establish that ownership.
		if (!state.ParentKnown || ambiguousLegacy) && old.Origin.Line == 0 && !old.Provisional {
			old.Provisional = true
			old.PendingUsage = &db.CodexForkUsage{TokenUsage: m.TokenUsage, ContextTokens: m.ContextTokens, OutputTokens: m.OutputTokens, HasContextTokens: m.HasContextTokens, HasOutputTokens: m.HasOutputTokens}
			m.TokenUsage = nil
			m.ContextTokens, m.OutputTokens, m.HasContextTokens, m.HasOutputTokens = 0, 0, false, false
		}
		state.PreserveHistory = true
		priorOrdinals[m.Ordinal] = len(merged)
		var savedKeys []string
		if staged != nil {
			for j := range m.ToolCalls {
				key := fmt.Sprintf("fork:%d:%d", m.Ordinal, j)
				if err := staged.stageForkCall(ctx, key, &m.ToolCalls[j]); err != nil {
					return c, nil, nil, err
				}
				savedKeys = append(savedKeys, key)
			}
		}
		merged = append(merged, entry{message: m, provenance: old, source: -1, stagedKeys: savedKeys})
	}
	for i, item := range incoming {
		if !used[i] {
			merged = append(merged, item)
		}
	}
	c.Messages = make([]db.Message, len(merged))
	state.Messages = make([]db.CodexForkMessage, len(merged))
	toolKeys := make(map[string]string)
	clear(occurrences)
	for i, item := range merged {
		m := item.message
		m.Ordinal, m.SessionID = i, c.Session.ID
		for j := range m.ToolCalls {
			call := &m.ToolCalls[j]
			if e.blockedResultCategories[call.Category] {
				call.ResultContent = ""
				for k := range call.ResultEvents {
					call.ResultEvents[k].Content = ""
				}
			}
		}
		digest, err := db.CodexForkMessageKey(m)
		if err != nil {
			return c, nil, nil, err
		}
		item.provenance.Digest = digest
		c.Messages[i], state.Messages[i] = m, item.provenance
		for j, call := range m.ToolCalls {
			final := db.StagedToolCallKey(call.ToolUseID, occurrences[call.ToolUseID])
			occurrences[call.ToolUseID]++
			if item.source >= 0 && sourceToolKeys[item.source][j] != "" {
				toolKeys[final] = sourceToolKeys[item.source][j]
			} else if item.source < 0 && len(item.stagedKeys) > j {
				toolKeys[final] = item.stagedKeys[j]
			}
		}
	}
	c.Session.FirstMessage = nil
	for _, m := range c.Messages {
		if m.Role == "user" && !m.IsSystem && m.SourceSubtype != parser.SourceSubtypeToolResult && strings.TrimSpace(m.Content) != "" {
			c.Session.FirstMessage = new(stringutil.TruncateRunes(strings.TrimSpace(strings.ReplaceAll(m.Content, "\n", " ")), 300, "..."))
			break
		}
	}
	// The session totals must describe retained rows, never the unfiltered
	// fallback parse. Usage without replay evidence is deliberately absent.
	if err := ingest.ApplySessionTokenTotalsFromMessagesContext(ctx, &c.Session, c.Messages); err != nil {
		return c, nil, nil, err
	}
	if !state.ParentKnown {
		log.Print(codexForkUnresolvedWarning)
		c.Parsed.UsageEvents = nil
		c.Parsed.Session.UsageEvents = nil
	}
	// Non-source usage, such as imported historical usage-only rows, is archive
	// history too. Codex message usage above must not erase it during a retry.
	if len(c.Parsed.UsageEvents) == 0 {
		for _, event := range retainedEvents {
			if event.MessageOrdinal != nil {
				ordinal, retained := priorOrdinals[*event.MessageOrdinal]
				if !retained {
					continue
				}
				event.MessageOrdinal = new(ordinal)
			}
			event.SessionID = c.Session.ID
			c.Parsed.UsageEvents = append(c.Parsed.UsageEvents, parser.ParsedUsageEvent{
				SessionID: event.SessionID, MessageOrdinal: event.MessageOrdinal, Source: event.Source, Model: event.Model, ProviderID: event.ProviderID,
				InputTokens: event.InputTokens, OutputTokens: event.OutputTokens, CacheCreationInputTokens: event.CacheCreationInputTokens, CacheReadInputTokens: event.CacheReadInputTokens,
				ReasoningTokens: event.ReasoningTokens, Cost: event.Cost, CostStatus: event.CostStatus, CostSource: event.CostSource, OccurredAt: event.OccurredAt, DedupKey: event.DedupKey,
			})
			state.PreserveHistory = true
		}
	}
	if state.PreserveHistory {
		archived, err := store.GetSession(ctx, priorID)
		if err != nil {
			return c, nil, nil, err
		}
		if archived != nil {
			for _, bound := range []struct {
				saved    *string
				current  **string
				earliest bool
			}{
				{archived.StartedAt, &c.Session.StartedAt, true},
				{archived.EndedAt, &c.Session.EndedAt, false},
			} {
				if bound.saved == nil {
					continue
				}
				savedTime, err := time.Parse(time.RFC3339Nano, *bound.saved)
				if err != nil {
					continue
				}
				if *bound.current == nil {
					*bound.current = bound.saved
					continue
				}
				currentTime, err := time.Parse(time.RFC3339Nano, **bound.current)
				if err != nil || (bound.earliest && savedTime.Before(currentTime)) || (!bound.earliest && savedTime.After(currentTime)) {
					*bound.current = bound.saved
				}
			}
		}
	}

	return c, state, toolKeys, nil
}

// Match the provider's result dedup identity, not slice length or event index.
// Keep archive order and append newly observed updates once. A current copy of
// an existing event supplies the current content policy and metadata.
func mergeCodexForkResultEvents(saved, current []db.ToolResultEvent) ([]db.ToolResultEvent, bool) {
	type identity struct{ agent, status, digest string }
	key := func(event db.ToolResultEvent) identity {
		digest := event.RawContentDigest
		if digest == nil {
			hash := sha256.Sum256([]byte(event.Content))
			digest = hash[:]
		}
		return identity{event.AgentID, event.Status, string(digest)}
	}
	currentByKey := make(map[identity]int, len(current))
	for i, event := range current {
		currentByKey[key(event)] = i
	}
	merged := make([]db.ToolResultEvent, 0, len(saved)+len(current))
	seen := make(map[identity]bool)
	retained := false
	appendEvent := func(event db.ToolResultEvent) {
		id := key(event)
		if !seen[id] {
			seen[id] = true
			event.EventIndex = len(merged)
			merged = append(merged, event)
		}
	}
	next := 0
	for _, event := range saved {
		if anchor, ok := currentByKey[key(event)]; ok {
			// Restored source events before a shared event precede that anchor.
			// Current order and metadata resolve conflicting old observations.
			for next <= anchor {
				appendEvent(current[next])
				next++
			}
		} else {
			retained = true
			appendEvent(event)
		}
	}
	for ; next < len(current); next++ {
		appendEvent(current[next])
	}
	return merged, retained
}

// Reproduce the canonical latest-per-agent summary's length without restoring
// blocked text. Participation was recorded before the content was blanked.
func codexForkResultSummaryLength(events []db.ToolResultEvent) int {
	latest := make(map[string]int)
	anonymous := 0
	hasAnonymous := false
	for _, event := range events {
		if event.SummaryParticipates != nil {
			if !*event.SummaryParticipates {
				continue
			}
		} else if strings.TrimSpace(event.Content) == "" {
			continue
		}
		agent := strings.TrimSpace(event.AgentID)
		if agent == "" {
			hasAnonymous = true
			anonymous = event.ContentLength
		} else {
			latest[agent] = event.ContentLength
		}
	}
	if len(latest) == 0 {
		return anonymous
	}
	total := 2 * (len(latest) - 1)
	for agent, length := range latest {
		total += length
		if len(latest) > 1 {
			total += len(agent) + 2
		}
	}
	if hasAnonymous {
		total += 2 + anonymous
	}
	return total
}
