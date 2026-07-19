package extract

import (
	"encoding/json"
	"os"
	"testing"
)

type goldenFixture struct {
	MaxWindowChars int `json:"max_window_chars"`
	Messages       []struct {
		Ordinal  int    `json:"ordinal"`
		Role     string `json:"role"`
		Content  string `json:"content"`
		IsSystem int    `json:"is_system"`
	} `json:"messages"`
	Units []struct {
		Kind         string `json:"kind"`
		Text         string `json:"text"`
		OrdinalStart int    `json:"ordinal_start"`
		OrdinalEnd   int    `json:"ordinal_end"`
	} `json:"units"`
}

// TestTurnsV1GoldenParity asserts that the segmenter reproduces the pinned
// golden units exactly. Resume cursors and entry identity both depend on this
// determinism, so any divergence here is a correctness bug, not a style
// choice.
func TestTurnsV1GoldenParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/turnsv1_golden.json")
	if err != nil {
		t.Fatalf("reading golden fixtures: %v", err)
	}
	var fixtures map[string]goldenFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("parsing golden fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures found")
	}
	for name, fixture := range fixtures {
		t.Run(name, func(t *testing.T) {
			segmenter := TurnsV1{MaxWindowChars: fixture.MaxWindowChars}
			messages := make([]Message, 0, len(fixture.Messages))
			for _, m := range fixture.Messages {
				messages = append(messages, Message{
					Ordinal:  m.Ordinal,
					Role:     m.Role,
					Content:  m.Content,
					IsSystem: m.IsSystem != 0,
				})
			}
			units := segmenter.Units(messages)
			if len(units) != len(fixture.Units) {
				t.Fatalf("unit count = %d, want %d", len(units), len(fixture.Units))
			}
			for i, want := range fixture.Units {
				got := units[i]
				if string(got.Role) != roleForKind(t, want.Kind) {
					t.Errorf("unit %d role = %q, want kind %q", i, got.Role, want.Kind)
				}
				if got.Text != want.Text {
					t.Errorf("unit %d text mismatch:\ngot:  %q\nwant: %q", i, got.Text, want.Text)
				}
				if got.OrdinalStart != want.OrdinalStart || got.OrdinalEnd != want.OrdinalEnd {
					t.Errorf("unit %d ordinals = (%d,%d), want (%d,%d)",
						i, got.OrdinalStart, got.OrdinalEnd, want.OrdinalStart, want.OrdinalEnd)
				}
			}
		})
	}
}

func roleForKind(t *testing.T, kind string) string {
	t.Helper()
	switch kind {
	case "intent":
		return string(RoleIntent)
	case "action_run":
		return string(RoleAction)
	default:
		t.Fatalf("unknown fixture unit kind %q", kind)
		return ""
	}
}

func TestTurnsV1Identity(t *testing.T) {
	segmenter := TurnsV1{MaxWindowChars: 50000}
	if segmenter.Name() != "turns-v1" {
		t.Errorf("Name() = %q, want turns-v1", segmenter.Name())
	}
	params := segmenter.Params()
	if params["max_window_chars"] != 50000 {
		t.Errorf("Params()[max_window_chars] = %v, want 50000", params["max_window_chars"])
	}
}

func TestTurnsV1PromptRoles(t *testing.T) {
	roles := TurnsV1{MaxWindowChars: 50000}.PromptRoles()
	if len(roles) != 2 || roles[0] != RoleIntent || roles[1] != RoleAction {
		t.Errorf("PromptRoles() = %v, want [intent action]", roles)
	}
}

func TestTurnsV1EmptySession(t *testing.T) {
	units := TurnsV1{MaxWindowChars: 50000}.Units(nil)
	if len(units) != 0 {
		t.Errorf("Units(nil) = %d units, want 0", len(units))
	}
}

// TestTurnsV1SplitsActionRunsAtOrdinalGaps pins that a run of assistant
// messages never packs across a missing ordinal. Ingest filtering can drop
// rows after ordinals are assigned (e.g. tool-result-only user messages), and
// evidence provenance requires gap-free transcript ranges — a unit spanning
// the hole would fail verification on every commit attempt.
func TestTurnsV1SplitsActionRunsAtOrdinalGaps(t *testing.T) {
	units := TurnsV1{MaxWindowChars: 50000}.Units([]Message{
		{Ordinal: 0, Role: "user", Content: "fix the bug"},
		{Ordinal: 1, Role: "assistant", Content: "first step"},
		{Ordinal: 3, Role: "assistant", Content: "second step"},
	})
	if len(units) != 3 {
		t.Fatalf("unit count = %d, want 3 (intent + one action unit per "+
			"side of the gap)", len(units))
	}
	first, second := units[1], units[2]
	if first.Role != RoleAction || first.OrdinalStart != 1 || first.OrdinalEnd != 1 {
		t.Errorf("unit 1 = %s (%d,%d), want action (1,1)",
			first.Role, first.OrdinalStart, first.OrdinalEnd)
	}
	if second.Role != RoleAction || second.OrdinalStart != 3 || second.OrdinalEnd != 3 {
		t.Errorf("unit 2 = %s (%d,%d), want action (3,3)",
			second.Role, second.OrdinalStart, second.OrdinalEnd)
	}
}

// TestTurnsV1PacksRunsAcrossSkippedRows pins the complement: system and
// empty rows are skipped from unit text but still occupy their ordinals in
// the stored transcript, so a run packed across them stays verifiable and
// must not be split.
func TestTurnsV1PacksRunsAcrossSkippedRows(t *testing.T) {
	units := TurnsV1{MaxWindowChars: 50000}.Units([]Message{
		{Ordinal: 0, Role: "assistant", Content: "a"},
		{Ordinal: 1, Role: "assistant", Content: "   "},
		{Ordinal: 2, Role: "user", Content: "sys note", IsSystem: true},
		{Ordinal: 3, Role: "assistant", Content: "b"},
	})
	if len(units) != 1 {
		t.Fatalf("unit count = %d, want 1 (skipped rows keep the run "+
			"contiguous)", len(units))
	}
	if units[0].OrdinalStart != 0 || units[0].OrdinalEnd != 3 {
		t.Errorf("unit range = (%d,%d), want (0,3)",
			units[0].OrdinalStart, units[0].OrdinalEnd)
	}
}
