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
//   - the output of all sessions parsed from one input stays within
//     providerFuzzMaxAmplification times the input size, plus
//     providerFuzzOutputSlack;
//   - every parsed session has an ID and belongs to the provider's agent.
//
// TestProviderFuzzSeedsReachParser checks each seed fixture against a
// hand-picked literal from its transcript, so a target whose layout the
// provider ignores fails instead of fuzzing nothing.
//
// Plain `go test` runs the seeds plus any regression inputs committed under
// testdata/fuzz/<Target>/. `go test -run '^$' -fuzz '^<Target>$'` explores
// from there; .github/workflows/fuzz.yml does that weekly for every target.
// Inputs land in os.TempDir. Database targets build a SQLite store per
// input, so put temp files on tmpfs when fuzzing locally; on a disk each
// input waits on syncs. On Linux, use TMPDIR=/dev/shm GOTMPDIR=/tmp: the
// second keeps the test binary off /dev/shm, which is often noexec.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime/debug"
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
	// Parse. Inputs parse in milliseconds on an idle machine, but SQLite
	// targets have taken ten seconds on a saturated one; a provider that
	// needs a minute is looping, not working.
	providerFuzzDeadline = 60 * time.Second
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
	place func(t *testing.T, root string, data []byte)
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

// providerFuzzListEnv makes every FuzzProvider target print its name and
// agent and skip, so TestProviderFuzzTargetsCoverRegistry can see which
// targets Go itself discovers.
const providerFuzzListEnv = "AGENTSVIEW_LIST_PROVIDER_FUZZ_TARGETS"

// fuzzProvider is the body of every FuzzProvider<Name> target.
func fuzzProvider(f *testing.F, agent AgentType) {
	f.Helper()
	if os.Getenv(providerFuzzListEnv) != "" {
		fmt.Printf("provider-fuzz-target %s %s\n", f.Name(), agent)
		f.SkipNow()
	}
	target := lookupProviderFuzzTarget(f, agent)
	isolateProviderFuzzHome(f)
	for _, seed := range target.seeds {
		data, err := os.ReadFile(filepath.Join("testdata", seed.file))
		require.NoError(f, err, "read seed %s", seed.file)
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		results := parseProviderFuzzInput(t, target, data)
		total := 0
		for _, result := range results {
			assert.NotEmpty(t, result.Session.ID, "parsed session has no ID")
			assert.Equal(t, agent, result.Session.Agent,
				"session %q has the wrong agent", result.Session.ID)
			total += parsedOutputBytes(reflect.ValueOf(result), 0)
		}
		// Sum across sessions: a parser that copies one large field into
		// many sessions amplifies the input as much as one huge session.
		limit := providerFuzzMaxAmplification*len(data) + providerFuzzOutputSlack
		assert.LessOrEqual(t, total, limit,
			"%d sessions hold %d bytes, over %dx the %d-byte input",
			len(results), total, providerFuzzMaxAmplification, len(data))
	})
}

// providerFuzzEnv is the part of *testing.T and *testing.F that home
// isolation needs.
type providerFuzzEnv interface {
	TempDir() string
	Setenv(key, value string)
}

