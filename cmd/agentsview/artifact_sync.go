package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

var runArtifactSyncCLI = artifact.Sync

const daemonArtifactExchangeResponseLimit = 1 << 20

var daemonArtifactExchangeHTTPClient = newDaemonArtifactExchangeHTTPClient()

func newDaemonArtifactExchangeHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	dialer := &net.Dialer{}
	transport.DialContext = func(
		ctx context.Context,
		network string,
		address string,
	) (net.Conn, error) {
		return dialLoopbackDaemon(ctx, dialer, network, address)
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(
			*http.Request,
			[]*http.Request,
		) error {
			return http.ErrUseLastResponse
		},
	}
}

func dialLoopbackDaemon(
	ctx context.Context,
	dialer *net.Dialer,
	network string,
	address string,
) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !strings.EqualFold(host, "localhost") {
		return dialer.DialContext(ctx, network, address)
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, errors.New("localhost did not resolve to a loopback address")
	}
	for _, candidate := range addresses {
		if !candidate.IP.IsLoopback() {
			return nil, errors.New("localhost resolved to a non-loopback address")
		}
	}
	var dialErr error
	for _, candidate := range addresses {
		connection, candidateErr := dialer.DialContext(
			ctx,
			network,
			net.JoinHostPort(candidate.String(), port),
		)
		if candidateErr == nil {
			return connection, nil
		}
		dialErr = errors.Join(dialErr, candidateErr)
	}
	return nil, dialErr
}

func validateArtifactSyncConfig(cfg SyncConfig) error {
	if cfg.Target != "" && cfg.Host != "" {
		return fmt.Errorf("--target cannot be combined with --host")
	}
	return nil
}

func runArtifactFolderSync(
	ctx context.Context,
	appCfg config.Config,
	database *db.DB,
	cfg SyncConfig,
) (artifact.SyncResult, error) {
	result, err := runArtifactSyncCLI(ctx, database, artifact.SyncOptions{
		DataDir:        appCfg.DataDir,
		Target:         cfg.Target,
		ForbiddenRoots: artifactSyncForbiddenRoots(appCfg),
		Full:           cfg.Full,
	})
	if err != nil {
		return result, &artifactFolderSyncError{cause: err}
	}
	return result, nil
}

func newDaemonArtifactExchangeRunner(
	appCfg config.Config,
	database *db.DB,
	engine *agentsync.Engine,
	emitter agentsync.Emitter,
) server.ArtifactExchangeRunner {
	return func(
		ctx context.Context,
		request server.ArtifactExchangeRequest,
	) (result artifact.SyncResult, err error) {
		work := func() error {
			result, err = runArtifactFolderSync(
				ctx,
				appCfg,
				database,
				SyncConfig{Target: request.Target, Full: request.Full},
			)
			return err
		}
		if engine == nil {
			err = work()
		} else {
			err = engine.RunExclusiveFlushed(work)
		}
		if result.ImportedSessions > 0 && emitter != nil {
			emitter.Emit("sessions")
		}
		return result, err
	}
}

