package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/parser"
)

func newAgentsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agents [agent...]",
		Short: "List supported agents and what AgentsView reads from them",
		Long: "Lists every agent AgentsView can read, where it looks for that\n" +
			"agent's sessions, and which session details its parser extracts.\n" +
			"Pass agent types to show only those agents. It does not need a\n" +
			"running daemon, configuration, or database.",
		GroupID:      groupMeta,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			agents, err := listAgents(args)
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.MarshalWrite(cmd.OutOrStdout(), agentsJSON{
					SchemaVersion: 1,
					Agents:        agents,
				}, json.Deterministic(true), jsontext.WithIndent("  "))
			}
			printAgents(cmd.OutOrStdout(), agents)
			return nil
		},
	}
	registerFormatFlags(cmd.Flags())
	return cmd
}

type agentsJSON struct {
	SchemaVersion int         `json:"schema_version"`
	Agents        []agentInfo `json:"agents"`
}

type agentInfo struct {
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	// ImportOnly agents are read only through `agentsview source import`.
	ImportOnly bool `json:"import_only"`
	FileBased  bool `json:"file_based"`
	// DefaultDirs are relative to the home directory. DataDirRoot agents
	// have none; their sessions live under the AgentsView data directory.
	DefaultDirs []string `json:"default_dirs"`
	DataDirRoot string   `json:"data_dir_root,omitempty"`
	EnvVar      string   `json:"env_var,omitempty"`
	// Content maps each session detail to supported, unsupported, or
	// not_applicable for this agent's parser.
	Content map[string]parser.CapabilitySupport `json:"content"`
}

func listAgents(filter []string) ([]agentInfo, error) {
	modes := parser.ProviderMigrationModes()
	byType := make(map[parser.AgentType]agentInfo, len(parser.Registry))
	var agents []agentInfo
	for _, factory := range parser.ProviderFactories() {
		def := factory.Definition()
		envVar := def.EnvVar
		if envVar == "" {
			envVar = def.NativeEnvVar
		}
		info := agentInfo{
			Type:        string(def.Type),
			DisplayName: def.DisplayName,
			ImportOnly:  modes[def.Type] == parser.ProviderMigrationImportOnly,
			FileBased:   def.FileBased,
			DefaultDirs: append([]string{}, def.DefaultDirs...),
			DataDirRoot: def.DataDirRoot,
			EnvVar:      envVar,
			Content:     contentCapabilities(factory.Capabilities().Content),
		}
		byType[def.Type] = info
		agents = append(agents, info)
	}
	if len(filter) == 0 {
		return agents, nil
	}
	selected := make([]agentInfo, 0, len(filter))
	for _, name := range filter {
		info, ok := byType[parser.AgentType(name)]
		if !ok {
			return nil, fmt.Errorf("unknown agent %q", name)
		}
		selected = append(selected, info)
	}
	return selected, nil
}

// contentCapabilities keys every ContentCapabilities field by its snake_case
// name so new capabilities appear without changing this command.
func contentCapabilities(caps parser.ContentCapabilities) map[string]parser.CapabilitySupport {
	v := reflect.ValueOf(caps)
	out := make(map[string]parser.CapabilitySupport, v.NumField())
	for i := range v.NumField() {
		out[snakeCase(v.Type().Field(i).Name)] = v.Field(i).Interface().(parser.CapabilitySupport)
	}
	return out
}

func snakeCase(name string) string {
	var b strings.Builder
	runes := []rune(name)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) &&
			(unicode.IsLower(runes[i-1]) ||
				(i+1 < len(runes) && unicode.IsLower(runes[i+1]))) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func printAgents(w io.Writer, agents []agentInfo) {
	names := contentNames()
	for i, a := range agents {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s (%s)\n", a.Type, a.DisplayName)
		switch {
		case a.ImportOnly:
			fmt.Fprintln(w, "  sessions: imported with `agentsview source import`")
		case a.DataDirRoot != "":
			fmt.Fprintf(w, "  sessions: <data dir>/%s\n", a.DataDirRoot)
		case len(a.DefaultDirs) > 0:
			dirs := make([]string, len(a.DefaultDirs))
			for j, d := range a.DefaultDirs {
				dirs[j] = "~/" + d
			}
			fmt.Fprintf(w, "  sessions: %s\n", strings.Join(dirs, ", "))
		}
		if a.EnvVar != "" {
			fmt.Fprintf(w, "  override: %s\n", a.EnvVar)
		}
		var reads []string
		for _, name := range names {
			if a.Content[name] == parser.CapabilitySupported {
				reads = append(reads, name)
			}
		}
		if len(reads) == 0 {
			reads = []string{"(none declared)"}
		}
		fmt.Fprintf(w, "  reads: %s\n", strings.Join(reads, ", "))
	}
}

// contentNames returns content capability names in declaration order.
func contentNames() []string {
	t := reflect.TypeFor[parser.ContentCapabilities]()
	names := make([]string, t.NumField())
	for i := range t.NumField() {
		names[i] = snakeCase(t.Field(i).Name)
	}
	return names
}
