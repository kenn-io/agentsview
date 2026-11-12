package parser

// Provider fuzz harness.
//
// Session files are untrusted input (SECURITY.md): a crafted or corrupted
// transcript must not crash the sync, hang it, or make it allocate far more
// than the file holds. Every registered provider gets a FuzzProvider<Name>
// target. Each one writes the fuzz input where the provider expects its
// native file, then runs the same Discover, Fingerprint, and Parse calls the
// sync engine makes. The targets assert invariants that hold for every
// provider:
//
//   - no panic;
//   - each input finishes within providerFuzzDeadline;
//   - parsed output stays within providerFuzzMaxAmplification times the
//     input size, plus providerFuzzOutputSlack;
//   - every parsed session has an ID and belongs to the provider's agent.
//
// TestProviderFuzzSeedsReachParser checks each seed fixture against a
// hand-picked literal from its transcript, so a target whose layout the
// provider ignores fails instead of fuzzing nothing.
//
// Plain `go test` runs the seeds plus any regression inputs committed under
// testdata/fuzz/<Target>/. `go test -run '^$' -fuzz '^<Target>$'` explores
// from there; .github/workflows/fuzz.yml does that on a schedule.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// providerFuzzDeadline bounds one input's Discover, Fingerprint, and
	// Parse. Fuzz inputs are at most a few hundred kilobytes; a provider
	// that needs this long on them is looping, not working.
	providerFuzzDeadline = 20 * time.Second
	// providerFuzzMaxAmplification bounds parsed output bytes per input
	// byte. Parsers copy text out of the transcript and add short labels,
	// so honest output stays well under this.
	providerFuzzMaxAmplification = 16
	// providerFuzzOutputSlack covers fixed text a parser adds no matter
	// how small the input is, such as titles and tool-call renderings.
	providerFuzzOutputSlack = 64 << 10
)

// providerFuzzSeed is one fixture under testdata and a literal the parsed
// output must contain when the fixture is parsed unchanged.
type providerFuzzSeed struct {
	file string
	want string
}

// providerFuzzTarget describes how to feed one provider.
type providerFuzzTarget struct {
	agent AgentType
	// path is where the fuzz input lands, relative to the provider root.
	path string
	// companions are fixed files written beside the input, relative to
	// the provider root, for formats that need more than one file.
	companions map[string]string
	// place overrides path for formats that embed the input in another
	// container, such as a SQLite row. It writes everything under root.
	place func(t testing.TB, root string, data []byte)
	seeds []providerFuzzSeed
}

var providerFuzzRegistry = struct {
	sync.Mutex
	targets map[AgentType]providerFuzzTarget
}{targets: map[AgentType]providerFuzzTarget{}}

// registerProviderFuzzTarget adds a target. Call it from init in the file
// that defines the matching FuzzProvider<Name> function.
func registerProviderFuzzTarget(target providerFuzzTarget) {
	providerFuzzRegistry.Lock()
	defer providerFuzzRegistry.Unlock()
	if _, dup := providerFuzzRegistry.targets[target.agent]; dup {
		panic("duplicate provider fuzz target for " + string(target.agent))
	}
	providerFuzzRegistry.targets[target.agent] = target
}

func lookupProviderFuzzTarget(tb testing.TB, agent AgentType) providerFuzzTarget {
	tb.Helper()
	providerFuzzRegistry.Lock()
	defer providerFuzzRegistry.Unlock()
	target, ok := providerFuzzRegistry.targets[agent]
	require.True(tb, ok, "no provider fuzz target registered for %s", agent)
	return target
}

// fuzzProvider is the body of every FuzzProvider<Name> target.
func fuzzProvider(f *testing.F, agent AgentType) {
	target := lookupProviderFuzzTarget(f, agent)
	isolateProviderFuzzHome(f)
	for _, seed := range target.seeds {
		data, err := os.ReadFile(filepath.Join("testdata", seed.file))
		require.NoError(f, err, "read seed %s", seed.file)
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		results := parseProviderFuzzInput(t, target, data)
		limit := providerFuzzMaxAmplification*len(data) + providerFuzzOutputSlack
		for _, result := range results {
			assert.NotEmpty(t, result.Session.ID, "parsed session has no ID")
			assert.Equal(t, agent, result.Session.Agent,
				"session %q has the wrong agent", result.Session.ID)
			assert.LessOrEqual(t, parsedResultBytes(result), limit,
				"session %q output exceeds %dx the %d-byte input",
				result.Session.ID, providerFuzzMaxAmplification, len(data))
		}
	})
}

// isolateProviderFuzzHome points home-relative lookups at an empty
// directory so a provider never reads the developer's real sessions.
func isolateProviderFuzzHome(tb interface {
	TempDir() string
	Setenv(key, value string)
}) {
	home := tb.TempDir()
	tb.Setenv("HOME", home)
	tb.Setenv("USERPROFILE", home)
	tb.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	tb.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	tb.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	tb.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	tb.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	tb.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
}

