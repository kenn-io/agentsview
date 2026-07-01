package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// sessionListIDs decodes `session list --format json` output into ordered IDs.
func sessionListIDs(t *testing.T, out string) []string {
	t.Helper()
	var got struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	ids := make([]string, len(got.Sessions))
	for i, s := range got.Sessions {
		ids[i] = s.ID
	}
	return ids
}
