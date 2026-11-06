package kept

import (
	"unsafe"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

// TopSessionBytes estimates the memory held by a session total.
func TopSessionBytes(e db.TopSessionEntry) int64 {
	return int64(unsafe.Sizeof(e)) + StringBytes(e.SessionID, e.DisplayName, e.Agent, e.Project, e.StartedAt, e.Machine, e.GroupKey, e.GroupLabel, e.SessionName)
}

// StringBytes counts the bytes held by strings, excluding their headers.
func StringBytes(values ...string) int64 {
	var n int64
	for _, v := range values {
		n += int64(len(v))
	}
	return n
}

func planUsageBytes(r db.PlanUsageSessionCounts) int64 {
	n := int64(unsafe.Sizeof(r))
	for model := range r.PlanOnlyByModel {
		n += int64(len(model)) + 24
	}
	return n
}

func dailyUsageBytes(r db.DailyUsageResult) int64 {
	n := int64(unsafe.Sizeof(r))
	if r.Pricing != nil {
		n += pricingBytes(*r.Pricing)
	}
	for key, project := range r.Projects {
		n += int64(unsafe.Sizeof(project)) + StringBytes(key, project.DisplayLabel, project.ProjectKey, string(project.Resolution))
		if identity := project.Identity; identity != nil {
			n += int64(unsafe.Sizeof(*identity)) + StringBytes(identity.Key, string(identity.Kind), identity.NormalizedRemote, identity.RootKey, identity.RepositoryKey)
		}
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
			n += int64(unsafe.Sizeof(row)) + StringBytes(row.ProjectKey, row.Project)
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

func pricingBytes(p export.PricingBlock) int64 {
	n := int64(unsafe.Sizeof(p)) + StringBytes(p.Source, p.TableVersion, p.Digest, string(p.CostSource))
	if p.LatestRowUpdatedAt != nil {
		n += int64(unsafe.Sizeof(*p.LatestRowUpdatedAt))
	}
	for _, model := range p.Fallback.Models {
		n += int64(unsafe.Sizeof(model)) + int64(len(model))
	}
	for model, provenance := range p.Models {
		n += int64(unsafe.Sizeof(model)+unsafe.Sizeof(provenance)) + StringBytes(model, string(provenance.CostSource))
		for _, rate := range provenance.Resolutions {
			n += int64(unsafe.Sizeof(rate)) + StringBytes(rate.PricedModel, string(rate.CostSource))
			if rate.MatchedPattern != nil {
				n += int64(unsafe.Sizeof(*rate.MatchedPattern)) + int64(len(*rate.MatchedPattern))
			}
			for _, band := range rate.Bands {
				n += int64(unsafe.Sizeof(band))
				if band.UpdatedAt != nil {
					n += int64(unsafe.Sizeof(*band.UpdatedAt))
				}
			}
			n += int64(len(rate.Application.Bands)) * int64(unsafe.Sizeof(export.AppliedPricingBand{}))
		}
	}
	return n
}
