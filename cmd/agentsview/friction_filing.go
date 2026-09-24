package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
)

var frictionFilingHTTPClient = &http.Client{Timeout: 2 * time.Minute}

// resolveFrictionWriteTransport sends filing writes to the daemon that owns
// the archive, or to --server. There is no local fallback: only the hub
// daemon files, and it enforces that itself.
func resolveFrictionWriteTransport(cmd *cobra.Command) (string, string, error) {
	if remote, _ := cmd.Flags().GetString("server"); strings.TrimSpace(remote) != "" {
		token, err := explicitServerToken(cmd)
		return strings.TrimSpace(remote), token, err
	}
	cfg, err := config.LoadPFlags(cmd.Flags())
	if err != nil {
		return "", "", fmt.Errorf("loading config: %w", err)
	}
	tr, err := ensureTransportContext(cmd.Context(), &cfg, transportIntentRead, 0)
	if err != nil || tr.Mode != transportHTTP || strings.TrimSpace(tr.URL) == "" {
		return "", "", errors.New("friction filing requires the running agentsview hub daemon; start it or pass --server")
	}
	return tr.URL, cfg.AuthToken, nil
}

func frictionAPI(cmd *cobra.Command) (*apiclient.Client, error) {
	base, token, err := resolveFrictionWriteTransport(cmd)
	if err != nil {
		return nil, err
	}
	return apiclient.NewHTTPClient(base, token, frictionFilingHTTPClient)
}

func newFrictionFileCommand() *cobra.Command {
	var date string
	var forceNew, dryRun bool
	cmd := &cobra.Command{
		Use:          "file (<fingerprint> | --date YYYY-MM-DD)",
		Short:        "File friction patterns to Kata (hub only)",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if (len(args) == 1) == (date != "") {
				return errors.New("pass exactly one of <fingerprint> or --date")
			}
			api, err := frictionAPI(cmd)
			if err != nil {
				return err
			}
			fps := args
			if date != "" {
				if fps, err = digestFingerprints(cmd.Context(), api, date); err != nil {
					return err
				}
			}
			for _, fp := range fps {
				if err := fileOne(cmd.Context(), cmd.OutOrStdout(), api, fp, forceNew, dryRun); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&date, "date", "", "File every pattern in this digest date")
	cmd.Flags().BoolVar(&forceNew, "force-new", false, "Skip the metadata lookup and ask Kata for a new issue")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the request that would be sent; write nothing")
	return cmd
}

func fileOne(ctx context.Context, w io.Writer, api *apiclient.Client, fp string, forceNew, dryRun bool) error {
	body := apiclient.FrictionFileRequest{}
	if forceNew {
		body.ForceNew = new(true)
	}
	if dryRun {
		body.DryRun = new(true)
	}
	resp, err := api.PostAPIV1FrictionPatternsFingerprintFileWithResponse(ctx, &apiclient.PostAPIV1FrictionPatternsFingerprintFileRequestOptions{
		PathParams: &apiclient.PostAPIV1FrictionPatternsFingerprintFilePath{Fingerprint: fp}, Body: &body})
	if resp == nil {
		return err
	}
	if resp.StatusCode != http.StatusOK || resp.JSON200 == nil {
		return errors.New(daemonErrorMessage(resp.StatusCode, resp.Body))
	}
	if p := resp.JSON200.Preview; p != nil {
		fmt.Fprintf(w, "Title:    %s\nPriority: %d\nLabels:   %s\nForceNew: %v\n\n%s\n", p.Title, p.Priority, strings.Join(p.Labels, ", "), p.ForceNew, p.Body)
		return nil
	}
	l := resp.JSON200.Link
	if l == nil {
		return errors.New("friction file: empty response")
	}
	switch l.State {
	case "linked":
		fmt.Fprintf(w, "%s  linked  %s  %s\n", fp, l.QualifiedID, l.WebURL)
	default:
		fmt.Fprintf(w, "%s  %s  %s  %s\n", fp, l.State, l.LastErrorCode, l.LastError)
	}
	return nil
}

func digestFingerprints(ctx context.Context, api *apiclient.Client, date string) ([]string, error) {
	resp, err := api.GetAPIV1FrictionDigestsDateWithResponse(ctx, &apiclient.GetAPIV1FrictionDigestsDateRequestOptions{
		PathParams: &apiclient.GetAPIV1FrictionDigestsDatePath{Date: date}})
	if resp == nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK || resp.JSON200 == nil {
		return nil, errors.New(daemonErrorMessage(resp.StatusCode, resp.Body))
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range resp.JSON200.Signals {
		if !seen[s.Fingerprint] {
			seen[s.Fingerprint] = true
			out = append(out, s.Fingerprint)
		}
	}
	return out, nil
}

func newFrictionLinkCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "link <fingerprint> <kata-ref>",
		Short:        "Link a friction pattern to an existing Kata issue (hub only)",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := frictionAPI(cmd)
			if err != nil {
				return err
			}
			resp, err := api.PutAPIV1FrictionPatternsFingerprintLinkWithResponse(cmd.Context(), &apiclient.PutAPIV1FrictionPatternsFingerprintLinkRequestOptions{
				PathParams: &apiclient.PutAPIV1FrictionPatternsFingerprintLinkPath{Fingerprint: args[0]},
				Body:       &apiclient.FrictionLinkRequest{IssueRef: args[1]}})
			if resp == nil {
				return err
			}
			if resp.StatusCode != http.StatusOK || resp.JSON200 == nil {
				return errors.New(daemonErrorMessage(resp.StatusCode, resp.Body))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s  linked  %s\n", args[0], resp.JSON200.QualifiedID)
			return nil
		},
	}
}

func newFrictionUnlinkCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "unlink <fingerprint>",
		Short:        "Remove a friction pattern's local Kata link (Kata is not changed)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := frictionAPI(cmd)
			if err != nil {
				return err
			}
			resp, err := api.DeleteAPIV1FrictionPatternsFingerprintLinkWithResponse(cmd.Context(), &apiclient.DeleteAPIV1FrictionPatternsFingerprintLinkRequestOptions{
				PathParams: &apiclient.DeleteAPIV1FrictionPatternsFingerprintLinkPath{Fingerprint: args[0]}})
			if resp == nil {
				return err
			}
			if resp.StatusCode != http.StatusOK {
				return errors.New(daemonErrorMessage(resp.StatusCode, resp.Body))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "unlinked %s\n", args[0])
			return nil
		},
	}
}
