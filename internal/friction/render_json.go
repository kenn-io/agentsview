package friction

import (
	"fmt"

	"go.kenn.io/agentsview/internal/serdejson"
)

// SummarySchemaVersion identifies AgentsView's own digest summary schema.
const SummarySchemaVersion = 1

// RenderSummaryJSON renders counts and P0 alerts with sorted keys.
func RenderSummaryJSON(s DigestSnapshot) []byte {
	groups := groupByKind(s.Signals)
	value := map[string]any{
		"date":             s.Date,
		"schema_version":   SummarySchemaVersion,
		"sessions_scanned": s.SessionsScanned,
		"corrections":      len(groups[KindCorrection]),
		"errors":           len(groups[KindError]),
		"workarounds":      len(groups[KindWorkaround]),
		"deferrals":        len(groups[KindDeferral]),
		"patterns":         len(groups[KindPattern]),
		"frustrations":     len(groups[KindFrustration]),
		"interruptions":    len(groups[KindInterruption]),
		"p0_alerts":        s.P0Alerts,
	}
	out, err := serdejson.Pretty(value)
	if err != nil {
		panic(fmt.Sprintf("friction: summary JSON: %v", err))
	}
	return append(out, '\n')
}
