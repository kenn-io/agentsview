package rawparity

import (
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"hegel.dev/go/hegel"
)

func relationshipSnapshot(kind, alias string) Snapshot {
	s := Snapshot{Messages: []db.Message{{Ordinal: 3, ToolCalls: []db.ToolCall{{CallIndex: 2, ResultEvents: []db.ToolResultEvent{{EventIndex: 4}}}}}}}
	switch kind {
	case "parent":
		s.Session.ParentSessionID = new(alias)
	case "call":
		s.Messages[0].ToolCalls[0].SubagentSessionID = alias
	case "event":
		s.Messages[0].ToolCalls[0].ResultEvents[0].SubagentSessionID = alias
	}
	return s
}

// A relationship means its source-owned physical targets, not its alias text.
// Missing proof, unmapped physical identity, or an extra target cannot compare
// equal to the single legacy target, even with identical stored alias strings.
func TestCompareResolvedMeaningProperty(t *testing.T) {
	for _, kind := range []string{"parent", "call", "event"} {
		t.Run(kind, func(t *testing.T) {
			propertyTest(t, func(ht *hegel.T) {
				alias := "target:" + hegel.Draw(ht, hegel.Text())
				physical := "physical:" + hegel.Draw(ht, hegel.Text())
				state := hegel.Draw(ht, hegel.Integers(0, 3)) // missing, mapped, unmapped, plural
				missing := hegel.Draw(ht, hegel.Booleans())
				link := Link{Kind: kind, Ordinal: 3, CallIndex: 2, EventIndex: 4, Missing: missing}
				if state != 0 {
					link.Targets = []string{physical}
				}
				if state == 3 {
					link.Targets = append(link.Targets, "other:"+physical)
				}
				matches := map[string]string{}
				if state == 1 || state == 3 {
					matches[physical] = alias
				}
				snapshot := relationshipSnapshot(kind, alias)
				got, err := CompareResolved(snapshot, snapshot, []Link{link}, matches)
				require.NoError(ht, err)
				if state == 1 && !missing {
					assert.Empty(ht, got.Differing)
				} else {
					assert.Equal(ht, []string{"relationships"}, got.Differing)
				}
			}, hegel.WithTestCases(1000))
		})
	}
}

func TestCompareResolvedRequiresEvidence(t *testing.T) {
	for _, kind := range []string{"parent", "call", "event"} {
		s := relationshipSnapshot(kind, "same-literal")
		got, err := CompareResolved(s, s, nil, map[string]string{"same-literal": "same-literal"})
		require.NoError(t, err)
		assert.Equal(t, []string{"relationships"}, got.Differing, kind)
	}
}

func TestCompareResolvedIdentityDomainCollision(t *testing.T) {
	for _, kind := range []string{"parent", "call", "event"} {
		t.Run(kind, func(t *testing.T) {
			snapshot := relationshipSnapshot(kind, "same-id")
			links := []Link{{Kind: kind, Ordinal: 3, CallIndex: 2, EventIndex: 4, Targets: []string{"same-id"}, Missing: false}}
			unmapped, err := CompareResolved(snapshot, snapshot, links, map[string]string{})
			require.NoError(t, err)
			assert.Equal(t, []string{"relationships"}, unmapped.Differing)

			mapped, err := CompareResolved(snapshot, snapshot, links, map[string]string{"same-id": "same-id"})
			require.NoError(t, err)
			assert.Empty(t, mapped.Differing)
		})
	}
}

func TestCompareResolvedPreservesSnapshotsProperty(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		alias := "target:" + hegel.Draw(ht, hegel.Text())
		kind := hegel.Draw(ht, hegel.SampledFrom([]string{"parent", "call", "event"}))
		s := relationshipSnapshot(kind, alias)

		link := Link{Kind: kind, Ordinal: 3, CallIndex: 2, EventIndex: 4, Targets: []string{"physical"}}
		if hegel.Draw(ht, hegel.Booleans()) {
			link.Kind = "unknown"
		}
		links := []Link{link}
		matches := map[string]string{"physical": alias}
		_, err := CompareResolved(s, s, links, matches)
		if link.Kind == "unknown" {
			require.Error(ht, err)
		} else {
			require.NoError(ht, err)
		}
		assert.Equal(ht, relationshipSnapshot(kind, alias), s)
		assert.Equal(ht, []Link{{Kind: link.Kind, Ordinal: 3, CallIndex: 2, EventIndex: 4, Targets: []string{"physical"}}}, links)
		assert.Equal(ht, map[string]string{"physical": alias}, matches)
	}, hegel.WithTestCases(1000))
}

