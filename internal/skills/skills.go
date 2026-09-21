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
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

//go:embed templates/*.md.tmpl
var templatesFS embed.FS

//go:generate go run ./cmd/render-memory-plugin -out ../../plugins/agentsview-memory -version 0.1.0

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

// claudeAgentDir is the Claude project agent directory, both as the prose
// fragment in the delegation guard and as the base of the search agent's
// install path.
const claudeAgentDir = ".claude/agents"

// delegatePhrases supplies the harness-specific instruction that replaces
// {{.Delegate}} in the template: whether the harness can dispatch a search
// subagent or must run the bounded probes itself.
var delegatePhrases = map[Harness]string{
	HarnessClaude: "Use the `agentsview-search-conversations` agent when this harness exposes it; " +
		"otherwise follow these steps directly",
	HarnessAgents: "Delegate to a permitted search agent if this harness provides one; " +
		"otherwise follow these steps directly",
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

var tmpl = template.Must(template.ParseFS(templatesFS, "templates/*.md.tmpl"))

// Remote is the optional remote-daemon targeting baked into generated
// examples so `skills install` on a shared archive does not teach the
// local SQLite default.
type Remote struct {
	Server    string `json:"server,omitempty"`
	TokenFile string `json:"token_file,omitempty"`
}

// Empty reports whether no remote targeting should be baked in.
func (r Remote) Empty() bool {
	return strings.TrimSpace(r.Server) == "" && strings.TrimSpace(r.TokenFile) == ""
}

// Validate rejects values that cannot survive the round trip into a skill
// file: a control character would break the single-line `# install-remote:`
// comment and the generated Markdown. URL shape is deliberately not checked,
// because no other `--server` in the CLI validates it and Args shell-quotes
// the value anyway.
func (r Remote) Validate() error {
	for _, f := range []struct{ name, value string }{
		{"server", r.Server},
		{"token file", r.TokenFile},
	} {
		if i := strings.IndexFunc(f.value, func(c rune) bool {
			return c < 0x20 || c == 0x7f
		}); i >= 0 {
			return fmt.Errorf(
				"skills: %s contains a control character at byte %d", f.name, i)
		}
	}
	return nil
}

// shellSafe reports whether c can appear unquoted in a POSIX shell word.
func shellSafe(c rune) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.ContainsRune("@%+=:,./-_~", c)
}

// shellQuote returns s as a single shell word, quoting only when needed so
// the common case stays readable. Generated examples are meant to be run
// verbatim, so a token path with a space must survive as one argument.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(c rune) bool { return !shellSafe(c) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Args returns the leading-space CLI flags to append to example commands,
// or empty when no remote is configured.
func (r Remote) Args() string {
	var b strings.Builder
	if s := strings.TrimSpace(r.Server); s != "" {
		b.WriteString(" --server ")
		b.WriteString(shellQuote(s))
	}
	if f := strings.TrimSpace(r.TokenFile); f != "" {
		b.WriteString(" --server-token-file ")
		b.WriteString(shellQuote(f))
	}
	return b.String()
}

const installRemotePrefix = "# install-remote: "

// ParseRemote reads a baked remote from an installed skill file. Missing or
// malformed remote lines yield an empty Remote rather than an error so list
// and reinstall keep working on older files.
func ParseRemote(content string) Remote {
	if !strings.HasPrefix(content, frontmatterFence) {
		return Remote{}
	}
	rest := strings.TrimPrefix(content, frontmatterFence)
	_, rest, ok := strings.Cut(rest, "\n")
	if !ok {
		return Remote{}
	}
	line, _, _ := strings.Cut(rest, "\n")
	if !strings.HasPrefix(line, installRemotePrefix) {
		return Remote{}
	}
	var remote Remote
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, installRemotePrefix)), &remote); err != nil {
		return Remote{}
	}
	if remote.Validate() != nil {
		return Remote{}
	}
	return remote
}

