package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/vector"
)

const (
	hostedEmbeddingBuilderVersion = "conversation-v1"
	hostedEmbeddingChunkerVersion = "rune-overlap-v1"
	hostedEmbeddingCorpusScope    = "hosted-sessions-v1"
)

func newPGEmbeddingsCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "embeddings", Short: "Manage hosted PostgreSQL embedding generations", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	cmd.AddCommand(newPGEmbeddingGenerationCommand("provision"))
	cmd.AddCommand(newPGEmbeddingGenerationCommand("rebuild"))
	cmd.AddCommand(newPGEmbeddingStatusCommand())
	cmd.AddCommand(newPGEmbeddingRetryFailedCommand())
	cmd.AddCommand(newPGEmbeddingActivateCommand())
	return cmd
}

func newPGEmbeddingGenerationCommand(action string) *cobra.Command {
	var profileName, runtimeRole, instanceKey, activationMode string
	cmd := &cobra.Command{
		Use: action + " [target]", Args: cobra.MaximumNArgs(1),
		Short: "Provision and select a hosted embedding generation",
		RunE: func(cmd *cobra.Command, args []string) error {
			if activationMode != "" && activationMode != postgres.HostedEmbeddingActivationAutomatic && activationMode != postgres.HostedEmbeddingActivationManual {
				return errors.New("--activation-mode must be automatic or manual")
			}
			target := ""
			if len(args) > 0 {
				target = args[0]
			}
			return runPGEmbeddingGeneration(cmd.Context(), cmd.OutOrStdout(), action, target, profileName, runtimeRole, instanceKey, activationMode)
		},
	}
	cmd.Flags().StringVar(&profileName, "profile", "", "Named hosted embedding profile")
	cmd.Flags().StringVar(&runtimeRole, "runtime-role", "", "Existing restricted PostgreSQL runtime role")
	cmd.Flags().StringVar(&instanceKey, "instance-key", "", "Stable generation instance key")
	cmd.Flags().StringVar(&activationMode, "activation-mode", "", "Activation policy: automatic or manual")
	_ = cmd.MarkFlagRequired("profile")
	_ = cmd.MarkFlagRequired("runtime-role")
	_ = cmd.MarkFlagRequired("instance-key")
	return cmd
}

func runPGEmbeddingGeneration(ctx context.Context, out interface{ Write([]byte) (int, error) }, action, target, profileName, runtimeRole, instanceKey, activationMode string) error {
	cfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	applyClassifierConfig(cfg)
	pgCfg, err := cfg.ResolvePGTarget(target)
	if err != nil {
		return err
	}
	if strings.TrimSpace(pgCfg.URL) == "" {
		return errors.New("selected PostgreSQL target has no usable url")
	}
	if pgCfg.RawTenant == "" || pgCfg.Schema == "" {
		return errors.New("hosted embedding provisioning requires raw_tenant and schema on the selected target")
	}
	profileName = strings.ToLower(strings.TrimSpace(profileName))
	profile, err := cfg.HostedEmbeddings.Profile(profileName)
	if err != nil {
		return err
	}
	recipe, err := hostedEmbeddingRecipe(profileName, profile)
	if err != nil {
		return err
	}
	owner, err := postgres.Open(pgCfg.URL, pgCfg.Schema, pgCfg.AllowInsecure)
	if err != nil {
		return errors.New("opening selected hosted embedding owner target failed")
	}
	defer owner.Close()
	if action == "rebuild" {
		completed, err := completedHostedEmbeddingInstance(ctx, owner, pgCfg.Schema, pgCfg.RawTenant, instanceKey)
		if err != nil {
			return fmt.Errorf("checking hosted embedding rebuild instance: %w", err)
		}
		if completed {
			return errors.New("hosted embedding rebuild requires a new instance key after a completed generation")
		}
	}
	generation, err := postgres.ProvisionHostedEmbeddingsWithOptions(ctx, owner, pgCfg.Schema, pgCfg.RawTenant, recipe, instanceKey, runtimeRole, postgres.HostedEmbeddingProvisionOptions{ActivationMode: activationMode})
	if err != nil {
		return fmt.Errorf("hosted embedding provisioning failed: %w", err)
	}
	_, err = fmt.Fprintf(out, "Generation %d profile=%s fingerprint=%s activation_mode=%s is desired. Check the restricted runtime target with: agentsview pg embeddings status RUNTIME_TARGET\n", generation.ID, generation.Recipe.ProfileName, generation.Recipe.Fingerprint, generation.ActivationMode)
	return err
}

func completedHostedEmbeddingInstance(ctx context.Context, owner *sql.DB, schema, tenant, instanceKey string) (bool, error) {
	var provisioned bool
	if err := owner.QueryRowContext(ctx, `SELECT to_regclass(format('%I.hosted_embedding_generations',$1::text)) IS NOT NULL`, schema).Scan(&provisioned); err != nil || !provisioned {
		return false, err
	}
	quotedSchema := `"` + schema + `"`
	var complete bool
	err := owner.QueryRowContext(ctx, `SELECT g.backfill_finished AND NOT EXISTS(
		SELECT 1 FROM `+quotedSchema+`.hosted_embedding_requirements r
		WHERE r.tenant_id=g.tenant_id AND r.generation_id=g.id
		AND (r.state<>'complete' OR r.completed_revision IS DISTINCT FROM r.required_revision)
	) FROM `+quotedSchema+`.hosted_embedding_generations g WHERE g.tenant_id=$1 AND g.instance_key=$2`, tenant, instanceKey).Scan(&complete)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return complete, err
}