// isolateProviderFuzzHome points home-relative lookups at an empty
// directory so a provider never reads the developer's real sessions.
func isolateProviderFuzzHome(tb providerFuzzEnv) {
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

	// Fuzzed cwd values are absolute paths on the host. Project discovery
	// would probe them for Git metadata and sibling repos, which reads
	// outside the fixture and makes timing depend on the machine.
	ctx, cancel := context.WithCancel(WithoutFilesystemProjectDiscovery(t.Context()))
	defer cancel()
	type parsed struct {
		results []ParseResult
		panic   any
		stack   []byte
	}
	done := make(chan parsed, 1)
	go func() {
		var out parsed
		defer func() {
			if out.panic = recover(); out.panic != nil {
				out.stack = debug.Stack()
			}
			done <- out
		}()
		out.results = discoverAndParse(ctx, provider)
	}()
	timer := time.NewTimer(providerFuzzDeadline)
	defer timer.Stop()
	select {
	case out := <-done:
		require.Nil(t, out.panic, "%s provider panicked:\n%s", target.agent, out.stack)
		return out.results
	case <-timer.C:
		require.FailNow(t, "provider did not finish",
			"%s provider did not finish within %s", target.agent, providerFuzzDeadline)
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

// parsedOutputBytes totals the variable-length data in a parse result:
// every string and byte slice, wherever it sits. Walking the value keeps
// new fields counted without updating a list.
func parsedOutputBytes(v reflect.Value, depth int) int {
	if depth > 32 || v.Type() == reflect.TypeFor[time.Time]() {
		// A time.Time points at its shared *Location, whose zone names
		// are not parser output.
		return 0
	}
	switch v.Kind() {
	case reflect.String:
		return v.Len()
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return v.Len()
		}
		n := 0
		for i := range v.Len() {
			n += parsedOutputBytes(v.Index(i), depth+1)
		}
		return n
	case reflect.Array:
		n := 0
		for i := range v.Len() {
			n += parsedOutputBytes(v.Index(i), depth+1)
		}
		return n
	case reflect.Map:
		n := 0
		iter := v.MapRange()
		for iter.Next() {
			n += parsedOutputBytes(iter.Key(), depth+1)
			n += parsedOutputBytes(iter.Value(), depth+1)
		}
		return n
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return 0
		}
		return parsedOutputBytes(v.Elem(), depth+1)
	case reflect.Struct:
		n := 0
		for _, field := range v.Fields() {
			n += parsedOutputBytes(field, depth+1)
		}
		return n
	default:
		return 0
	}
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

func TestParsedOutputBytesCountsEveryField(t *testing.T) {
	usage := []byte(`{"in":1}`)
	result := ParseResult{
		Session: ParsedSession{
			ID:           "abc",
			FirstMessage: "hello",
			StartedAt:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("zone", 3600)),
		},
		Messages: []ParsedMessage{{
			Content:         "hi",
			Model:           "model-x",
			ReasoningEffort: "high",
			TokenUsage:      usage,
			DialogueText:    new(string),
			ToolCalls: []ParsedToolCall{{
				InputJSON:    "{}",
				ResultEvents: []ParsedToolResultEvent{{Content: "out"}},
			}},
		}},
		Checkpoint: []byte("cp"),
	}
	*result.Messages[0].DialogueText = "dlg"

	// abc + hello + hi + model-x + high + {"in":1} + dlg + {} + out + cp
	assert.Equal(t, 3+5+2+7+4+8+3+2+3+2, parsedOutputBytes(reflect.ValueOf(result), 0))
}

// providerFuzzExempt lists registered agents that have no session
// provider to fuzz, with the reason.
var providerFuzzExempt = map[AgentType]string{
	AgentClaudeAI:   "import-only: archives are parsed by internal/importer",
	AgentChatGPT:    "import-only: archives are parsed by internal/importer",
	AgentGeminiApps: "import-only: archives are parsed by internal/importer",
}

// TestProviderFuzzTargetsCoverRegistry fails when a provider has no
// FuzzProvider function. It reruns this test binary so Go's own target
// discovery, the same the fuzz workflow relies on, decides what exists.
func TestProviderFuzzTargetsCoverRegistry(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), exe, "-test.run", "^FuzzProvider")
	cmd.Env = append(os.Environ(), providerFuzzListEnv+"=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "list provider fuzz targets:\n%s", out)

	targets := map[AgentType][]string{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 3 && fields[0] == "provider-fuzz-target" {
			agent := AgentType(fields[2])
			targets[agent] = append(targets[agent], fields[1])
		}
	}
	require.NoError(t, scanner.Err())

	var missing []string
	for _, def := range Registry {
		if _, exempt := providerFuzzExempt[def.Type]; exempt {
			continue
		}
		names := targets[def.Type]
		if len(names) == 0 {
			missing = append(missing, string(def.Type))
			continue
		}
		assert.Len(t, names, 1, "%s has more than one fuzz target", def.Type)
	}
	slices.Sort(missing)
	assert.Empty(t, missing, "providers without a FuzzProvider target")
}
