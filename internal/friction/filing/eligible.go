// Package filing files Friction Log patterns to Kata and keeps each
// pattern's issue link. Only the AgentsView hub files, so two machines that
// share an archive never file the same pattern twice.
package filing

// EligibleHost reports whether this process is the filing hub. A pg serve
// process is a hub, as is a standalone instance without a PG push target.
func EligibleHost(isPGServe, hasPushTarget bool) bool {
	return isPGServe || !hasPushTarget
}
