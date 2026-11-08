package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/ctxio"
	"go.kenn.io/agentsview/internal/db"
)

type sessionPushResult struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	Status    string `json:"status"`
}

func newPushSessionCommand() *cobra.Command {
	var target, agent, tokenEnv, tokenFile string
	cmd := &cobra.Command{
		Use: "push <session-id>", Short: "Push exactly one archived session to an opted-in remote instance", GroupID: groupData,
		Long: "Push one exact canonical archive ID, without syncing sources or following parent/child references.\n" +
			"Codex IDs start with codex:, Pi IDs with pi:, and Claude IDs are unprefixed.\n" +
			"The first accepted snapshot is immutable: identical retries succeed, changed snapshots conflict.\n" +
			"The receiver needs writable SQLite, require_auth=true and accept_session_push=true.\n" +
			"Literal token arguments are not accepted.\n" +
			"Use AGENTSVIEW_DATA_DIR to select the local archive.",
		Args: cobra.ExactArgs(1), SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("token-file") && strings.TrimSpace(tokenFile) == "" {
				return errors.New("push: selected token file must name a file")
			}
			if _, err := sessionPushURL(target); err != nil {
				return err
			}
			envName := tokenEnv
			if tokenFile != "" {
				envName = ""
			}
			token, err := sessionPushToken(cmd.Context(), envName, tokenFile)
			if err != nil {
				return err
			}
			cfg, err := config.LoadReadOnlyFor(config.ScopeLocal)
			if err != nil {
				return errors.New("push: cannot read local archive configuration")
			}
			applyClassifierConfig(cfg)
			database, err := db.OpenReadOnly(cmd.Context(), cfg.DBPath)
			if err != nil {
				return errors.New("push: cannot open local archive; upgrade and sync it before pushing")
			}
			defer database.Close()
			database.SetArchiveContent(cfg.ArchiveContent)
			payload, err := artifact.EncodeSessionPush(cmd.Context(), database, args[0], agent)
			if err != nil {
				return fmt.Errorf("push: %w", err)
			}
			// The validated packet supplies the stored agent even when --agent was omitted.
			_, write, err := artifact.DecodeSessionPush(payload)
			if err != nil {
				return errors.New("push: cannot validate local session")
			}
			result, err := sendSessionPush(cmd.Context(), target, token, payload, args[0], write.Session.Agent)
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.MarshalWrite(cmd.OutOrStdout(), result)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Session %s: %s\n", result.SessionID, result.Status)
			return err
		},
	}
	cmd.Flags().StringVar(&target, "to", "", "Receiver base URL (HTTPS except loopback)")
	cmd.Flags().StringVar(&agent, "agent", "", "Require this exact stored agent; does not resolve IDs")
	cmd.Flags().StringVar(&tokenEnv, "token-env", "AGENTSVIEW_PUSH_TOKEN", "Environment variable containing the receiver bearer token")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "File containing the receiver bearer token")
	registerFormatFlags(cmd.Flags())
	cmd.MarkFlagsMutuallyExclusive("token-env", "token-file")
	return cmd
}

func sessionPushToken(ctx context.Context, envName, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var token string
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return "", errors.New("push: cannot read selected token file")
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(ctxio.Reader{Context: ctx, Reader: f}, 4097))
		if err != nil || len(data) > 4096 {
			return "", errors.New("push: cannot read selected token file")
		}
		token = string(data)
	} else {
		token = os.Getenv(envName)
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 4096 {
		return "", errors.New("push: selected token source is empty or invalid")
	}
	for _, r := range token {
		if r <= 32 || r >= 127 {
			return "", errors.New("push: selected token source is invalid")
		}
	}
	return token, nil
}

func sessionPushURL(base string) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.Opaque != "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(base, "#") {
		return nil, errors.New("push: --to must be an absolute HTTP(S) base URL without credentials, query or fragment")
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("push: HTTPS is required except for loopback receivers")
		}
	}
	// Append before unescaping so encoded separators retain their mount boundary.
	u.RawPath = strings.TrimRight(u.EscapedPath(), "/") + "/api/v1/sessions/ingest"
	u.Path, err = url.PathUnescape(u.RawPath)
	if err != nil {
		return nil, errors.New("push: invalid receiver base path")
	}
	return u, nil
}

func sendSessionPush(ctx context.Context, base, token string, payload []byte, id, agent string) (sessionPushResult, error) {
	var result sessionPushResult
	u, err := sessionPushURL(base)
	if err != nil {
		return result, err
	}
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	requestBase := strings.TrimSuffix(u.String(), "/api/v1/sessions/ingest")
	resp, err := apiclient.RawRequest(requestBase, client, func(api *apiclient.Client) error {
		_, err := api.PostAPIV1SessionsIngestWithResponse(ctx, &apiclient.PostAPIV1SessionsIngestRequestOptions{})
		return err
	}, func(_ context.Context, req *http.Request) error {
		// Keep the validated closure's exact bytes, including numeric precision.
		// The generated operation owns the route and method; RawRequest keeps
		// response handling bounded and applies the existing signing transport.
		req.Body = io.NopCloser(bytes.NewReader(payload))
		req.ContentLength = int64(len(payload))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload)), nil }
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		return nil
	})
	if err != nil {
		return result, errors.New("push: cannot contact receiver")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		reason := "receiver rejected the session"
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			reason = "receiver redirect refused"
		case http.StatusUnauthorized:
			reason = "receiver rejected the bearer token"
		case http.StatusForbidden, http.StatusNotFound:
			reason = "receiver is unavailable or does not accept session pushes"
		case http.StatusConflict:
			reason = "session conflicts with receiver's accepted snapshot or maintenance is busy"
		case http.StatusRequestEntityTooLarge:
			reason = "session exceeds receiver size limit"
		case http.StatusUnsupportedMediaType:
			reason = "receiver rejected the transfer encoding"
		case http.StatusNotImplemented:
			reason = "receiver does not support session pushes"
		case http.StatusServiceUnavailable:
			reason = "receiver is temporarily unavailable"
		}
		return result, fmt.Errorf("push: %s (HTTP %d)", reason, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(data) > 4096 {
		return result, errors.New("push: invalid receiver confirmation")
	}
	if err := json.Unmarshal(data, &result); err != nil || result.SessionID != id || result.Agent != agent || (result.Status != "created" && result.Status != "unchanged") {
		return sessionPushResult{}, errors.New("push: invalid receiver confirmation")
	}
	return result, nil
}
