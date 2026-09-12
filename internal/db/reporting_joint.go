package db

import (
	"fmt"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// Apply scope after the complete day's survivor selection. Filtering duplicate
// candidates first would let a second project claim the same charged usage.
func scopeJointReporting(
	sessions []activity.SessionMeta, events []activity.ActivityEvent,
	usage []activity.UsageRow, sessionByID map[string]activity.SessionMeta,
	projects map[string]export.ProjectMapEntry, keys []string,
) ([]activity.SessionMeta, []string, []activity.ActivityEvent, []activity.UsageRow) {
	selectedKeys := make(map[string]bool, len(keys))
	for _, key := range keys {
		selectedKeys[key] = true
	}
	selected := func(project string) bool {
		return len(keys) == 0 || selectedKeys[export.ProjectKeyForEntry(projects[project])]
	}
	keptSessions := make([]activity.SessionMeta, 0, len(sessions))
	ids := make([]string, 0, len(sessions))
	for _, session := range sessions {
		if selected(session.Project) {
			keptSessions = append(keptSessions, session)
			ids = append(ids, session.SessionID)
		}
	}
	keptEvents := make([]activity.ActivityEvent, 0, len(events))
	for _, event := range events {
		session, known := sessionByID[event.SessionID]
		if len(keys) == 0 || known && selected(session.Project) {
			keptEvents = append(keptEvents, event)
		}
	}
	keptUsage := make([]activity.UsageRow, 0, len(usage))
	for _, row := range usage {
		session, known := sessionByID[row.SessionID]
		if len(keys) == 0 || known && selected(session.Project) {
			keptUsage = append(keptUsage, row)
		}
	}
	return keptSessions, ids, keptEvents, keptUsage
}

type reportingCellKey struct {
	bucket                               string
	projectKey, agent, model, automation string
}

type reportingCellState struct {
	cell  export.ReportingCell
	usage reportingUsageAccum
}

func jointReportingHour(
	start time.Time, bucket time.Duration, activityCells []activity.JointActivityCell, usage []activity.UsageRow,
	sessions map[string]activity.SessionMeta, projects map[string]export.ProjectMapEntry,
	projectKeys []string,
) (*export.ReportingJoint, error) {
	states := make(map[reportingCellKey]*reportingCellState)
	cellFor := func(bucket time.Time, project, projectKey, agent, model, automation string) *reportingCellState {
		if model == "" {
			model = "unknown"
		}
		if agent == "" {
			agent = "unknown"
		}
		key := reportingCellKey{bucket.UTC().Format(time.RFC3339),
			projectKey, agent, model, automation}
		state := states[key]
		label := export.SafeProjectDisplayLabel(project)
		if state == nil {
			state = &reportingCellState{cell: export.ReportingCell{
				BucketStart: key.bucket, Project: label, ProjectKey: key.projectKey,
				Agent: agent, Model: model, Automation: automation,
			}}
			states[key] = state
		} else if label < state.cell.Project {
			state.cell.Project = label
		}
		return state
	}
	for _, cell := range activityCells {
		state := cellFor(cell.BucketStart, cell.Project, cell.ProjectKey, cell.Agent, cell.Model, cell.Category)
		state.cell.AgentMinutes += cell.AgentMinutes
		state.cell.MaxAgents = cell.MaxAgents
	}
	end := start.Add(time.Hour)
	for _, row := range usage {
		at, err := parseTimestamp(row.Timestamp)
		if err != nil || at.Before(start) || !at.Before(end) {
			continue
		}
		session, known := sessions[row.SessionID]
		agent, automation := row.Agent, "unknown"
		projectKey := ""
		if known {
			projectKey = export.ProjectKeyForEntry(projects[session.Project])
			automation = session.ActivityCategory()
			if agent == "" {
				agent = session.Agent
			}
		}
		state := cellFor(at.UTC().Truncate(bucket), session.Project, projectKey, agent, row.Model, automation)
		if err := state.usage.add(row); err != nil {
			return nil, fmt.Errorf("sum joint cell usage: %w", err)
		}
		if !row.Priced {
			state.cell.Pricing.UnpricedRows++
		}
		// Token pricing may be unknown while a service fee is still known.
		// Keep that fee in the cost partition as well as the usage total.
		cost := &state.cell.Pricing.ComputedCost
		switch {
		case row.CostAllocated:
			cost = &state.cell.Pricing.AllocatedCost
		case row.CostSource == export.CostSourceReported:
			cost = &state.cell.Pricing.ReportedCost
		}
		*cost, err = money.Add(*cost, row.Cost)
		if err != nil {
			return nil, fmt.Errorf("sum joint cell pricing: %w", err)
		}
	}
	keys := slices.Clone(projectKeys)
	slices.Sort(keys)
	joint := &export.ReportingJoint{ProjectKeys: slices.Compact(keys), Cells: make([]export.ReportingCell, 0, len(states))}
	for _, state := range states {
		state.cell.Usage = export.ReportingUsageTotals{
			InputTokens: state.usage.inputTokens, OutputTokens: state.usage.outputTokens,
			CacheCreationTokens: state.usage.cacheCreationTokens, CacheReadTokens: state.usage.cacheReadTokens,
			Cost: state.usage.cost,
		}
		joint.Cells = append(joint.Cells, state.cell)
	}
	return joint, nil
}
