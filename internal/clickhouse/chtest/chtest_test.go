package chtest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContainerRequestCarriesLease(t *testing.T) {
	t.Setenv("TEST_SERVICE_LEASE", "agentsview-test-123")

	req := containerRequest(nil)
	require.Equal(t, "agentsview-test-123", req.Labels["test-service.lease"])
}
