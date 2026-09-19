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
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func newPGServiceCommand() *cobra.Command {
	return newServiceCommands(pgServiceKind)
}

func newClickHouseServiceCommand() *cobra.Command {
	return newServiceCommands(clickHouseServiceKind)
}

func newServiceCommands(kind serviceKind) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "service",
		Short:        fmt.Sprintf("Install and manage the %s push --watch background service", kind.Name),
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newServiceInstallCommand(kind))
	cmd.AddCommand(newServiceUninstallCommand(kind))
	cmd.AddCommand(newServiceStatusCommand(kind))
	cmd.AddCommand(newServiceStartCommand(kind))
	cmd.AddCommand(newServiceStopCommand(kind))
	cmd.AddCommand(newServiceLogsCommand(kind))
	return cmd
}

func newServiceInstallCommand(kind serviceKind) *cobra.Command {
	return &cobra.Command{
		Use:          "install",
		Short:        "Install and start the auto-push service",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runServiceInstall(kind)
		},
	}
}

func newServiceUninstallCommand(kind serviceKind) *cobra.Command {
	return &cobra.Command{
		Use:          "uninstall",
		Short:        "Stop and remove the auto-push service",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runServiceSimple(kind, "uninstall")
		},
	}
}

func newServiceStatusCommand(kind serviceKind) *cobra.Command {
	return &cobra.Command{
		Use:          "status",
		Short:        "Show the auto-push service status",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runServiceStatus(kind)
		},
	}
}

func newServiceStartCommand(kind serviceKind) *cobra.Command {
	return &cobra.Command{
		Use:          "start",
		Short:        "Start the auto-push service",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runServiceSimple(kind, "start")
		},
	}
}

func newServiceStopCommand(kind serviceKind) *cobra.Command {
	return &cobra.Command{
		Use:          "stop",
		Short:        "Stop the auto-push service",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runServiceSimple(kind, "stop")
		},
	}
}

func newServiceLogsCommand(kind serviceKind) *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:          "logs",
		Short:        "Show the auto-push service log",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runServiceLogs(kind, follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow the log output")
	return cmd
}

// loadServiceConfig loads minimal config and ensures the data dir.
func loadServiceConfig(kind serviceKind) config.Config {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		fatal("%s service: loading config: %v", kind.Name, err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		fatal("%s service: creating data dir: %v", kind.Name, err)
	}
	return appCfg
}

func runServiceInstall(kind serviceKind) {
	appCfg := loadServiceConfig(kind)
	spec, err := buildServiceSpec(appCfg, kind)
	if err != nil {
		fatal("%s service install: %v", kind.Name, err)
	}
	warnUninheritedServiceEnv(
		os.Stdout, setEnvVarsAffectingService(kind, os.LookupEnv),
	)
	mgr, err := newServiceManager(kind)
	if err != nil {
		fatal("%s service install: %v", kind.Name, err)
	}
	ctx := context.Background()

	if err := mgr.install(ctx, spec); err != nil {
		fatal("%s service install: %v", kind.Name, err)
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
	fmt.Printf("View logs with: agentsview %s service logs -f\n", kind.Name)
}

func runServiceStatus(kind serviceKind) {
	mgr, err := newServiceManager(kind)
	if err != nil {
		fatal("%s service status: %v", kind.Name, err)
	}
	ctx := context.Background()
	out, _ := mgr.status(ctx)
	// Show the last successful push time from local sync state.
	appCfg := loadServiceConfig(kind)
	database, derr := openReadOnlyDB(ctx, appCfg)
	if derr != nil {
		writeServiceStatus(os.Stdout, out, "", false)
		return
	}
	defer database.Close()
	lastPush, gerr := readServiceLastPush(ctx, kind, appCfg, database)
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

func readServiceLastPush(ctx context.Context,
	kind serviceKind,
	appCfg config.Config,
	database *db.DB,
) (string, error) {
	backend, err := replicaBackendNamed(kind.Name)
	if err != nil {
		return "", err
	}
	target, err := storage.DefaultTarget(backend, appCfg)
	if err != nil {
		return "", err
	}
	return backend.LastPushAt(
		ctx, database, target,
		target.Projects, target.ExcludeProjects,
	)
}

func runServiceSimple(kind serviceKind, action string) {
	mgr, err := newServiceManager(kind)
	if err != nil {
		fatal("%s service %s: %v", kind.Name, action, err)
	}
	ctx := context.Background()
	switch action {
	case "uninstall":
		if err := mgr.uninstall(ctx); err != nil {
			fatal("%s service uninstall: %v", kind.Name, err)
		}
		fmt.Println("Service stopped and removed.")
	case "start":
		if err := mgr.start(ctx); err != nil {
			fatal("%s service start: %v", kind.Name, err)
		}
		fmt.Println("Service started.")
	case "stop":
		if err := mgr.stop(ctx); err != nil {
			fatal("%s service stop: %v", kind.Name, err)
		}
		fmt.Println("Service stopped.")
	}
}

func runServiceLogs(kind serviceKind, follow bool) {
	appCfg := loadServiceConfig(kind)
	logPath := filepath.Join(appCfg.DataDir, kind.LogName)
	if err := tailFile(os.Stdout, logPath, follow); err != nil {
		fatal("%s service logs: %v", kind.Name, err)
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
