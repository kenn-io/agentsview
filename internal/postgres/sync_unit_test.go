package postgres

import (
	"errors"
	"testing"
)

func TestIsUndefinedTable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{
			"unrelated error",
			errors.New("connection refused"),
			false,
		},
		{
			"generic does not exist",
			errors.New(
				`column "foo" does not exist`,
			),
			false,
		},
		{
			"SQLSTATE 42P01",
			errors.New(
				`ERROR: relation "sessions" ` +
					`does not exist (SQLSTATE 42P01)`,
			),
			true,
		},
		{
			"bare SQLSTATE",
			errors.New("42P01"),
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isUndefinedTable(tt.err)
			if got != tt.want {
				t.Errorf(
					"isUndefinedTable(%v) = %v, want %v",
					tt.err, got, tt.want,
				)
			}
		})
	}
}