func runDaemonArtifactExchange(
	ctx context.Context,
	tr transport,
	authToken string,
	target string,
	full bool,
) (artifact.SyncResult, error) {
	target, err := filepath.Abs(target)
	if err != nil {
		return artifact.SyncResult{}, &daemonArtifactExchangeError{cause: err}
	}
	baseURL, err := validatedLoopbackDaemonURL(tr.URL)
	if err != nil {
		return artifact.SyncResult{}, &daemonArtifactExchangeError{cause: err}
	}
	body, err := json.Marshal(server.ArtifactExchangeRequest{
		Target: target,
		Full:   full,
	})
	if err != nil {
		return artifact.SyncResult{}, &daemonArtifactExchangeError{cause: err}
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		baseURL+"/api/v1/artifacts/exchange",
		strings.NewReader(string(body)),
	)
	if err != nil {
		return artifact.SyncResult{}, &daemonArtifactExchangeError{cause: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", baseURL)
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	response, err := daemonArtifactExchangeHTTPClient.Do(req)
	if err != nil {
		return artifact.SyncResult{}, &daemonArtifactExchangeError{cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return artifact.SyncResult{}, &daemonArtifactExchangeError{
			cause: fmt.Errorf("daemon returned HTTP %d", response.StatusCode),
		}
	}

	var result artifact.SyncResult
	if err := json.UnmarshalRead(
		io.LimitReader(response.Body, daemonArtifactExchangeResponseLimit+1),
		&result,
		json.RejectUnknownMembers(true),
	); err != nil {
		return artifact.SyncResult{}, &daemonArtifactExchangeError{cause: err}
	}
	return result, nil
}

func validatedLoopbackDaemonURL(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "http" ||
		parsed.User != nil ||
		parsed.Host == "" ||
		(parsed.Path != "" && parsed.Path != "/") ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return "", errors.New("unsafe daemon endpoint")
	}
	hostname := parsed.Hostname()
	ip := net.ParseIP(hostname)
	if !strings.EqualFold(hostname, "localhost") &&
		(ip == nil || !ip.IsLoopback()) {
		return "", errors.New("daemon endpoint is not loopback")
	}
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

func runLocalAndArtifactFolderSync(
	ctx context.Context,
	appCfg config.Config,
	database *db.DB,
	cfg SyncConfig,
) (artifact.SyncResult, error) {
	if _, _, err := runLocalSyncResult(
		ctx,
		appCfg,
		database,
		cfg.Full,
	); err != nil {
		return artifact.SyncResult{}, err
	}
	return runArtifactFolderSync(ctx, appCfg, database, cfg)
}

func artifactSyncForbiddenRoots(appCfg config.Config) []string {
	roots := make([]string, 0, 1+len(appCfg.AgentDirs))
	seen := make(map[string]struct{}, 1+len(appCfg.AgentDirs))
	appendRoot := func(root string) {
		if strings.TrimSpace(root) == "" {
			return
		}
		if isRemoteSourceRoot(root) {
			return
		}
		root = filepath.Clean(root)
		if _, ok := seen[root]; ok {
			return
		}
		seen[root] = struct{}{}
		roots = append(roots, root)
	}
	appendRoot(appCfg.DataDir)
	for _, def := range parser.Registry {
		for _, root := range appCfg.AgentDirs[def.Type] {
			appendRoot(root)
		}
	}
	return roots
}

type artifactFolderSyncError struct {
	cause error
}

type daemonArtifactExchangeError struct {
	cause error
}

func (e *daemonArtifactExchangeError) Error() string {
	return "daemon artifact exchange failed"
}

func (e *daemonArtifactExchangeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *artifactFolderSyncError) Error() string {
	return "artifact folder sync failed"
}

func (e *artifactFolderSyncError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func printArtifactSyncSummary(w io.Writer, result artifact.SyncResult) {
	fmt.Fprintf(
		w,
		"Artifacts: exported %s; imported %s and %s; received %s; published %s",
		artifactSyncCount(result.ExportedSessions, "session"),
		artifactSyncCount(result.ImportedSessions, "session"),
		artifactSyncCount(result.ImportedMessages, "message"),
		artifactSyncCount(result.ReceivedArtifacts, "object"),
		artifactSyncCount(result.PublishedArtifacts, "object"),
	)
	if result.RejectedSessions > 0 {
		fmt.Fprintf(
			w,
			"; rejected %s",
			artifactSyncCount(result.RejectedSessions, "session"),
		)
	}
	if result.Quarantined > 0 {
		fmt.Fprintf(
			w,
			"; quarantined %s",
			artifactSyncCount(result.Quarantined, "object"),
		)
	}
	fmt.Fprintln(w)
	if result.More {
		fmt.Fprintln(
			w,
			"Artifact work remains; run the sync command again.",
		)
	}
}

func artifactSyncCount(count int, noun string) string {
	if count == 1 {
		return fmt.Sprintf("%d %s", count, noun)
	}
	return fmt.Sprintf("%d %ss", count, noun)
}
