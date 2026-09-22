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
	// Persist this process's OS create time alongside the kit process
	// identity so notify can reject a PID whose owning process was reused
	// even when the kit identity cannot be read. Same best-effort contract
	// as the daemon runtime record: a missing create time disables only
	// this extra check.
	if ct, ok := processCreateTimeMillis(os.Getpid()); ok {
		record.Metadata[runtimeCreateTime] = strconv.FormatInt(ct, 10)
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
	var lastSignalErr error
	for _, record := range records {
		if record.Service != service ||
			record.Metadata[replicaWatchLifecycleTarget] != target {
			continue
		}
		// Skip records for processes that are already gone: a crashed
		// watcher's stale record must not stop the search for a live owner
		// listed after it.
		if !daemon.ProcessAlive(record.PID) {
			continue
		}
		// Require a positive identity match before signaling: kit identity
		// or the recorded create time must agree with the live process. A
		// record whose identity cannot be verified at all (both unknown) is
		// treated as unavailable rather than risking a signal to a reused
		// PID, and a positive mismatch on either signal is always rejected.
		kitIdentity := daemon.CompareRuntimeProcessIdentity(record)
		if kitIdentity == daemon.ProcessIdentityMismatch {
			continue
		}
		createTime := processCreateTimeStateForPID(
			record.PID, record.Metadata[runtimeCreateTime],
		)
		if createTime == processCreateTimeMismatch {
			continue
		}
		if kitIdentity != daemon.ProcessIdentityMatch &&
			createTime != processCreateTimeMatch {
			continue
		}
		process, err := os.FindProcess(record.PID)
		if err != nil {
			lastSignalErr = fmt.Errorf("find %s watch owner: %w", backend, err)
			continue
		}
		// A signal can still fail for an owner that died between the liveness
		// and identity checks; keep looking at the remaining records before
		// giving up.
		if err := process.Signal(syscall.SIGUSR1); err != nil {
			lastSignalErr = fmt.Errorf("notify %s watch owner: %w", backend, err)
			continue
		}
		return nil
	}
	if lastSignalErr != nil {
		return lastSignalErr
	}
	return errors.New(
		backend + " push --watch owner is not running for the selected target; " +
			"start or restart its configured background service",
	)
}
