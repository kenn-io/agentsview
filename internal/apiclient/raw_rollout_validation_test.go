package apiclient

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRawRolloutWildcardValidation(t *testing.T) {
	for _, scope := range []struct{ device, provider string }{{"", ""}, {"device-a", ""}, {"", "codex"}, {"device-a", "codex"}} {
		t.Run(scope.device+"/"+scope.provider, func(t *testing.T) {
			body := fmt.Sprintf(`{"device_id":%q,"provider":%q,"state":"shadow","served":0,"shadow":0,"superseded":0,"unresolved_pins":0}`, scope.device, scope.provider)
			for _, missing := range []string{"", "device_id", "provider"} {
				var fields map[string]json.RawMessage
				require.NoError(t, json.Unmarshal([]byte(body), &fields))
				if missing != "" {
					delete(fields, missing)
				}
				encoded, err := json.Marshal(fields)
				require.NoError(t, err)
				var row RawsyncRolloutScopeStatus
				require.NoError(t, json.Unmarshal(encoded, &row))
				var status RawsyncStatus
				require.NoError(t, json.Unmarshal([]byte(`{"source_heads":[],"devices":[],"rollout":[`+string(encoded)+`],"parse_jobs":{},"uploads":{}}`), &status))
				if missing == "" {
					assert.NoError(t, row.Validate())
					assert.NoError(t, status.Validate())
				} else {
					require.Error(t, row.Validate(), "missing %s", missing)
					require.Error(t, status.Validate(), "missing %s in nested rollout", missing)
				}
			}
		})
	}
}
