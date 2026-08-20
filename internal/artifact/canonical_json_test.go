package artifact

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalJSONSortsStructAndMapKeys(t *testing.T) {
	t.Parallel()

	type inner struct {
		Zeta  string `json:"zeta"`
		Alpha string `json:"alpha"`
	}
	v := struct {
		Inner inner             `json:"inner"`
		Tags  map[string]string `json:"tags"`
	}{
		Inner: inner{Zeta: "z", Alpha: "a"},
		Tags:  map[string]string{"b": "2", "a": "1"},
	}

	data, err := canonicalJSON(v)
	require.NoError(t, err)
	assert.Equal(t,
		"{\"inner\":{\"alpha\":\"a\",\"zeta\":\"z\"},\"tags\":{\"a\":\"1\",\"b\":\"2\"}}\n",
		string(data),
	)
}

func TestCanonicalJSONPreservesSliceOrder(t *testing.T) {
	t.Parallel()

	v := struct {
		Items []string `json:"items"`
	}{Items: []string{"z", "a", "m"}}

	data, err := canonicalJSON(v)
	require.NoError(t, err)
	assert.Equal(t, "{\"items\":[\"z\",\"a\",\"m\"]}\n", string(data))
}

func TestCanonicalJSONRecanonicalizesRawMessage(t *testing.T) {
	t.Parallel()

	type wrapper struct {
		Value jsontext.Value `json:"value"`
	}
	v := wrapper{Value: jsontext.Value(`{ "b" : 2, "a" : 1 }`)}

	data, err := canonicalJSON(v)
	require.NoError(t, err)
	assert.Equal(t, "{\"value\":{\"a\":1,\"b\":2}}\n", string(data))
}

func TestCanonicalJSONRejectsTrailingRawMessageContent(t *testing.T) {
	t.Parallel()

	type wrapper struct {
		Value jsontext.Value `json:"value"`
	}
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "second value", raw: `{"a":1}{"b":2}`, wantErr: true},
		{name: "trailing garbage", raw: `{"a":1}garbage`, wantErr: true},
		{name: "trailing scalar", raw: `1 2`, wantErr: true},
		{name: "trailing whitespace", raw: "{\"a\":1} \n\t", wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := canonicalJSON(wrapper{Value: jsontext.Value(tt.raw)})
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCanonicalJSONEmptyRawMessageEncodesAsNull(t *testing.T) {
	t.Parallel()

	type wrapper struct {
		Value jsontext.Value `json:"value"`
	}

	data, err := canonicalJSON(wrapper{})
	require.NoError(t, err)
	assert.Equal(t, "{\"value\":null}\n", string(data))
}

func TestCanonicalJSONPreservesLargeNumberPrecision(t *testing.T) {
	t.Parallel()

	type wrapper struct {
		Value jsontext.Value `json:"value"`
	}
	// 2^53+1 must remain exact when canonicalizing a raw JSON value.
	v := wrapper{Value: jsontext.Value(`9007199254740993`)}

	data, err := canonicalJSON(v)
	require.NoError(t, err)
	assert.Equal(t, "{\"value\":9007199254740993}\n", string(data))
}

func TestCanonicalJSONNilPointerAndInterfaceEncodeAsNull(t *testing.T) {
	t.Parallel()

	var nilPointer *int
	data, err := canonicalJSON(nilPointer)
	require.NoError(t, err)
	assert.Equal(t, "null\n", string(data))

	var nilInterface any
	data, err = canonicalJSON(nilInterface)
	require.NoError(t, err)
	assert.Equal(t, "null\n", string(data))
}

func TestCanonicalJSONDereferencesPopulatedPointerFields(t *testing.T) {
	t.Parallel()

	name := "Fixture"
	v := struct {
		Name *string `json:"name"`
	}{Name: &name}

	data, err := canonicalJSON(v)
	require.NoError(t, err)
	assert.Equal(t, "{\"name\":\"Fixture\"}\n", string(data))
}

func TestCanonicalJSONOmitsEmptyFieldsAndKeepsZeroValuesWithoutTag(t *testing.T) {
	t.Parallel()

	type v struct {
		Kept    int    `json:"kept"`
		Skipped string `json:"skipped,omitempty"`
		Ignored string `json:"-"`
	}

	data, err := canonicalJSON(v{Kept: 0, Skipped: "", Ignored: "hidden"})
	require.NoError(t, err)
	assert.Equal(t, "{\"kept\":0}\n", string(data))
}

func TestCanonicalJSONCanonicalizesIntegerMapKeys(t *testing.T) {
	t.Parallel()

	v := map[int]string{1: "a"}

	data, err := canonicalJSON(v)
	require.NoError(t, err)
	assert.Equal(t, "{\"1\":\"a\"}\n", string(data))
}

func TestCanonicalJSONRejectsUnsupportedKind(t *testing.T) {
	t.Parallel()

	v := struct {
		Ch chan int `json:"ch"`
	}{Ch: make(chan int)}

	_, err := canonicalJSON(v)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "encoding canonical artifact JSON")
}
