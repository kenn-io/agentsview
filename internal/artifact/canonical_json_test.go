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
