package sync

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"log"
	"maps"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
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
func (e *Engine) reconcileCodexFork(ctx context.Context, c ingest.Candidate) (ingest.Candidate, *db.CodexForkState, map[string]string, error) {
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
	prior, err := store.GetAllMessages(ctx, priorID)
	if err != nil {
		return c, nil, nil, err
	}
	if len(prior) == 0 && c.Session.FilePath != nil {
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
			prior, err = store.GetAllMessages(ctx, id)
			if err != nil {
				return c, nil, nil, err
			}
			priorID = id
			break
		}
	}
	previous, err := reader.GetCodexForkState(ctx, priorID)
	if err != nil {
		return c, nil, nil, err
	}
	state := &db.CodexForkState{ParentID: info.ParentID, ParentKnown: info.Resolved, ParentTurns: maps.Clone(info.ParentTurns)}
	if previous != nil && previous.ParentID == info.ParentID && previous.ParentKnown {
		state.ParentKnown = true
		if state.ParentTurns == nil {
			state.ParentTurns = make(map[string]struct{})
		}
		maps.Copy(state.ParentTurns, previous.ParentTurns)
	}

	// Healthy, resolved sources retain their existing authoritative replacement
	// semantics. Only a missing-parent interval needs archive history merging.
	if info.Resolved && (previous == nil || !previous.PreserveHistory) {
		prior = nil
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
	}
	incoming := make([]entry, 0, len(c.Messages))
	replaying := state.ParentKnown
	for i, m := range c.Messages {
		origin := c.Parsed.Messages[i].CodexOrigin
		if replaying {
			if _, inherited := state.ParentTurns[origin.TurnID]; inherited || origin.TurnID == "" {
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
		incoming = append(incoming, entry{m, db.CodexForkMessage{Origin: origin, Provisional: provisional, PendingUsage: pendingUsage}, i})
	}

	// Occurrence queues preserve repeated identical messages. Legacy rows lack
	// source origins; pair them by content, timestamp and tool identities once,
	// then persist their proven source positions with this write.
	byOrigin := make(map[parser.CodexMessageOrigin]int)
	byContent := make(map[[32]byte][]int)
	for i, item := range incoming {
		byOrigin[item.provenance.Origin] = i
		_, projected := e.db.ProjectSessionForStorage(c.Session, []db.Message{item.message})
		if len(projected) == 0 {
			continue
		}
		key, err := codexForkMessageKey(projected[0])
		if err != nil {
			return c, nil, nil, err
		}
		byContent[key] = append(byContent[key], i)
	}
	used := make([]bool, len(incoming))
	merged := make([]entry, 0, len(prior)+len(incoming))
	next := 0
	for _, m := range prior {
		if err := ctx.Err(); err != nil {
			return c, nil, nil, err
		}
		var old db.CodexForkMessage
		if previous != nil && m.Ordinal < len(previous.Messages) {
			key, err := codexForkMessageKey(m)
			if err != nil {
				return c, nil, nil, err
			}
			if key == previous.Messages[m.Ordinal].Digest {
				old = previous.Messages[m.Ordinal]
			}
		}
		match := -1
		if old.Origin.Line > 0 {
			if index, ok := byOrigin[old.Origin]; ok && !used[index] {
				match = index
			}
		}
		if match < 0 {
			key, err := codexForkMessageKey(m)
			if err != nil {
				return c, nil, nil, err
			}
			for _, index := range byContent[key] {
				if !used[index] && (old.Origin.Line == 0 || old.Origin.TurnID == incoming[index].provenance.Origin.TurnID) {
					match = index
					break
				}
			}
		}
		if match >= 0 {
			current := &incoming[match]
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
			}
			// A truncated source can retain a call but lose some or all of its
			// following output records. Keep the more complete archived results.
			for j := range current.message.ToolCalls {
				if j >= len(m.ToolCalls) {
					break
				}
				call, saved := &current.message.ToolCalls[j], m.ToolCalls[j]
				if call.ToolUseID == saved.ToolUseID && call.InputJSON == saved.InputJSON && len(call.ResultEvents) < len(saved.ResultEvents) {
					call.ResultEvents, call.ResultContent, call.ResultContentLength = saved.ResultEvents, saved.ResultContent, saved.ResultContentLength
					sourceToolKeys[current.source][j] = ""
				}
			}
			for next <= match {
				if !used[next] {
					merged = append(merged, incoming[next])
					used[next] = true
				}
				next++
			}
			continue
		}
		// Remove only records positively identified as inherited replay. Source
		// truncation alone never erases archived child messages or tool results.
		if state.ParentKnown && old.Origin.Line > 0 {
			if _, inherited := state.ParentTurns[old.Origin.TurnID]; inherited {
				continue
			}
			if old.Provisional && old.Origin.TurnID != "" && old.PendingUsage != nil {
				usage := old.PendingUsage
				m.TokenUsage, m.ContextTokens, m.OutputTokens = usage.TokenUsage, usage.ContextTokens, usage.OutputTokens
				m.HasContextTokens, m.HasOutputTokens = usage.HasContextTokens, usage.HasOutputTokens
				old.Provisional, old.PendingUsage = false, nil
			}
		}
		// A pre-provenance row missing from this source cannot prove whether
		// its usage is child work or replay. Retain the figures without counting
		// them until returning source records establish that ownership.
		if !state.ParentKnown && old.Origin.Line == 0 && !old.Provisional {
			old.Provisional = true
			old.PendingUsage = &db.CodexForkUsage{TokenUsage: m.TokenUsage, ContextTokens: m.ContextTokens, OutputTokens: m.OutputTokens, HasContextTokens: m.HasContextTokens, HasOutputTokens: m.HasOutputTokens}
			m.TokenUsage = nil
			m.ContextTokens, m.OutputTokens, m.HasContextTokens, m.HasOutputTokens = 0, 0, false, false
		}
		state.PreserveHistory = true
		merged = append(merged, entry{m, old, -1})
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
		digest, err := codexForkMessageKey(m)
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
			}
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
		events, err := reader.GetUsageEvents(ctx, priorID)
		if err != nil {
			return c, nil, nil, err
		}
		// Conversion is intentionally left to the existing normalized writer.
		state.RetainedUsage = events
	}
	return c, state, toolKeys, nil
}

func codexForkMessageKey(m db.Message) ([32]byte, error) {
	// Legacy archives can lack model, length and rendering metadata. Match
	// visible content and source timestamp, with occurrence queues for repeats.
	// Tool identities include inputs, while hydrated output belongs to history.
	type callKey struct{ Name, ID, Input string }
	key := struct {
		Role, Content, Thinking, Timestamp string
		Calls                              []callKey
	}{Role: m.Role, Content: m.Content, Thinking: m.ThinkingText, Timestamp: m.Timestamp}
	for _, call := range m.ToolCalls {
		key.Calls = append(key.Calls, callKey{call.ToolName, call.ToolUseID, call.InputJSON})
	}
	encoded, err := json.Marshal(key)
	return sha256.Sum256(encoded), err
}
