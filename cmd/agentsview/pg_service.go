package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
)

func newPGServiceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "service",
		Short:        "Install and manage the pg push --watch background service",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newPGServiceInstallCommand())
	cmd.AddCommand(newPGServiceUninstallCommand())
	cmd.AddCommand(newPGServiceStatusCommand())
	cmd.AddCommand(newPGServiceStartCommand())
	cmd.AddCommand(newPGServiceStopCommand())
	cmd.AddCommand(newPGServiceLogsCommand())
	return cmd
}

func newPGServiceInstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "install",
		Short:        "Install and start the auto-push service",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runServiceInstall()
		},
	}
}

func newPGServiceUninstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "uninstall",
		Short:        "Stop and remove the auto-push service",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runServiceSimple("uninstall")
		},
	}
}

func newPGServiceStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "status",
		Short:        "Show the auto-push service status",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runServiceStatus()
		},
	}
}

func newPGServiceStartCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "start",
		Short:        "Start the auto-push service",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runServiceSimple("start")
		},
	}
}

func newPGServiceStopCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "stop",
		Short:        "Stop the auto-push service",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runServiceSimple("stop")
		},
	}
}

func newPGServiceLogsCommand() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:          "logs",
		Short:        "Show the auto-push service log",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runServiceLogs(follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow the log output")
	return cmd
}

// loadServiceConfig loads minimal config and ensures the data dir.
func loadServiceConfig() config.Config {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		fatal("pg service: loading config: %v", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		fatal("pg service: creating data dir: %v", err)
	}
	return appCfg
}

func runServiceInstall() {
	appCfg := loadServiceConfig()
	spec, err := buildServiceSpec(appCfg)
	if err != nil {
		fatal("pg service install: %v", err)
	}
	warnUninheritedServiceEnv(
		os.Stdout, setEnvVarsAffectingService(os.LookupEnv),
	)
	mgr, err := newServiceManager()
	if err != nil {
		fatal("pg service install: %v", err)
	}
	ctx := context.Background()

	if err := mgr.install(ctx, spec); err != nil {
		fatal("pg service install: %v", err)
	}
	fmt.Printf("Installed service unit at %s\n", mgr.unitPath())

	// Surface the linger requirement for systemd headless boxes.
	if lc, ok := mgr.(lingerChecker); ok && !lc.lingerEnabled(ctx) {
		fmt.Println()
		fmt.Println(
			"WARNING: user lingering is not enabled. Without it, the service " +
				"stops when you log out and will not start at boot.",
		)
		fmt.Printf("Run this to enable it:\n  %s\n", lc.enableLingerCmd())
		if promptYesNo(os.Stdin, "Enable lingering now?") {
			parts := strings.Fields(lc.enableLingerCmd())
			if out, lerr := defaultRunner(ctx, parts[0], parts[1:]...); lerr != nil {
				fmt.Printf(
					"Could not enable lingering (%v: %s).\n"+
						"You may need elevated privileges: sudo %s\n",
					lerr, strings.TrimSpace(out), lc.enableLingerCmd(),
				)
			} else {
				fmt.Println("Lingering enabled.")
			}
		}
	}

	fmt.Println()
	fmt.Println("Service installed and started.")
	fmt.Println("View logs with: agentsview pg service logs -f")
}

func runServiceStatus() {
	mgr, err := newServiceManager()
	if err != nil {
		fatal("pg service status: %v", err)
	}
	ctx := context.Background()
	out, _ := mgr.status(ctx)
	// Show the last successful push time from local sync state.
	appCfg := loadServiceConfig()
	database, derr := openReadOnlyDB(appCfg)
	if derr != nil {
		writeServiceStatus(os.Stdout, out, "", false)
		return
	}
	defer database.Close()
	lastPush, gerr := readServiceLastPush(appCfg, database)
	if gerr != nil {
		writeServiceStatus(os.Stdout, out, "", false)
		return
	}
	writeServiceStatus(os.Stdout, out, lastPush, true)
}

