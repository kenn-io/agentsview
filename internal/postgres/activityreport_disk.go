package postgres

import (
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/stringutil"
)

const pgActivityDiskVersion = "pg-activity-inputs-1|"

type pgActivityDiskRow struct {
	SessionID, UsageSource, Model, ProviderID, Agent                          string
	ClaudeMessageID, ClaudeRequestID, SourceUUID, UsageDedupKey               string
	US, Cost, WebSearches                                                     int64
	Input, Output, CacheCreate, CacheRead, Reasoning                          int64
	Ordinal, CacheCreate1h                                                    int32
	OrdinalValid, CostValid, CopilotReported, PricingTimeAbsent, CostRecorded bool
}

type pgActivityDiskPart struct {
	Identity          string
	Revision          int64
	GapCap            float64
	Runs              activity.SessionRuns
	Users, Assistants []int64
	Usage             []pgActivityDiskRow
	Candidate         bool
}

func pgActivityRowToDisk(r pgActivityUsageRow) pgActivityDiskRow {
	return pgActivityDiskRow{
		SessionID: r.sessionID.Value(), UsageSource: r.usageSource.Value(), Model: r.model.Value(), ProviderID: r.providerID.Value(), Agent: r.agent.Value(),
		ClaudeMessageID: r.claudeMessageID, ClaudeRequestID: r.claudeRequestID, SourceUUID: r.sourceUUID, UsageDedupKey: r.usageDedupKey,
		US: r.us, Cost: r.cost, WebSearches: r.webSearches, Input: r.input, Output: r.output, CacheCreate: r.cacheCreate, CacheRead: r.cacheRead, Reasoning: r.reasoning,
		Ordinal: r.ordinal, CacheCreate1h: r.cacheCreate1h, OrdinalValid: r.ordinalValid, CostValid: r.costValid, CopilotReported: r.copilotReported, PricingTimeAbsent: r.pricingTimeAbsent, CostRecorded: r.costRecorded,
	}
}

func (r pgActivityDiskRow) activityRow(intern stringutil.Interner) pgActivityUsageRow {
	return pgActivityUsageRow{
		sessionID: intern.Intern(r.SessionID), usageSource: intern.Intern(r.UsageSource), model: intern.Intern(r.Model), providerID: intern.Intern(r.ProviderID), agent: intern.Intern(r.Agent),
		claudeMessageID: r.ClaudeMessageID, claudeRequestID: r.ClaudeRequestID, sourceUUID: r.SourceUUID, usageDedupKey: r.UsageDedupKey,
		us: r.US, cost: r.Cost, webSearches: r.WebSearches, input: r.Input, output: r.Output, cacheCreate: r.CacheCreate, cacheRead: r.CacheRead, reasoning: r.Reasoning,
		ordinal: r.Ordinal, cacheCreate1h: r.CacheCreate1h, ordinalValid: r.OrdinalValid, costValid: r.CostValid, copilotReported: r.CopilotReported, pricingTimeAbsent: r.PricingTimeAbsent, costRecorded: r.CostRecorded,
	}
}

func (s *Store) saveActivityInputs() {
	if !s.activityDisk.Enabled() {
		return
	}
	for _, slot := range s.runtimeState().activityInputs.Snapshot() {
		if slot.Rows[0].identity == "" {
			continue
		}
		input := slot.Rows[0]
		parts := make(map[string]pgActivityDiskPart, len(input.sessions))
		for id, session := range input.sessions {
			parts[id] = pgActivityDiskPart{Runs: session.runs, Users: session.users, Assistants: session.assistants, Candidate: input.candidates[id]}
		}
		for _, row := range input.usage {
			id := row.sessionID.Value()
			part := parts[id]
			part.Usage = append(part.Usage, pgActivityRowToDisk(row))
			parts[id] = part
		}
		rows := make([]kept.Part[pgActivityDiskPart], 0, len(parts)+1)
		rows = append(rows, kept.Part[pgActivityDiskPart]{Value: pgActivityDiskPart{Identity: input.identity, Revision: input.revision, GapCap: input.gapCap}})
		for id, part := range parts {
			rows = append(rows, kept.Part[pgActivityDiskPart]{ID: id, Value: part})
		}
		data, err := kept.EncodeRows(rows, 256<<20, func(part kept.Part[pgActivityDiskPart]) int64 {
			size := int64(len(part.ID) + len(part.Value.Identity))
			size += int64(len(part.Value.Users)+len(part.Value.Assistants)) * 8
			size += part.Value.Runs.Bytes()
			for _, row := range part.Value.Usage {
				size += int64(len(row.SessionID) + len(row.UsageSource) + len(row.Model) + len(row.ProviderID) + len(row.Agent) +
					len(row.ClaudeMessageID) + len(row.ClaudeRequestID) + len(row.SourceUUID) + len(row.UsageDedupKey))
			}
			return size
		})
		if err == nil {
			s.activityDisk.Put(slot.Slot, pgActivityDiskVersion+slot.Version, data)
		}
	}
}

func (s *Store) loadActivityInputs(slot, version, identity string) (pgActivityInputs, bool) {
	parts := kept.LoadParts[pgActivityDiskPart](&s.activityDisk, slot, pgActivityDiskVersion+version)
	meta, ok := parts[""]
	if !ok || meta.Value.Identity != identity {
		return pgActivityInputs{}, false
	}
	delete(parts, "")
	input := pgActivityInputs{identity: identity, revision: meta.Value.Revision, gapCap: meta.Value.GapCap, sessions: map[string]pgActivitySession{}, candidates: map[string]bool{}}
	intern := make(stringutil.Interner)
	for _, row := range parts {
		part := row.Value
		if part.Candidate {
			input.candidates[row.ID] = true
			input.sessions[row.ID] = pgActivitySession{runs: part.Runs, users: part.Users, assistants: part.Assistants}
		}
		for _, usage := range part.Usage {
			input.usage = append(input.usage, usage.activityRow(intern))
		}
	}
	return input, true
}
