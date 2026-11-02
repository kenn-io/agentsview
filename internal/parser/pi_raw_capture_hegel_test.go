//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64))

package parser

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Physical custody is independent of normalized header validity.
func TestPiRawCaptureMalformedCustodyProperty(t *testing.T) {
	for _, agent := range []AgentType{AgentPi, AgentOMP, AgentOMO, AgentStepCode, AgentPrimeAgent} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			dir := root
			if agent != AgentPrimeAgent {
				dir = filepath.Join(root, "project")
			}
			require.NoError(t, os.MkdirAll(dir, 0o700))
			outside := filepath.Join(t.TempDir(), "outside.jsonl")
			require.NoError(t, os.WriteFile(outside, []byte(`{"type":"session","id":"outside-header"}`+"\n"), 0o600))
			file := filepath.Join(dir, "invalid.jsonl")
			provider, _ := NewProvider(agent, ProviderConfig{Roots: []string{root}})
			hegel.Test(t, func(ht *hegel.T) {
				bytes := hegel.Draw(ht, hegel.Lists(hegel.Integers(uint8(0), uint8(math.MaxUint8))))
				// Limit bytes written per example while drawing the entire byte domain.
				if len(bytes) > 8192 {
					bytes = bytes[:8192]
				}
				contents := append([]byte("invalid session header\n"), bytes...)
				if hegel.Draw(ht, hegel.Booleans()) {
					contents = []byte(`{"type":"session","parentSession":` + strconv.Quote(outside) + `,"payload":` + strconv.Quote(string(bytes)))
				}
				require.NoError(ht, os.WriteFile(file, contents, 0o600))
				discovery, err := DiscoverRawCaptureSources(ht.Context(), provider)
				require.NoError(ht, err)
				require.True(ht, discovery.Complete)
				require.Len(ht, discovery.Sources, 1)
				plan, supported, err := ResolveRawCapturePlan(ht.Context(), provider, discovery.Sources[0])
				require.NoError(ht, err)
				require.True(ht, supported)
				require.Len(ht, plan.Entries, 1)
				expectedPath := "project/invalid.jsonl"
				if agent == AgentPrimeAgent {
					expectedPath = "invalid.jsonl"
				}
				assert.Equal(ht, expectedPath, plan.Entries[0].Path)
				captured, err := os.ReadFile(plan.Entries[0].LocalPath)
				require.NoError(ht, err)
				assert.Equal(ht, contents, captured)
				_, err = provider.Parse(ht.Context(), ParseRequest{Source: discovery.Sources[0]})
				assert.Error(ht, err)
			}, hegel.WithTestCases(1000))
		})
	}
}

func TestPiRawCaptureRejectsOutsideParentProperty(t *testing.T) {
	for _, agent := range []AgentType{AgentPi, AgentOMP, AgentOMO, AgentStepCode, AgentPrimeAgent} {
		t.Run(string(agent), func(t *testing.T) {
			base := t.TempDir()
			hegel.Test(t, func(ht *hegel.T) {
				name := strconv.FormatUint(hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64))), 16)
				extraRoot := hegel.Draw(ht, hegel.Booleans())
				root, err := os.MkdirTemp(base, "root-")
				require.NoError(ht, err)
				defer os.RemoveAll(root)
				outside, err := os.MkdirTemp(base, "outside-")
				require.NoError(ht, err)
				defer os.RemoveAll(outside)
				parent := filepath.Join(outside, name+".jsonl")
				require.NoError(ht, os.WriteFile(parent, []byte(`{"type":"session","id":"outside-header"}`+"\n"), 0o600))
				dir := root
				if agent != AgentPrimeAgent {
					dir = filepath.Join(root, "project")
				}
				require.NoError(ht, os.MkdirAll(dir, 0o700))
				child := filepath.Join(dir, "child.jsonl")
				require.NoError(ht, os.WriteFile(child, []byte(`{"type":"session","id":"child","parentSession":`+strconv.Quote(parent)+"}\n"), 0o600))
				roots := []string{root}
				if extraRoot {
					roots = append(roots, outside)
				}
				provider, _ := NewProvider(agent, ProviderConfig{Roots: roots})
				sources, err := RawCaptureSourcesForChangedPath(ht.Context(), provider, ChangedPathRequest{Path: child})
				require.NoError(ht, err)
				require.Len(ht, sources, 1)
				_, _, err = ResolveRawCapturePlan(ht.Context(), provider, sources[0])
				assert.ErrorIs(ht, err, ErrInvalidRawCapturePlan)
			}, hegel.WithTestCases(1000))
		})
	}
}

