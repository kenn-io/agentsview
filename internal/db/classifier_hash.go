package db

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"
)

// classifierAlgorithmVersion bumps when the matching *logic*
// changes (e.g. a future case-insensitivity flag). Pattern
// edits do NOT bump this — those are detected automatically
// by including the pattern slices in the hash. Bumping this
// constant invalidates every stored hash and forces a
// backfill on next open of any DB.
//
// (2: heals DBs poisoned by the orphan-copy classification gap
// in ResyncAll prior to the ForceBackfillIsAutomated wiring.
// Without this bump, those DBs already have the v1 hash stored
// and would skip the backfill on Open.)
const classifierAlgorithmVersion = 2

// ClassifierHash returns a stable hex-encoded SHA-256 over
// the algorithm version, all built-in pattern slices, and the
// currently configured user patterns. Inputs are sorted
// before hashing so config order doesn't affect the result.
// Tagged + length-prefixed encoding prevents splice
// collisions between slice boundaries.
func ClassifierHash() string {
	h := sha256.New()
	fmt.Fprintf(h, "v%d\n", classifierAlgorithmVersion)
	writeSorted(h, "P", automatedPrefixes)
	writeSorted(h, "S", automatedSubstrings)
	writeSorted(h, "E", automatedExactMatches)
	writeSorted(h, "UP", UserAutomationPrefixes())
	writeSorted(h, "US", UserAutomationSubstrings())
	writeSorted(h, "UE", UserAutomationExactMatches())
	return hex.EncodeToString(h.Sum(nil))
}

func writeSorted(h hash.Hash, tag string, items []string) {
	sorted := append([]string(nil), items...)
	sort.Strings(sorted)
	for _, s := range sorted {
		fmt.Fprintf(h, "%s\t%d\t%s\n", tag, len(s), s)
	}
}
