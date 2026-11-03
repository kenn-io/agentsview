package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/capture"
	"go.kenn.io/agentsview/internal/config"
)

type captureRunFlags struct {
	provider          string
	occurrence        string
	captureDir        string
	result            string
	providerRoot      string
	providerSessionID string
	claudeWorkDir     string
	finalizationWait  time.Duration
	quiescence        time.Duration
}

func newCaptureCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "capture",
		Short:        "Capture one non-interactive agent execution",
		GroupID:      groupUsage,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newCaptureRunCommand())
	cmd.AddCommand(newCaptureReportCommand())
	return cmd
}

func newCaptureRunCommand() *cobra.Command {
	flags := captureRunFlags{}
	cmd := &cobra.Command{
		Use:   "run --provider claude|codex --occurrence ID --capture-dir DIR --result FILE -- COMMAND [ARG...]",
		Short: "Run and report one exact non-interactive execution",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 0 {
				return errors.New("producer command must follow --")
			}
			cfg, err := config.LoadCapture()
			if err != nil {
				return fmt.Errorf("loading capture pricing configuration: %w", err)
			}
			limits := capture.DefaultLimits()
			limits.FinalizationWait = flags.finalizationWait
			limits.Quiescence = flags.quiescence
			outcome, runErr := capture.Run(cmd.Context(), capture.RunOptions{
				Provider:          capture.Provider(flags.provider),
				OccurrenceID:      flags.occurrence,
				CaptureDir:        flags.captureDir,
				ResultPath:        flags.result,
				ProviderRoot:      flags.providerRoot,
				ProviderSessionID: flags.providerSessionID,
				ClaudeWorkDir:     flags.claudeWorkDir,
				Command:           args,
				Streams: capture.Streams{
					Stdin: cmd.InOrStdin(), Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr(),
				},
				Limits:            limits,
				CustomPricing:     cfg.CustomModelPricing,
				AgentsViewVersion: version,
			})
			if runErr != nil {
				exitCode := outcome.ExitCode
				if exitCode == 0 {
					exitCode = capture.ReportFailureExitCode
				}
				return withExitCode(runErr, exitCode)
			}
			if outcome.ExitCode != 0 {
				return withSilentExitCode(
					fmt.Errorf("producer exited with status %d", outcome.ExitCode),
					outcome.ExitCode,
				)
			}
			return nil
		},
	}
	cmd.SilenceUsage = true
	cmd.Flags().StringVar(&flags.provider, "provider", "", "Producer adapter: claude or codex")
	cmd.Flags().StringVar(&flags.occurrence, "occurrence", "", "Opaque occurrence ID for this execution")
	cmd.Flags().StringVar(&flags.captureDir, "capture-dir", "", "Private directory holding recoverable capture state")
	cmd.Flags().StringVar(&flags.result, "result", "", "Usage result JSON file")
	cmd.Flags().StringVar(&flags.providerRoot, "provider-root", "", "Explicit producer session root")
	cmd.Flags().StringVar(&flags.providerSessionID, "session-id", "", "Exact Claude UUID when the child already supplies --session-id")
	cmd.Flags().StringVar(&flags.claudeWorkDir, "claude-work-dir", "", "Actual Claude working directory used inside a wrapper")
	cmd.Flags().DurationVar(
		&flags.finalizationWait, "finalization-timeout",
		capture.DefaultLimits().FinalizationWait,
		"Maximum wait for final provider data",
	)
	cmd.Flags().DurationVar(
		&flags.quiescence, "quiescence",
		capture.DefaultLimits().Quiescence,
		"Required unchanged interval before ingestion",
	)
	_ = cmd.MarkFlagRequired("provider")
	_ = cmd.MarkFlagRequired("occurrence")
	_ = cmd.MarkFlagRequired("capture-dir")
	_ = cmd.MarkFlagRequired("result")
	return cmd
}

func newCaptureReportCommand() *cobra.Command {
	var captureDir, result string
	cmd := &cobra.Command{
		Use:          "report --capture-dir DIR --result FILE|-",
		Short:        "Recover or replay a one-shot usage result",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := capture.Report(cmd.Context(), capture.ReportOptions{
				CaptureDir: captureDir, ResultPath: result,
				Stdout: cmd.OutOrStdout(),
				LoadCustomPricing: func() (map[string]config.CustomModelRate, error) {
					cfg, loadErr := config.LoadCapture()
					if loadErr != nil {
						return nil, fmt.Errorf("loading capture pricing configuration: %w", loadErr)
					}
					return cfg.CustomModelPricing, nil
				},
				AgentsViewVersion: version,
			})
			return withExitCode(err, capture.ReportFailureExitCode)
		},
	}
	cmd.Flags().StringVar(&captureDir, "capture-dir", "", "Private directory holding recoverable capture state")
	cmd.Flags().StringVar(&result, "result", "-", "Usage result JSON file, or - for standard output")
	_ = cmd.MarkFlagRequired("capture-dir")
	return cmd
}
