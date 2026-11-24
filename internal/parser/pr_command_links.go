package parser

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/export"
	"mvdan.cc/sh/v3/syntax"
)

var (
	repoPathRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	prHostRE   = regexp.MustCompile(`^[A-Za-z0-9.-]+(?::[0-9]+)?$`)
)

func shellLiteral(parts []syntax.WordPart) (string, bool) {
	var b strings.Builder
	for _, part := range parts {
		switch v := part.(type) {
		case *syntax.Lit:
			b.WriteString(v.Value)
		case *syntax.SglQuoted:
			if v.Dollar {
				return "", false
			}
			b.WriteString(v.Value)
		case *syntax.DblQuoted:
			if v.Dollar {
				return "", false
			}
			value, ok := shellLiteral(v.Parts)
			if !ok {
				return "", false
			}
			b.WriteString(value)
		default:
			return "", false
		}
	}
	return b.String(), true
}

func shellWords(args []*syntax.Word) ([]string, []bool) {
	words := make([]string, len(args))
	literal := make([]bool, len(args))
	for i, word := range args {
		words[i], literal[i] = shellLiteral(word.Parts)
	}
	return words, literal
}

func selectPRHost(explicitHost, remoteHost string, specified, hostUnknown bool) string {
	if explicitHost != "" {
		return explicitHost
	}
	if remoteHost != "" && !hostUnknown {
		return remoteHost
	}
	if specified && !hostUnknown {
		return "github.com"
	}
	return ""
}

// AnalyzePRCommand extracts links and shared shell context from one parse.
func AnalyzePRCommand(ctx context.Context, command, cwd, remote string, allowBare, hostUnknown bool) ([]PRLink, bool, bool, bool, []PRLink, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, false, false, nil, err
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, false, false, false, nil, ctx.Err()
	}
	directoryChanged, repositoryChanged, calls := bashChangesRepositoryContext(file)
	remoteHost, remoteRepo := "", ""
	if normalized, ok := export.NormalizeGitRemote(remote); ok {
		remoteHost, remoteRepo, _ = strings.Cut(normalized, "/")
	}
	var links [][]PRLink
	var dependsOnRemote []PRLink
	for _, entry := range calls {
		if err := ctx.Err(); err != nil {
			return nil, false, false, false, nil, err
		}
		args := entry.call.Args
		bareSafe := allowBare && !entry.directory && !entry.repository
		words, literal := shellWords(args)
		if len(words) < 4 || !literal[0] || !literal[1] || !literal[2] || words[0] != "gh" || words[1] != "pr" {
			continue
		}
		callHostUnknown := hostUnknown || entry.repository
		if words[2] == "co" {
			words[2] = "checkout"
		}
		switch words[2] {
		case "view", "checkout", "edit", "diff", "checks", "comment", "review", "ready", "merge", "close", "reopen", "update-branch", "lock", "unlock", "revert":
		default:
			continue
		}
		selector, repo := "", ""
		specified, invalid := false, false
		for i := 3; i < len(words); i++ {
			if !literal[i] {
				invalid = true
				continue
			}
			word := words[i]
			switch {
			case word == "-R" || word == "--repo":
				specified = true
				if i+1 >= len(words) || !literal[i+1] {
					invalid = true
					continue
				}
				i++
				repo = words[i]
			case strings.HasPrefix(word, "--repo="):
				specified = true
				repo = strings.TrimPrefix(word, "--repo=")
			case strings.HasPrefix(word, "-R") && len(word) > 2:
				specified = true
				repo = strings.TrimPrefix(word[2:], "=")
			case strings.HasPrefix(word, "-"):
				invalid = true
			case selector == "":
				selector = word
			}
		}
		if invalid {
			continue
		}
		if strings.HasPrefix(selector, "https://") || strings.HasPrefix(selector, "http://") {
			found, err := PromptPRLinksContext(ctx, selector)
			if err != nil {
				return nil, false, false, false, nil, err
			}
			links = append(links, found)
			continue
		}
		number, err := strconv.Atoi(strings.TrimPrefix(selector, "#"))
		if err != nil || number <= 0 || invalid {
			continue
		}
		if !specified {
			if !bareSafe {
				continue
			}
			repo = remoteRepo
		}
		explicitHost := ""
		if specified && strings.Count(repo, "/") == 2 {
			explicitHost, repo, _ = strings.Cut(repo, "/")
		}
		if explicitHost != "" && !prHostRE.MatchString(explicitHost) {
			continue
		}
		if !repoPathRE.MatchString(repo) {
			continue
		}
		selectedHost := selectPRHost(explicitHost, remoteHost, specified, callHostUnknown)
		if selectedHost == "" {
			continue
		}
		if link, ok := NewPRLink(fmt.Sprintf("https://%s/%s/pull/%d", selectedHost, repo, number), repo, number); ok {
			links = append(links, []PRLink{link})
			if !specified || explicitHost == "" {
				dependsOnRemote = append(dependsOnRemote, link)
			}
		}
	}
	return CombinePRLinks(links...), directoryChanged, repositoryChanged, repositoryChanged, dependsOnRemote, nil
}

