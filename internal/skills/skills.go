// Package skills renders the AgentsView skill files that teach coding
// agents (Claude Code, Codex, and similar harnesses) how to search the
// AgentsView archive for prior session history. Each harness has its own
// discovery convention (~/.claude/skills, ~/.agents/skills), but shares
// one template body with a harness-specific delegation instruction.
package skills

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

//go:embed templates/finding-history.md.tmpl
var templatesFS embed.FS

// Harness identifies a skill discovery convention.
type Harness string

const (
	HarnessClaude Harness = "claude" // ~/.claude/skills
	HarnessAgents Harness = "agents" // ~/.agents/skills (Codex et al.)
)

// AllHarnesses returns every harness a skill can be rendered for.
func AllHarnesses() []Harness {
	return []Harness{HarnessClaude, HarnessAgents}
}

// skillName is the directory and frontmatter name for the only skill this
// package currently renders.
const skillName = "agentsview-finding-history"

// delegatePhrases supplies the harness-specific instruction that replaces
// {{.Delegate}} in the template: whether the harness can dispatch a search
// subagent or must run the bounded probes itself.
var delegatePhrases = map[Harness]string{
	HarnessClaude: "Dispatch a search subagent (e.g. the Task/Agent tool)",
	HarnessAgents: "Delegate to a search subagent if your harness supports one; " +
		"otherwise run the bounded probes yourself in order",
}

// skillsSubdir is the harness-specific path segment under the install base,
// e.g. ".claude/skills" or ".agents/skills".
var skillsSubdir = map[Harness]string{
	HarnessClaude: filepath.Join(".claude", "skills"),
	HarnessAgents: filepath.Join(".agents", "skills"),
}

// headerFormat is the second line of every rendered file: a YAML comment
// inserted just inside the frontmatter fence, so the file still begins with
// "---" and frontmatter parsers (which require the fence as the first bytes)
// keep discovering the skill. version is recorded for humans; hash is
// authoritative for staleness and tamper detection.
const headerFormat = "# generated-by: agentsview %s hash:%s — do not edit; " +
	"re-run `agentsview skills install`"

// headerPattern extracts the hash recorded in a generated-by header line.
// It must match headerFormat exactly so parsing round-trips.
var headerPattern = regexp.MustCompile(
	"^# generated-by: agentsview \\S+ hash:([0-9a-f]{64}) — do not edit; " +
		"re-run `agentsview skills install`$",
)

// frontmatterFence opens every skill file; the template body starts with it
// and Render re-emits it above the generated-by header.
const frontmatterFence = "---\n"

var tmpl = template.Must(template.ParseFS(templatesFS, "templates/finding-history.md.tmpl"))

// templateData is the data passed to the finding-history template.
type templateData struct {
	Delegate string
}

// Rendered is one skill file ready to install.
type Rendered struct {
	Name    string // "agentsview-finding-history"
	Content string // full file: frontmatter fence, generated-by header, rest
	Hash    string // sha256 hex of Content minus the header line
}

// Render produces the skill for a harness. version is the CLI version
// string, recorded in the header for humans (hash is authoritative). The
// generated-by header is inserted as line two, inside the frontmatter
// fence, so the rendered file still begins with "---".
func Render(h Harness, version string) (Rendered, error) {
	delegate, ok := delegatePhrases[h]
	if !ok {
		return Rendered{}, fmt.Errorf("skills: unknown harness %q", h)
	}

	var body bytes.Buffer
	data := templateData{Delegate: delegate}
	if err := tmpl.ExecuteTemplate(&body, "finding-history.md.tmpl", data); err != nil {
		return Rendered{}, fmt.Errorf("skills: render %s template: %w", h, err)
	}
	if !strings.HasPrefix(body.String(), frontmatterFence) {
		return Rendered{}, fmt.Errorf(
			"skills: %s template must start with a %q frontmatter fence", h, "---")
	}

	hash := bodyHash(body.String())
	header := fmt.Sprintf(headerFormat, version, hash)
	content := frontmatterFence + header + "\n" +
		strings.TrimPrefix(body.String(), frontmatterFence)

	return Rendered{
		Name:    skillName,
		Content: content,
		Hash:    hash,
	}, nil
}

// TargetDir returns the directory the skill installs into for a harness:
// <base>/<claude-or-agents path>/agentsview-finding-history. base is the
// home dir for user-level installs or the project root for --project.
func TargetDir(h Harness, base string) string {
	return filepath.Join(base, skillsSubdir[h], skillName)
}

// InstalledState classifies an existing file against a fresh render.
type InstalledState int

const (
	StateMissing  InstalledState = iota // no file at the target path
	StateCurrent                        // content == fresh render
	StateStale                          // unmodified generated file, but older render
	StateModified                       // content no longer matches its recorded hash
	StateForeign                        // no generated-by header
)

// Classify compares an existing file's content against a fresh render.
// existing is the file's current content, or nil if no file exists at the
// target path. It never mutates fresh or existing.
func Classify(existing []byte, fresh Rendered) InstalledState {
	if existing == nil {
		return StateMissing
	}

	content := string(existing)
	if !strings.HasPrefix(content, frontmatterFence) {
		return StateForeign
	}
	headerLine, rest, hasRest := strings.Cut(
		strings.TrimPrefix(content, frontmatterFence), "\n")
	if !hasRest {
		rest = ""
	}

	match := headerPattern.FindStringSubmatch(headerLine)
	if match == nil {
		return StateForeign
	}
	recordedHash := match[1]

	if recordedHash != bodyHash(frontmatterFence+rest) {
		return StateModified
	}
	if recordedHash == fresh.Hash {
		return StateCurrent
	}
	return StateStale
}

// bodyHash returns the sha256 hex digest of a rendered file's body, i.e.
// its content minus the generated-by header line.
func bodyHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}
