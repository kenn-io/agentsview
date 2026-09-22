// Command render-memory-plugin writes the generated skill and Claude search
// agent bundled by the native AgentsView memory plugin.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/agentsview/internal/skills"
)

func main() {
	var out string
	var version string
	flag.StringVar(&out, "out", "plugins/agentsview-memory", "plugin root")
	flag.StringVar(&version, "version", "0.1.0", "plugin version")
	flag.Parse()

	artifacts, err := skills.RenderPluginPackage(version)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, artifact := range artifacts {
		path := filepath.Join(out, artifact.RelativePath)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(path, []byte(artifact.Content), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
