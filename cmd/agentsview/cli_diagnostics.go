package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/servicehttp"
)

// cliDiagnosticError adds presentation without changing Error or the cause.
// Explicit cliExitError codes and silent report outcomes take precedence.
type cliDiagnosticError struct {
	err     error
	kind    string
	code    int
	hint    string
	display string
	json    bool
}

func (e *cliDiagnosticError) Error() string { return e.err.Error() }
func (e *cliDiagnosticError) Unwrap() error { return e.err }

func withCLIHint(err error, hint string) error {
	if err == nil {
		return nil
	}
	detail := cliDiagnosticError{kind: "error"}
	if previous, ok := errors.AsType[*cliDiagnosticError](err); ok {
		detail = *previous
	}
	detail.err, detail.hint = err, hint
	return &detail
}

func cliUsageError(err error, hint string) error {
	if err == nil {
		return nil
	}
	return &cliDiagnosticError{err: err, kind: "usage", code: cliUsageExitCode, hint: hint}
}

func cliValidationError(err error, hint string) error {
	if err == nil {
		return nil
	}
	return &cliDiagnosticError{err: err, kind: "validation", code: cliValidationExitCode, hint: hint}
}

func cliUnavailableError(err error, hint string) error {
	if err == nil {
		return nil
	}
	return &cliDiagnosticError{err: err, kind: "unavailable", code: cliUnavailableExitCode, hint: hint}
}

func commandHelpHint(cmd *cobra.Command) string {
	path := "agentsview"
	if cmd != nil {
		path = cmd.CommandPath()
	}
	return "run `" + path + " --help` for flags and examples"
}

func isProtocolCommand(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Hidden {
			return true
		}
	}
	return false
}

// installCLIDiagnostics wraps only visible command syntax validators. Runtime
// failures, including pre-run failures, are handled at ExecuteContextC's boundary.
func installCLIDiagnostics(root *cobra.Command) {
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if isProtocolCommand(cmd) {
			return err
		}
		if valueErr, ok := errors.AsType[*pflag.InvalidValueError](err); ok {
			validationErr := cliValidationError(err, commandHelpHint(cmd))
			d, ok := errors.AsType[*cliDiagnosticError](validationErr)
			if !ok {
				return validationErr
			}
			d.display = fmt.Sprintf("invalid --%s %q: expected %s", valueErr.GetFlag().Name, valueErr.GetValue(), valueErr.GetFlag().Value.Type())
			return d
		}
		return cliUsageError(err, commandHelpHint(cmd))
	})
	var visit func(*cobra.Command)
	visit = func(cmd *cobra.Command) {
		if cmd.Hidden {
			return
		}
		if cmd.SuggestionsMinimumDistance <= 0 {
			cmd.SuggestionsMinimumDistance = 2
		}
		if validate := cmd.Args; validate != nil {
			cmd.Args = func(c *cobra.Command, args []string) error {
				err := validate(c, args)
				if err == nil {
					return nil
				}
				if _, alreadyClassified := errors.AsType[*cliDiagnosticError](err); alreadyClassified {
					return err
				}
				hint := commandHelpHint(c)
				if c.HasSubCommands() && len(args) > 0 {
					if suggestions := c.SuggestionsFor(args[0]); len(suggestions) > 0 {
						hint = "did you mean " + strings.Join(suggestions, ", ") + "? " + hint
					}
				}
				return cliUsageError(err, hint)
			}
		}
		for _, child := range cmd.Commands() {
			visit(child)
		}
	}
	visit(root)
}

func contextualizeCLIError(cmd *cobra.Command, err error, jsonOutput bool) error {
	if err == nil || isSilentExitError(err) {
		return err
	}
	if isProtocolCommand(cmd) {
		return &cliDiagnosticError{err: err}
	}
	d := cliDiagnosticError{err: err, kind: "error"}
	if detail, ok := errors.AsType[*cliDiagnosticError](err); ok {
		d.kind, d.code, d.hint, d.display = detail.kind, detail.code, detail.hint, detail.display
	} else if cmd != nil {
		// Cobra performs these checks outside Args and RunE. Reusing its validators
		// identifies syntax failures without guessing from message text.
		if cmd.ValidateRequiredFlags() != nil || cmd.ValidateFlagGroups() != nil {
			d.kind, d.code = "usage", cliUsageExitCode
		}
	}
	applyCLIRecovery(cmd, err, &d)
	if d.hint == "" {
		d.hint = commandRecoveryHint(cmd)
	}
	d.json = jsonOutput
	return &d
}

