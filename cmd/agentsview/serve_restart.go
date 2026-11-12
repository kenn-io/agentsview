package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"syscall"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/server"
)

func runServeLoop(ctx context.Context, cfg config.Config, opts serveOptions, port int, run func(context.Context, config.Config, serveOptions, int) error) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	exe := os.Args[0]
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := run(ctx, cfg, opts, port)
		restart, ok := errors.AsType[*foregroundRestartError](err)
		if !ok {
			return err
		}
		if release != nil {
			release()
		}
		release = restart.release
		if ctx.Err() != nil {
			return nil
		}
		os.Args = append([]string{exe}, restart.args...)
		cfg, opts, port = restart.cfg, restart.opts, restart.port
	}
}

// checkRestartAddress refuses a Settings restart when the replacement must
// bind an explicit port on a different address and that port is taken. The
// current server stops before the replacement binds, so a busy address would
// leave no server running. An address that overlaps the current listener
// cannot be probed while that listener is open, so those restarts go ahead
// unchecked.
func checkRestartAddress(ctx context.Context, current, next config.Config) error {
	if !next.PortExplicit || next.Port == 0 || !restartAddressesDisjoint(ctx, current.Host, next.Host) {
		return nil
	}
	port, err := server.FindAvailablePort(ctx, next.Host, next.Port)
	if err == nil && port == next.Port {
		return nil
	}
	return fmt.Errorf(
		"port %d on %s is unavailable, so AgentsView would stop without a replacement; free the port or change the host, then restart",
		next.Port, next.Host,
	)
}

// restartAddressesDisjoint reports whether no address of next is one the
// current listener holds. A name that does not resolve counts as disjoint so
// the port probe reports it.
func restartAddressesDisjoint(ctx context.Context, current, next string) bool {
	if current == next || current == "" || next == "" {
		return false
	}
	currentAddrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", current)
	if err != nil {
		return true
	}
	nextAddrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", next)
	if err != nil {
		return true
	}
	held := make(map[netip.Addr]bool, len(currentAddrs))
	for _, addr := range currentAddrs {
		if addr.Unmap().IsUnspecified() {
			return false
		}
		held[addr.Unmap()] = true
	}
	for _, addr := range nextAddrs {
		if addr.Unmap().IsUnspecified() || held[addr.Unmap()] {
			return false
		}
	}
	return true
}
