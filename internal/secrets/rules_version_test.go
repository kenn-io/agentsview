package secrets

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestActiveRulesVersionsSupersedePreRevocationStamps pins that stamp values
// written by binaries predating in-write scan-freshness revocation (v6 rules,
// where a transcript append left the stamp intact and a failed deferred
// rescan could strand it stale) never read as fresh again. These exact hashes
// are permanently untrustworthy: a session carrying one may have unscanned
// content behind a current-looking stamp, and rows copied later from a
// machine still running an old binary carry them too, so they must be stale
// by value — a one-time local migration could not catch those.
//
// This test lives apart from rules_test.go so it can change without
// re-uploading that file's secret-shaped scanner fixtures, which trip push
// protection on every new blob that contains them.
func TestActiveRulesVersionsSupersedePreRevocationStamps(t *testing.T) {
	preRevocationStamps := []string{
		// RulesVersion() at rulesAlgorithmVersion = 6.
		"5ed31683d8eea0bb1c18862fad98d9bc31b431ae6d63d1459da8e5cdd7df35f2",
		// DefiniteRulesVersion() at rulesAlgorithmVersion = 6.
		"0a7d7777baa25ea72b7fb79d79db8052dee1f7a2fa1e28a5384feb2ad8ac3478",
	}
	for _, stale := range preRevocationStamps {
		assert.NotContains(t, ActiveRulesVersions(), stale,
			"a pre-revocation stamp must never satisfy a freshness check")
	}
}