// requestedErrorJSON reads output selectors even after a parse error. Known
// non-boolean flags consume their next token, so --server --json is a value,
// not an output selector. The terminator and final repeated values matter.
func requestedErrorJSON(cmd *cobra.Command, args []string) bool {
	if cmd == nil || isProtocolCommand(cmd) {
		return false
	}
	jsonSelected := false
	format := "human"
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		var flag *pflag.Flag
		if strings.HasPrefix(arg, "--") {
			flag = cmd.Flag(name)
		} else {
			short := strings.TrimPrefix(arg, "-")
			if len(short) > 0 {
				flag = cmd.Flags().ShorthandLookup(short[:1])
				if flag == nil {
					flag = cmd.InheritedFlags().ShorthandLookup(short[:1])
				}
				if len(short) > 1 {
					value, hasValue = short[1:], true
				}
			}
		}
		if flag == nil {
			continue
		}
		if !hasValue {
			if flag.NoOptDefVal != "" {
				value = flag.NoOptDefVal
			} else if i+1 < len(args) {
				i++
				value = args[i]
			}
		}
		switch flag.Name {
		case "json":
			if selected, err := strconv.ParseBool(value); err == nil {
				jsonSelected = selected
			}
		case "format":
			// A rejected enum value leaves the last successfully selected format
			// intact, matching formatFlag.Set when parsing fails.
			typeName := flag.Value.Type()
			if !strings.Contains(typeName, "|") || slices.Contains(strings.Split(typeName, "|"), value) {
				format = value
			}
		}
	}
	return jsonSelected || format == "json"
}

