package duckdb

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateQuackClientURL(t *testing.T) {
	tests := []struct {
		name          string
		url           string
		token         string
		allowInsecure bool
		wantErr       string
	}{
		{
			name:  "loopback http allowed",
			url:   "quack:http://127.0.0.1:9494",
			token: "secret",
		},
		{
			name:  "native loopback hostport allowed",
			url:   "quack:127.0.0.1:9494",
			token: "secret",
		},
		{
			name:  "native loopback slashes allowed",
			url:   "quack://127.0.0.1:9494",
			token: "secret",
		},
		{
			name:  "https remote allowed",
			url:   "quack:https://duck.example.com",
			token: "secret",
		},
		{
			name:          "explicit insecure remote allowed",
			url:           "quack:http://duck.example.com",
			token:         "secret",
			allowInsecure: true,
		},
		{
			name:    "native remote rejected",
			url:     "quack:duck.example.com:9494",
			token:   "secret",
			wantErr: "loopback",
		},
		{
			name:          "native remote explicitly allowed",
			url:           "quack:duck.example.com:9494",
			token:         "secret",
			allowInsecure: true,
		},
		{
			name:    "token required",
			url:     "quack:http://127.0.0.1:9494",
			wantErr: "token is required",
		},
		{
			name:    "plain remote rejected",
			url:     "quack:http://duck.example.com",
			token:   "secret",
			wantErr: "plain HTTP",
		},
		{
			name:    "quack scheme required",
			url:     "http://127.0.0.1:9494",
			token:   "secret",
			wantErr: "quack",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateQuackClientURL(tt.url, tt.token, tt.allowInsecure)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestRedactQuackURL(t *testing.T) {
	got := RedactQuackURL("quack:https://duck.example.com/db?token=secret&x=1")
	assert.NotContains(t, got, "secret")
	assert.Contains(t, got, "token=%3Credacted%3E")
	assert.Contains(t, got, "x=1")
}

func TestValidateQuackServeURI(t *testing.T) {
	tests := []struct {
		name      string
		uri       string
		allow     bool
		wantError string
	}{
		{name: "localhost default port", uri: "quack:localhost"},
		{name: "loopback hostport", uri: "quack:127.0.0.1:9494"},
		{name: "loopback slashes", uri: "quack://127.0.0.1:9494"},
		{name: "loopback ipv6", uri: "quack:[::1]:9494"},
		{name: "external denied", uri: "quack:0.0.0.0:9494", wantError: "loopback"},
		{name: "external allowed", uri: "quack:0.0.0.0:9494", allow: true},
		{name: "scheme required", uri: "http://127.0.0.1:9494", wantError: "quack"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateQuackServeURI(tt.uri, tt.allow)
			if tt.wantError == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantError)
		})
	}
}
