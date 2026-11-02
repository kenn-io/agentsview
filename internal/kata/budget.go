package kata

import "time"

// ProbeBudget covers local discovery, three sequential requests, and the
// caller's response allowance. A non-positive request timeout uses the default.
// A negative allowance is treated as zero.
func ProbeBudget(perRequest, allowance time.Duration) time.Duration {
	if perRequest <= 0 {
		perRequest = DefaultTimeout
	}
	if allowance < 0 {
		allowance = 0
	}
	const maxDuration = time.Duration(1<<63 - 1)
	if perRequest > (maxDuration-allowance)/3 {
		return maxDuration
	}
	return allowance + 3*perRequest
}
