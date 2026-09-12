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

const pgEmbeddingActivationTimeout = 30 * time.Second

var (
	errPGEmbeddingActivationSelection    = errors.New("hosted embedding generation is not the selected desired generation")
	errPGEmbeddingActivationAvailability = errors.New("selected hosted embedding profile or credential is unavailable")
	errPGEmbeddingActivationNotReady     = errors.New("hosted embedding generation is not ready for activation")
)

type pgEmbeddingActivateResultDTO struct {
	GenerationID int64 `json:"generation_id"`
	Activated    bool  `json:"activated"`
}

func newPGEmbeddingActivateCommand() *cobra.Command {
	var generation int64
	cmd := &cobra.Command{
		Use:   "activate [target]",
		Short: "Activate a ready hosted embedding generation",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if generation <= 0 {
				return errors.New("--generation must be a positive integer")
			}
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), pgEmbeddingActivationTimeout)
			defer cancel()
			return runPGEmbeddingActivate(ctx, cmd.OutOrStdout(), target, generation, outputFormat(cmd) == "json")
		},
	}
	cmd.Flags().Int64Var(&generation, "generation", 0, "Desired generation ID to activate")
	registerFormatFlags(cmd.Flags())
	_ = cmd.MarkFlagRequired("generation")
	return cmd
}

func runPGEmbeddingActivate(ctx context.Context, out io.Writer, target string, generationID int64, asJSON bool) error {
	if generationID <= 0 {
		return errors.New("--generation must be a positive integer")
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
		return errors.New("hosted embedding activation requires raw_tenant and schema on the selected target")
	}
	database, err := postgres.OpenHostedContext(ctx, pgCfg.URL, pgCfg.Schema, pgCfg.RawTenant, pgCfg.AllowInsecure)
	if err != nil {
		if ctx.Err() != nil {
			return pgEmbeddingActivateBoundaryError(ctx.Err())
		}
		return errors.New("opening selected hosted embedding runtime target failed")
	}
	defer database.Close()
	store, err := postgres.NewHostedEmbeddingStore(ctx, database, postgres.HostedEmbeddingOptions{Schema: pgCfg.Schema, Tenant: pgCfg.RawTenant})
	if errors.Is(err, postgres.ErrHostedEmbeddingUnprovisioned) {
		return errors.New("hosted embeddings are not provisioned on the selected runtime target")
	}
	if err != nil {
		return errors.New("opening selected hosted embedding activation store failed")
	}
	desired, err := store.Desired(ctx)
	if err != nil {
		return pgEmbeddingActivateBoundaryError(err)
	}
	if desired == nil || desired.ID != generationID {
		return errPGEmbeddingActivationSelection
	}
	if err = newHostedEmbeddingResolver(cfg.HostedEmbeddings).available(*desired); err != nil {
		return errPGEmbeddingActivationAvailability
	}
	activated, err := store.Activate(ctx, generationID)
	if err != nil {
		return pgEmbeddingActivateBoundaryError(err)
	}
	if !activated {
		return errPGEmbeddingActivationNotReady
	}
	return writePGEmbeddingActivateResult(out, pgEmbeddingActivateResultDTO{GenerationID: generationID, Activated: true}, asJSON)
}

func pgEmbeddingActivateBoundaryError(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("hosted embedding activation timed out")
	case errors.Is(err, context.Canceled):
		return errors.New("hosted embedding activation canceled")
	default:
		return errors.New("activating hosted embedding generation failed")
	}
}

func writePGEmbeddingActivateResult(out io.Writer, result pgEmbeddingActivateResultDTO, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(result)
	}
	_, err := fmt.Fprintf(out, "Generation %d activated.\n", result.GenerationID)
	return err
}
