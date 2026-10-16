package main

import (
	"strings"

	"go.kenn.io/agentsview/internal/config"
)

// deploymentArgs selects the image's command under AGENTSVIEW_MODE. It
// prefixes serve or pg serve only when argv is empty or starts with a flag,
// so explicit subcommands and root help/version requests stay unchanged.
func deploymentArgs(args []string) ([]string, error) {
	mode, set, err := config.DeploymentMode()
	if err != nil || !set {
		return args, err
	}
	if len(args) > 0 {
		if !strings.HasPrefix(args[0], "-") {
			return args, nil
		}
		name, _, _ := strings.Cut(args[0], "=")
		switch name {
		case "--help", "-h", "--version", "-v":
			return args, nil
		}
	}
	if mode == config.DeploymentModePGServe {
		return append([]string{"pg", "serve"}, args...), nil
	}
	return append([]string{"serve"}, args...), nil
}
