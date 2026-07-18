package extract

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveProfileMatchesModelName(t *testing.T) {
	profile, err := ResolveProfile("", "qwen3.6-27b-mtp")
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if profile.Name != "qwen" {
		t.Fatalf("profile = %q, want qwen", profile.Name)
	}
	body, ok := profile.Request.ExtraBody["chat_template_kwargs"]
	if !ok {
		t.Fatal("qwen profile must carry chat_template_kwargs")
	}
	kwargs, ok := body.(map[string]any)
	if !ok || kwargs["enable_thinking"] != false {
		t.Fatalf("qwen profile must disable thinking, got %v", body)
	}
}

func TestResolveProfileFallsBackToBase(t *testing.T) {
	profile, err := ResolveProfile("", "some-foundation-model")
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if profile.Name != "base" {
		t.Fatalf("profile = %q, want base", profile.Name)
	}
	if profile.Request.MaxTokens <= 0 {
		t.Fatalf("base profile MaxTokens = %d, must be a working default",
			profile.Request.MaxTokens)
	}
}

func TestResolveProfileExplicitWinsAndUnknownErrors(t *testing.T) {
	profile, err := ResolveProfile("base", "qwen3.6-27b-mtp")
	if err != nil || profile.Name != "base" {
		t.Fatalf("explicit base: profile=%v err=%v", profile.Name, err)
	}
	if _, err := ResolveProfile("nonexistent", "m"); err == nil {
		t.Fatal("unknown explicit profile must error")
	}
}

func TestResolveProfileCopiesAreIsolated(t *testing.T) {
	// Resolved profiles get mutated by callers (merging config, editing
	// request shape); that must never write through to the built-in
	// registry and corrupt later resolutions.
	first, err := ResolveProfile("", "qwen3.6-27b-mtp")
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	kwargs, ok := first.Request.ExtraBody["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatal("qwen profile must carry chat_template_kwargs")
	}
	kwargs["enable_thinking"] = true
	first.Request.ExtraBody["injected"] = "x"
	first.MatchPrefixes[0] = "mutated"

	second, err := ResolveProfile("", "qwen3.6-27b-mtp")
	if err != nil {
		t.Fatalf("ResolveProfile after mutation: %v", err)
	}
	if second.Name != "qwen" {
		t.Fatalf("profile = %q, prefix mutation must not leak into registry",
			second.Name)
	}
	if _, ok := second.Request.ExtraBody["injected"]; ok {
		t.Fatal("top-level extra-body mutation leaked into registry")
	}
	secondKwargs := second.Request.ExtraBody["chat_template_kwargs"].(map[string]any)
	if secondKwargs["enable_thinking"] != false {
		t.Fatal("nested extra-body mutation leaked into registry")
	}
}

func TestPromptsForMergesProfileAndOverrides(t *testing.T) {
	base, err := ResolveProfile("base", "m")
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	prompts := PromptsFor(base, map[PromptRole]string{RoleIntent: "override"})
	if prompts[RoleIntent] != "override" {
		t.Fatalf("override must win, got %q", prompts[RoleIntent])
	}
	if prompts[RoleAction] == "" {
		t.Fatal("unoverridden roles keep the base prompt")
	}
}

func TestLoadPromptOverridesReadsRoleFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, "intent.txt"), []byte("custom intent\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	overrides, err := LoadPromptOverrides(dir)
	if err != nil {
		t.Fatalf("LoadPromptOverrides: %v", err)
	}
	if overrides[RoleIntent] != "custom intent" {
		t.Fatalf("intent override = %q", overrides[RoleIntent])
	}
	if _, ok := overrides[RoleAction]; ok {
		t.Fatal("absent files must not produce overrides")
	}
}

func TestFingerprintIsStableAndSensitive(t *testing.T) {
	seg := TurnsV1{MaxWindowChars: 50000}
	prompts := PromptsFor(mustProfile(t, "base"), nil)
	shape := RequestShape{Temperature: 0}
	id := ModelIdentity{Model: "model-x"}

	a, err := Fingerprint(id, seg, prompts, shape)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	b, err := Fingerprint(id, seg, prompts, shape)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if a != b {
		t.Fatalf("fingerprint not stable: %s vs %s", a, b)
	}

	changedModel, _ := Fingerprint(
		ModelIdentity{Model: "model-y"}, seg, prompts, shape,
	)
	if changedModel == a {
		t.Fatal("model change must change the fingerprint")
	}
	changedDeployment, _ := Fingerprint(
		ModelIdentity{Model: "model-x", Deployment: "gpu-b"},
		seg, prompts, shape,
	)
	if changedDeployment == a {
		t.Fatal("deployment label change must change the fingerprint: two " +
			"deployments can serve different weights under one model name")
	}
	changedSeg, _ := Fingerprint(
		id, TurnsV1{MaxWindowChars: 40000}, prompts, shape,
	)
	if changedSeg == a {
		t.Fatal("segmenter parameter change must change the fingerprint")
	}
	changedPrompt, _ := Fingerprint(
		id, seg,
		PromptsFor(mustProfile(t, "base"),
			map[PromptRole]string{RoleIntent: "edited"}),
		shape,
	)
	if changedPrompt == a {
		t.Fatal("prompt change must change the fingerprint")
	}
	changedTokens, _ := Fingerprint(
		id, seg, prompts, RequestShape{Temperature: 0, MaxTokens: 512},
	)
	if changedTokens == a {
		t.Fatal("max_tokens change must change the fingerprint")
	}
}

func mustProfile(t *testing.T, name string) Profile {
	t.Helper()
	profile, err := ResolveProfile(name, "")
	if err != nil {
		t.Fatalf("ResolveProfile(%s): %v", name, err)
	}
	return profile
}