// SamePRWorkingDirectory compares recorded checkout paths across platforms.
func SamePRWorkingDirectory(cwd, dir string) bool {
	return cwd != "" && dir != "" && path.Clean(strings.ReplaceAll(cwd, `\`, "/")) == path.Clean(strings.ReplaceAll(dir, `\`, "/"))
}

type prShellCall struct {
	call                  *syntax.CallExpr
	directory, repository bool
}

func bashChangesRepositoryContext(file *syntax.File) (bool, bool, []prShellCall) {
	functions := make(map[string]bool)
	syntax.Walk(file, func(node syntax.Node) bool {
		if fn, ok := node.(*syntax.FuncDecl); ok {
			functions[fn.Name.Value] = true
			return false
		}
		return true
	})
	var directory, repository bool
	var calls []prShellCall
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.FuncDecl:
			return false
		case *syntax.Assign:
			if n.Name != nil && (n.Name.Value == "GH_REPO" || n.Name.Value == "GH_HOST") {
				repository = true
			}
		case *syntax.CallExpr:
			callDirectory, callRepository := directory, repository
			args := n.Args
			if len(args) > 0 {
				name, _ := shellLiteral(args[0].Parts)
				if name == "gh" {
					syntax.Walk(n, func(child syntax.Node) bool {
						if a, ok := child.(*syntax.Assign); ok && a.Name != nil && (a.Name.Value == "GH_REPO" || a.Name.Value == "GH_HOST") {
							callRepository = true
						}
						if inner, ok := child.(*syntax.CallExpr); ok && inner != n {
							dir, repo := shellCallChangesPRContext(inner, functions)
							callDirectory = callDirectory || dir
							callRepository = callRepository || repo
						}
						return true
					})
				}
			}
			calls = append(calls, prShellCall{n, callDirectory, callRepository})
			dir, repo := shellCallChangesPRContext(n, functions)
			directory = directory || dir
			repository = repository || repo
		}
		return true
	})
	return directory, repository, calls
}

func shellCallChangesPRContext(call *syntax.CallExpr, functions map[string]bool) (bool, bool) {
	words := make([]string, len(call.Args))
	for i, arg := range call.Args {
		words[i], _ = shellLiteral(arg.Parts)
	}
	for len(words) > 1 && (words[0] == "command" || words[0] == "builtin") {
		wrapper := words[0]
		words = words[1:]
		if wrapper == "command" {
			for len(words) > 0 && strings.HasPrefix(words[0], "-") && words[0] != "--" {
				flags := strings.TrimPrefix(words[0], "-")
				if flags == "" || strings.Trim(flags, "p") != "" {
					break
				}
				words = words[1:]
			}
		}
		if len(words) > 0 && words[0] == "--" {
			words = words[1:]
		}
	}
	if len(words) == 0 {
		return false, false
	}
	name := words[0]
	return name == "cd" || name == "pushd" || name == "popd", name == "eval" || name == "source" || name == "." || functions[name] || repositoryMutation(words)
}

func repositoryMutation(words []string) bool {
	if len(words) < 2 {
		return false
	}
	if words[0] == "unset" {
		for _, word := range words[1:] {
			if word == "GH_REPO" || word == "GH_HOST" {
				return true
			}
		}
	}
	if words[0] == "gh" && len(words) >= 3 && words[1] == "repo" && words[2] == "set-default" {
		for _, word := range words[3:] {
			if word == "--view" || word == "-v" {
				return false
			}
		}
		return true
	}
	return words[0] == "git" && gitRepositoryMutation(words[1:])
}

// gitRepositoryMutation reports whether git's subcommand, after its global
// options, can change a repository's remotes or branches.
func gitRepositoryMutation(words []string) bool {
	i := 0
	for i < len(words) && strings.HasPrefix(words[i], "-") {
		option := words[i]
		i++
		switch option {
		case "-C", "-c", "--git-dir", "--work-tree", "--namespace", "--config-env", "--super-prefix":
			if i >= len(words) {
				return true
			}
			i++
		}
	}
	if i >= len(words) {
		return false
	}
	words = words[i:]
	switch words[0] {
	case "config":
		return gitConfigMutation(words[1:])
	case "remote":
		return len(words) > 1 && (words[1] == "add" || words[1] == "remove" || words[1] == "rename" || words[1] == "set-url")
	}
	return false
}

func gitConfigMutation(args []string) bool {
	write := false
	for _, word := range args {
		if word == "--get" || word == "--get-all" || word == "--list" || word == "-l" {
			return false
		}
		if word == "--add" || word == "--replace-all" || word == "--unset" || word == "--unset-all" || word == "--rename-section" || word == "--remove-section" || word == "set" || word == "unset" {
			write = true
		}
	}
	for i, word := range args {
		key := strings.ToLower(word)
		if strings.HasPrefix(key, "remote.") || strings.HasPrefix(key, "branch.") || strings.HasPrefix(key, "url.") {
			return write || i+1 < len(args)
		}
	}
	return false
}
