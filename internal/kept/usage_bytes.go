package kept

import (
	"unsafe"

	"go.kenn.io/agentsview/internal/db"
)

// TopSessionBytes estimates the memory held by a session total.
func TopSessionBytes(e db.TopSessionEntry) int64 {
	return int64(unsafe.Sizeof(e)) + stringBytes(e.SessionID, e.DisplayName, e.Agent, e.Project, e.StartedAt, e.Machine, e.GroupKey, e.GroupLabel, e.SessionName)
}

func stringBytes(values ...string) int64 {
	var n int64
	for _, v := range values {
		n += int64(len(v))
	}
	return n
}

func dailyUsageBytes(r db.DailyUsageResult) int64 {
	n := int64(unsafe.Sizeof(r))
	for key, project := range r.Projects {
		n += int64(unsafe.Sizeof(project)) + stringBytes(key, project.DisplayLabel, project.ProjectKey)
	}
	for key := range r.SessionCounts.ByProject {
		n += int64(len(key)) + 24
	}
	for key := range r.SessionCounts.ByAgent {
		n += int64(len(key)) + 24
	}
	for _, d := range r.Daily {
		n += int64(unsafe.Sizeof(d)) + int64(len(d.Date))
		for _, model := range d.ModelsUsed {
			n += int64(len(model)) + 16
		}
		for _, row := range d.ModelBreakdowns {
			n += int64(unsafe.Sizeof(row)) + int64(len(row.ModelName))
		}
		for _, row := range d.ProjectBreakdowns {
			n += int64(unsafe.Sizeof(row)) + stringBytes(row.ProjectKey, row.Project)
		}
		for _, row := range d.AgentBreakdowns {
			n += int64(unsafe.Sizeof(row)) + int64(len(row.Agent))
		}
		for _, row := range d.MachineBreakdowns {
			n += int64(unsafe.Sizeof(row)) + int64(len(row.MachineName))
		}
	}
	return n
}
