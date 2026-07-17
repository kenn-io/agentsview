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
