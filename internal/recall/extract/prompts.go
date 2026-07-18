package extract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Base prompts shipped in the binary. They work for instruction-following
// models with no configuration; model profiles and user override files
// replace them per role when a model needs different phrasing.
const (
	baseIntentPrompt = `You distill one user message from a coding-agent session into memory entries for a keyword search index. The text is what the human asked the agent to do, or their feedback on work in progress.

Extract entries capturing the user's intent: the task requested, its targets and constraints (files, repos, branches, tools, APIs, versions, deadlines), corrections or feedback on prior work, standing preferences about how work should be done, and stated success criteria. Emit one entry per distinct item - the count should follow the content, so a one-line instruction may yield a single entry while a detailed request yields several. Do not pad.

Rules:
- Each entry must stand alone without the surrounding session.
- Quote exact file paths, commands, identifiers, and values verbatim.
- type: 'fact' for the requested task and stated context, 'preference' for standing user preferences about tools or workflow, 'open_question' for things the user left undecided.
- title: one specific sentence, at most 120 characters.
- body: 1-3 dense sentences with the exact identifiers and values.
- entities: the exact searchable strings (file paths, function names, commands, project names) the entry is about.`

	baseActionPrompt = `You distill one segment of a coding agent's recorded work (its replies, file edits, commands, and their results) into memory entries for a keyword search index.

Record BOTH what the agent did and what it learned. Emit one entry per distinct item - the count should follow the content, so a short segment may yield one or two entries while a dense one yields many. Do not pad and do not stop early when more distinct items remain.

- 'procedure': a sequence of steps that accomplished something, with the exact commands, files, and values used, and its outcome.
- 'fact' (state change): how the code or system changed - "X went from A to B" - with the exact before and after values.
- 'fact' (finding): concrete facts learned about the codebase or environment: where things live, how components connect, config values, versions, schema shapes, API behaviors, test results. Record these generously even when they are incidental to the task - a future search may need any of them, so incidental findings are wanted content, not padding.
- 'decision': alternatives the agent proposed or considered, and why one was chosen or rejected.
- 'warning': errors hit, failing commands, pitfalls or gotchas discovered.

Rules:
- Each entry must stand alone without the session transcript.
- Quote exact file paths, commands, error messages, and values verbatim.
- Record outcomes explicitly: what succeeded and what failed.
- title: one specific sentence, at most 120 characters.
- body: 1-4 dense sentences with the exact identifiers and values.
- entities: the exact searchable strings (file paths, function names, commands, error strings, URLs) the entry is about.`
)

// basePrompts returns the embedded defaults for every prompt role. The
// generic role reuses the action prompt: it records both sides of the
// conversation for strategies that do not split by speaker.
func basePrompts() map[PromptRole]string {
	return map[PromptRole]string{
		RoleIntent:  baseIntentPrompt,
		RoleAction:  baseActionPrompt,
		RoleGeneric: baseActionPrompt,
	}
}

// RequestShape carries every model-dependent parameter that changes
// extraction output — what is sent to the server (temperature, token
// budget, extra body) and how the client recovers from truncation (the
// compact floor). Keeping them in one fingerprinted struct means no output-
// affecting knob can change without producing a new generation. ExtraBody
// is merged into the request top-level, giving server-specific knobs
// (template arguments, sampling controls) a home without the client
// knowing about any particular server.
type RequestShape struct {
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
	// CompactFloorChars bounds the compact retry: a truncated unit longer
	// than this (in code points) surfaces a split error so no entries are
	// lost; a unit at or below it (too small for splitting to help) gets
	// one entry-capped compact retry. Callers set it to SplitFloorChars of
	// their window budget; zero disables the compact retry entirely.
	CompactFloorChars int            `json:"compact_floor_chars"`
	ExtraBody         map[string]any `json:"extra_body,omitempty"`
}

// Profile bundles the prompt variants and request shape a family of models
// needs. Profiles are data: adding support for a model family is a new
// entry here or a user override, not new code.
type Profile struct {
	Name string
	// MatchPrefixes selects this profile automatically when the configured
	// model name starts with one of these (case-insensitive).
	MatchPrefixes []string
	// Prompts overrides base prompts per role; absent roles keep the base.
	Prompts map[PromptRole]string
	Request RequestShape
}

var builtinProfiles = []Profile{
	{
		Name: "qwen",
		// These models interleave hidden reasoning by default; with
		// constrained decoding that burns the whole token budget before
		// any content is produced, so the template must disable it.
		MatchPrefixes: []string{"qwen"},
		Request: RequestShape{
			Temperature: 0,
			MaxTokens:   defaultMaxTokens,
			ExtraBody: map[string]any{
				"chat_template_kwargs": map[string]any{
					"enable_thinking": false,
				},
			},
		},
	},
	{
		Name:    "base",
		Request: RequestShape{Temperature: 0, MaxTokens: defaultMaxTokens},
	},
}

// defaultMaxTokens is the built-in profiles' output budget: generous enough
// for a dense window's worth of entries without letting a runaway response
// occupy the server indefinitely. Configuration can override it per model.
const defaultMaxTokens = 4096

