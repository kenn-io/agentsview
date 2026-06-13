package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/config"
)

const managedCaddyStartGrace = 300 * time.Millisecond

type managedCaddy struct {
	cancel context.CancelFunc
	errCh  chan error
}

func browserURL(cfg config.Config) string {
	return browserURLWithPlatform(cfg, runningInWSL, interfaceIPv4)
}

func browserURLWithPlatform(
	cfg config.Config,
	isWSL func() bool,
	ifaceIPv4 func(string) (string, bool),
) string {
	if cfg.PublicURL != "" {
		return cfg.PublicURL
	}
	host := cfg.Host
	if host == "0.0.0.0" || host == "::" {
		if isWSL != nil && isWSL() {
			if ip, ok := ifaceIPv4("eth0"); ok {
				host = ip
			} else {
				host = "127.0.0.1"
			}
		} else {
			host = "127.0.0.1"
		}
	}
	return fmt.Sprintf("http://%s:%d", host, cfg.Port)
}

func runningInWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	if _, err := os.Stat("/proc/sys/fs/binfmt_misc/WSLInterop"); err == nil {
		return true
	}
	return false
}

func interfaceIPv4(name string) (string, bool) {
	iface, err := net.InterfaceByName(name)
	if err != nil || iface == nil {
		return "", false
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return "", false
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		default:
			continue
		}
		if ip == nil || ip.IsLoopback() {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			return v4.String(), true
		}
	}
	return "", false
}

func rewriteConfiguredPublicURLPort(
	publicURL string,
	publicOrigins []string,
	fromPort int,
	toPort int,
) (string, []string, bool, error) {
	if publicURL == "" || fromPort == toPort {
		return publicURL, publicOrigins, false, nil
	}
	u, err := url.Parse(publicURL)
	if err != nil {
		return publicURL, publicOrigins, false, err
	}
	if u == nil || u.Host == "" {
		return publicURL, publicOrigins, false, fmt.Errorf(
			"%q must include a host", publicURL,
		)
	}

	shouldRewrite := false
	if port := u.Port(); port != "" {
		explicitPort, err := strconv.Atoi(port)
		if err != nil {
			return publicURL, publicOrigins, false, err
		}
		shouldRewrite = explicitPort == fromPort
	} else {
		shouldRewrite = defaultSchemePort(u.Scheme) == fromPort
	}
	if !shouldRewrite {
		return publicURL, publicOrigins, false, nil
	}

	updatedURL := withURLPort(u, toPort)
	updatedOrigins := make([]string, 0, len(publicOrigins))
	replaced := false
	for _, origin := range publicOrigins {
		if origin == publicURL {
			updatedOrigins = append(updatedOrigins, updatedURL)
			replaced = true
			continue
		}
		updatedOrigins = append(updatedOrigins, origin)
	}
	if !replaced {
		updatedOrigins = append(updatedOrigins, updatedURL)
	}
	return updatedURL, updatedOrigins, true, nil
}

func validateServeConfig(cfg config.Config) error {
	if cfg.Proxy.Mode == "" {
		return nil
	}
	if cfg.Proxy.Mode != "caddy" {
		return fmt.Errorf("unsupported proxy mode %q", cfg.Proxy.Mode)
	}
	if cfg.PublicURL == "" {
		return fmt.Errorf("managed caddy requires public_url")
	}
	if !isLoopbackHost(cfg.Host) {
		return fmt.Errorf(
			"managed caddy requires a loopback backend host, got %q",
			cfg.Host,
		)
	}
	bindHost := cfg.Proxy.BindHost
	if strings.TrimSpace(bindHost) == "" {
		bindHost = "127.0.0.1"
	}
	if !isLoopbackHost(bindHost) &&
		len(cfg.Proxy.AllowedSubnets) == 0 {
		return fmt.Errorf(
			"managed caddy non-loopback binds require at least one allowed_subnet",
		)
	}
	if _, err := exec.LookPath(cfg.Proxy.Bin); err != nil {
		return fmt.Errorf(
			"finding caddy binary %q: %w",
			cfg.Proxy.Bin, err,
		)
	}

	u, err := url.Parse(cfg.PublicURL)
	if err != nil {
		return fmt.Errorf("parsing public url: %w", err)
	}
	if u == nil {
		return fmt.Errorf("parsing public url: invalid URL")
	}
	switch u.Scheme {
	case "https":
		if cfg.Proxy.TLSCert == "" || cfg.Proxy.TLSKey == "" {
			return fmt.Errorf(
				"managed caddy HTTPS mode requires both tls_cert and tls_key",
			)
		}
		if err := requireReadableFile(cfg.Proxy.TLSCert); err != nil {
			return fmt.Errorf("tls_cert: %w", err)
		}
		if err := requireReadableFile(cfg.Proxy.TLSKey); err != nil {
			return fmt.Errorf("tls_key: %w", err)
		}
	case "http":
		if cfg.Proxy.TLSCert != "" || cfg.Proxy.TLSKey != "" {
			return fmt.Errorf(
				"managed caddy HTTP mode must not set tls_cert or tls_key",
			)
		}
	default:
		return fmt.Errorf(
			"managed caddy requires public_url to use http or https",
		)
	}

	return nil
}

func isLoopbackHost(host string) bool {
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func requireReadableFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return f.Close()
}