func writeServiceStatus(
	out io.Writer,
	serviceOut, lastPush string,
	lastPushAvailable bool,
) {
	fmt.Fprint(out, serviceOut)
	if serviceOut != "" && !strings.HasSuffix(serviceOut, "\n") {
		fmt.Fprintln(out)
	}
	if !lastPushAvailable {
		return
	}
	fmt.Fprintf(out, "Last push: %s\n", valueOrNever(lastPush))
}

func readServiceLastPush(
	appCfg config.Config,
	database postgres.SyncStateStore,
) (string, error) {
	targets, err := resolvePGTargetSelections(appCfg, "", false)
	if err != nil {
		return "", err
	}
	target := targets[0]
	target, err = resolvePGTargetConfig(appCfg, target)
	if err != nil {
		return "", err
	}
	return postgres.ReadLastPushAt(
		database,
		target.SyncStateTarget,
		target.PG.Projects,
		target.PG.ExcludeProjects,
		target.MigrateLegacySyncState,
	)
}

func runServiceSimple(action string) {
	mgr, err := newServiceManager()
	if err != nil {
		fatal("pg service %s: %v", action, err)
	}
	ctx := context.Background()
	switch action {
	case "uninstall":
		if err := mgr.uninstall(ctx); err != nil {
			fatal("pg service uninstall: %v", err)
		}
		fmt.Println("Service stopped and removed.")
	case "start":
		if err := mgr.start(ctx); err != nil {
			fatal("pg service start: %v", err)
		}
		fmt.Println("Service started.")
	case "stop":
		if err := mgr.stop(ctx); err != nil {
			fatal("pg service stop: %v", err)
		}
		fmt.Println("Service stopped.")
	}
}

func runServiceLogs(follow bool) {
	appCfg := loadServiceConfig()
	logPath := filepath.Join(appCfg.DataDir, "pg-watch.log")
	if err := tailFile(os.Stdout, logPath, follow); err != nil {
		fatal("pg service logs: %v", err)
	}
}

// tailFile prints the contents of path to w. When follow is true it
// keeps printing appended data until the process is interrupted.
func tailFile(w io.Writer, path string, follow bool) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf(
				"no log yet at %s (has the service run?)", path,
			)
		}
		return err
	}
	defer f.Close()
	if _, err := io.Copy(w, f); err != nil {
		return err
	}
	if !follow {
		return nil
	}
	for {
		time.Sleep(500 * time.Millisecond)
		// If the file shrank (the daemon truncated the log on
		// restart), our offset is now past EOF; seek back to the
		// start so we keep streaming new output.
		if info, serr := f.Stat(); serr == nil {
			if pos, perr := f.Seek(0, io.SeekCurrent); perr == nil && info.Size() < pos {
				if _, serr := f.Seek(0, io.SeekStart); serr != nil {
					return serr
				}
			}
		}
		if _, err := io.Copy(w, f); err != nil {
			return err
		}
	}
}

// warnUninheritedServiceEnv advises that environment variables set in the
// installing shell affect runtime behavior but are not inherited by the
// background service (which only receives AGENTSVIEW_DATA_DIR). It warns
// rather than rejects: the service still runs using config.toml or
// defaults, so this surfaces a potential divergence without overriding
// the intended "environment overrides config" semantics.
func warnUninheritedServiceEnv(w io.Writer, names []string) {
	if len(names) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w,
		"WARNING: these environment variables affect how the service runs "+
			"but are NOT inherited by it (the service only receives "+
			"AGENTSVIEW_DATA_DIR):\n  %s\n",
		strings.Join(names, ", "),
	)
	fmt.Fprintln(w,
		"The background service reads these from config.toml or uses "+
			"built-in defaults. Set them in config.toml if the service "+
			"should match your shell.",
	)
}

// promptYesNo asks a yes/no question, reading the answer from in and
// defaulting to no. in is a parameter (rather than os.Stdin directly) so
// tests can supply a reader without mutating global process state.
func promptYesNo(in io.Reader, question string) bool {
	fmt.Printf("%s [y/N]: ", question)
	r := bufio.NewReader(in)
	line, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}
