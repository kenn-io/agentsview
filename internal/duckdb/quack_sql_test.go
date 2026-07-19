//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
)

func TestDuckValueLiteralFormatsTimestampWithoutZone(t *testing.T) {
	got, err := duckValueLiteral(time.Date(
		2026, time.January, 10, 3, 4, 5, 123456789, time.UTC,
	))
	require.NoError(t, err)

	assert.Equal(t, "TIMESTAMP '2026-01-10 03:04:05.123456'", got)
}

func TestDuckSQLWithArgsExecutesQuotedMultilineString(t *testing.T) {
	ctx := context.Background()
	duck := openTestDuckDB(t)
	want := "first line\nquoted ' value\ncontains $$ delimiter text"

	stmt, err := duckSQLWithArgs(`SELECT ?`, want)
	require.NoError(t, err)

	var got string
	require.NoError(t, duck.QueryRowContext(ctx, stmt).Scan(&got))
	assert.Equal(t, want, got)
}

func TestDuckSQLWithArgsStripsNULFromStringLiteral(t *testing.T) {
	ctx := context.Background()
	duck := openTestDuckDB(t)

	stmt, err := duckSQLWithArgs(`SELECT ?`, "before\x00after")
	require.NoError(t, err)

	var got string
	require.NoError(t, duck.QueryRowContext(ctx, stmt).Scan(&got))
	assert.Equal(t, "beforeafter", got)
}

func TestDuckSQLWithArgsExecutesStringPointer(t *testing.T) {
	ctx := context.Background()
	duck := openTestDuckDB(t)
	want := "pinned note\nquoted ' value"

	stmt, err := duckSQLWithArgs(`SELECT ?`, &want)
	require.NoError(t, err)

	var got string
	require.NoError(t, duck.QueryRowContext(ctx, stmt).Scan(&got))
	assert.Equal(t, want, got)
}

func TestDuckValueLiteralFormatsNullableNumericPointers(t *testing.T) {
	score := 88
	fileSize := int64(4096)
	contextPressure := 0.875

	tests := []struct {
		name string
		in   any
		want string
	}{
		{name: "int pointer", in: &score, want: "88"},
		{name: "int64 pointer", in: &fileSize, want: "4096"},
		{name: "float64 pointer", in: &contextPressure, want: "0.875"},
		{name: "nil int pointer", in: (*int)(nil), want: "NULL"},
		{name: "nil int64 pointer", in: (*int64)(nil), want: "NULL"},
		{name: "nil float64 pointer", in: (*float64)(nil), want: "NULL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := duckValueLiteral(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDuckSQLWithArgsExecutesNamedStringKinds(t *testing.T) {
	ctx := context.Background()
	duck := openTestDuckDB(t)

	stmt, err := duckSQLWithArgs(
		`SELECT ?, ?, ?`,
		export.WorktreeLinked,
		export.CheckoutBranch,
		export.ProjectResolutionAmbiguous,
	)
	require.NoError(t, err)

	var relationship, checkout, resolution string
	require.NoError(t, duck.QueryRowContext(ctx, stmt).
		Scan(&relationship, &checkout, &resolution))
	assert.Equal(t, string(export.WorktreeLinked), relationship)
	assert.Equal(t, string(export.CheckoutBranch), checkout)
	assert.Equal(t, string(export.ProjectResolutionAmbiguous), resolution)
}

func TestDuckValueLiteralFormatsNamedScalarKinds(t *testing.T) {
	type namedInt int32
	type namedUint uint16
	type namedFloat float64
	type namedBool bool

	tests := []struct {
		name string
		in   any
		want string
	}{
		{name: "named int", in: namedInt(-7), want: "-7"},
		{name: "named uint", in: namedUint(42), want: "42"},
		{name: "named float", in: namedFloat(0.875), want: "0.875"},
		{name: "named bool true", in: namedBool(true), want: "TRUE"},
		{name: "named bool false", in: namedBool(false), want: "FALSE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := duckValueLiteral(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