// Cancellation is never a complete inventory, including an empty root set.
func TestPiRawCaptureCancelledInventoryProperty(t *testing.T) {
	for _, agent := range []AgentType{AgentPi, AgentOMP, AgentOMO, AgentStepCode, AgentPrimeAgent} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			hegel.Test(t, func(ht *hegel.T) {
				roots := hegel.Draw(ht, hegel.Lists(hegel.Booleans()))
				if len(roots) > 32 {
					roots = roots[:32]
				}
				configured := make([]string, len(roots))
				for i, missing := range roots {
					configured[i] = root
					if missing {
						configured[i] = filepath.Join(root, "missing")
					}
				}
				provider, _ := NewProvider(agent, ProviderConfig{Roots: configured})
				ctx, cancel := context.WithCancel(ht.Context())
				cancel()
				inventory, err := DiscoverRawCaptureSources(ctx, provider)
				assert.ErrorIs(ht, err, context.Canceled)
				assert.False(ht, inventory.Complete)
			}, hegel.WithTestCases(1000))
		})
	}
}

// Manifest v1 must not infer a companion as the primary from a longer suffix.
func TestPiRawCaptureAmbiguousPrimaryProperty(t *testing.T) {
	for _, agent := range []AgentType{AgentPi, AgentOMP, AgentOMO, AgentStepCode, AgentPrimeAgent} {
		t.Run(string(agent), func(t *testing.T) {
			base := t.TempDir()
			hegel.Test(t, func(ht *hegel.T) {
				name := strconv.FormatUint(hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64))), 16)
				root, err := os.MkdirTemp(base, "root-")
				require.NoError(ht, err)
				defer os.RemoveAll(root)
				dir := root
				if agent != AgentPrimeAgent {
					dir = filepath.Join(root, name)
				}
				child := filepath.Join(dir, "child.jsonl")
				suffix := strings.TrimLeft(strings.TrimPrefix(child, filepath.VolumeName(child)), string(filepath.Separator))
				parent := filepath.Join(root, suffix)
				require.NoError(ht, os.MkdirAll(filepath.Dir(parent), 0o700))
				require.NoError(ht, os.WriteFile(parent, []byte(`{"type":"session","id":"parent"}`+"\n"), 0o600))
				require.NoError(ht, os.MkdirAll(dir, 0o700))
				require.NoError(ht, os.WriteFile(child, []byte(`{"type":"session","id":"child","parentSession":`+strconv.Quote(parent)+"}\n"), 0o600))
				provider, _ := NewProvider(agent, ProviderConfig{Roots: []string{root}})
				sources, err := RawCaptureSourcesForChangedPath(ht.Context(), provider, ChangedPathRequest{Path: child})
				require.NoError(ht, err)
				require.Len(ht, sources, 1)
				_, _, err = ResolveRawCapturePlan(ht.Context(), provider, sources[0])
				assert.ErrorIs(ht, err, ErrInvalidRawCapturePlan)
			}, hegel.WithTestCases(1000))
		})
	}
}

// Streaming inventory and primary-event classification preserve native layouts.
func TestPiRawCaptureLayoutProperty(t *testing.T) {
	for _, agent := range []AgentType{AgentPi, AgentOMP, AgentOMO, AgentStepCode, AgentPrimeAgent} {
		t.Run(string(agent), func(t *testing.T) {
			base := t.TempDir()
			hegel.Test(t, func(ht *hegel.T) {
				parts := hegel.Draw(ht, hegel.Lists(hegel.Integers(uint64(0), uint64(math.MaxUint64))))
				if len(parts) > 16 {
					parts = parts[:16]
				}
				repeated := hegel.Draw(ht, hegel.Booleans())
				root, err := os.MkdirTemp(base, "layout-")
				require.NoError(ht, err)
				defer os.RemoveAll(root)
				dir := root
				for _, part := range parts {
					dir = filepath.Join(dir, strconv.FormatUint(part, 16))
				}
				require.NoError(ht, os.MkdirAll(dir, 0o700))
				file := filepath.Join(dir, "session.jsonl")
				require.NoError(ht, os.WriteFile(file, []byte(`{"type":"session","id":"layout-session"}`+"\n"), 0o600))
				roots := []string{root}
				if repeated {
					roots = append(roots, root)
				}
				provider, _ := NewProvider(agent, ProviderConfig{Roots: roots})
				discovery, err := DiscoverRawCaptureSources(ht.Context(), provider)
				require.NoError(ht, err)
				require.True(ht, discovery.Complete)
				changed, err := RawCaptureSourcesForChangedPath(ht.Context(), provider, ChangedPathRequest{Path: file})
				require.NoError(ht, err)
				accepted := true
				switch agent {
				case AgentPrimeAgent:
					accepted = len(parts) == 0
				case AgentOMP:
					accepted = len(parts) > 0
				}
				if accepted {
					require.Len(ht, discovery.Sources, 1)
					require.Len(ht, changed, 1)
					assert.Equal(ht, file, discovery.Sources[0].Key)
					assert.Equal(ht, file, changed[0].Key)
					assert.Equal(ht, agent, discovery.Sources[0].Provider)
				} else {
					assert.Empty(ht, discovery.Sources)
					assert.Empty(ht, changed)
				}
			}, hegel.WithTestCases(1000))
		})
	}
}

