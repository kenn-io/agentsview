package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
)

const pgEmbeddingRecoveryTimeout = 30 * time.Second

type pgEmbeddingRetryResultDTO struct {
	GenerationID int64 `json:"generation_id"`
	Examined     int   `json:"examined"`
	Retried      int   `json:"retried"`
	Skipped      int   `json:"skipped"`
}

func newPGEmbeddingRetryFailedCommand() *cobra.Command {
	var generationID int64
	var batchSize int
	cmd := &cobra.Command{
		Use:   "retry-failed [target]",
		Short: "Queue a bounded batch of failed hosted embeddings",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if generationID <= 0 {
				return errors.New("--generation must be a positive integer")
			}
			if batchSize < 1 || batchSize > 256 {
				return errors.New("--batch-size must be between 1 and 256")
			}
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), pgEmbeddingRecoveryTimeout)
			defer cancel()
			return runPGEmbeddingRetryFailed(ctx, cmd.OutOrStdout(), target, generationID, batchSize, outputFormat(cmd) == "json")
		},
	}
	cmd.Flags().Int64Var(&generationID, "generation", 0, "Active or desired generation ID")
	cmd.Flags().IntVar(&batchSize, "batch-size", 64, "Maximum failed requirements to examine (1-256)")
	registerFormatFlags(cmd.Flags())
	_ = cmd.MarkFlagRequired("generation")
	return cmd
}

func runPGEmbeddingRetryFailed(ctx context.Context, out io.Writer, target string, generationID int64, batchSize int, asJSON bool) error {
	if generationID <= 0 {
		return errors.New("--generation must be a positive integer")
	}
	if batchSize < 1 || batchSize > 256 {
		return errors.New("--batch-size must be between 1 and 256")
	}
	cfg, err := config.LoadReadOnly()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	pgCfg, err := cfg.ResolvePGTarget(target)
	if err != nil {
		return err
	}
	if strings.TrimSpace(pgCfg.URL) == "" {
		return errors.New("selected PostgreSQL target has no usable url")
	}
	if pgCfg.RawTenant == "" || pgCfg.Schema == "" {
		return errors.New("hosted embedding recovery requires raw_tenant and schema on the selected target")
	}
	database, err := postgres.OpenHostedContext(ctx, pgCfg.URL, pgCfg.Schema, pgCfg.RawTenant, pgCfg.AllowInsecure)
	if err != nil {
		return errors.New("opening selected hosted embedding runtime target failed")
	}
	defer database.Close()
	store, err := postgres.NewHostedEmbeddingStore(ctx, database, postgres.HostedEmbeddingOptions{Schema: pgCfg.Schema, Tenant: pgCfg.RawTenant})
	if errors.Is(err, postgres.ErrHostedEmbeddingUnprovisioned) {
		return errors.New("hosted embeddings are not provisioned on the selected runtime target")
	}
	if err != nil {
		return errors.New("opening selected hosted embedding recovery store failed")
	}
	result, err := store.RetryFailed(ctx, generationID, batchSize)
	if err != nil {
		return pgEmbeddingRetryBoundaryError(err)
	}
	return writePGEmbeddingRetryResult(out, result, asJSON)
}

func pgEmbeddingRetryBoundaryError(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("hosted embedding recovery timed out; retry with a smaller --batch-size")
	case errors.Is(err, context.Canceled):
		return errors.New("hosted embedding recovery canceled")
	case errors.Is(err, postgres.ErrHostedEmbeddingGenerationNotServiced):
		return errors.New("hosted embedding generation is not active or desired")
	case errors.Is(err, postgres.ErrHostedEmbeddingRecoveryIndexUnavailable):
		return errors.New("hosted embedding recovery index unavailable; with the owner role, reprovision the current desired instance (or the active instance if no desired generation exists); provisioning reselects it")
	default:
		return errors.New("retrying failed hosted embeddings failed")
	}
}

func writePGEmbeddingRetryResult(out io.Writer, result postgres.HostedEmbeddingRetryResult, asJSON bool) error {
	dto := pgEmbeddingRetryResultDTO{
		GenerationID: result.GenerationID,
		Examined:     result.Examined,
		Retried:      result.Retried,
		Skipped:      result.Skipped,
	}
	if asJSON {
		return json.NewEncoder(out).Encode(dto)
	}
	if _, err := fmt.Fprintf(out, "Generation %d: examined=%d retried=%d skipped=%d\n", dto.GenerationID, dto.Examined, dto.Retried, dto.Skipped); err != nil {
		return err
	}
	if dto.Skipped > 0 {
		_, err := fmt.Fprintln(out, "Skipped failures remain unchanged and await normal worker reconciliation.")
		return err
	}
	return nil
}
