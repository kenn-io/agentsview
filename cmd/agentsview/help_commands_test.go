package main

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/shlex"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentAdvertisedCommandsParse(t *testing.T) {
	root := newRootCommand()
	help, err := executeCommand(root, "--help")
	require.NoError(t, err)
	sources := map[string]string{"root help": help}
	var sessionGetHelp string
	unsupportedCommonFlags := map[string][]string{
		"agentsview session watch":  {"--format", "--json"},
		"agentsview session export": {"--format", "--json", "--pg", "--server", "--server-token-file"},
		"agentsview session prune":  {"--format", "--json", "--pg", "--server", "--server-token-file"},
	}
	walkHelpCommands(root, func(cmd *cobra.Command) {
		sources[cmd.CommandPath()] = cmd.Long
		if cmd.Parent() == nil {
			return
		}
		path := strings.TrimPrefix(cmd.CommandPath(), "agentsview ")
		if slices.Contains(agentHelpCoreCommands, path) {
			require.NotEmpty(t, cmd.Example, "%s needs a usable invocation", cmd.CommandPath())
			if !cmd.HasSubCommands() {
				require.NotEmpty(t, cmd.Long, "%s needs an explanation and its relevant limits", cmd.CommandPath())
			}
		}
		args := append(strings.Fields(path), "--help")
		rendered, err := executeCommand(newRootCommand(), args...)
		require.NoError(t, err, "%s help", cmd.CommandPath())
		if cmd.CommandPath() == "agentsview session get" {
			sessionGetHelp = rendered
		}
		assert.Contains(t, rendered, cmd.CommandPath())
		if help, ok := agentCommandHelp[path]; ok {
			renderedText := strings.Join(strings.Fields(rendered), " ")
			for _, configured := range []string{help.long, help.extra} {
				if configured != "" {
					assert.Contains(t, renderedText, strings.Join(strings.Fields(configured), " "), "%s help must render its configured description", cmd.CommandPath())
				}
			}
		}
		if rejected, ok := unsupportedCommonFlags[cmd.CommandPath()]; ok {
			commonFlags := commonCLIFlagsSection(rendered)
			for _, name := range rejected {
				assert.NotContains(t, commonFlags, name, "%s common flags should omit %s", cmd.CommandPath(), name)
			}
			if cmd.CommandPath() != "agentsview session watch" {
				assert.Empty(t, commonFlags, "%s has no supported inherited flags", cmd.CommandPath())
			}
		}
		if cmd.CommandPath() == "agentsview session watch" {
			assert.Contains(t, commonCLIFlagsSection(rendered), "--server")
		}
		if slices.Contains(agentHelpCoreCommands, path) {
			assert.NotContains(t, rendered, "Deprecated:", "%s is a canonical command", cmd.CommandPath())
		}
		if cmd.HasSubCommands() {
			require.NotEmpty(t, cmd.Long, "%s needs a concept explanation", cmd.CommandPath())
		}
		if cmd.Example != "" {
			assert.Contains(t, rendered, "Examples:")
			assert.Contains(t, rendered, strings.TrimSpace(cmd.Example), "%s help must render its declared example", cmd.CommandPath())
			require.NoError(t, validateCommandExamples(cmd.CommandPath(), cmd.Example))
		}
	})
	require.NotEmpty(t, sessionGetHelp, "session get help must be rendered")
	assert.Contains(t, sessionGetHelp, "Common flags:")
	assert.Contains(t, sessionGetHelp, "--server")
	assert.Contains(t, sessionGetHelp, "agentsview session --help")
	assert.NotContains(t, sessionGetHelp, "\nGlobal Flags:\n")
	for path := range agentCommandHelp {
		commandPath := "agentsview " + path
		assert.Contains(t, sources, commandPath)
		cmd, rest, err := root.Find(strings.Fields(path))
		require.NoError(t, err)
		require.Empty(t, rest)
		require.NotEmpty(t, cmd.Example, "%s needs a usable invocation", commandPath)
	}
	for name, source := range sources {
		commands, err := advertisedHelpCommands(source)
		require.NoError(t, err, name)
		if name == "root help" {
			require.NotEmpty(t, commands, name)
		}
		for _, line := range commands {
			require.NoError(t, validateAdvertisedCommand(line), "%s: %s", name, line)
		}
	}
}

