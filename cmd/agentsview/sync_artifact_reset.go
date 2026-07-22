package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

type syncArtifactResetResponse struct {
	artifact.RepositoryResetResult
	ManualCleanup    string `json:"manual_cleanup"`
	ForeignArtifacts string `json:"foreign_artifacts"`
}

type syncArtifactResetDependencies struct {
	findDaemon        func(string, ...string) *DaemonRuntime
	localDaemonActive func(string, ...string) bool
	openDirect        func(context.Context, config.Config) (*db.DB, func(), error)
	resetRepository   func(context.Context, string, *db.DB, string, *artifact.Repository) (*artifact.Repository, artifact.RepositoryResetResult, error)
}

func newSyncArtifactResetCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "artifact-reset",
		Short:        "Move aside a failed artifact vault and rebuild it from SQLite",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSyncArtifactReset(cmd)
		},
	}
}

func runSyncArtifactReset(cmd *cobra.Command) error {
	cfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	return runSyncArtifactResetWith(cmd, cfg, syncArtifactResetDependencies{})
}

func runSyncArtifactResetWith(
	cmd *cobra.Command,
	cfg config.Config,
	deps syncArtifactResetDependencies,
) (retErr error) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if deps.findDaemon == nil {
		deps.findDaemon = FindDaemonRuntime
	}
	if deps.localDaemonActive == nil {
		deps.localDaemonActive = IsLocalDaemonActive
	}
	if deps.openDirect == nil {
		deps.openDirect = openArtifactResetDirect
	}
	if deps.resetRepository == nil {
		deps.resetRepository = artifact.ResetRepository
	}

	if runtime := deps.findDaemon(cfg.DataDir, cfg.AuthToken); runtime != nil {
		if runtime.ReadOnly {
			return errors.New("a read-only daemon owns artifact access; stop it before reset")
		}
		response, err := runSyncArtifactResetDaemon(ctx, runtime, cfg.AuthToken)
		if err != nil {
			return err
		}
		printSyncArtifactReset(cmd.OutOrStdout(), response)
		return nil
	}
	if deps.localDaemonActive(cfg.DataDir, cfg.AuthToken) {
		return errors.New("the writable daemon owns the artifact vault but is not responding; refusing direct reset")
	}

	database, cleanup, err := deps.openDirect(ctx, cfg)
	if err != nil {
		return fmt.Errorf("acquiring direct artifact reset ownership: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	origin := cfg.ArtifactOriginID
	if origin == "" {
		origin, err = artifact.StoredOrigin(database)
		if err != nil {
			return err
		}
	}
	fresh, result, err := deps.resetRepository(
		ctx, cfg.DataDir, database, origin, nil,
	)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, fresh.Close()) }()
	printSyncArtifactReset(cmd.OutOrStdout(), syncArtifactResetResponse{
		RepositoryResetResult: result,
		ManualCleanup:         artifact.ArtifactResetManualCleanupWarning,
		ForeignArtifacts:      artifact.ArtifactResetForeignRelayWarning,
	})
	return nil
}

func openArtifactResetDirect(
	ctx context.Context, cfg config.Config,
) (*db.DB, func(), error) {
	database, lock, err := openWriteDB(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	return database, func() { closeWriteDB(database, lock) }, nil
}

func runSyncArtifactResetDaemon(
	ctx context.Context, runtime *DaemonRuntime, authToken string,
) (syncArtifactResetResponse, error) {
	endpoint, origin, err := daemonArtifactExchangeEndpoint(transportFromRuntime(runtime))
	if err != nil {
		return syncArtifactResetResponse{}, err
	}
	endpoint = strings.TrimSuffix(endpoint, "/exchange") + "/reset"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
	if err != nil {
		return syncArtifactResetResponse{}, err
	}
	req.Header.Set("Origin", origin)
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		return syncArtifactResetResponse{}, fmt.Errorf("daemon artifact reset request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return syncArtifactResetResponse{}, fmt.Errorf(
			"daemon artifact reset returned HTTP %d: %s", resp.StatusCode, message,
		)
	}
	var response syncArtifactResetResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&response); err != nil {
		return syncArtifactResetResponse{}, errors.New("decoding daemon artifact reset response")
	}
	return response, nil
}

func printSyncArtifactReset(w io.Writer, response syncArtifactResetResponse) {
	fmt.Fprintln(w, "Artifact vault reset complete")
	fmt.Fprintf(w, "Moved-aside diagnostic vault: %s\n", response.DiagnosticRoot)
	fmt.Fprintf(w, "Fresh artifact vault: %s\n", response.VaultRoot)
	fmt.Fprintf(w, "Republished %d local session(s); checkpoint sequence %d\n",
		response.Export.ExportedSessions, response.Export.CheckpointSequence)
	fmt.Fprintln(w, response.ManualCleanup)
	fmt.Fprintln(w, response.ForeignArtifacts)
}
