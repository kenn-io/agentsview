//go:build windows

package main

import (
	"context"
	"errors"
)

func installReplicaWatchLifecycle(
	_, _, _ string,
) (<-chan struct{}, func(), error) {
	return nil, func() {}, nil
}

func notifyReplicaWatchLifecycle(
	context.Context, string, string, string,
) error {
	return errors.New(
		"hosted contributor lifecycle notification is unavailable on Windows",
	)
}
