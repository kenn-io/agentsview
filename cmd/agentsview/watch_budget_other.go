//go:build !linux

package main

// Windows and kqueue use one handle per watched directory; macOS FSEvents ignores this budget.
const recursiveWatchBudget = 8192
