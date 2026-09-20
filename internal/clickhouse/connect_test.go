package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckTransportSecurity(t *testing.T) {
	tests := []struct {
		name          string
		dsn           string
		allowInsecure bool
		wantErr       string
	}{
		{name: "loopback native plain", dsn: "clickhouse://localhost:9000/agentsview"},
		{name: "loopback ip plain", dsn: "clickhouse://127.0.0.1:9000/agentsview"},
		{name: "loopback http plain", dsn: "http://localhost:8123/agentsview"},
		{
			name:    "remote native plain",
			dsn:     "clickhouse://user:pw@ch.example.internal:9000/agentsview",
			wantErr: "add secure=true",
		},
		{name: "remote native tls", dsn: "clickhouse://ch.example.internal:9440/agentsview?secure=true"},
		{
			name:    "remote native skip_verify",
			dsn:     "clickhouse://user:pw@ch.example.internal:9440/agentsview?secure=true&skip_verify=true",
			wantErr: "skip_verify",
		},
		{
			name:    "remote https skip_verify",
			dsn:     "https://user:pw@ch.example.internal:8443/agentsview?skip_verify=true",
			wantErr: "skip_verify",
		},
		{
			name: "loopback skip_verify",
			dsn:  "clickhouse://localhost:9440/agentsview?secure=true&skip_verify=true",
		},
		{
			name:          "remote skip_verify allowed",
			dsn:           "clickhouse://ch.example.internal:9440/agentsview?secure=true&skip_verify=true",
			allowInsecure: true,
		},
		{
			name:    "remote http plain",
			dsn:     "http://ch.example.internal:8123/agentsview",
			wantErr: "use an https:// url",
		},
		{name: "remote https", dsn: "https://ch.example.internal:8443/agentsview"},
		{
			name:          "remote plain allowed",
			dsn:           "clickhouse://ch.example.internal:9000/agentsview",
			allowInsecure: true,
		},
		{name: "invalid url", dsn: "clickhouse://a:b:c", wantErr: "parsing clickhouse url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckTransportSecurity(tt.dsn, tt.allowInsecure)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.NotContains(t, err.Error(), "pw", "credentials must not leak")
		})
	}
}

func TestTargetDatabaseName(t *testing.T) {
	tests := []struct {
		name    string
		target  Target
		want    string
		wantErr bool
	}{
		{name: "explicit", target: Target{URL: "clickhouse://h:9000/other", Database: "mirror_a"}, want: "mirror_a"},
		{name: "dsn path", target: Target{URL: "clickhouse://h:9000/other"}, want: "other"},
		{name: "default", target: Target{URL: "clickhouse://h:9000"}, want: DefaultDatabase},
		{name: "invalid", target: Target{URL: "clickhouse://h:9000", Database: "a-b"}, wantErr: true},
		{name: "invalid path", target: Target{URL: "clickhouse://h:9000/a%20b"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.target.DatabaseName()
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRedactDSNAndFingerprint(t *testing.T) {
	dsn := "clickhouse://alice:s3cret@ch.example.internal:9440/agentsview?secure=true"
	assert.Equal(t, "ch.example.internal:9440", RedactDSN(dsn))

	fp1, err := TargetFingerprint(Target{URL: dsn})
	require.NoError(t, err)
	fp2, err := TargetFingerprint(Target{URL: "clickhouse://alice:other@CH.EXAMPLE.INTERNAL:9440/agentsview?secure=true"})
	require.NoError(t, err)
	assert.Equal(t, fp1, fp2, "password and host case do not change the target identity")
	fp3, err := TargetFingerprint(Target{URL: dsn, Database: "mirror_b"})
	require.NoError(t, err)
	assert.NotEqual(t, fp1, fp3)
	assert.NotContains(t, fp1, "s3cret")
}