func commonCLIFlagsSection(rendered string) string {
	_, section, found := strings.Cut(rendered, "\nCommon flags:\n")
	if !found {
		return ""
	}
	section, _, _ = strings.Cut(section, "\n\n")
	return section
}

func TestAgentCommandCheckerAcceptsAndRejectsExamples(t *testing.T) {
	inline, err := advertisedHelpCommands(`Use 'agentsview session usage ID --json' or "agentsview session usage ID --json".`)
	require.NoError(t, err)
	assert.Equal(t, []string{"agentsview session usage ID --json", "agentsview session usage ID --json"}, inline)
	quotedOperand := "agentsview session search 'agentsview imaginary' --json"
	quotedCommands, err := advertisedHelpCommands(quotedOperand)
	require.NoError(t, err)
	require.Len(t, quotedCommands, 1)
	assert.Equal(t, []string{quotedOperand}, quotedCommands)
	require.NoError(t, validateAdvertisedCommand(quotedCommands[0]))
	continued := "agentsview session search " + "\\\n" + "  'a query' " + "\\\n" + "  --json"
	require.NoError(t, validateCommandExamples("agentsview session search", continued))
	commands, err := advertisedHelpCommands(continued)
	require.NoError(t, err)
	require.Len(t, commands, 1)
	require.NoError(t, validateAdvertisedCommand(commands[0]))
	unfinished := "agentsview session search " + "\\"
	require.Error(t, validateCommandExamples("agentsview session search", unfinished))
	require.Error(t, validateCommandExamples("agentsview source list", "agentsview report stats --json"))
	for _, line := range []string{
		"agentsview openapi >",
		"agentsview openapi >>> out",
		"agentsview openapi > out > other",
		"agentsview openapi > out extra",
		"agentsview session search foo) --json",
		"agentsview session search $( --json",
		"agentsview session get <id --json",
		"agentsview session search query<file",
		"agentsview session search 'unterminated",
		"agentsview session search word; --json",
		"agentsview session search word| --json",
		"agentsview session search word& --json",
		"agentsview session made-up --help",
		"agentsview session list --nonexistent",
		"agentsview session get",
	} {
		require.Error(t, validateAdvertisedCommand(line), line)
	}
	for _, line := range []string{
		"agentsview openapi>>openapi.json # append",
		"agentsview session get <id> --json",
		"agentsview session search 'literal (parentheses) $(value) $' --json",
		"agentsview session search \"literal (parentheses) \\$(value) \\$\" --json",
		`agentsview session search escaped\(parentheses\)\$ --json`,
		"agentsview session search \">\" --json",
		`agentsview session search \> --json`,
		"agentsview session search '# > literal' --json",
		"agentsview session search query --json # > ignored",
		"agentsview session search 'pipe | ampersand & semicolon ;' --json",
		`agentsview session search escaped\|operator --json`,
	} {
		require.NoError(t, validateAdvertisedCommand(line), line)
	}
}

func walkHelpCommands(cmd *cobra.Command, visit func(*cobra.Command)) {
	if cmd.Hidden || cmd.Name() == "completion" {
		return
	}
	visit(cmd)
	for _, child := range cmd.Commands() {
		walkHelpCommands(child, visit)
	}
}

var helpInlineCommands = regexp.MustCompile("`([^`]+)`|'([^']+)'|\"([^\"]+)\"")

func advertisedHelpCommands(source string) ([]string, error) {
	var commands []string
	add := func(line string) {
		if line == "agentsview <command> [flags]" || line == "agentsview [flags]" ||
			strings.Contains(line, " - local web viewer for AI agent sessions") {
			return
		}
		commands = append(commands, line)
	}
	lines, err := logicalHelpLines(source)
	if err != nil {
		return nil, err
	}
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if startsHelpCommand(line) {
			add(line)
			continue
		}
		for _, match := range helpInlineCommands.FindAllStringSubmatch(line, -1) {
			for _, span := range match[1:] {
				if startsHelpCommand(strings.TrimSpace(span)) {
					add(strings.TrimSpace(span))
				}
			}
		}
	}
	return commands, nil
}

func validateAdvertisedCommand(line string) error {
	_, err := resolveAdvertisedCommand(line)
	return err
}

