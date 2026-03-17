package pgutil

import "testing"

func TestCheckSSL(t *testing.T) {
	tests := []struct {
		name    string
		dsn     string
		wantErr bool
	}{
		{"loopback localhost", "postgres://user:pass@localhost:5432/db", false},
		{"loopback 127.0.0.1", "postgres://user:pass@127.0.0.1:5432/db", false},
		{"loopback ::1", "postgres://user:pass@[::1]:5432/db", false},
		{"empty host defaults local", "", false},
		{"remote with require", "postgres://user:pass@remote:5432/db?sslmode=require", false},
		{"remote with verify-full", "postgres://user:pass@remote:5432/db?sslmode=verify-full", false},
		// No explicit sslmode defaults to prefer, which has a plaintext fallback.
		{"remote no sslmode", "postgres://user:pass@remote:5432/db", true},
		{"remote sslmode=disable", "postgres://user:pass@remote:5432/db?sslmode=disable", true},
		// prefer and allow have plaintext fallback paths and are rejected.
		{"remote sslmode=prefer", "postgres://user:pass@remote:5432/db?sslmode=prefer", true},
		{"remote sslmode=allow", "postgres://user:pass@remote:5432/db?sslmode=allow", true},
		{"kv remote require", "host=remote sslmode=require", false},
		{"kv remote disable", "host=remote sslmode=disable", true},
		{"kv unix socket", "host=/var/run/postgresql sslmode=disable", false},
		{"uri query host disable", "postgres:///db?host=remote&sslmode=disable", true},
		{"uri query host require", "postgres:///db?host=remote&sslmode=require", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckSSL(tt.dsn)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckSSL() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