// branchedFrom takes precedence without opening the parentSession hint.
func TestPiRawCaptureBranchPrecedenceProperty(t *testing.T) {
	for _, agent := range []AgentType{AgentPi, AgentOMP, AgentOMO, AgentStepCode, AgentPrimeAgent} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			dir := root
			if agent != AgentPrimeAgent {
				dir = filepath.Join(root, "project")
			}
			require.NoError(t, os.MkdirAll(dir, 0o700))
			outside := filepath.Join(t.TempDir(), "parent.jsonl")
			require.NoError(t, os.WriteFile(outside, []byte(`{"type":"session","id":"outside"}`+"\n"), 0o600))
			child := filepath.Join(dir, "child.jsonl")
			provider, _ := NewProvider(agent, ProviderConfig{Roots: []string{root}})
			hegel.Test(t, func(ht *hegel.T) {
				branchID := strconv.FormatUint(hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64))), 16)
				body := `{"type":"session","id":"child","branchedFrom":` + strconv.Quote(branchID+".jsonl") + `,"parentSession":` + strconv.Quote(outside) + "}\n"
				require.NoError(ht, os.WriteFile(child, []byte(body), 0o600))
				sources, err := RawCaptureSourcesForChangedPath(ht.Context(), provider, ChangedPathRequest{Path: child})
				require.NoError(ht, err)
				require.Len(ht, sources, 1)
				plan, supported, err := ResolveRawCapturePlan(ht.Context(), provider, sources[0])
				require.NoError(ht, err)
				require.True(ht, supported)
				require.Len(ht, plan.Entries, 1)
				outcome, err := provider.Parse(ht.Context(), ParseRequest{Source: sources[0]})
				require.NoError(ht, err)
				require.Len(ht, outcome.Results, 1)
				assert.Equal(ht, provider.Definition().IDPrefix+branchID, outcome.Results[0].Result.Session.ParentSessionID)
			}, hegel.WithTestCases(1000))
		})
	}
}

// A referenced directory or escaping symlink cannot become a captured file.
func TestPiRawCaptureRejectsUnsafeDependencyProperty(t *testing.T) {
	for _, agent := range []AgentType{AgentPi, AgentOMP, AgentOMO, AgentStepCode, AgentPrimeAgent} {
		t.Run(string(agent), func(t *testing.T) {
			base := t.TempDir()
			hegel.Test(t, func(ht *hegel.T) {
				name := strconv.FormatUint(hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64))), 16)
				symlink := hegel.Draw(ht, hegel.Booleans())
				root, err := os.MkdirTemp(base, "unsafe-")
				require.NoError(ht, err)
				defer os.RemoveAll(root)
				dir := root
				if agent != AgentPrimeAgent {
					dir = filepath.Join(root, "project")
				}
				require.NoError(ht, os.MkdirAll(dir, 0o700))
				parent := filepath.Join(dir, name+".jsonl")
				if symlink {
					outside, err := os.MkdirTemp(base, "outside-")
					require.NoError(ht, err)
					defer os.RemoveAll(outside)
					target := filepath.Join(outside, "parent.jsonl")
					require.NoError(ht, os.WriteFile(target, []byte(`{"type":"session","id":"outside"}`+"\n"), 0o600))
					require.NoError(ht, os.Symlink(target, parent))
				} else {
					require.NoError(ht, os.Mkdir(parent, 0o700))
				}
				child := filepath.Join(dir, "child.jsonl")
				require.NoError(ht, os.WriteFile(child, []byte(`{"type":"session","id":"child","parentSession":`+strconv.Quote(name+".jsonl")+"}\n"), 0o600))
				provider, _ := NewProvider(agent, ProviderConfig{Roots: []string{root}})
				sources, err := RawCaptureSourcesForChangedPath(ht.Context(), provider, ChangedPathRequest{Path: child})
				require.NoError(ht, err)
				require.Len(ht, sources, 1)
				_, _, err = ResolveRawCapturePlan(ht.Context(), provider, sources[0])
				assert.ErrorIs(ht, err, ErrInvalidRawCapturePlan)
			}, hegel.WithTestCases(1000))
		})
	}
}

