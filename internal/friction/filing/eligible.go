// Package filing files Friction Log patterns to Kata. Only the agentsview
// hub files; the rest of this package arrives with filing.
package filing

// EligibleHost reports whether this process is the filing hub. A pg serve
// process is a hub, as is a standalone instance without a PG push target.
func EligibleHost(isPGServe, hasPushTarget bool) bool {
	return isPGServe || !hasPushTarget
}