// templateData is the data passed to the finding-history template.
type templateData struct {
	Delegate   string
	ServerArgs string
	// AgentDir is the harness's project-level agent directory, included in
	// the delegation guard when the harness installs the search agent. Empty
	// for harnesses that do not install one, so the guard text omits the
	// project-directory clause.
	AgentDir string
}

// Rendered is one skill file ready to install.
type Rendered struct {
	Name         string // artifact name from its frontmatter
	RelativePath string // install path relative to the user or project base
	Content      string // full file: frontmatter fence, generated-by header, rest
	Hash         string // sha256 hex of Content minus the header line
}

// Render produces the skill for a harness. version is the CLI version
// string, recorded in the header for humans (hash is authoritative). remote
// is baked into example commands and an `# install-remote:` JSON comment so
// list/reinstall can round-trip it. The generated-by header is inserted as
// line two, inside the frontmatter fence, so the rendered file still begins
// with "---".
func Render(h Harness, version string, remote Remote) (Rendered, error) {
	delegate, ok := delegatePhrases[h]
	if !ok {
		return Rendered{}, fmt.Errorf("skills: unknown harness %q", h)
	}
	if err := remote.Validate(); err != nil {
		return Rendered{}, err
	}

	data := templateData{Delegate: delegate, ServerArgs: remote.Args()}
	if h == HarnessClaude {
		data.AgentDir = claudeAgentDir
	}
	var remoteLine string
	if !remote.Empty() {
		payload, err := json.Marshal(remote)
		if err != nil {
			return Rendered{}, fmt.Errorf("skills: encode remote: %w", err)
		}
		remoteLine = installRemotePrefix + string(payload) + "\n"
	}
	return renderTemplate(
		skillName,
		"",
		"finding-history.md.tmpl",
		data,
		version,
		remoteLine,
	)
}

// RenderPackage produces every artifact supported by h. Both harnesses get
// the recall skill; Claude also gets the bounded search agent used by it.
func RenderPackage(h Harness, version string, remote Remote) ([]Rendered, error) {
	skill, err := Render(h, version, remote)
	if err != nil {
		return nil, err
	}
	skill.RelativePath = filepath.Join(skillsSubdir[h], skillName, "SKILL.md")
	artifacts := []Rendered{skill}

	if h == HarnessClaude {
		agent, err := renderTemplate(
			"agentsview-search-conversations",
			filepath.Join(claudeAgentDir, "agentsview-search-conversations.md"),
			"search-conversations.md.tmpl",
			templateData{},
			version,
			"",
		)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, agent)
	}

	return artifacts, nil
}

// RenderPluginPackage produces the native plugin artifacts from the same
// templates as the standalone Claude package. Keeping the rendered content
// byte-identical lets lifecycle diagnostics recognize a duplicate standalone
// install without maintaining a second copy of the behavioral instructions.
func RenderPluginPackage(version string) ([]Rendered, error) {
	artifacts, err := RenderPackage(HarnessClaude, version, Remote{})
	if err != nil {
		return nil, err
	}
	for i := range artifacts {
		artifacts[i].RelativePath = strings.TrimPrefix(
			filepath.ToSlash(artifacts[i].RelativePath), ".claude/",
		)
	}
	return artifacts, nil
}

func renderTemplate(
	name string,
	relativePath string,
	templateName string,
	data templateData,
	version string,
	remoteLine string,
) (Rendered, error) {
	var body bytes.Buffer
	if err := tmpl.ExecuteTemplate(&body, templateName, data); err != nil {
		return Rendered{}, fmt.Errorf("skills: render %s template: %w", name, err)
	}
	if !strings.HasPrefix(body.String(), frontmatterFence) {
		return Rendered{}, fmt.Errorf(
			"skills: %s template must start with a %q frontmatter fence", name, "---")
	}

	rest := strings.TrimPrefix(body.String(), frontmatterFence)
	hashed := frontmatterFence + remoteLine + rest
	hash := bodyHash(hashed)
	header := fmt.Sprintf(headerFormat, version, hash)
	content := frontmatterFence + header + "\n" + remoteLine + rest

	return Rendered{
		Name:         name,
		RelativePath: relativePath,
		Content:      content,
		Hash:         hash,
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
