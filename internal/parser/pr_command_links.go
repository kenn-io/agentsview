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

// quotedValue stays one argument when it contains a simple shell variable.
func quotedValue(word *syntax.Word) bool {
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if strings.ContainsAny(p.Value, "*?[~") {
				return false
			}
		case *syntax.SglQuoted:
		case *syntax.DblQuoted:
			if p.Dollar {
				return false
			}
			for _, inner := range p.Parts {
				switch v := inner.(type) {
				case *syntax.Lit:
				case *syntax.ParamExp:
					if v.Param == nil || v.Param.Value == "@" || v.Param.Value == "*" || v.Index != nil || v.Exp != nil || v.Repl != nil || v.Slice != nil || v.NestedParam != nil {
						return false
					}
				default:
					return false
				}
			}
		default:
			return false
		}
	}
	return true
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

// gh flags differ by subcommand, including short flags that take a value in
// one command and are boolean in another. Unknown flags are ambiguous.
func prCommandFlag(command, flag string) (takesValue, ok bool) {
	flags := map[string][2]string{
		"view":          {"-q --jq --json -t --template", "-c --comments -w --web"},
		"checkout":      {"-b --branch --worktree", "--detach -f --force --recurse-submodules"},
		"edit":          {"--add-assignee --add-label --add-project --add-reviewer --attach -B --base -b --body -F --body-file -m --milestone --remove-assignee --remove-label --remove-project --remove-reviewer -t --title", "--remove-milestone"},
		"diff":          {"--color -e --exclude", "--allow-escape-sequences --name-only --patch -w --web"},
		"checks":        {"-i --interval -q --jq --json -t --template", "--fail-fast --required --watch -w --web"},
		"comment":       {"--attach -b --body -F --body-file", "--create-if-none --delete-last --edit-last -e --editor -w --web --yes"},
		"review":        {"-b --body -F --body-file", "-a --approve -c --comment -r --request-changes"},
		"ready":         {"", "--undo"},
		"merge":         {"-A --author-email -b --body -F --body-file --match-head-commit -t --subject", "--admin --auto -d --delete-branch --disable-auto -m --merge -r --rebase -s --squash"},
		"close":         {"-c --comment", "-d --delete-branch"},
		"reopen":        {"-c --comment", ""},
		"update-branch": {"", "--rebase"},
		"lock":          {"-r --reason", ""},
		"unlock":        {"", ""},
		"revert":        {"-b --body -F --body-file -t --title", "-d --draft"},
	}
	for kind, list := range flags[command] {
		for name := range strings.FieldsSeq(list) {
			if flag == name {
				return kind == 0, true
			}
		}
	}
	return false, false
}

// unwrapPRCommand removes execution wrappers whose arguments have fixed roles.
func unwrapPRCommand(args []*syntax.Word, allowEnvOverride bool) []*syntax.Word {
	for len(args) > 0 {
		words, literal := shellWords(args)
		if !literal[0] {
			return nil
		}
		switch words[0] {
		case "command", "env":
			i, ok := wrappedCommandStart(words, literal, allowEnvOverride)
			if !ok {
				return nil
			}
			args = args[i:]
		case "timeout":
			i := 1
			for i < len(words) && literal[i] && strings.HasPrefix(words[i], "-") {
				flag := words[i]
				i++
				switch flag {
				case "--":
					goto duration
				case "-k", "--kill-after", "-s", "--signal":
					i++
				case "--foreground", "--preserve-status", "-v", "--verbose":
				default:
					if !strings.HasPrefix(flag, "--kill-after=") && !strings.HasPrefix(flag, "--signal=") {
						return nil
					}
				}
			}
		duration:
			if i >= len(words) || !literal[i] || words[i] == "" {
				return nil
			}
			args = args[i+1:]
		default:
			return args
		}
	}
	return nil
}

// wrappedCommandStart skips command's -p flags or env's assignments. Strict
// matching fails when env sets the repository or host gh would use.
func wrappedCommandStart(words []string, literal []bool, allowEnvOverride bool) (int, bool) {
	i := 1
	for i < len(words) && literal[i] && (words[0] == "command" && words[i] == "-p" || words[0] == "env" && strings.Contains(words[i], "=")) {
		name, _, _ := strings.Cut(words[i], "=")
		if !allowEnvOverride && words[0] == "env" && (name == "GH_HOST" || name == "GH_REPO") {
			return 0, false
		}
		i++
	}
	if i < len(words) && literal[i] && words[i] == "--" {
		i++
	}
	return i, true
}