// Input order and duplicate source proofs do not change a resolved parent set.
func TestCompareResolvedParentSetProperty(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		names := hegel.Draw(ht, hegel.Lists(hegel.Text()))
		raw, legacy := Snapshot{}, Snapshot{}
		matches := map[string]string{}
		targets := []string{}
		for _, name := range names {
			physical, alias := "physical:"+name, "legacy:"+name
			targets = append(targets, physical, physical)
			matches[physical] = alias
			legacy.Session.ParentSessionIDs = append(legacy.Session.ParentSessionIDs, alias)
		}
		slices.Reverse(targets)
		var links []Link
		if len(targets) > 0 {
			links = []Link{{Kind: "parent", Targets: targets}}
		}
		got, err := CompareResolved(raw, legacy, links, matches)
		require.NoError(ht, err)
		assert.Empty(ht, got.Differing)
	}, hegel.WithTestCases(1000))
}

func TestCompareResolvedRejectsInvalidCoordinates(t *testing.T) {
	s := relationshipSnapshot("call", "target")
	for _, link := range []Link{
		{Kind: "unknown"},
		{Kind: "call", Ordinal: 9, CallIndex: 2},
		{Kind: "call", Ordinal: 3, CallIndex: 9},
		{Kind: "event", Ordinal: 3, CallIndex: 2, EventIndex: 9},
	} {
		_, err := CompareResolved(s, s, []Link{link}, nil)
		require.Error(t, err, link)
	}
	link := Link{Kind: "call", Ordinal: 3, CallIndex: 2, Targets: []string{"physical"}}
	_, err := CompareResolved(s, s, []Link{link, link}, map[string]string{"physical": "target"})
	require.ErrorContains(t, err, "duplicate")
	conflicting := link
	conflicting.EventIndex = 99 // call edges have no event coordinate
	conflicting.Missing = true
	_, err = CompareResolved(s, s, []Link{conflicting, link}, map[string]string{"physical": "target"})
	require.ErrorContains(t, err, "duplicate")
}

func TestCompareResolvedCoordinatesProperty(t *testing.T) {
	for _, kind := range []string{"call", "event"} {
		t.Run(kind, func(t *testing.T) {
			propertyTest(t, func(ht *hegel.T) {
				alias := hegel.Draw(ht, hegel.SampledFrom([]string{"\x00unresolved", "\x00raw:physical", "\x00plural:[]", "\x00legacy:target"})) + hegel.Draw(ht, hegel.Text())
				ordinal := hegel.Draw(ht, hegel.Integers(0, math.MaxInt-1))
				raw, legacy := relationshipSnapshot(kind, alias), relationshipSnapshot(kind, alias)
				raw.Messages[0].Ordinal = ordinal
				legacy.Messages[0].Ordinal = ordinal
				// Same content at a second coordinate distinguishes identity from order.
				rawOther, legacyOther := relationshipSnapshot(kind, "").Messages[0], relationshipSnapshot(kind, "").Messages[0]
				rawOther.Ordinal = ordinal + 1
				legacyOther.Ordinal = ordinal + 1
				raw.Messages = append([]db.Message{rawOther}, raw.Messages...)
				legacy.Messages = append(legacy.Messages, legacyOther)
				link := Link{Kind: kind, Ordinal: ordinal, CallIndex: 2, EventIndex: 4, Targets: []string{"physical"}}
				got, err := CompareResolved(raw, legacy, []Link{link}, map[string]string{"physical": alias})
				require.NoError(ht, err)
				assert.Empty(ht, got.Differing)
				link.Ordinal++
				moved, err := CompareResolved(raw, legacy, []Link{link}, map[string]string{"physical": alias})
				require.NoError(ht, err)
				assert.Equal(ht, []string{"relationships"}, moved.Differing)
			}, hegel.WithTestCases(1000))
		})
	}
}

