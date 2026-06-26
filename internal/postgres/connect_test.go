package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCheckSSL(t *testing.T) {
	tests := []struct {
		name    string
		dsn     string
		wantErr bool
	}{
		{
			"loopback localhost",
			"postgres://user:pass@localhost:5432/db",
			false,
		},
		{
			"loopback 127.0.0.1",
			"postgres://user:pass@127.0.0.1:5432/db",
			false,
		},
		{
			"loopback ::1",
			"postgres://user:pass@[::1]:5432/db",
			false,
		},
		{
			"empty host defaults local",
			"",
			false,
		},
		{
			"remote with require",
			"postgres://u:p@remote:5432/db?sslmode=require",
			false,
		},
		{
			"remote with verify-full",
			"postgres://u:p@remote:5432/db?sslmode=verify-full",
			false,
		},
		{
			"remote no sslmode",
			"postgres://u:p@remote:5432/db",
			true,
		},
		{
			"remote sslmode=disable",
			"postgres://u:p@remote:5432/db?sslmode=disable",
			true,
		},
		{
			"remote sslmode=prefer",
			"postgres://u:p@remote:5432/db?sslmode=prefer",
			true,
		},
		{
			"remote sslmode=allow",
			"postgres://u:p@remote:5432/db?sslmode=allow",
			true,
		},
		{
			"kv remote require",
			"host=remote sslmode=require",
			false,
		},
		{
			"kv remote disable",
			"host=remote sslmode=disable",
			true,
		},
		{
			"kv unix socket",
			"host=/var/run/postgresql sslmode=disable",
			false,
		},
		{
			"uri query host disable",
			"postgres:///db?host=remote&sslmode=disable",
			true,
		},
		{
			"uri query host require",
			"postgres:///db?host=remote&sslmode=require",
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckSSL(tt.dsn)
			assert.Equal(t, tt.wantErr, err != nil, "err = %v", err)
		})
	}
}

func TestRedactDSN(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{
			"strips credentials",
			"postgres://user:secret@myhost:5432/db",
			"myhost",
		},
		{
			"empty dsn",
			"",
			"",
		},
		{
			"invalid dsn",
			"://bad",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, RedactDSN(tt.dsn))
		})
	}
}

func TestIsLoopback(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"", true},
		{"localhost", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"/var/run/postgresql", true},
		{"remote.host.com", false},
		{"10.0.0.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			assert.Equal(t, tt.want, isLoopback(tt.host))
		})
	}
}

func TestQuoteIdentifier(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"simple", "agentsview", `"agentsview"`, false},
		{"underscore", "my_schema", `"my_schema"`, false},
		{"empty", "", "", true},
		{"has spaces", "bad schema", "", true},
		{"has semicolon", "bad;drop", "", true},
		{"starts with digit", "1bad", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := quoteIdentifier(tt.input)
			assert.Equal(t, tt.wantErr, err != nil, "err = %v", err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPGTargetFingerprint(t *testing.T) {
	base, err := pgTargetFingerprint(
		"postgres://alice:secret@db.example.com:5432/agents?sslmode=require&application_name=agentsview",
		"agentsview",
	)
	assert.NoError(t, err)

	samePasswordChanged, err := pgTargetFingerprint(
		"postgres://alice:new-secret@db.example.com:5432/agents?sslmode=require&application_name=other",
		"agentsview",
	)
	assert.NoError(t, err)
	assert.Equal(t, base, samePasswordChanged)

	baseWithFallback, err := pgTargetFingerprint(
		"postgres://alice:secret@db.example.com:5432/agents?sslmode=require&application_name=agentsview&host=db.example.com,standby-a.example.com&port=5432,6432",
		"agentsview",
	)
	assert.NoError(t, err)

	sameFallbackNoiseChanged, err := pgTargetFingerprint(
		"postgres://alice:new-secret@db.example.com:5432/agents?sslmode=require&application_name=other&host=DB.EXAMPLE.COM,standby-a.example.com&port=5432,6432",
		"agentsview",
	)
	assert.NoError(t, err)
	assert.Equal(t, baseWithFallback, sameFallbackNoiseChanged)

	cases := []struct {
		name   string
		dsn    string
		schema string
	}{
		{
			name:   "host change",
			dsn:    "postgres://alice:secret@db2.example.com:5432/agents?sslmode=require",
			schema: "agentsview",
		},
		{
			name:   "database change",
			dsn:    "postgres://alice:secret@db.example.com:5432/agents_archive?sslmode=require",
			schema: "agentsview",
		},
		{
			name:   "user change",
			dsn:    "postgres://bob:secret@db.example.com:5432/agents?sslmode=require",
			schema: "agentsview",
		},
		{
			name:   "schema change",
			dsn:    "postgres://alice:secret@db.example.com:5432/agents?sslmode=require",
			schema: "agentsview_alt",
		},
		{
			name:   "fallback host change",
			dsn:    "postgres://alice:secret@db.example.com:5432/agents?sslmode=require&host=db.example.com,standby-b.example.com&port=5432,6432",
			schema: "agentsview",
		},
		{
			name:   "fallback port change",
			dsn:    "postgres://alice:secret@db.example.com:5432/agents?sslmode=require&host=db.example.com,standby-a.example.com&port=5432,7432",
			schema: "agentsview",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pgTargetFingerprint(tc.dsn, tc.schema)
			assert.NoError(t, err)
			wantDifferentFrom := base
			if tc.name == "fallback host change" || tc.name == "fallback port change" {
				wantDifferentFrom = baseWithFallback
			}
			assert.NotEqual(t, wantDifferentFrom, got)
		})
	}
}
