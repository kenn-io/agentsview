//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"

	"go.kenn.io/kit/daemon"
)

const replicaWatchLifecycleTarget = "target"

func installReplicaWatchLifecycle(
	dataDir, backend, target string,
) (<-chan struct{}, func(), error) {
	store := daemon.RuntimeStore{
		Dir: dataDir, Prefix: backend + "-watch",
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	wake := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-signals:
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}
	}()

	record := daemon.NewRuntimeRecord(
		"agentsview-"+backend+"-watch",
		version,
		daemon.Endpoint{
			Network: "signal", Address: strconv.Itoa(os.Getpid()),
		},
	)
	record.Metadata = map[string]string{
		replicaWatchLifecycleTarget: target,
	}
	path, err := store.Write(record)
	if err != nil {
		signal.Stop(signals)
		close(done)
		return nil, nil, fmt.Errorf("publish lifecycle runtime: %w", err)
	}

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			signal.Stop(signals)
			close(done)
			_ = os.Remove(path)
		})
	}
	return wake, cleanup, nil
}

func notifyReplicaWatchLifecycle(
	ctx context.Context, dataDir, backend, target string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store := daemon.RuntimeStore{
		Dir: dataDir, Prefix: backend + "-watch",
	}
	records, err := store.List()
	if err != nil {
		return fmt.Errorf("discover %s watch owner: %w", backend, err)
	}
	service := "agentsview-" + backend + "-watch"
	for _, record := range records {
		if record.Service != service ||
			record.Metadata[replicaWatchLifecycleTarget] != target {
			continue
		}
		if daemon.CompareRuntimeProcessIdentity(record) != daemon.ProcessIdentityMatch {
			continue
		}
		process, err := os.FindProcess(record.PID)
		if err != nil {
			return fmt.Errorf("find %s watch owner: %w", backend, err)
		}
		if err := process.Signal(syscall.SIGUSR1); err != nil {
			return fmt.Errorf("notify %s watch owner: %w", backend, err)
		}
		return nil
	}
	return errors.New(
		backend + " push --watch owner is not running for the selected target; " +
			"start or restart its configured background service",
	)
}