func resolveAdvertisedCommand(line string) (*cobra.Command, error) {
	words, err := splitHelpCommand(line)
	if err != nil {
		return nil, err
	}
	if len(words) == 0 || words[0] != "agentsview" {
		return nil, errors.New("expected an agentsview command")
	}
	return validateAdvertisedArgs(words)
}

func validateCommandExamples(path, source string) error {
	ownerPath := strings.TrimSpace(path)
	lines, err := logicalHelpLines(source)
	if err != nil {
		return err
	}
	count := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		count++
		cmd, err := resolveAdvertisedCommand(line)
		if err != nil {
			return fmt.Errorf("%s example %q: %w", path, line, err)
		}
		if cmd.CommandPath() != ownerPath && !strings.HasPrefix(cmd.CommandPath(), ownerPath+" ") {
			return fmt.Errorf("%s example %q resolves to %s outside its command path", path, line, cmd.CommandPath())
		}
	}
	if count == 0 {
		return fmt.Errorf("%s has no command examples", path)
	}
	return nil
}

func validateAdvertisedArgs(words []string) (*cobra.Command, error) {
	for i, word := range words {
		if strings.HasPrefix(word, "<") && strings.HasSuffix(word, ">") {
			words[i] = "example"
		}
	}
	root := newRootCommand()
	cmd, rest, err := root.Find(words[1:])
	if err != nil {
		return nil, err
	}
	cmd.InitDefaultHelpFlag()
	if err := cmd.ParseFlags(rest); err != nil {
		return nil, err
	}
	args := cmd.Flags().Args()
	if cmd.HasSubCommands() && len(args) > 0 {
		return nil, fmt.Errorf("unknown command path: %v", args)
	}
	help, _ := cmd.Flags().GetBool("help")
	if help {
		return cmd, nil
	}
	if err := cmd.ValidateArgs(args); err != nil {
		return nil, err
	}
	if err := cmd.ValidateRequiredFlags(); err != nil {
		return nil, err
	}
	if err := cmd.ValidateFlagGroups(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// Join shell continuations; single quotes and comments keep a backslash literal.
func logicalHelpLines(source string) ([]string, error) {
	var out strings.Builder
	var quote byte
	comment, word := false, false
	for i := 0; i < len(source); i++ {
		c := source[i]
		if c == '\n' {
			quote, comment, word = 0, false, false
		} else if !comment {
			if c == '\\' && quote != '\'' {
				if i+1 == len(source) {
					return nil, errors.New("unfinished help continuation")
				}
				i++
				if source[i] != '\n' {
					word = true
					out.WriteByte(c)
					out.WriteByte(source[i])
				}
				continue
			}
			if quote != 0 {
				if c == quote {
					quote = 0
				}
			} else if c == '\'' || c == '"' {
				quote, word = c, true
			} else if c == '#' && !word {
				comment = true
			} else {
				word = !strings.ContainsRune(" \t", rune(c))
			}
		}
		out.WriteByte(c)
	}
	return strings.Split(out.String(), "\n"), nil
}

// Find unquoted, unescaped redirects before shlex erases quoting; <id> stays a word.
func splitHelpCommand(line string) ([]string, error) {
	var quote byte
	redirect, target, word := -1, -1, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '\\' && quote != '\'' {
			word = true
			i++
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote, word = c, true
		case ';', '|', '&', '(', ')':
			return nil, fmt.Errorf("unsupported shell operator %q", c)
		case '#':
			if !word {
				line = line[:i]
				i = len(line)
			}
		case '<':
			end := strings.IndexByte(line[i:], '>')
			if end > 1 && !strings.ContainsAny(line[i:i+end], " \t") {
				i, word = i+end, true
			} else {
				return nil, errors.New("unsupported input redirection")
			}
		case '>':
			if redirect >= 0 {
				return nil, errors.New("repeated output redirection")
			}
			redirect = i
			if i+1 < len(line) && line[i+1] == '>' {
				i++
			}
			target = i + 1
			word = false
		default:
			word = !strings.ContainsRune(" \t", rune(c))
		}
	}
	if redirect >= 0 {
		filenames, err := shlex.Split(line[target:])
		if err != nil {
			return nil, err
		}
		if len(filenames) != 1 || filenames[0] == "" {
			return nil, errors.New("output redirection needs one filename")
		}
		line = line[:redirect]
	}
	return shlex.Split(line)
}

func startsHelpCommand(line string) bool {
	return strings.HasPrefix(line, "agentsview")
}
