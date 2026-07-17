package postgres

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckCodexEncryptedPayloadCompatToleratesUnavailablePushMetadata(
	t *testing.T,
) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "missing table",
			err:  errors.New(`relation "sync_metadata" does not exist (SQLSTATE 42P01)`),
		},
		{
			name: "legacy columns",
			err:  errors.New(`column "key" does not exist (SQLSTATE 42703)`),
		},
		{
			name: "read-only role lacks permission",
			err:  errors.New(`permission denied for table sync_metadata (SQLSTATE 42501)`),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pg, state := newSchemaProbeDB(t, nil)
			state.codexGuardCount = 4
			state.queryErrors = []schemaProbeQueryError{{
				contains: "from sync_metadata where key",
				err:      tt.err,
			}}

			require.NoError(t,
				CheckCodexEncryptedPayloadCompat(t.Context(), pg),
				"clean read-only data must not depend on push metadata",
			)
		})
	}
}
