package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/remotecred"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

type readTargetKind string

const (
	readTargetLocal  readTargetKind = "local"
	readTargetServer readTargetKind = "server"
	readTargetPG     readTargetKind = "pg"
	readTargetRemote readTargetKind = "remote"
)

type readTargetIntent bool

const (
	readTargetDefault      readTargetIntent = true
	readTargetExplicitOnly readTargetIntent = false
)

type readTarget struct {
	Kind                                readTargetKind
	URL, Cred, Origin, CredentialSource string
	client                              *http.Client
}

func localReadRequested(cmd *cobra.Command) bool { v, _ := cmd.Flags().GetBool("local"); return v }
func validateTargetFlags(cmd *cobra.Command) error {
	server, _ := cmd.Flags().GetString("server")
	if server != "" && pgReadRequested(cmd) {
		return errors.New("--server and --pg are mutually exclusive")
	}
	if localReadRequested(cmd) && (server != "" || pgReadRequested(cmd)) {
		return errors.New("--local cannot be combined with --server or --pg")
	}
	return nil
}

// resolveReadTarget chooses a target before opening any selected credential or
// setting up local storage. Memory callers apply their existing env flags first.
func resolveReadTarget(cmd *cobra.Command, intent readTargetIntent) (readTarget, error) {
	server, _ := cmd.Flags().GetString("server")
	if server != "" {
		token, err := explicitServerToken(cmd)
		source := "none"
		if path, _ := cmd.Flags().GetString("server-token-file"); path != "" {
			source = "--server-token-file"
		} else if token != "" {
			source = "AGENTSVIEW_SERVER_TOKEN"
		}
		return readTarget{Kind: readTargetServer, URL: server, Cred: token, Origin: "--server", CredentialSource: source}, err
	}
	if err := validateTargetFlags(cmd); err != nil {
		return readTarget{}, err
	}
	if pgReadRequested(cmd) {
		return readTarget{Kind: readTargetPG, Origin: "--pg"}, nil
	}
	if localReadRequested(cmd) {
		return readTarget{Kind: readTargetLocal, Origin: "--local"}, nil
	}
	if !intent {
		return readTarget{Kind: readTargetLocal, Origin: "local default"}, nil
	}
	r, err := config.LoadRemote()
	if err != nil {
		return readTarget{}, err
	}
	if r.URL == "" {
		if r.TokenFile != "" {
			return readTarget{}, errors.New("remote token_file requires remote url; try --local")
		}
		return readTarget{Kind: readTargetLocal, Origin: "local default"}, nil
	}
	client, err := remotecred.Client(r.URL, r.AllowInsecureHTTP)
	if err != nil {
		return readTarget{}, err
	}
	target := readTarget{Kind: readTargetRemote, URL: r.URL, Origin: "[remote]", CredentialSource: "AGENTSVIEW_SERVER_TOKEN", client: client}
	if os.Getenv("AGENTSVIEW_REMOTE_URL") != "" {
		target.Origin = "AGENTSVIEW_REMOTE_URL"
	}
	if r.TokenFile != "" {
		target.Cred, err = remotecred.ReadToken(r.TokenFile)
		target.CredentialSource = "[remote].token_file"
		if os.Getenv("AGENTSVIEW_REMOTE_TOKEN_FILE") != "" {
			target.CredentialSource = "AGENTSVIEW_REMOTE_TOKEN_FILE"
		}
	} else {
		target.Cred = strings.TrimSpace(os.Getenv("AGENTSVIEW_SERVER_TOKEN"))
		if target.Cred == "" {
			target.CredentialSource = "none"
		}
	}
	return target, target.remoteError(err)
}

func (t readTarget) isHTTP() bool { return t.Kind == readTargetServer || t.Kind == readTargetRemote }

func (t readTarget) probe(ctx context.Context) (servicehttp.HTTPServerCapabilities, error) {
	caps, err := servicehttp.ProbeHTTPServerCapabilitiesWithClient(ctx, t.URL, t.Cred, t.client)
	return caps, t.remoteError(err)
}

func (t readTarget) remoteError(err error) error {
	if err == nil || t.Kind != readTargetRemote {
		return err
	}
	return remotecred.WrapError(t.URL, err)
}

func (t readTarget) backend(caps *servicehttp.HTTPServerCapabilities) service.SessionService {
	if caps != nil {
		return servicehttp.NewHTTPBackendForServerWithClient(t.URL, t.Cred, *caps, t.client)
	}
	return servicehttp.NewHTTPBackendWithClient(t.URL, t.Cred, false, "", t.client)
}

func newDoctorRemoteCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "remote", Short: "Diagnose the selected remote read target", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		target, err := resolveReadTarget(cmd, readTargetDefault)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Target: %s\nOrigin: %s\nURL: %s\nCredential: %s\n", target.Kind, target.Origin, remotecred.Redact(target.URL), target.CredentialSource)
		if !target.isHTTP() {
			return nil
		}
		caps, err := target.probe(cmd.Context())
		if err != nil {
			return fmt.Errorf("remote %s probe failed; try --local: %w", remotecred.Redact(target.URL), err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Probe: OK (API %d, read-only %t, session stats %t)\n", caps.APIVersion, caps.ReadOnly, caps.SessionStatsAvailable)
		return nil
	}}
	cmd.Flags().String("server", "", "Explicit server URL")
	cmd.Flags().String("server-token-file", "", "Explicit server token file")
	cmd.Flags().Bool("pg", false, "Select configured PostgreSQL")
	return cmd
}
