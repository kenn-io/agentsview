package db

import "encoding/json/v2"

// UsageSlot names the selection, excluding options applied after the read.
// Time-dependent or unencodable filters bypass reuse.
func UsageSlot(f UsageFilter, keepBreakdowns bool) string {
	if TerminationUsesTime(f.Termination) {
		return ""
	}
	f.Progress = nil
	f.ModelAliases = nil
	f.TopSessionsByGroup = false
	f.TopSessionsSort = ""
	f.TopSessionsTokenTypes = 0
	f.SkipSessionCounts, f.PairwiseSides = false, false
	if !keepBreakdowns {
		f.Breakdowns = false
	}
	key, err := json.Marshal(f, json.Deterministic(true), json.FormatNilSliceAsNull(true))
	if err != nil {
		return ""
	}
	return string(key)
}