// placeProviderFuzzInput writes data and the target's companions under a
// fresh root and returns the root.
func placeProviderFuzzInput(t *testing.T, target providerFuzzTarget, data []byte) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range target.companions {
		writeProviderFuzzFile(t, filepath.Join(root, filepath.FromSlash(rel)), []byte(content))
	}
	if target.place != nil {
		target.place(t, root, data)
		return root
	}
	require.NotEmpty(t, target.path, "%s target needs a path or place func", target.agent)
	writeProviderFuzzFile(t, filepath.Join(root, filepath.FromSlash(target.path)), data)
	return root
}

func writeProviderFuzzFile(tb testing.TB, path string, data []byte) {
	tb.Helper()
	require.NoError(tb, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(tb, os.WriteFile(path, data, 0o644))
}

// parseProviderFuzzInput places data, then discovers and parses every
// source the provider finds. Parse errors are expected for malformed input
// and are not failures; panics and hangs are.
func parseProviderFuzzInput(t *testing.T, target providerFuzzTarget, data []byte) []ParseResult {
	t.Helper()
	root := placeProviderFuzzInput(t, target, data)
	provider, ok := NewProvider(target.agent, ProviderConfig{
		Roots:   []string{root},
		Machine: "fuzz",
	})
	require.True(t, ok, "no provider for %s", target.agent)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type parsed struct {
		results []ParseResult
		panic   any
	}
	done := make(chan parsed, 1)
	go func() {
		var out parsed
		defer func() {
			out.panic = recover()
			done <- out
		}()
		out.results = discoverAndParse(ctx, provider)
	}()
	timer := time.NewTimer(providerFuzzDeadline)
	defer timer.Stop()
	select {
	case out := <-done:
		require.Nil(t, out.panic, "%s provider panicked", target.agent)
		return out.results
	case <-timer.C:
		t.Fatalf("%s provider did not finish within %s", target.agent, providerFuzzDeadline)
		return nil
	}
}

func discoverAndParse(ctx context.Context, provider Provider) []ParseResult {
	sources, err := provider.Discover(ctx)
	if err != nil {
		return nil
	}
	var results []ParseResult
	for _, source := range sources {
		fingerprint, err := provider.Fingerprint(ctx, source)
		if err != nil {
			continue
		}
		outcome, err := provider.Parse(ctx, ParseRequest{
			Source:      source,
			Fingerprint: fingerprint,
			Machine:     "fuzz",
			ForceParse:  true,
		})
		if err != nil {
			continue
		}
		for _, result := range outcome.Results {
			results = append(results, result.Result)
		}
	}
	return results
}

// parsedResultBytes totals the text a parse produced.
func parsedResultBytes(result ParseResult) int {
	n := len(result.Session.FirstMessage) + len(result.Session.SessionName)
	for _, msg := range result.Messages {
		n += len(msg.Content) + len(msg.ThinkingText) + len(msg.TokenUsage)
		if msg.DialogueText != nil {
			n += len(*msg.DialogueText)
		}
		for _, call := range msg.ToolCalls {
			n += len(call.InputJSON) + len(call.Rendering)
			for _, event := range call.ResultEvents {
				n += len(event.Content)
			}
		}
		for _, res := range msg.ToolResults {
			n += len(res.ContentRaw)
		}
	}
	return n
}

// parsedResultText joins the text a reviewer would look for in a seed.
func parsedResultText(results []ParseResult) string {
	var b strings.Builder
	for _, result := range results {
		b.WriteString(result.Session.FirstMessage)
		b.WriteByte('\n')
		for _, msg := range result.Messages {
			b.WriteString(msg.Content)
			b.WriteByte('\n')
			b.WriteString(msg.ThinkingText)
			b.WriteByte('\n')
			for _, call := range msg.ToolCalls {
				b.WriteString(call.InputJSON)
				b.WriteByte('\n')
			}
			for _, res := range msg.ToolResults {
				b.WriteString(res.ContentRaw)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

// TestProviderFuzzSeedsReachParser parses each seed fixture unchanged and
// requires the literal it names, so every target starts from input the
// provider really parses.
func TestProviderFuzzSeedsReachParser(t *testing.T) {
	isolateProviderFuzzHome(t)
	providerFuzzRegistry.Lock()
	targets := make([]providerFuzzTarget, 0, len(providerFuzzRegistry.targets))
	for _, target := range providerFuzzRegistry.targets {
		targets = append(targets, target)
	}
	providerFuzzRegistry.Unlock()
	slices.SortFunc(targets, func(a, b providerFuzzTarget) int {
		return strings.Compare(string(a.agent), string(b.agent))
	})

	for _, target := range targets {
		for _, seed := range target.seeds {
			t.Run(string(target.agent)+"/"+seed.file, func(t *testing.T) {
				require.NotEmpty(t, seed.want, "seed needs a literal to look for")
				data, err := os.ReadFile(filepath.Join("testdata", seed.file))
				require.NoError(t, err)
				results := parseProviderFuzzInput(t, target, data)
				require.NotEmpty(t, results, "seed produced no sessions")
				assert.Contains(t, parsedResultText(results), seed.want)
			})
		}
	}
}