type hostedEmbeddingStatusDTO struct {
	Provisioned     bool                          `json:"provisioned"`
	Active          *hostedEmbeddingGenerationDTO `json:"active,omitempty"`
	Desired         *hostedEmbeddingGenerationDTO `json:"desired,omitempty"`
	ActivationReady bool                          `json:"activation_ready"`
	ActiveAvailable bool                          `json:"active_available"`
}

type hostedEmbeddingGenerationDTO struct {
	ID               int64            `json:"id"`
	InstanceKey      string           `json:"instance_key"`
	Profile          string           `json:"profile"`
	Fingerprint      string           `json:"fingerprint"`
	Model            string           `json:"model"`
	Dimensions       int              `json:"dimensions"`
	ActivationMode   string           `json:"activation_mode"`
	BackfillFinished bool             `json:"backfill_finished"`
	Ready            int64            `json:"ready"`
	Leased           int64            `json:"leased"`
	Retry            int64            `json:"retry"`
	Failed           int64            `json:"failed"`
	Complete         int64            `json:"complete"`
	Errors           map[string]int64 `json:"errors,omitempty"`
}

func hostedEmbeddingGenerationStatusDTO(status *postgres.HostedEmbeddingGenerationStatus) *hostedEmbeddingGenerationDTO {
	if status == nil {
		return nil
	}
	g := status.Generation
	return &hostedEmbeddingGenerationDTO{ID: g.ID, InstanceKey: g.InstanceKey, Profile: g.Recipe.ProfileName, Fingerprint: g.Recipe.Fingerprint, Model: g.Recipe.Model, Dimensions: g.Recipe.Dimensions, ActivationMode: g.ActivationMode, BackfillFinished: status.BackfillFinished, Ready: status.Ready, Leased: status.Leased, Retry: status.Retry, Failed: status.Failed, Complete: status.Complete, Errors: status.Errors}
}

func newPGEmbeddingStatusCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "status [target]", Short: "Show hosted embedding generation status", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		target := ""
		if len(args) > 0 {
			target = args[0]
		}
		return runPGEmbeddingStatus(cmd.Context(), cmd.OutOrStdout(), target, outputFormat(cmd) == "json")
	}}
	registerFormatFlags(cmd.Flags())
	return cmd
}

func runPGEmbeddingStatus(ctx context.Context, out interface{ Write([]byte) (int, error) }, target string, asJSON bool) error {
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
	pool, err := postgres.OpenHosted(pgCfg.URL, pgCfg.Schema, pgCfg.RawTenant, pgCfg.AllowInsecure)
	if err != nil {
		return errors.New("opening selected hosted embedding runtime target failed")
	}
	defer pool.Close()
	store, err := postgres.NewHostedEmbeddingStore(ctx, pool, postgres.HostedEmbeddingOptions{Schema: pgCfg.Schema, Tenant: pgCfg.RawTenant})
	dto := hostedEmbeddingStatusDTO{}
	if errors.Is(err, postgres.ErrHostedEmbeddingUnprovisioned) {
		if asJSON {
			return json.NewEncoder(out).Encode(dto)
		}
		_, err = fmt.Fprintln(out, "Hosted embeddings are not provisioned.")
		return err
	}
	if err != nil {
		return err
	}
	status, err := store.Status(ctx)
	if err != nil {
		return err
	}
	dto = hostedEmbeddingStatusDTO{Provisioned: true, Active: hostedEmbeddingGenerationStatusDTO(status.Active), Desired: hostedEmbeddingGenerationStatusDTO(status.Desired), ActivationReady: status.ActivationReady, ActiveAvailable: status.ActiveAvailable}
	if asJSON {
		return json.NewEncoder(out).Encode(dto)
	}
	_, err = fmt.Fprintf(out, "Hosted embeddings: active_available=%t activation_ready=%t\n", dto.ActiveAvailable, dto.ActivationReady)
	if err != nil {
		return err
	}
	for label, generation := range map[string]*hostedEmbeddingGenerationDTO{"active": dto.Active, "desired": dto.Desired} {
		if generation == nil {
			continue
		}
		if _, err = fmt.Fprintf(out, "%s: id=%d profile=%s activation_mode=%s backfill=%t ready=%d leased=%d retry=%d failed=%d complete=%d\n", label, generation.ID, generation.Profile, generation.ActivationMode, generation.BackfillFinished, generation.Ready, generation.Leased, generation.Retry, generation.Failed, generation.Complete); err != nil {
			return err
		}
	}
	return nil
}

func hostedEmbeddingRecipe(name string, profile config.HostedEmbeddingProfile) (postgres.HostedEmbeddingRecipe, error) {
	c := profile.Embeddings
	return postgres.CanonicalHostedEmbeddingRecipe(postgres.HostedEmbeddingRecipe{
		ProfileName: name, Model: c.Model, Dimensions: c.Dimension,
		BuilderVersion: hostedEmbeddingBuilderVersion, ChunkerVersion: hostedEmbeddingChunkerVersion,
		MaxInputChars: c.MaxInputChars, ChunkOverlapChars: vector.ChunkOverlap(c.MaxInputChars),
		DocumentPrefix: c.DocumentPrefix, QueryPrefix: c.QueryPrefix, InputSuffix: c.InputSuffix,
		RequestDimensions: c.RequestDimensions, IncludeAutomated: profile.IncludeAutomated,
		EncodingType: postgres.HostedEmbeddingEncoding, CorpusScope: hostedEmbeddingCorpusScope,
	})
}