// A validated in-root symlink is a supported physical file, whether its target
// is absolute or relative and whether it supplies the primary or parent.
func TestPiRawCaptureSafeSymlinkProperty(t *testing.T) {
	for _, agent := range []AgentType{AgentPi, AgentOMP, AgentOMO, AgentStepCode, AgentPrimeAgent} {
		t.Run(string(agent), func(t *testing.T) {
			base := t.TempDir()
			hegel.Test(t, func(ht *hegel.T) {
				id := strconv.FormatUint(hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64))), 16)
				primaryLink := hegel.Draw(ht, hegel.Booleans())
				absolute := hegel.Draw(ht, hegel.Booleans())
				root, err := os.MkdirTemp(base, "link-")
				require.NoError(ht, err)
				defer os.RemoveAll(root)
				dir := root
				if agent != AgentPrimeAgent {
					dir = filepath.Join(root, "project")
				}
				require.NoError(ht, os.MkdirAll(dir, 0o700))
				target := filepath.Join(root, "data", id+".jsonl")
				require.NoError(ht, os.MkdirAll(filepath.Dir(target), 0o700))
				targetID := "parent-" + id
				if primaryLink {
					targetID = "child-" + id
				}
				require.NoError(ht, os.WriteFile(target, []byte(`{"type":"session","id":`+strconv.Quote(targetID)+"}\n"), 0o600))
				child := filepath.Join(dir, "child.jsonl")
				link := filepath.Join(dir, "parent.jsonl")
				if primaryLink {
					link = child
				}
				linkTarget := target
				if !absolute {
					linkTarget, err = filepath.Rel(dir, target)
					require.NoError(ht, err)
				}
				require.NoError(ht, os.Symlink(linkTarget, link))
				if !primaryLink {
					require.NoError(ht, os.WriteFile(child, []byte(`{"type":"session","id":"child","parentSession":"parent.jsonl"}`+"\n"), 0o600))
				}
				provider, _ := NewProvider(agent, ProviderConfig{Roots: []string{root}})
				sources, err := RawCaptureSourcesForChangedPath(ht.Context(), provider, ChangedPathRequest{Path: child})
				require.NoError(ht, err)
				require.Len(ht, sources, 1)
				plan, supported, err := ResolveRawCapturePlan(ht.Context(), provider, sources[0])
				require.NoError(ht, err)
				require.True(ht, supported)
				prefix := "project/"
				if agent == AgentPrimeAgent {
					prefix = ""
				}
				expected := []string{prefix + "child.jsonl"}
				if !primaryLink {
					expected = append(expected, prefix+"parent.jsonl")
				}
				var paths []string
				for _, entry := range plan.Entries {
					paths = append(paths, entry.Path)
				}
				assert.ElementsMatch(ht, expected, paths)
			}, hegel.WithTestCases(1000))
		})
	}
}

// Explicit and structural parent spellings denote one logical capture entry.
func TestPiRawCaptureEquivalentStructuralParentProperty(t *testing.T) {
	for _, agent := range []AgentType{AgentPi, AgentOMO} {
		t.Run(string(agent), func(t *testing.T) {
			base := t.TempDir()
			hegel.Test(t, func(ht *hegel.T) {
				id := strconv.FormatUint(hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64))), 16)
				repeated := hegel.Draw(ht, hegel.Booleans())
				root, err := os.MkdirTemp(base, "parent-")
				require.NoError(ht, err)
				defer os.RemoveAll(root)
				parent := filepath.Join(root, "project", id+".jsonl")
				child := filepath.Join(root, "project", id, "task/run-0/session.jsonl")
				require.NoError(ht, os.MkdirAll(filepath.Dir(child), 0o700))
				require.NoError(ht, os.WriteFile(parent, []byte(`{"type":"session","id":"parent-header"}`+"\n"), 0o600))
				reference := root + string(filepath.Separator) + "project" + string(filepath.Separator) + "." + string(filepath.Separator) + id + ".jsonl"
				if repeated {
					reference = root + string(filepath.Separator) + "project" + string(filepath.Separator) + string(filepath.Separator) + id + ".jsonl"
				}
				require.NoError(ht, os.WriteFile(child, []byte(`{"type":"session","id":"child","parentSession":`+strconv.Quote(reference)+"}\n"), 0o600))
				provider, _ := NewProvider(agent, ProviderConfig{Roots: []string{root}})
				sources, err := RawCaptureSourcesForChangedPath(ht.Context(), provider, ChangedPathRequest{Path: child})
				require.NoError(ht, err)
				require.Len(ht, sources, 1)
				plan, supported, err := ResolveRawCapturePlan(ht.Context(), provider, sources[0])
				require.NoError(ht, err)
				require.True(ht, supported)
				var paths []string
				for _, entry := range plan.Entries {
					paths = append(paths, entry.Path)
				}
				assert.ElementsMatch(ht, []string{"project/" + id + ".jsonl", "project/" + id + "/task/run-0/session.jsonl"}, paths)
			}, hegel.WithTestCases(1000))
		})
	}
}
