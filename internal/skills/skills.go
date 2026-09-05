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
}

// Rendered is one skill file ready to install.
type Rendered struct {
	Name    string // "agentsview-finding-history"
	Content string // full file: frontmatter fence, generated-by header, rest
	Hash    string // sha256 hex of Content minus the header line
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

	var body bytes.Buffer
	data := templateData{Delegate: delegate, ServerArgs: remote.Args()}
	if err := tmpl.ExecuteTemplate(&body, "finding-history.md.tmpl", data); err != nil {
		return Rendered{}, fmt.Errorf("skills: render %s template: %w", h, err)
	}
	if !strings.HasPrefix(body.String(), frontmatterFence) {
		return Rendered{}, fmt.Errorf(
			"skills: %s template must start with a %q frontmatter fence", h, "---")
	}

	rest := strings.TrimPrefix(body.String(), frontmatterFence)
	var remoteLine string
	if !remote.Empty() {
		payload, err := json.Marshal(remote)
		if err != nil {
			return Rendered{}, fmt.Errorf("skills: encode remote: %w", err)
		}
		remoteLine = installRemotePrefix + string(payload) + "\n"
	}
	hashed := frontmatterFence + remoteLine + rest
	hash := bodyHash(hashed)
	header := fmt.Sprintf(headerFormat, version, hash)
	content := frontmatterFence + header + "\n" + remoteLine + rest

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