func writeCLIDiagnostic(stderr io.Writer, err error) {
	d, ok := errors.AsType[*cliDiagnosticError](err)
	if !ok {
		d = &cliDiagnosticError{err: err, kind: "error", hint: commandHelpHint(nil)}
	}
	message := err.Error()
	if d.display != "" {
		message = d.display
	}
	if d.json {
		payload := struct {
			Error   string `json:"error"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		}{d.kind, strings.ToValidUTF8(message, "\uFFFD"), strings.ToValidUTF8(d.hint, "\uFFFD")}
		data, marshalErr := json.Marshal(payload)
		if marshalErr == nil {
			fmt.Fprintln(stderr, string(data))
			return
		}
	}
	fmt.Fprintf(stderr, "fatal: %s\n", message)
	if d.hint != "" {
		fmt.Fprintf(stderr, "hint: %s\n", d.hint)
	}
}

const (
	sessionLookupHint  = "run `agentsview session list --json` for canonical IDs; run `agentsview sync` on the archive host if the session is new"
	configRecoveryHint = "fix TOML syntax or invalid settings in config.toml under AGENTSVIEW_DATA_DIR (default ~/.agentsview); use a separate AGENTSVIEW_DATA_DIR to select another configuration"
)

func commandRecoveryHint(cmd *cobra.Command) string {
	hint := commandHelpHint(cmd)
	if cmd == nil {
		return hint
	}
	top := cmd
	for top.Parent() != nil && top.Parent().Parent() != nil {
		top = top.Parent()
	}
	switch top.Name() {
	case "pg":
		return "check AGENTSVIEW_PG_URL or [pg].url in config.toml (named targets use default_pg); " + hint
	case "duckdb":
		return "check AGENTSVIEW_DUCKDB_PATH / AGENTSVIEW_DUCKDB_URL or [duckdb].path / [duckdb].url in config.toml; " + hint
	case "clickhouse":
		return "check AGENTSVIEW_CLICKHOUSE_URL or [clickhouse].url in config.toml (named targets use default_clickhouse); " + hint
	case "raw-sync":
		return "set --server or AGENTSVIEW_RAW_SYNC_URL, --device-id or AGENTSVIEW_RAW_SYNC_DEVICE_ID, and AGENTSVIEW_RAW_SYNC_CREDENTIAL; " + hint
	case "sync":
		return "check the named host in [[remote_hosts]] in config.toml, or omit --host to sync all configured sources; " + hint
	}
	return hint
}

func applyCLIRecovery(cmd *cobra.Command, err error, d *cliDiagnosticError) {
	if _, ok := errors.AsType[toml.ParseError](err); ok {
		d.hint = configRecoveryHint
		return
	}
	if errors.Is(err, errSessionNotFound) {
		d.kind, d.hint = "not_found", sessionLookupHint
		return
	}
	// Some HTTP operations translate 501 into this sentinel and include the
	// configured URL in their original error. Keep the cause, hide its URL.
	if errors.Is(err, db.ErrReadOnly) {
		d.display = "selected data source is read-only"
		d.hint = "use a writable daemon or archive; " + commandHelpHint(cmd)
		if cmd != nil && cmd.Flag("server") != nil {
			d.hint = "use --server URL to select a writable daemon; " + commandHelpHint(cmd)
		}
		return
	}
	if errors.Is(err, errLocalDaemonUnreachable) {
		d.kind, d.code = "unavailable", cliUnavailableExitCode
		d.hint = "run `agentsview daemon status`; if the local daemon is stuck, run `agentsview daemon restart`"
		return
	}
	if errors.Is(err, db.ErrSemanticUnavailable) {
		d.kind, d.code = "unavailable", cliUnavailableExitCode
		d.hint = "run `agentsview embeddings build` on the archive host; " + commandHelpHint(cmd)
		if cmd != nil && cmd.CommandPath() == "agentsview session search" {
			d.hint += "; use --fts or --regex while semantic search is unavailable"
		}
		return
	}
	if status, message, ok := servicehttp.HTTPErrorDetails(err); ok {
		switch status {
		case http.StatusUnauthorized:
			d.kind, d.display = "authentication", "server rejected authentication"
			d.hint = "check --server-token-file or AGENTSVIEW_SERVER_TOKEN for an explicit --server; for a local daemon, check its saved authentication configuration"
		case http.StatusForbidden:
			if message == "" {
				d.kind, d.display = "authentication", "server rejected authentication"
				d.hint = "check --server-token-file or AGENTSVIEW_SERVER_TOKEN for an explicit --server; for a local daemon, check its saved authentication configuration"
			} else {
				d.kind, d.display = "forbidden", message
				d.hint = commandHelpHint(cmd)
			}
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			d.kind, d.code = "validation", cliValidationExitCode
			d.display = message
			if d.display == "" {
				d.display = "server rejected the request"
			}
			d.hint = commandHelpHint(cmd)
		case http.StatusConflict:
			d.kind = "conflict"
			if message == "" {
				d.display = "server reported a conflict"
				d.hint = "inspect the server response and logs; " + commandHelpHint(cmd)
			} else {
				d.display = message
				d.hint = commandHelpHint(cmd)
			}
		default:
			d.display = fmt.Sprintf("server error (HTTP %d)", status)
			d.hint = "retry the command and inspect the server logs on the host serving this archive; " + commandHelpHint(cmd)
		}
		return
	}
	if _, ok := errors.AsType[*url.Error](err); ok {
		d.kind, d.code, d.display = "unavailable", cliUnavailableExitCode, "network request failed"
		top := cmd
		for top != nil && top.Parent() != nil && top.Parent().Parent() != nil {
			top = top.Parent()
		}
		if top != nil && top.Name() == "raw-sync" {
			d.display = "cannot reach raw-sync server"
			d.hint = "check --server or AGENTSVIEW_RAW_SYNC_URL and the raw-sync server's status, then retry; " + commandHelpHint(cmd)
			return
		}
		if top != nil && top.Name() == "update" {
			d.display = "cannot reach update service"
			d.hint = "check network access to GitHub and retry; " + commandHelpHint(cmd)
			return
		}
		remote := ""
		if cmd != nil && cmd.Flag("server") != nil {
			remote = cmd.Flag("server").Value.String()
		}
		if remote != "" {
			d.display = "cannot reach server"
			d.hint = "check --server URL and the server's status on that host, then retry; " + commandHelpHint(cmd)
		} else if top != nil && slices.Contains([]string{"session", "stats", "usage", "activity", "health"}, top.Name()) {
			d.display = "cannot reach server"
			d.hint = "run `agentsview daemon status` to check the local server; " + commandHelpHint(cmd)
		}
	}
}

// Legacy fatal handlers cannot return through ExecuteContextC. Reconstruct only
// command metadata here; never execute hooks or load configuration. This is a
// process-only fallback, with no mutable global invocation state.
func processCLIError(err error) error {
	root := newRootCommand()
	args, _ := normalizeLegacyLongFlags(os.Args[1:], collectLongFlags(root))
	if deployed, deployErr := deploymentArgs(args); deployErr == nil {
		args = deployed
	}
	cmd, _, _ := root.Find(args)
	if cmd == nil {
		cmd = root
	}
	return contextualizeCLIError(cmd, err, requestedErrorJSON(cmd, args))
}
