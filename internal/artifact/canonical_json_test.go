package artifact

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalJSONSortsStructAndMapKeys(t *testing.T) {
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
	v := struct {
		Items []string `json:"items"`
	}{Items: []string{"z", "a", "m"}}

	data, err := canonicalJSON(v)
	require.NoError(t, err)
	assert.Equal(t, "{\"items\":[\"z\",\"a\",\"m\"]}\n", string(data))
}

func TestCanonicalJSONRecanonicalizesRawMessage(t *testing.T) {
	type wrapper struct {
		Value json.RawMessage `json:"value"`
	}
	v := wrapper{Value: json.RawMessage(`{ "b" : 2, "a" : 1 }`)}

	data, err := canonicalJSON(v)
	require.NoError(t, err)
	assert.Equal(t, "{\"value\":{\"a\":1,\"b\":2}}\n", string(data))
}

func TestCanonicalJSONEmptyRawMessageEncodesAsNull(t *testing.T) {
	type wrapper struct {
		Value json.RawMessage `json:"value"`
	}

	data, err := canonicalJSON(wrapper{})
	require.NoError(t, err)
	assert.Equal(t, "{\"value\":null}\n", string(data))
}

func TestCanonicalJSONPreservesLargeNumberPrecision(t *testing.T) {
	type wrapper struct {
		Value json.RawMessage `json:"value"`
	}
	// 2^53+1: unsafe to round-trip through float64, so this only survives if
	// the RawMessage decode path uses json.Number instead of float64.
	v := wrapper{Value: json.RawMessage(`9007199254740993`)}

	data, err := canonicalJSON(v)
	require.NoError(t, err)
	assert.Equal(t, "{\"value\":9007199254740993}\n", string(data))
}

func TestCanonicalJSONNilPointerAndInterfaceEncodeAsNull(t *testing.T) {
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
	name := "Fixture"
	v := struct {
		Name *string `json:"name"`
	}{Name: &name}

	data, err := canonicalJSON(v)
	require.NoError(t, err)
	assert.Equal(t, "{\"name\":\"Fixture\"}\n", string(data))
}

func TestCanonicalJSONOmitsEmptyFieldsAndKeepsZeroValuesWithoutTag(t *testing.T) {
	type v struct {
		Kept    int    `json:"kept"`
		Skipped string `json:"skipped,omitempty"`
		Ignored string `json:"-"`
	}

	data, err := canonicalJSON(v{Kept: 0, Skipped: "", Ignored: "hidden"})
	require.NoError(t, err)
	assert.Equal(t, "{\"kept\":0}\n", string(data))
}

func TestCanonicalJSONRejectsNonStringMapKeys(t *testing.T) {
	v := map[int]string{1: "a"}

	_, err := canonicalJSON(v)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported canonical map key type")
}

func TestCanonicalJSONRejectsUnsupportedKind(t *testing.T) {
	v := struct {
		Ch chan int `json:"ch"`
	}{Ch: make(chan int)}

	_, err := canonicalJSON(v)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported canonical JSON kind")
}