func managedCaddyConfigPath(dataDir, mode string) string {
	return filepath.Join(dataDir, "managed-caddy", mode, "Caddyfile")
}

func prepareManagedCaddyConfig(
	cfg config.Config,
	mode string,
	backendAddr string,
) (path string, content string, err error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return "", "", fmt.Errorf("managed caddy mode must not be empty")
	}

	path = managedCaddyConfigPath(cfg.DataDir, mode)
	content = buildManagedCaddyfile(
		cfg.PublicURL,
		cfg.Proxy.BindHost,
		backendAddr,
		cfg.Proxy.TLSCert,
		cfg.Proxy.TLSKey,
		cfg.Proxy.AllowedSubnets,
	)
	return path, content, nil
}

func startManagedCaddy(
	parent context.Context,
	cfg config.Config,
	mode string,
) (*managedCaddy, error) {
	configPath, content, err := prepareManagedCaddyConfig(
		cfg,
		mode,
		net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
	)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return nil, fmt.Errorf("creating managed caddy dir: %w", err)
	}
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		return nil, fmt.Errorf("writing managed caddy config: %w", err)
	}

	validateCmd := exec.CommandContext(
		parent,
		cfg.Proxy.Bin,
		"validate",
		"--config", configPath,
		"--adapter", "caddyfile",
	)
	if out, err := validateCmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return nil, fmt.Errorf(
				"validating managed caddy config: %w: %s",
				err, msg,
			)
		}
		return nil, fmt.Errorf("validating managed caddy config: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(
		ctx,
		cfg.Proxy.Bin,
		"run",
		"--config", configPath,
		"--adapter", "caddyfile",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("starting managed caddy: %w", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Wait()
	}()

	select {
	case err := <-errCh:
		cancel()
		if err == nil {
			return nil, fmt.Errorf("managed caddy exited immediately")
		}
		return nil, fmt.Errorf("managed caddy exited immediately: %w", err)
	case <-time.After(managedCaddyStartGrace):
	case <-parent.Done():
		cancel()
		return nil, parent.Err()
	}

	return &managedCaddy{
		cancel: cancel,
		errCh:  errCh,
	}, nil
}

func (m *managedCaddy) Stop() {
	if m == nil || m.cancel == nil {
		return
	}
	m.cancel()
}

func (m *managedCaddy) Err() <-chan error {
	if m == nil {
		return nil
	}
	return m.errCh
}

func buildManagedCaddyfile(
	publicURL string,
	bindHost string,
	backendAddr string,
	tlsCert string,
	tlsKey string,
	allowedSubnets []string,
) string {
	var b strings.Builder
	b.WriteString("{\n")
	b.WriteString("\tadmin off\n")
	b.WriteString("\tauto_https off\n")
	b.WriteString("}\n\n")
	b.WriteString(publicURL)
	b.WriteString(" {\n")
	if bindHost != "" {
		fmt.Fprintf(&b, "\tbind %s\n", bindHost)
	}
	if len(allowedSubnets) > 0 {
		b.WriteString("\t@blocked not remote_ip")
		for _, subnet := range allowedSubnets {
			b.WriteString(" ")
			b.WriteString(subnet)
		}
		b.WriteString("\n")
		b.WriteString("\trespond @blocked \"Forbidden\" 403\n")
	}
	if tlsCert != "" || tlsKey != "" {
		fmt.Fprintf(
			&b,
			"\ttls %s %s\n",
			strconv.Quote(tlsCert),
			strconv.Quote(tlsKey),
		)
	}
	fmt.Fprintf(&b, "\treverse_proxy %s\n", backendAddr)
	b.WriteString("}\n")
	return b.String()
}

func waitForLocalPort(
	ctx context.Context,
	host string,
	port int,
	timeout time.Duration,
	errCh <-chan error,
) error {
	deadline := time.Now().Add(timeout)
	address := net.JoinHostPort(readinessProbeHost(host), strconv.Itoa(port))
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == nil {
				return fmt.Errorf(
					"service exited before becoming ready on %s",
					address,
				)
			}
			return err
		default:
		}
		conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		lastErr = err
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case err := <-errCh:
			timer.Stop()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == nil {
				return fmt.Errorf(
					"service exited before becoming ready on %s",
					address,
				)
			}
			return err
		case <-timer.C:
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out waiting for %s", address)
	}
	return lastErr
}

func readinessProbeHost(host string) string {
	switch host {
	case "", "0.0.0.0":
		return "127.0.0.1"
	case "::":
		return "::1"
	default:
		return host
	}
}

func defaultSchemePort(scheme string) int {
	if strings.EqualFold(scheme, "https") {
		return 443
	}
	return 80
}

func withURLPort(u *url.URL, port int) string {
	host := u.Hostname()
	if host == "" {
		return u.String()
	}
	scheme := strings.ToLower(u.Scheme)
	defaultPort := defaultSchemePort(scheme)
	if port == defaultPort {
		return scheme + "://" + hostLiteral(host)
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port))
}

func hostLiteral(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func publicURLPort(publicURL string) (int, error) {
	u, err := url.Parse(publicURL)
	if err != nil {
		return 0, err
	}
	if u == nil {
		return 0, fmt.Errorf("invalid public URL")
	}
	if port := u.Port(); port != "" {
		return strconv.Atoi(port)
	}
	return defaultSchemePort(u.Scheme), nil
}
