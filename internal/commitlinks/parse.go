// Package commitlinks parses recorded commit evidence without executing it.
package commitlinks

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"path/filepath"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

const (
	MaxCommandBytes = 256 << 10
	MaxResultBytes  = 1 << 20
)

// Summary is Git's successful commit summary, as observed in a tool result.
type Summary struct{ Branch, SHA, Subject string }

var summaryLine = regexp.MustCompile(`^\[(.+?) (?:\(root-commit\) )?([0-9a-f]{7,40})\] (.*)$`)
var trailerLine = regexp.MustCompile(`^Agent-Session:[ \t]*([a-z0-9][a-z0-9-]{0,31}):([A-Za-z0-9][A-Za-z0-9._-]{0,127})[ \t]*$`)
var uuidID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ParseSummaries accepts complete summary lines, including root and detached commits.
// Oversized results are unsupported rather than partially accepted.
func ParseSummaries(result string) []Summary {
	if len(result) > MaxResultBytes {
		return nil
	}
	var out []Summary
	seen := map[string]bool{}
	for line := range strings.SplitSeq(result, "\n") {
		m := summaryLine.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
		if m == nil || seen[m[2]] {
			continue
		}
		seen[m[2]] = true
		out = append(out, Summary{Branch: m[1], SHA: m[2], Subject: m[3]})
	}
	return out
}

// ParseTrailers maps validated agent-native identities to archive-native IDs,
// preserving encounter order. Archive namespace binding is a separate operation.
func ParseTrailers(message string) []string {
	if len(message) > MaxCommandBytes {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for line := range strings.SplitSeq(strings.ReplaceAll(message, "\r\n", "\n"), "\n") {
		m := trailerLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if (m[1] == "claude" || m[1] == "codex") && !uuidID.MatchString(m[2]) {
			continue
		}
		id := m[2]
		if m[1] != "claude" {
			id = m[1] + ":" + id
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// Invocation records a recognized commit command and its proven directory.
// An empty Repository explicitly forbids resolving against a guessed cwd.
type Invocation struct{ Repository string }

// CommandInput reads the normalized shell input keys. Array commands are not
// interpreted as shell programs. A literal workdir overrides the session cwd.
func CommandInput(input, cwd string) (command, directory string) {
	if len(input) > MaxCommandBytes {
		return "", ""
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal([]byte(input), &fields) != nil {
		return "", ""
	}
	for _, key := range []string{"command", "cmd"} {
		if raw, ok := fields[key]; ok && json.Unmarshal(raw, &command) == nil && command != "" {
			break
		}
	}
	directory = cwd
	for _, key := range []string{"workdir", "cwd"} {
		var value string
		if json.Unmarshal(fields[key], &value) == nil && value != "" {
			if filepath.IsAbs(value) {
				directory = filepath.Clean(value)
			} else {
				directory = filepath.Join(cwd, value)
			}
			break
		}
	}
	return command, directory
}

// CommitCommand recognizes literal git commit invocations in Bash syntax.
// It does not execute expansions, functions, aliases, configuration, or scripts.
// Multiple repository contexts, cd, and explicit dynamic targets remain unknown.
// A summary is a transcript observation, not proof against fabricated output.
func CommitCommand(command, cwd string) (Invocation, bool) {
	if command == "" || len(command) > MaxCommandBytes {
		return Invocation{}, false
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return Invocation{}, false
	}
	unknownCwd := false
	overriddenGit := false
	targetUnproven := false
	syntax.Walk(file, func(n syntax.Node) bool {
		if assign, ok := n.(*syntax.Assign); ok && assign.Name != nil && strings.HasPrefix(assign.Name.Value, "GIT_") {
			targetUnproven = true
		}
		if _, ok := n.(*syntax.FuncDecl); ok {
			overriddenGit = true
			return false
		}
		if call, ok := n.(*syntax.CallExpr); ok {
			for _, a := range call.Assigns {
				if strings.HasPrefix(a.Name.Value, "GIT_") {
					targetUnproven = true
				}
			}
		}
		if call, ok := n.(*syntax.CallExpr); ok && len(call.Args) > 0 {
			name, _ := literal(call.Args[0])
			if name == "alias" || name == "unalias" {
				overriddenGit = true
			}
			if name == "cd" || name == "pushd" || name == "popd" || name == "eval" || name == "source" || name == "." {
				unknownCwd = true
			}
		}
		return true
	})
	if overriddenGit {
		return Invocation{}, false
	}
	var repos []string
	syntax.Walk(file, func(n syntax.Node) bool {
		switch node := n.(type) {
		case *syntax.FuncDecl:
			return false
		case *syntax.CallExpr:
			if len(node.Args) == 0 {
				return true
			}
			name, ok := literal(node.Args[0])
			if !ok || (name != "git" && !strings.HasSuffix(name, "/git")) {
				return true
			}
			repo := cwd
			if unknownCwd {
				repo = ""
			}
			for _, a := range node.Assigns {
				if strings.HasPrefix(a.Name.Value, "GIT_") {
					repo = ""
				}
			}
			if dir, ok := gitCommit(node.Args[1:], repo, targetUnproven); ok {
				repos = append(repos, dir)
			}
		}
		return true
	})
	if len(repos) == 0 {
		return Invocation{}, false
	}
	repo := repos[0]
	for _, r := range repos[1:] {
		if r != repo {
			repo = ""
			break
		}
	}
	return Invocation{Repository: repo}, true
}

func gitCommit(words []*syntax.Word, repo string, targetUnproven bool) (string, bool) {
	for i := 0; i < len(words); i++ {
		arg, known := literal(words[i])
		if !known {
			return "", false
		}
		if arg == "commit" {
			for j := i + 1; j < len(words); j++ {
				v, _ := literal(words[j])
				if v == "--" {
					break
				}
				if v == "--dry-run" {
					return "", false
				}
				if v == "-m" || v == "--message" || v == "-F" || v == "--file" {
					j++
				}
			}
			if targetUnproven {
				return "", true
			}
			return repo, true
		}
		switch {
		case arg == "-C":
			i++
			if i >= len(words) {
				return "", false
			}
			path, ok := literal(words[i])
			if !ok {
				repo = ""
				continue
			}
			if filepath.IsAbs(path) {
				repo = filepath.Clean(path)
			} else if repo != "" {
				repo = filepath.Join(repo, path)
			}
		case arg == "-c" || arg == "--config-env":
			i++
			if i >= len(words) {
				return "", false
			}
		case arg == "--git-dir" || arg == "--work-tree":
			targetUnproven = true
			repo = ""
			i++
			if i >= len(words) {
				return "", false
			}
		case strings.HasPrefix(arg, "--git-dir=") || strings.HasPrefix(arg, "--work-tree=") || arg == "--bare":
			targetUnproven = true
			repo = ""
		case strings.HasPrefix(arg, "--config-env=") || strings.HasPrefix(arg, "-c") && len(arg) > 2:
		case arg == "--no-pager" || arg == "--paginate" || arg == "--no-optional-locks" || arg == "--literal-pathspecs":
		default:
			return "", false
		}
	}
	return "", false
}

func literal(word *syntax.Word) (string, bool) {
	var b strings.Builder
	var add func([]syntax.WordPart) bool
	add = func(parts []syntax.WordPart) bool {
		for _, part := range parts {
			switch p := part.(type) {
			case *syntax.Lit:
				b.WriteString(p.Value)
			case *syntax.SglQuoted:
				b.WriteString(p.Value)
			case *syntax.DblQuoted:
				if !add(p.Parts) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	ok := add(word.Parts)
	return b.String(), ok
}