// ResolveProfile picks the profile for a model. An explicit name wins and
// must exist; otherwise the model name selects a profile by prefix, falling
// back to the base profile. The returned profile is a deep copy: callers
// merge configuration into it, and none of that may write through to the
// built-in registry.
func ResolveProfile(explicit, model string) (Profile, error) {
	if explicit != "" {
		for _, profile := range builtinProfiles {
			if profile.Name == explicit {
				return profile.clone(), nil
			}
		}
		names := make([]string, 0, len(builtinProfiles))
		for _, profile := range builtinProfiles {
			names = append(names, profile.Name)
		}
		return Profile{}, fmt.Errorf(
			"unknown extraction prompt profile %q (have: %s)",
			explicit, strings.Join(names, ", "),
		)
	}
	lowerModel := strings.ToLower(model)
	for _, profile := range builtinProfiles {
		for _, prefix := range profile.MatchPrefixes {
			if strings.HasPrefix(lowerModel, prefix) {
				return profile.clone(), nil
			}
		}
	}
	return ResolveProfile("base", model)
}

func (p Profile) clone() Profile {
	out := p
	out.MatchPrefixes = slices.Clone(p.MatchPrefixes)
	out.Prompts = maps.Clone(p.Prompts)
	if p.Request.ExtraBody != nil {
		out.Request.ExtraBody = cloneJSONMap(p.Request.ExtraBody)
	}
	return out
}

// cloneJSONMap deep-copies the JSON-shaped values profiles carry: nested
// maps and slices are duplicated, scalars pass through.
func cloneJSONMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for key, item := range m {
		out[key] = cloneJSONValue(item)
	}
	return out
}

func cloneJSONValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return cloneJSONMap(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cloneJSONValue(item)
		}
		return out
	default:
		return v
	}
}

// PromptsFor resolves the effective prompt per role: user overrides win,
// then the profile's variants, then the embedded base prompts.
func PromptsFor(
	profile Profile, overrides map[PromptRole]string,
) map[PromptRole]string {
	prompts := basePrompts()
	maps.Copy(prompts, profile.Prompts)
	maps.Copy(prompts, overrides)
	return prompts
}

// LoadPromptOverrides reads per-role prompt files (intent.txt, action.txt,
// generic.txt) from a directory. Missing files are simply not overridden;
// an empty file is an error because it would silently blank a prompt.
func LoadPromptOverrides(dir string) (map[PromptRole]string, error) {
	overrides := map[PromptRole]string{}
	for _, role := range []PromptRole{RoleIntent, RoleAction, RoleGeneric} {
		path := filepath.Join(dir, string(role)+".txt")
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading prompt override %s: %w", path, err)
		}
		prompt := strings.TrimSpace(string(raw))
		if prompt == "" {
			return nil, fmt.Errorf(
				"prompt override %s is empty; delete the file to use the "+
					"default prompt", path,
			)
		}
		overrides[role] = prompt
	}
	return overrides, nil
}

// ModelIdentity names what produces the output: the model, and optionally
// a deployment label distinguishing servers that expose different weights
// under the same model name. The label is deliberately not the endpoint
// URL: moving a deployment to a new address or port does not change its
// output, and hashing the URL would orphan the whole corpus on every move.
// Leave Deployment empty when the model name alone identifies the weights.
type ModelIdentity struct {
	Model      string `json:"model"`
	Deployment string `json:"deployment,omitempty"`
}

// Fingerprint digests everything that changes extraction output: the model
// identity (name plus optional deployment label), the segmentation strategy
// and its parameters, the resolved prompts, the request shape (including
// the output token budget), and the extraction protocol version covering
// the response schema and recovery behavior baked into this binary. Two
// configurations with the same fingerprint produce interchangeable corpora;
// any difference is a new generation.
func Fingerprint(
	model ModelIdentity,
	segmenter Segmenter,
	prompts map[PromptRole]string,
	request RequestShape,
) (string, error) {
	promptDigests := map[string]string{}
	for role, prompt := range prompts {
		sum := sha256.Sum256([]byte(prompt))
		promptDigests[string(role)] = hex.EncodeToString(sum[:])
	}
	identity := struct {
		Protocol      int               `json:"protocol"`
		Model         ModelIdentity     `json:"model"`
		Segmenter     string            `json:"segmenter"`
		Params        map[string]any    `json:"params"`
		PromptDigests map[string]string `json:"prompt_digests"`
		Request       RequestShape      `json:"request"`
	}{
		Protocol:      extractionProtocolVersion,
		Model:         model,
		Segmenter:     segmenter.Name(),
		Params:        segmenter.Params(),
		PromptDigests: promptDigests,
		Request:       request,
	}
	// encoding/json writes map keys in sorted order, which makes the
	// encoding canonical for the JSON-shaped values profiles carry.
	canonical, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("encoding extraction identity: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// ProfileNames lists the built-in profiles for error messages and doctor
// output, sorted for stable display.
func ProfileNames() []string {
	names := make([]string, 0, len(builtinProfiles))
	for _, profile := range builtinProfiles {
		names = append(names, profile.Name)
	}
	sort.Strings(names)
	return names
}