// ghPRArgs moves inherited repository flags after the PR action.
func ghPRArgs(args []*syntax.Word) []*syntax.Word {
	words, literal := shellWords(args)
	if len(words) == 0 || !literal[0] || words[0] != "gh" {
		return nil
	}
	i := 1
	for i < len(words) && literal[i] && strings.HasPrefix(words[i], "-") {
		switch {
		case words[i] == "-R" || words[i] == "--repo":
			i += 2
		case strings.HasPrefix(words[i], "-R") && len(words[i]) > 2, strings.HasPrefix(words[i], "--repo="):
			i++
		default:
			return nil
		}
	}
	if i+1 >= len(args) || !literal[i] || !literal[i+1] || words[i] != "pr" {
		return nil
	}
	result := append([]*syntax.Word{args[0], args[i], args[i+1]}, args[1:i]...)
	return append(result, args[i+2:]...)
}

func shellCommand(args []*syntax.Word) (string, bool) {
	words, literal := shellWords(args)
	if len(words) < 3 || !literal[0] {
		return "", false
	}
	switch path.Base(words[0]) {
	case "sh", "bash", "zsh":
	default:
		return "", false
	}
	for i := 1; i < len(words); i++ {
		if !literal[i] || !strings.HasPrefix(words[i], "-") || strings.HasPrefix(words[i], "--") {
			return "", false
		}
		if strings.Contains(words[i][1:], "n") {
			return "", false
		}
		if strings.Contains(words[i][1:], "c") {
			if i+1 < len(words) && literal[i+1] {
				return words[i+1], true
			}
			return "", false
		}
	}
	return "", false
}

// AnalyzePRCommand extracts links and shared shell context from one parse.
func AnalyzePRCommand(ctx context.Context, command, cwd, remote string, allowBare, hostUnknown bool) ([]PRLink, bool, bool, []PRLink, error) {
	return analyzePRCommand(ctx, command, cwd, remote, allowBare, hostUnknown, 0)
}

