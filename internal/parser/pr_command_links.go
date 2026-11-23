package parser

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/export"
	"mvdan.cc/sh/v3/syntax"
)

var (
	repoPartRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	repoPathRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

type shellCall struct {
	Words         []string
	Literal       []bool
	Tainted       bool
	HostUncertain bool
	Repo          string
	RepoSet       bool
	Stdin         string
}

func shellLiteral(parts []syntax.WordPart) (string, bool) {
	var b strings.Builder
	for _, part := range parts {
		switch v := part.(type) {
		case *syntax.Lit:
			b.WriteString(v.Value)
		case *syntax.SglQuoted:
			if v.Dollar {
				return b.String(), false
			}
			b.WriteString(v.Value)
		case *syntax.DblQuoted:
			if v.Dollar {
				return b.String(), false
			}
			value, ok := shellLiteral(v.Parts)
			if !ok {
				b.WriteString(value)
				return b.String(), false
			}
			b.WriteString(value)
		default:
			return b.String(), false
		}
	}
	return b.String(), true
}

// SamePRWorkingDirectory compares paths using the recorded checkout's syntax.
func SamePRWorkingDirectory(cwd, dir string) bool {
	return cwd != "" && dir != "" && path.Clean(strings.ReplaceAll(cwd, `\`, "/")) == path.Clean(strings.ReplaceAll(dir, `\`, "/"))
}

// PRShellContext records which parts of a shared shell's context are unknown.
// CheckoutUncertain follows directory changes and clears when the shell
// returns to the recorded checkout. RepoUncertain is permanent: it covers
// changes to the repository GitHub CLI targets. Both a subshell's directory
// change and its host change end with the subshell.
type PRShellContext struct {
	CheckoutUncertain bool
	HostUncertain     bool
	RepoUncertain     bool
}

// UncertainPRShell is the state after input that cannot be read.
func UncertainPRShell() PRShellContext {
	return PRShellContext{CheckoutUncertain: true, HostUncertain: true, RepoUncertain: true}
}

// Changed reports whether any part of the shell context is unknown.
func (s PRShellContext) Changed() bool {
	return s.CheckoutUncertain || s.HostUncertain || s.RepoUncertain
}

// CommandPRLinks derives links and reports whether the shell context changed.
func CommandPRLinks(ctx context.Context, command, cwd, remote string) ([]PRLink, bool, error) {
	links, state, err := CommandPRLinksWithContext(ctx, command, cwd, remote, PRShellContext{})
	return links, state.Changed(), err
}

// CommandPRLinksWithContext carries shared shell state between tool calls.
func CommandPRLinksWithContext(ctx context.Context, command, cwd, remote string, state PRShellContext) ([]PRLink, PRShellContext, error) {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, UncertainPRShell(), nil //nolint:nilerr // Malformed optional shell input leaves the shell context unknown.
	}
	w := &prShellWalker{cwd: cwd, state: state, configRepo: state.RepoUncertain, stdin: map[*syntax.CallExpr]string{}}
	syntax.Walk(file, w.visit)
	var lists [][]PRLink
	for _, call := range w.calls {
		links, nested, err := call.links(ctx, cwd, remote)
		if err != nil {
			return nil, PRShellContext{}, err
		}
		w.state.RepoUncertain = w.state.RepoUncertain || nested.RepoUncertain
		lists = append(lists, links)
	}
	return CombinePRLinks(lists...), w.state, ctx.Err()
}

var shellNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// programName names the program a command word runs, ignoring its directory,
// case, and Windows executable suffix.
func programName(word string) string {
	return strings.TrimSuffix(strings.ToLower(path.Base(strings.ReplaceAll(word, `\`, "/"))), ".exe")
}

type prShellScope struct {
	restore, checkout, host, repo bool
}

// prShellWalker follows a shell script in execution order.
type prShellWalker struct {
	cwd    string
	state  PRShellContext
	calls  []shellCall
	scopes []prShellScope
	stdin  map[*syntax.CallExpr]string
	// configRepo records repository configuration changes, which outlast a
	// subshell. Environment changes such as GH_REPO end with it.
	configRepo bool
}

func (w *prShellWalker) visit(node syntax.Node) bool {
	if node == nil {
		scope := w.scopes[len(w.scopes)-1]
		w.scopes = w.scopes[:len(w.scopes)-1]
		w.leave(scope)
		return true
	}
	if _, ok := node.(*syntax.FuncDecl); ok {
		w.configRepo, w.state.RepoUncertain, w.state.HostUncertain = true, true, true
		return false
	}
	scope := prShellScope{checkout: w.state.CheckoutUncertain, host: w.state.HostUncertain, repo: w.state.RepoUncertain}
	switch node.(type) {
	case *syntax.Subshell, *syntax.CmdSubst, *syntax.ProcSubst:
		scope.restore = true
	}
	w.scopes = append(w.scopes, scope)
	switch n := node.(type) {
	case *syntax.Stmt:
		w.recordHeredoc(n, n)
	case *syntax.BinaryCmd:
		if n.Op == syntax.Pipe {
			w.recordHeredoc(n.X, n.Y)
		}
	case *syntax.DeclClause:
		for _, assignment := range n.Args {
			if assignment.Name != nil {
				w.persistEnv(assignment.Name.Value)
			}
		}
	case *syntax.CallExpr:
		w.callExpr(n)
	}
	return true
}

// leave ends a scope, restoring the shell state a subshell cannot change.
func (w *prShellWalker) leave(scope prShellScope) {
	if scope.restore {
		w.state.CheckoutUncertain, w.state.HostUncertain = scope.checkout, scope.host
		w.state.RepoUncertain = scope.repo || w.configRepo
	}
}

// persistRepo records a repository configuration change that outlasts a
// subshell.
func (w *prShellWalker) persistRepo(changed bool) {
	w.configRepo = w.configRepo || changed
	w.state.RepoUncertain = w.state.RepoUncertain || changed
}

// recordHeredoc gives a command the literal here-documents of from as input.
func (w *prShellWalker) recordHeredoc(from, to *syntax.Stmt) {
	call, ok := to.Cmd.(*syntax.CallExpr)
	if !ok {
		return
	}
	for _, redirect := range from.Redirs {
		if redirect.Hdoc == nil {
			continue
		}
		if text, literal := shellLiteral(redirect.Hdoc.Parts); literal {
			w.stdin[call] += text + "\n"
		}
	}
}

// persistEnv records an environment change that outlasts one command.
func (w *prShellWalker) persistEnv(name string) {
	switch name {
	case "GH_REPO":
		w.state.RepoUncertain = true
	case "GH_HOST":
		w.state.HostUncertain = true
	}
}

func (w *prShellWalker) callExpr(n *syntax.CallExpr) {
	call := shellCall{HostUncertain: w.state.HostUncertain, Stdin: w.stdin[n]}
	for _, assignment := range n.Assigns {
		if assignment.Name == nil {
			continue
		}
		var value string
		literal := assignment.Value == nil
		if !literal {
			value, literal = shellLiteral(assignment.Value.Parts)
		}
		call.setEnv(assignment.Name.Value, value, literal)
		if len(n.Args) == 0 {
			w.persistEnv(assignment.Name.Value)
		}
	}
	for _, word := range n.Args {
		value, literal := shellLiteral(word.Parts)
		call.Words = append(call.Words, value)
		call.Literal = append(call.Literal, literal)
	}
	call.unwrapEnv()
	w.applyBuiltin(call)
	call.Tainted = call.Tainted || w.state.CheckoutUncertain || w.state.RepoUncertain
	w.calls = append(w.calls, call)
}

// applyBuiltin applies a command's effect on the shell that runs it.
func (w *prShellWalker) applyBuiltin(call shellCall) {
	if len(call.Words) == 0 || !call.Literal[0] {
		return
	}
	switch programName(call.Words[0]) {
	case "cd":
		w.state.CheckoutUncertain = len(call.Words) != 2 || !call.Literal[1] || !SamePRWorkingDirectory(w.cwd, call.Words[1])
	case "pushd", "popd", "eval", "source", ".":
		w.persistRepo(true)
	case "git":
		w.persistRepo(call.changesRemote())
	case "gh":
		w.persistRepo(call.setsDefaultRepo())
	}
}

// setEnv applies a per-command environment assignment.
func (c *shellCall) setEnv(name, value string, literal bool) {
	switch name {
	case "GH_HOST":
		c.HostUncertain = true
	case "GH_REPO":
		c.Tainted, c.RepoSet, c.Repo = true, true, ""
		if literal {
			c.Repo = value
		}
	}
}

// unwrapEnv replaces `env NAME=value command` with the command it runs.
func (c *shellCall) unwrapEnv() {
	if len(c.Words) == 0 || !c.Literal[0] || programName(c.Words[0]) != "env" {
		return
	}
	i := 1
	for ; i < len(c.Words); i++ {
		name, value, ok := strings.Cut(c.Words[i], "=")
		if !ok || !shellNameRE.MatchString(name) {
			break
		}
		c.setEnv(name, value, c.Literal[i])
	}
	c.Words, c.Literal = c.Words[i:], c.Literal[i:]
}

// changesRemote reports whether a git command rewrites a remote URL.
func (c shellCall) changesRemote() bool {
	for i, word := range c.Words {
		if word == "remote" && i+1 < len(c.Words) && slices.Contains([]string{"set-url", "add", "remove", "rm", "rename"}, c.Words[i+1]) {
			return true
		}
		if strings.HasPrefix(word, "remote.") && strings.HasSuffix(word, ".url") {
			return true
		}
	}
	return false
}

// setsDefaultRepo reports whether gh changes its default repository.
func (c shellCall) setsDefaultRepo() bool {
	return len(c.Words) > 2 && c.Words[1] == "repo" && c.Words[2] == "set-default" &&
		!slices.Contains(c.Words[3:], "--view") && !slices.Contains(c.Words[3:], "-v")
}

// links derives links from one command and reports shell state it leaves.
func (c shellCall) links(ctx context.Context, cwd, remote string) ([]PRLink, PRShellContext, error) {
	if err := ctx.Err(); err != nil {
		return nil, PRShellContext{}, err
	}
	callRemote := remote
	if c.Tainted {
		callRemote = ""
	}
	lists := [][]PRLink{ghPRCallLinks(ctx, c, callRemote)}
	var nested PRShellContext
	if script, ok := c.shellScript(); ok {
		links, state, err := CommandPRLinksWithContext(ctx, script, cwd, callRemote, PRShellContext{HostUncertain: c.HostUncertain})
		if err != nil {
			return nil, PRShellContext{}, err
		}
		lists, nested = append(lists, links), state
	}
	links, err := PromptPRLinksContext(ctx, c.launcherPrompt())
	return CombinePRLinks(append(lists, links)...), nested, err
}

// shellScript returns the script a `sh -c` style command runs.
func (c shellCall) shellScript() (string, bool) {
	if len(c.Words) == 0 || !c.Literal[0] {
		return "", false
	}
	switch programName(c.Words[0]) {
	case "sh", "bash", "zsh":
		for i := 1; i+1 < len(c.Words); i++ {
			if c.Literal[i] && (c.Words[i] == "-c" || c.Words[i] == "-lc") && c.Literal[i+1] {
				return c.Words[i+1], true
			}
		}
	}
	return "", false
}

// repoFlag reports whether args[i] is a -R or --repo option, its value, and
// the index of its last word, or -1 when the value is missing or not literal.
func repoFlag(call shellCall, i int) (string, int, bool) {
	arg := call.Words[i]
	switch {
	case arg == "-R" || arg == "--repo":
		if i+1 >= len(call.Words) || !call.Literal[i+1] {
			return "", -1, true
		}
		return call.Words[i+1], i + 1, true
	case strings.HasPrefix(arg, "--repo="):
		return strings.TrimPrefix(arg, "--repo="), i, true
	case strings.HasPrefix(arg, "-R") && len(arg) > 2:
		return strings.TrimPrefix(arg[2:], "="), i, true
	}
	return "", i, false
}

var ghPRValueFlags = map[string]string{
	"view":     "--json --jq -q --template -t",
	"diff":     "--color --exclude -e",
	"checks":   "--interval -i --jq -q --json --template -t",
	"merge":    "--author-email -A --body -b --body-file -F --match-head-commit --subject -t",
	"edit":     "--add-assignee --add-label --add-project --add-reviewer --attach --base -B --body -b --body-file -F --milestone -m --remove-assignee --remove-label --remove-milestone --remove-project --remove-reviewer --title -t",
	"comment":  "--attach --body -b --body-file -F",
	"review":   "--body -b --body-file -F",
	"close":    "--comment -c",
	"reopen":   "--comment -c",
	"checkout": "--branch -b --worktree",
	"lock":     "--reason -r",
	"revert":   "--body -b --body-file -F --title -t",
	"ready":    "",
	"unlock":   "",

	"update-branch": "",
}

// ghPRTarget is the repository and pull request a gh command names.
type ghPRTarget struct {
	repo, selector string
	repoSpecified  bool
}

// ghPRTargetOf reads a literal `gh pr <action>` command's target.
func ghPRTargetOf(call shellCall) (ghPRTarget, bool) {
	args := call.Words
	if len(args) < 4 || !call.Literal[0] || programName(args[0]) != "gh" {
		return ghPRTarget{}, false
	}
	target := ghPRTarget{repo: call.Repo, repoSpecified: call.RepoSet}
	i := 1
	for i < len(args) && call.Literal[i] {
		value, last, isRepo := repoFlag(call, i)
		if !isRepo {
			break
		}
		if last < 0 {
			return ghPRTarget{}, false
		}
		target.repo, target.repoSpecified, i = value, true, last+1
	}
	if i+2 >= len(args) || !call.Literal[i] || !call.Literal[i+1] || args[i] != "pr" {
		return ghPRTarget{}, false
	}
	action := args[i+1]
	if action == "co" {
		action = "checkout"
	}
	valueFlags, known := ghPRValueFlags[action]
	if !known {
		return ghPRTarget{}, false
	}
	takesValue := strings.Fields(valueFlags)
	for j := i + 2; j < len(args); j++ {
		if !call.Literal[j] {
			if strings.HasPrefix(args[j], "-R") || strings.HasPrefix(args[j], "--repo") {
				return ghPRTarget{}, false
			}
			continue
		}
		if value, last, isRepo := repoFlag(call, j); isRepo {
			if last < 0 {
				return ghPRTarget{}, false
			}
			target.repo, target.repoSpecified, j = value, true, last
			continue
		}
		flag, _, attached := strings.Cut(args[j], "=")
		switch {
		case strings.HasPrefix(args[j], "-"):
			if !attached && slices.Contains(takesValue, flag) {
				j++
			}
			if j >= len(args) {
				return ghPRTarget{}, false
			}
		case target.selector == "":
			target.selector = args[j]
		}
	}
	return target, true
}

func ghPRCallLinks(ctx context.Context, call shellCall, remote string) []PRLink {
	target, ok := ghPRTargetOf(call)
	if !ok {
		return nil
	}
	selector := target.selector
	if links, _ := PromptPRLinksContext(ctx, selector); len(links) == 1 && (strings.HasPrefix(selector, "https://") || strings.HasPrefix(selector, "http://")) {
		return links
	}
	number, err := strconv.Atoi(strings.TrimPrefix(selector, "#"))
	if err != nil || number <= 0 {
		return nil
	}
	host, repo := "github.com", target.repo
	if normalized, ok := export.NormalizeGitRemote(repo); ok && strings.Contains(repo, "://") {
		repo = normalized
	}
	if call.HostUncertain && (repo == "" || len(strings.Split(repo, "/")) != 3) {
		return nil
	}
	if repo == "" && target.repoSpecified {
		return nil
	}
	if repo == "" {
		if normalized, ok := export.NormalizeGitRemote(remote); ok {
			host, repo, _ = strings.Cut(normalized, "/")
		}
	} else if parts := strings.Split(repo, "/"); len(parts) == 3 {
		host, repo = parts[0], strings.Join(parts[1:], "/")
	}
	if !repoPartRE.MatchString(host) || !repoPathRE.MatchString(repo) {
		return nil
	}
	if link, ok := NewPRLink(fmt.Sprintf("https://%s/%s/pull/%d", host, repo, number), repo, number); ok {
		return []PRLink{link}
	}
	return nil
}

// launcherPrompt returns the literal arguments and piped input of a claude or
// codex command, skipping option values.
func (c shellCall) launcherPrompt() string {
	if len(c.Words) == 0 || !c.Literal[0] {
		return ""
	}
	program := programName(c.Words[0])
	if program != "claude" && program != "codex" {
		return ""
	}
	var prompt []string
	for i := 1; i < len(c.Words); i++ {
		arg := c.Words[i]
		switch {
		case !c.Literal[i]:
		case strings.HasPrefix(arg, "-"):
			if !strings.Contains(arg, "=") && launcherValueFlag(program, arg) {
				i++
			}
		default:
			prompt = append(prompt, arg)
		}
	}
	return strings.Join(append(prompt, c.Stdin), "\n")
}

func launcherValueFlag(program, flag string) bool {
	switch flag {
	case "-m", "--model", "-c", "--config", "--settings", "--output-format", "--permission-mode", "--resume", "-r", "--output-schema", "--output-last-message", "-o", "-C", "--cd", "--add-dir", "--system-prompt", "--append-system-prompt":
		return true
	}
	return program == "codex" && (flag == "-p" || flag == "--profile" || flag == "-s" || flag == "--sandbox")
}
