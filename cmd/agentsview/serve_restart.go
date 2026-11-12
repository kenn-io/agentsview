package main

import (
	"context"
	"errors"
	"os"

	"go.kenn.io/agentsview/internal/config"
)

func runServeLoop(ctx context.Context, cfg config.Config, opts serveOptions, port int, run func(context.Context, config.Config, serveOptions, int) error) error {
	exe := os.Args[0]
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	for {
		err := run(ctx, cfg, opts, port)
		restart, ok := errors.AsType[*foregroundRestartError](err)
		if !ok {
			return err
		}
		if release != nil {
			release()
		}
		release = restart.release
		os.Args = append([]string{exe}, restart.args...)
		cfg, opts, port = restart.cfg, restart.opts, restart.port
	}
}
