package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func executeWithLegacyFlagCompat(args []string, stdout, stderr io.Writer) error {
	cmd := newRootCommand()
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)

	normalized, rewrites := normalizeLegacyLongFlags(args, collectLongFlags(cmd))
	if len(rewrites) > 0 {
		fmt.Fprint(stderr, legacyLongFlagWarning(rewrites))
	}
	cmd.SetArgs(normalized)
	return cmd.Execute()
}

func collectLongFlags(root *cobra.Command) map[string]struct{} {
	flags := make(map[string]struct{})
	var visit func(cmd *cobra.Command)
	visit = func(cmd *cobra.Command) {
		cmd.Flags().VisitAll(func(flag *pflag.Flag) {
			if len(flag.Name) > 1 {
				flags[flag.Name] = struct{}{}
			}
		})
		cmd.PersistentFlags().VisitAll(func(flag *pflag.Flag) {
			if len(flag.Name) > 1 {
				flags[flag.Name] = struct{}{}
			}
		})
		for _, child := range cmd.Commands() {
			visit(child)
		}
	}
	visit(root)
	return flags
}

func normalizeLegacyLongFlags(args []string, flags map[string]struct{}) ([]string, []string) {
	normalized := make([]string, 0, len(args))
	rewrites := make([]string, 0)
	seen := make(map[string]struct{})
	stop := false

	for _, arg := range args {
		if stop || arg == "" || arg == "-" || !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
			normalized = append(normalized, arg)
			if arg == "--" {
				stop = true
			}
			continue
		}

		name := strings.TrimPrefix(arg, "-")
		suffix := ""
		if idx := strings.IndexByte(name, '='); idx >= 0 {
			suffix = name[idx:]
			name = name[:idx]
		}
		if name == "" || len(name) == 1 || isNegativeNumber(name) {
			normalized = append(normalized, arg)
			continue
		}
		if _, ok := flags[name]; !ok {
			normalized = append(normalized, arg)
			continue
		}

		normalized = append(normalized, "--"+name+suffix)
		rewrite := fmt.Sprintf("-%s -> --%s", name, name)
		if _, ok := seen[rewrite]; ok {
			continue
		}
		seen[rewrite] = struct{}{}
		rewrites = append(rewrites, rewrite)
	}

	return normalized, rewrites
}

func legacyLongFlagWarning(rewrites []string) string {
	return "warning: deprecated single-dash long flags detected; use GNU-style long flags instead: " + strings.Join(rewrites, ", ") + "\n"
}

func isNegativeNumber(arg string) bool {
	for i, r := range arg {
		if i == 0 {
			if r < '0' || r > '9' {
				return false
			}
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return arg != ""
}

func execute() error {
	return executeWithLegacyFlagCompat(os.Args[1:], os.Stdout, os.Stderr)
}
