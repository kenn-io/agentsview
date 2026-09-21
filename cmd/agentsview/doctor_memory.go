package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/skills"
)

const memoryPluginPackageVersion = "0.1.0"

type doctorMemoryTargetSelection struct {
	Kind   string `json:"kind"`
	server string
}

type doctorMemoryComponentStatus struct {
	Status service.MemoryReadinessState `json:"status"`
	Reason string                       `json:"reason,omitempty"`
}

type doctorMemoryStandaloneStatus struct {
	Harness string `json:"harness"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
}

type doctorMemoryClientStatus struct {
	Status     service.MemoryReadinessState   `json:"status"`
	Reason     string                         `json:"reason,omitempty"`
	Skill      doctorMemoryComponentStatus    `json:"skill"`
	MCP        doctorMemoryComponentStatus    `json:"mcp"`
	Hook       doctorMemoryComponentStatus    `json:"hook"`
	Standalone []doctorMemoryStandaloneStatus `json:"standalone_skills,omitempty"`
}

type doctorMemoryReport struct {
	Status          service.MemoryReadinessState `json:"status"`
	ObservedAt      string                       `json:"observed_at"`
	TargetSelection doctorMemoryTargetSelection  `json:"target_selection"`
	Target          service.MemoryStatus         `json:"target"`
	Client          doctorMemoryClientStatus     `json:"client"`
}

func newDoctorMemoryCommand() *cobra.Command {
	var pluginRoot string
	cmd := &cobra.Command{
		Use:          "memory",
		Short:        "Diagnose conversation-memory readiness",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := applyMemoryTargetEnv(cmd, "memory"); err != nil {
				return err
			}
			selection := doctorMemorySelection(cmd)
			svc, cleanup, err := resolveDoctorMemoryService(cmd, selection)
			if err != nil {
				return fmt.Errorf("doctor memory: resolve target: %w", err)
			}
			defer cleanup()
			target, err := service.GetMemoryStatus(cmd.Context(), svc)
			if err != nil {
				return fmt.Errorf("doctor memory: read target status: %w", err)
			}
			if selection.Kind == "local" && target.ServerVersion == "" {
				target.ServerVersion = version
			}
			root := resolveDoctorMemoryPluginRoot(pluginRoot)
			client := inspectDoctorMemoryClient(root, selection)
			report := doctorMemoryReport{
				Status:          aggregateDoctorMemoryStatus(target.Status, client.Status),
				ObservedAt:      target.ObservedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
				TargetSelection: selection,
				Target:          target,
				Client:          client,
			}
			if outputFormat(cmd) == "json" {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), report)
			}
			return writeDoctorMemoryReport(cmd.OutOrStdout(), report)
		},
	}
	cmd.Flags().String("server", "", "Remote daemon URL")
	cmd.Flags().String("server-token-file", "",
		"File containing bearer token for explicit --server requests")
	cmd.Flags().Bool("pg", false,
		"Read memory readiness from configured PostgreSQL")
	cmd.Flags().StringVar(&pluginRoot, "plugin-root", "",
		"Native memory plugin root to inspect")
	registerFormatFlags(cmd.Flags())
	return cmd
}

func resolveDoctorMemoryService(
	cmd *cobra.Command, selection doctorMemoryTargetSelection,
) (service.SessionService, func(), error) {
	if selection.Kind == "server" {
		return resolveService(cmd)
	}
	// Diagnostics must not generate installation identity or cursor-secret
	// state. Both local and PostgreSQL reads begin with the read-only loader.
	cfg, err := config.LoadReadOnly()
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}
	if selection.Kind == "postgresql" {
		pgCfg, usePG, err := resolvePGReadConfig(cmd, cfg)
		if err != nil {
			return nil, nil, err
		}
		if !usePG {
			return nil, nil, errors.New("doctor memory: PostgreSQL target was not selected")
		}
		return newPGReadService(cfg, pgCfg)
	}
	// A direct read-only archive handle exposes the same bounded readiness
	// provider without starting a daemon or scheduling sync work.
	return newService(cmd.Context(), cfg, transport{Mode: transportDirect})
}

func doctorMemorySelection(cmd *cobra.Command) doctorMemoryTargetSelection {
	server, _ := cmd.Flags().GetString("server")
	server = strings.TrimRight(strings.TrimSpace(server), "/")
	if server != "" {
		return doctorMemoryTargetSelection{Kind: "server", server: server}
	}
	if pgReadRequested(cmd) {
		return doctorMemoryTargetSelection{Kind: "postgresql"}
	}
	return doctorMemoryTargetSelection{Kind: "local"}
}

func resolveDoctorMemoryPluginRoot(explicit string) string {
	if root := strings.TrimSpace(explicit); root != "" {
		return root
	}
	for _, name := range []string{"PLUGIN_ROOT", "CLAUDE_PLUGIN_ROOT"} {
		if root := strings.TrimSpace(os.Getenv(name)); root != "" {
			return root
		}
	}
	return ""
}

func inspectDoctorMemoryClient(
	pluginRoot string, selection doctorMemoryTargetSelection,
) doctorMemoryClientStatus {
	unknown := doctorMemoryComponentStatus{
		Status: service.MemoryUnknown, Reason: "plugin_root_unavailable",
	}
	client := doctorMemoryClientStatus{
		Status: service.MemoryUnknown, Reason: "plugin_root_unavailable",
		Skill: unknown, MCP: unknown, Hook: unknown,
	}
	if pluginRoot != "" {
		client.Skill = inspectDoctorMemoryPluginSkills(pluginRoot)
		client.MCP = inspectDoctorMemoryPluginMCP(pluginRoot)
		client.Hook = inspectDoctorMemoryPluginHook(pluginRoot)
		client.Status = aggregateDoctorMemoryComponents(
			client.Skill.Status, client.MCP.Status, client.Hook.Status,
		)
		client.Reason = componentAggregateReason(client)
	}

	client.Standalone = inspectDoctorMemoryStandaloneSkills(selection)
	if len(client.Standalone) == 0 {
		return client
	}
	for _, standalone := range client.Standalone {
		if standalone.Reason == "target_mismatch" {
			client.Status = service.MemoryPartial
			client.Reason = "standalone_target_mismatch"
			return client
		}
	}
	client.Status = service.MemoryPartial
	if pluginRoot != "" {
		client.Reason = "duplicate_standalone_install"
	} else {
		client.Reason = "standalone_install_has_no_native_mcp_or_hook"
	}
	return client
}

func inspectDoctorMemoryPluginSkills(root string) doctorMemoryComponentStatus {
	artifacts, err := skills.RenderPluginPackage(memoryPluginPackageVersion)
	if err != nil {
		return doctorMemoryComponentStatus{Status: service.MemoryUnknown, Reason: "render_failed"}
	}
	worst := service.MemoryReady
	reason := ""
	for _, artifact := range artifacts {
		body, readErr := os.ReadFile(filepath.Join(root, artifact.RelativePath))
		if os.IsNotExist(readErr) {
			return doctorMemoryComponentStatus{Status: service.MemoryUnavailable, Reason: "missing"}
		}
		if readErr != nil {
			return doctorMemoryComponentStatus{Status: service.MemoryUnknown, Reason: "unreadable"}
		}
		switch skills.Classify(body, artifact) {
		case skills.StateCurrent:
		case skills.StateStale:
			worst, reason = service.MemoryPartial, "stale"
		case skills.StateModified:
			worst, reason = service.MemoryPartial, "modified"
		case skills.StateForeign:
			worst, reason = service.MemoryPartial, "unrecognized"
		default:
			return doctorMemoryComponentStatus{Status: service.MemoryUnavailable, Reason: "missing"}
		}
	}
	return doctorMemoryComponentStatus{Status: worst, Reason: reason}
}

func inspectDoctorMemoryPluginMCP(root string) doctorMemoryComponentStatus {
	for _, check := range []struct {
		relative string
		required []string
	}{
		{relative: ".mcp.json", required: []string{
			`"mcpServers"`, `"agentsview"`, `"--profile"`, `"memory"`,
		}},
		{relative: ".claude-plugin/plugin.json", required: []string{
			`"mcpServers"`, `"agentsview"`, `"--profile"`, `"memory"`,
		}},
		{relative: ".codex-plugin/plugin.json", required: []string{
			`"mcpServers"`, `"./.mcp.json"`,
		}},
	} {
		body, err := readDoctorMemoryJSON(filepath.Join(root, check.relative))
		if os.IsNotExist(err) {
			return doctorMemoryComponentStatus{Status: service.MemoryUnavailable, Reason: "missing"}
		}
		if err != nil {
			return doctorMemoryComponentStatus{Status: service.MemoryPartial, Reason: "invalid"}
		}
		for _, required := range check.required {
			if !bytes.Contains(body, []byte(required)) {
				return doctorMemoryComponentStatus{Status: service.MemoryPartial, Reason: "wrong_profile"}
			}
		}
	}
	return doctorMemoryComponentStatus{Status: service.MemoryReady}
}

func inspectDoctorMemoryPluginHook(root string) doctorMemoryComponentStatus {
	hooks, err := readDoctorMemoryJSON(filepath.Join(root, "hooks", "hooks.json"))
	if os.IsNotExist(err) {
		return doctorMemoryComponentStatus{Status: service.MemoryUnavailable, Reason: "missing"}
	}
	if err != nil || !bytes.Contains(hooks, []byte("SessionStart")) {
		return doctorMemoryComponentStatus{Status: service.MemoryPartial, Reason: "invalid"}
	}
	script, err := os.ReadFile(filepath.Join(root, "scripts", "session-start.sh"))
	if os.IsNotExist(err) {
		return doctorMemoryComponentStatus{Status: service.MemoryUnavailable, Reason: "missing"}
	}
	if err != nil {
		return doctorMemoryComponentStatus{Status: service.MemoryUnknown, Reason: "unreadable"}
	}
	if !bytes.Contains(script, []byte("agentsview memory session-start")) {
		return doctorMemoryComponentStatus{Status: service.MemoryPartial, Reason: "invalid"}
	}
	return doctorMemoryComponentStatus{Status: service.MemoryReady}
}

func readDoctorMemoryJSON(path string) ([]byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	return body, nil
}

func inspectDoctorMemoryStandaloneSkills(
	selection doctorMemoryTargetSelection,
) []doctorMemoryStandaloneStatus {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var rows []doctorMemoryStandaloneStatus
	for _, harness := range skills.AllHarnesses() {
		path := filepath.Join(skills.TargetDir(harness, home), skillFileName)
		body, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		row := doctorMemoryStandaloneStatus{Harness: string(harness)}
		if err != nil {
			row.State = "unknown"
			row.Reason = "unreadable"
			rows = append(rows, row)
			continue
		}
		remote := skills.ParseRemote(string(body))
		pkg, renderErr := skills.RenderPackage(harness, version, remote)
		if renderErr != nil || len(pkg) == 0 {
			row.State = "unknown"
			row.Reason = "render_failed"
		} else {
			row.State = skillStateString(skills.Classify(body, pkg[0]))
			if !doctorMemoryTargetMatches(selection, remote) {
				row.Reason = "target_mismatch"
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func doctorMemoryTargetMatches(
	selection doctorMemoryTargetSelection, remote skills.Remote,
) bool {
	switch selection.Kind {
	case "local":
		return remote.Empty()
	case "server":
		return strings.TrimRight(strings.TrimSpace(remote.Server), "/") == selection.server
	default:
		return false
	}
}

func aggregateDoctorMemoryComponents(
	states ...service.MemoryReadinessState,
) service.MemoryReadinessState {
	allReady := true
	allUnavailable := true
	for _, state := range states {
		allReady = allReady && state == service.MemoryReady
		allUnavailable = allUnavailable && state == service.MemoryUnavailable
		if state == service.MemoryUnknown {
			return service.MemoryUnknown
		}
	}
	if allReady {
		return service.MemoryReady
	}
	if allUnavailable {
		return service.MemoryUnavailable
	}
	return service.MemoryPartial
}

func componentAggregateReason(client doctorMemoryClientStatus) string {
	if client.Status == service.MemoryReady {
		return ""
	}
	if client.Skill.Status == service.MemoryUnavailable &&
		client.MCP.Status == service.MemoryUnavailable &&
		client.Hook.Status == service.MemoryUnavailable {
		return "plugin_not_installed"
	}
	return "plugin_incomplete"
}

func aggregateDoctorMemoryStatus(
	target, client service.MemoryReadinessState,
) service.MemoryReadinessState {
	if target == service.MemoryReady && client == service.MemoryReady {
		return service.MemoryReady
	}
	if target == service.MemoryUnavailable && client == service.MemoryUnavailable {
		return service.MemoryUnavailable
	}
	if target == service.MemoryUnknown {
		return service.MemoryUnknown
	}
	return service.MemoryPartial
}

func writeDoctorMemoryReport(w io.Writer, report doctorMemoryReport) error {
	fmt.Fprintln(w, "Memory Diagnostics")
	fmt.Fprintf(w, "Status: %s\n", report.Status)
	fmt.Fprintf(w, "Observed: %s\n", report.ObservedAt)
	fmt.Fprintf(w, "Target: %s\n", report.TargetSelection.Kind)
	if report.Target.ServerVersion != "" {
		fmt.Fprintf(w, "Server version: %s\n",
			sanitizeTerminal(report.Target.ServerVersion))
	}
	archiveDetail := report.Target.Archive.Backend
	if report.Target.Archive.ReadOnly {
		archiveDetail += ", read-only"
	}
	fmt.Fprintf(w, "Archive: %s (%s)\n", report.Target.Status,
		sanitizeTerminal(archiveDetail))
	if report.Target.Archive.Identity != "" {
		fmt.Fprintf(w, "Archive identity: %s\n",
			sanitizeTerminal(report.Target.Archive.Identity))
	}
	writeDoctorMemoryCapability(w, "Lexical search", report.Target.Lexical.Status,
		report.Target.Lexical.Reason)
	writeDoctorMemoryCapability(w, "Semantic search", report.Target.Semantic.Status,
		report.Target.Semantic.Reason)
	if report.Target.Semantic.Generation != "" {
		fmt.Fprintf(w, "Semantic generation: %s (%d embedded, %d missing)\n",
			sanitizeTerminal(report.Target.Semantic.Generation),
			report.Target.Semantic.Embedded, report.Target.Semantic.Missing)
	}
	writeDoctorMemoryCapability(w, "Source freshness", report.Target.Sources.Status,
		report.Target.Sources.Reason)
	writeDoctorMemoryCapability(w, "Client integration", report.Client.Status,
		report.Client.Reason)
	writeDoctorMemoryCapability(w, "  Skill", report.Client.Skill.Status,
		report.Client.Skill.Reason)
	writeDoctorMemoryCapability(w, "  MCP", report.Client.MCP.Status,
		report.Client.MCP.Reason)
	writeDoctorMemoryCapability(w, "  SessionStart hook", report.Client.Hook.Status,
		report.Client.Hook.Reason)
	if report.Client.Reason == "plugin_root_unavailable" {
		fmt.Fprintln(w, "Native plugin root was not provided; pass --plugin-root to inspect it.")
	}
	for _, standalone := range report.Client.Standalone {
		detail := standalone.State
		if standalone.Reason != "" {
			detail += ", " + standalone.Reason
		}
		fmt.Fprintf(w, "Standalone %s skill: %s\n",
			sanitizeTerminal(standalone.Harness), sanitizeTerminal(detail))
	}
	return nil
}

func writeDoctorMemoryCapability(
	w io.Writer, label string, status service.MemoryReadinessState, reason string,
) {
	if reason == "" {
		fmt.Fprintf(w, "%s: %s\n", label, status)
		return
	}
	fmt.Fprintf(w, "%s: %s (%s)\n", label, status, sanitizeTerminal(reason))
}