func analyzePRCommand(ctx context.Context, command, cwd, remote string, allowBare, hostUnknown bool, depth int) ([]PRLink, bool, bool, []PRLink, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, false, nil, err
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, false, false, nil, ctx.Err()
	}
	directoryChanged, repositoryChanged, calls := bashChangesRepositoryContext(file, cwd)
	remoteHost, remoteRepo := "", ""
	if normalized, ok := export.NormalizeGitRemote(remote); ok {
		remoteHost, remoteRepo, _ = strings.Cut(normalized, "/")
	}
	var links [][]PRLink
	var dependsOnRemote []PRLink
	for _, entry := range calls {
		if err := ctx.Err(); err != nil {
			return nil, false, false, nil, err
		}
		args := unwrapPRCommand(entry.call.Args, false)
		bareSafe := allowBare && !entry.directory && !entry.repository
		if inner, ok := shellCommand(args); ok && depth < 8 {
			found, _, nested, dependencies, err := analyzePRCommand(ctx, inner, cwd, remote, bareSafe, hostUnknown || entry.repository, depth+1)
			if err != nil {
				return nil, false, false, nil, err
			}
			links = append(links, found)
			dependsOnRemote = append(dependsOnRemote, dependencies...)
			// A nested shell's remote change outlives it; its directory change does not.
			repositoryChanged, allowBare, hostUnknown = repositoryChanged || nested, allowBare && !nested, hostUnknown || nested
			continue
		}
		args = ghPRArgs(args)
		if args == nil {
			continue
		}
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
				// An attached, quoted value cannot add shell arguments.
				if len(args[i].Parts) > 1 && quotedValue(args[i]) {
					if prefix, ok := args[i].Parts[0].(*syntax.Lit); ok {
						flag, _, attached := strings.Cut(prefix.Value, "=")
						takesValue, known := prCommandFlag(words[2], flag)
						if attached && known && takesValue {
							continue
						}
					}
				}
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
				flag, _, attached := strings.Cut(word, "=")
				if !strings.HasPrefix(flag, "--") && len(flag) > 2 {
					flag, attached = flag[:2], true
				}
				takesValue, ok := prCommandFlag(words[2], flag)
				if !ok && selector != "" {
					// An unknown option may consume a later repository flag.
					for j := i + 1; j < len(words); j++ {
						if !literal[j] || words[j] == "--repo" || strings.HasPrefix(words[j], "--repo=") || strings.HasPrefix(words[j], "-R") {
							invalid = true
						}
					}
					i = len(words)
				} else if !ok || (attached && !takesValue) {
					invalid = true
				} else if takesValue && !attached {
					if i+1 >= len(words) || (!literal[i+1] && !quotedValue(args[i+1])) {
						invalid = true
					} else {
						i++
					}
				}
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
				return nil, false, false, nil, err
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
		explicitHost, repo, valid := splitPRRepository(repo, specified)
		if !valid {
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
	return CombinePRLinks(links...), directoryChanged, repositoryChanged, dependsOnRemote, nil
}

// splitPRRepository separates an explicit host from a -R repository and
// reports whether both parts are well formed.
func splitPRRepository(repo string, specified bool) (host, name string, valid bool) {
	if specified && strings.Count(repo, "/") == 2 {
		host, repo, _ = strings.Cut(repo, "/")
	}
	return host, repo, (host == "" || prHostRE.MatchString(host)) && repoPathRE.MatchString(repo)
}

// SamePRWorkingDirectory compares recorded checkout paths across platforms.
func SamePRWorkingDirectory(cwd, dir string) bool {
	return cwd != "" && dir != "" && path.Clean(strings.ReplaceAll(cwd, `\`, "/")) == path.Clean(strings.ReplaceAll(dir, `\`, "/"))
}

type prShellCall struct {
	call                  *syntax.CallExpr
	directory, repository bool
}

func bashChangesRepositoryContext(file *syntax.File, cwd string) (bool, bool, []prShellCall) {
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
	callAssignments := make(map[*syntax.Assign]bool)
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.FuncDecl:
			return false
		case *syntax.Assign:
			if !callAssignments[n] && n.Name != nil && (n.Name.Value == "GH_REPO" || n.Name.Value == "GH_HOST") {
				repository = true
			}
		case *syntax.CallExpr:
			if len(n.Args) > 0 {
				for _, assignment := range n.Assigns {
					callAssignments[assignment] = true
				}
			}
			callDirectory, callRepository := directory, repository
			args := unwrapPRCommand(n.Args, false)
			if len(args) > 0 {
				name, _ := shellLiteral(args[0].Parts)
				if name == "gh" || path.Base(name) == "bash" || path.Base(name) == "sh" || path.Base(name) == "zsh" {
					syntax.Walk(n, func(child syntax.Node) bool {
						if a, ok := child.(*syntax.Assign); ok && a.Name != nil && (a.Name.Value == "GH_REPO" || a.Name.Value == "GH_HOST") {
							callRepository = true
						}
						if inner, ok := child.(*syntax.CallExpr); ok && inner != n {
							dir, repo := shellCallChangesPRContext(inner, functions, cwd)
							callDirectory = callDirectory || dir
							callRepository = callRepository || repo
						}
						return true
					})
				}
			}
			calls = append(calls, prShellCall{n, callDirectory, callRepository})
			dir, repo := shellCallChangesPRContext(n, functions, cwd)
			directory = directory || dir
			repository = repository || repo
		}
		return true
	})
	return directory, repository, calls
}

// leavesCheckout reports whether words change directory, keeping a cd or
// pushd to the session checkout's own absolute path in place.
func leavesCheckout(words []string, cwd string) bool {
	switch words[0] {
	case "cd", "pushd":
		return len(words) != 2 || !path.IsAbs(words[1]) || !SamePRWorkingDirectory(cwd, words[1])
	}
	return words[0] == "popd"
}

func shellCallChangesPRContext(call *syntax.CallExpr, functions map[string]bool, cwd string) (bool, bool) {
	args := unwrapPRCommand(call.Args, true)
	words := make([]string, len(args))
	for i, arg := range args {
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
	return leavesCheckout(words, cwd), name == "eval" || name == "source" || name == "." || functions[name] || repositoryMutation(words) || len(unwrapPRCommand(call.Args, false)) == 0 && (path.Base(name) == "sh" || path.Base(name) == "bash" || path.Base(name) == "zsh")
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
