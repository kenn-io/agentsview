package serdejson

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Expected bytes were captured from serde_json 1.0.149 Value serialization.
func TestCompactRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "sorted keys and unescaped HTML and separators",
			in:   `{"b":1,"a":"<>&  é"}`,
			want: "{\"a\":\"<>&  é\",\"b\":1}",
		},
		{
			name: "control escapes and DEL",
			in:   `"\u0001\u001f\u007f\"\\/\b\f\n\r\t"`,
			want: "\"\\u0001\\u001f\u007f\\\"\\\\/\\b\\f\\n\\r\\t\"",
		},
		{
			name: "nested map sort",
			in:   `{"z":{"y":1,"x":[true,false,null]}}`,
			want: `{"z":{"x":[true,false,null],"y":1}}`,
		},
		{
			name: "empty containers",
			in:   `{"k":" <&>","e":[],"o":{},"n":[{"a":{}}]}`,
			want: `{"e":[],"k":" <&>","n":[{"a":{}}],"o":{}}`,
		},
		{
			name: "large exact integers",
			in:   `[18446744073709551615,9007199254740993]`,
			want: `[18446744073709551615,9007199254740993]`,
		},
		{
			name: "float zero and integer fraction",
			in:   `[1.0,17.0,-0,1e5]`,
			want: `[1.0,17.0,-0.0,100000.0]`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := Decode([]byte(tt.in))
			require.NoError(t, err)
			got, err := Compact(v)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
			assert.Equal(t, tt.want, CompactString(v))
		})
	}
}

func TestPretty(t *testing.T) {
	v, err := Decode([]byte(`{"k":" <&>","e":[],"o":{},"n":[{"a":{}}]}`))
	require.NoError(t, err)
	got, err := Pretty(v)
	require.NoError(t, err)
	assert.Equal(t, "{\n  \"e\": [],\n  \"k\": \" <&>\",\n  \"n\": [\n    {\n      \"a\": {}\n    }\n  ],\n  \"o\": {}\n}", string(got))
}

func TestCompactGoValues(t *testing.T) {
	type inner struct {
		Z string `json:"z"`
		A *int   `json:"a,omitempty"`
	}
	type outer struct {
		Second int     `json:"second"`
		First  *string `json:"first"`
		Skip   string  `json:"-"`
		In     inner   `json:"in"`
	}
	type optional struct {
		Zero   int            `json:"zero,omitempty"`
		Empty  string         `json:"empty,omitempty"`
		List   []string       `json:"list,omitempty"`
		Lookup map[string]int `json:"lookup,omitempty"`
		Nil    *int           `json:"nil,omitempty"`
	}
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, `null`},
		{"struct field order and null", outer{Second: 2, Skip: "x", In: inner{Z: "<"}}, `{"second":2,"first":null,"in":{"z":"<"}}`},
		{"omitempty skips only nil", optional{List: []string{}, Lookup: map[string]int{}}, `{"zero":0,"empty":"","list":[],"lookup":{}}`},
		{"go floats and integers", []any{1.0, 0.5, uint64(18446744073709551615), int64(-3)}, `[1.0,0.5,18446744073709551615,-3]`},
		{"float32 uses its own shortest digits", float32(0.1), `0.1`},
		{"float32 lower plain threshold", float32(1e-5), `0.00001`},
		{"float32 lower exponent threshold", float32(1e-6), `1e-6`},
		{"float32 upper plain threshold", float32(1e15), `1000000000000000.0`},
		{"float32 upper exponent threshold", float32(1e16), `1e+16`},
		{"float32 negative zero", float32(math.Copysign(0, -1)), `-0.0`},
		{"float32 NaN", float32(math.NaN()), `null`},
		{"float32 positive infinity", float32(math.Inf(1)), `null`},
		{"float32 negative infinity", float32(math.Inf(-1)), `null`},
		{"invalid UTF-8 becomes replacement", "a\xffb", "\"a�b\""},
		{"string map keys sort", map[string]string{"b": "1", "a": "2"}, `{"a":"2","b":"1"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Compact(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
	_, err := Compact(map[int]string{1: "x"})
	require.Error(t, err)
	assert.Panics(t, func() { CompactString(make(chan int)) })
}
