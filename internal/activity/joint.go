package activity

import (
	"cmp"
	"context"
	"slices"
	"time"
)

// JointActivityCell retains the dimensions of an activity contribution without
// retaining session identity. Project is an internal label; exporters resolve
// it through their canonical project map before serialization.
type JointActivityCell struct {
	BucketStart  time.Time
	Project      string
	ProjectKey   string
	Agent        string
	Model        string
	Category     string
	AgentMinutes float64
	MaxAgents    int
}

// AggregateCandidatesWithJointActivity also collects sparse joint cells from
// the same effective intervals as the ordinary report. Ordinary report reads
// do not pay for this extra aggregation. Usage cells remain the export owner's
// responsibility, after its complete-scope survivor and cost allocation pass.
func AggregateCandidatesWithJointActivity(
	ctx context.Context, p Params, sessions []SessionMeta,
	candidates []IntervalCandidate, usage []UsageRow,
) (Report, error) {
	joint := &jointActivityAccumulator{
		windows: rangeWindows(p), sessions: make(map[string]SessionMeta, len(sessions)),
		cells: make(map[jointActivityKey]*jointActivityState),
	}
	for _, session := range sessions {
		joint.sessions[session.SessionID] = session
	}
	artifacts, err := buildCandidateArtifactsFromSource(ctx, p, sessions,
		func(ctx context.Context, yield func(IntervalCandidate) error) error {
			for _, candidate := range candidates {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := yield(candidate); err != nil {
					return err
				}
			}
			return nil
		}, usage, false, joint)
	if err != nil {
		return Report{}, err
	}
	artifacts.Report.BySession = artifacts.Sessions
	artifacts.Report.JointActivity, err = joint.finish(ctx)
	return artifacts.Report, err
}

type jointActivityKey struct {
	bucket                int
	project, agent, model string
	category              string
}

type jointActivityState struct {
	cell   JointActivityCell
	deltas map[time.Time]int
}

type jointActivityAccumulator struct {
	windows  []BucketWindow
	sessions map[string]SessionMeta
	cells    map[jointActivityKey]*jointActivityState
}

func (a *jointActivityAccumulator) add(iv interval) {
	session := a.sessions[iv.sessionID]
	if session.Agent == "" {
		session.Agent = "unknown"
	}
	project := session.ProjectKey
	if project == "" {
		project = session.Project
	}
	category := session.ActivityCategory()
	for i := max(0, windowIndex(a.windows, iv.start)); i < len(a.windows) && a.windows[i].Start.Before(iv.end); i++ {
		window := a.windows[i]
		start, end := maxTime(iv.start, window.Start), minTime(iv.end, window.End)
		if !end.After(start) {
			continue
		}
		key := jointActivityKey{i, project, session.Agent, iv.model, category}
		state := a.cells[key]
		if state == nil {
			state = &jointActivityState{
				cell: JointActivityCell{BucketStart: window.Start, Project: session.Project, ProjectKey: session.ProjectKey,
					Agent: session.Agent, Model: iv.model, Category: category},
				deltas: make(map[time.Time]int),
			}
			a.cells[key] = state
		}
		state.cell.AgentMinutes += end.Sub(start).Minutes()
		state.deltas[start]++
		state.deltas[end]--
	}
}

func (a *jointActivityAccumulator) finish(ctx context.Context) ([]JointActivityCell, error) {
	cells := make([]JointActivityCell, 0, len(a.cells))
	for _, state := range a.cells {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		points := make([]time.Time, 0, len(state.deltas))
		for at := range state.deltas {
			points = append(points, at)
		}
		slices.SortFunc(points, time.Time.Compare)
		live := 0
		for _, at := range points {
			// Merge same-instant exits and entries: intervals are half-open.
			live += state.deltas[at]
			state.cell.MaxAgents = max(state.cell.MaxAgents, live)
		}
		cells = append(cells, state.cell)
	}
	slices.SortFunc(cells, func(a, b JointActivityCell) int {
		for _, order := range []int{a.BucketStart.Compare(b.BucketStart),
			cmp.Compare(a.Project, b.Project), cmp.Compare(a.Agent, b.Agent), cmp.Compare(a.Model, b.Model)} {
			if order != 0 {
				return order
			}
		}
		return cmp.Compare(a.Category, b.Category)
	})
	return cells, nil
}
