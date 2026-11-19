//go:build linux

package main

import "math"

// Inotify watches use no file descriptor; AddRecursive polls at the kernel's max_user_watches limit.
const recursiveWatchBudget = math.MaxInt