func TestCompareResolvedRejectsMalformedTarget(t *testing.T) {
	_, err := CompareResolved(relationshipSnapshot("call", "target"), relationshipSnapshot("call", "target"),
		[]Link{{Kind: "call", Ordinal: 3, CallIndex: 2, Targets: []string{"valid", "\xff"}}}, nil)
	assert.Error(t, err)
}

// No relationship row cap may discard late coordinates in a long transcript.
func TestCompareResolvedCompleteCoordinates(t *testing.T) {
	raw, legacy := Snapshot{}, Snapshot{}
	links := make([]Link, 10000)
	for i := range links {
		raw.Messages = append(raw.Messages, db.Message{Ordinal: i * 3, ToolCalls: []db.ToolCall{{CallIndex: 7, SubagentSessionID: "target"}}})
		legacy.Messages = append(legacy.Messages, db.Message{Ordinal: i * 3, ToolCalls: []db.ToolCall{{CallIndex: 7, SubagentSessionID: "target"}}})
		links[i] = Link{Kind: "call", Ordinal: i * 3, CallIndex: 7, Targets: []string{"physical"}}
	}
	got, err := CompareResolved(raw, legacy, links, map[string]string{"physical": "target"})
	require.NoError(t, err)
	assert.Empty(t, got.Differing)
	links[len(links)-1].Missing = true
	got, err = CompareResolved(raw, legacy, links, map[string]string{"physical": "target"})
	require.NoError(t, err)
	assert.Equal(t, []string{"relationships"}, got.Differing)
}

func TestCompareResolvedSiblingCoordinatesProperty(t *testing.T) {
	for _, kind := range []string{"call", "event"} {
		t.Run(kind, func(t *testing.T) {
			propertyTest(t, func(ht *hegel.T) {
				first, second := "first:"+hegel.Draw(ht, hegel.Text()), "second:"+hegel.Draw(ht, hegel.Text())
				raw, legacy := relationshipSnapshot(kind, first), relationshipSnapshot(kind, first)
				links := []Link{{Kind: kind, Ordinal: 3, CallIndex: 2, EventIndex: 4, Targets: []string{"one"}}, {Kind: kind, Ordinal: 3, CallIndex: 2, EventIndex: 5, Targets: []string{"two"}}}
				if kind == "call" {
					raw.Messages[0].ToolCalls = append(raw.Messages[0].ToolCalls, db.ToolCall{CallIndex: 5, SubagentSessionID: second})
					legacy.Messages[0].ToolCalls = append(legacy.Messages[0].ToolCalls, db.ToolCall{CallIndex: 5, SubagentSessionID: second})
					links[1].CallIndex = 5
					if hegel.Draw(ht, hegel.Booleans()) {
						slices.Reverse(raw.Messages[0].ToolCalls)
					}
				} else {
					raw.Messages[0].ToolCalls[0].ResultEvents = append(raw.Messages[0].ToolCalls[0].ResultEvents, db.ToolResultEvent{EventIndex: 5, SubagentSessionID: second})
					legacy.Messages[0].ToolCalls[0].ResultEvents = append(legacy.Messages[0].ToolCalls[0].ResultEvents, db.ToolResultEvent{EventIndex: 5, SubagentSessionID: second})
					if hegel.Draw(ht, hegel.Booleans()) {
						slices.Reverse(raw.Messages[0].ToolCalls[0].ResultEvents)
					}
				}
				matches := map[string]string{"one": first, "two": second}
				got, err := CompareResolved(raw, legacy, links, matches)
				require.NoError(ht, err)
				assert.Empty(ht, got.Differing)
				links[0].Targets, links[1].Targets = links[1].Targets, links[0].Targets
				swapped, err := CompareResolved(raw, legacy, links, matches)
				require.NoError(ht, err)
				assert.Equal(ht, []string{"relationships"}, swapped.Differing)
			}, hegel.WithTestCases(1000))
		})
	}
}
