package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func resetClassifierPatterns() {
	SetUserAutomationPrefixes(nil)
	SetUserAutomationSubstrings(nil)
	SetUserAutomationExactMatches(nil)
}

func TestClassifierHashStable(t *testing.T) {
	t.Cleanup(resetClassifierPatterns)
	SetUserAutomationPrefixes([]string{"foo", "bar"})
	a := ClassifierHash()
	b := ClassifierHash()
	assert.Equal(t, a, b, "hash unstable")
}

func TestClassifierHashChangesWithUserPrefixes(t *testing.T) {
	t.Cleanup(resetClassifierPatterns)
	resetClassifierPatterns()
	base := ClassifierHash()
	SetUserAutomationPrefixes([]string{"You are analyzing an essay"})
	with := ClassifierHash()
	assert.NotEqual(t, base, with,
		"hash did not change when user prefixes changed")
}

func TestClassifierHashChangesWithUserSubstrings(t *testing.T) {
	t.Cleanup(resetClassifierPatterns)
	SetUserAutomationSubstrings(nil)
	base := ClassifierHash()
	SetUserAutomationSubstrings([]string{"embedded marker"})
	with := ClassifierHash()
	assert.NotEqual(t, base, with,
		"hash did not change when user substrings changed")
}

func TestClassifierHashChangesWithUserExactMatches(t *testing.T) {
	t.Cleanup(resetClassifierPatterns)
	SetUserAutomationExactMatches(nil)
	base := ClassifierHash()
	SetUserAutomationExactMatches([]string{"Give a one-word answer: YES"})
	with := ClassifierHash()
	assert.NotEqual(t, base, with,
		"hash did not change when user exact matches changed")
}

func TestClassifierHashOrderIndependent(t *testing.T) {
	t.Cleanup(resetClassifierPatterns)
	SetUserAutomationPrefixes([]string{"alpha", "beta", "gamma"})
	a := ClassifierHash()
	SetUserAutomationPrefixes([]string{"gamma", "alpha", "beta"})
	b := ClassifierHash()
	assert.Equal(t, a, b, "hash not order-independent")
}

// TestClassifierHashTagSeparation guards against the case
// where two different categorizations produce the same hash
// because the tag prefix was dropped from the encoding.
func TestClassifierHashTagSeparation(t *testing.T) {
	t.Cleanup(resetClassifierPatterns)
	SetUserAutomationPrefixes([]string{"Warmup"})
	got := ClassifierHash()
	resetClassifierPatterns()
	bareBuiltins := ClassifierHash()
	assert.NotEqual(t, got, bareBuiltins,
		"user prefix 'Warmup' collided with built-in exact-match 'Warmup'")
}

func TestClassifierHashSeparatesUserCategories(t *testing.T) {
	t.Cleanup(resetClassifierPatterns)

	SetUserAutomationPrefixes([]string{"You are analyzing an essay"})
	prefixHash := ClassifierHash()

	resetClassifierPatterns()
	SetUserAutomationSubstrings([]string{"You are analyzing an essay"})
	substringHash := ClassifierHash()

	resetClassifierPatterns()
	SetUserAutomationExactMatches([]string{"You are analyzing an essay"})
	exactHash := ClassifierHash()

	assert.NotEqual(t, prefixHash, substringHash)
	assert.NotEqual(t, prefixHash, exactHash)
	assert.NotEqual(t, substringHash, exactHash)
}

// TestClassifierHashCurrentAlgoVersion is a forced-bump
// guard: it pins the algorithm version at construction time.
// If a future change to the matching logic forgets to bump
// classifierAlgorithmVersion, this test still passes (false
// negative) — but if someone bumps the version intentionally
// the test must be updated to match. The check exists to
// surface accidental version-constant edits during review.
func TestClassifierHashCurrentAlgoVersion(t *testing.T) {
	assert.Equal(t, 2, classifierAlgorithmVersion,
		"classifierAlgorithmVersion changed; update this test and confirm "+
			"matching semantics actually changed (not just pattern edits, "+
			"which the hash already detects)")
}
