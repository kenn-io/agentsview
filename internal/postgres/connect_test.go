package postgres

import "testing"

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
			if (err != nil) != tt.wantErr {
				t.Errorf(
					"CheckSSL() error = %v, wantErr %v",
					err, tt.wantErr,
				)
			}
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
			got := RedactDSN(tt.dsn)
			if got != tt.want {
				t.Errorf(
					"RedactDSN() = %q, want %q",
					got, tt.want,
				)
			}
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
			if got := isLoopback(tt.host); got != tt.want {
				t.Errorf(
					"isLoopback(%q) = %v, want %v",
					tt.host, got, tt.want,
				)
			}
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
			if (err != nil) != tt.wantErr {
				t.Errorf(
					"quoteIdentifier() err = %v, wantErr %v",
					err, tt.wantErr,
				)
			}
			if got != tt.want {
				t.Errorf(
					"quoteIdentifier() = %q, want %q",
					got, tt.want,
				)
			}
		})
	}
}
