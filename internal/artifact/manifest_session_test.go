package artifact

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestManifestSessionMatchesDBSessionWireFormat pins the manifest wire DTO to
// the JSON-visible fields of db.Session with a fully populated value: every
// exported field gets a distinct non-zero value, so a missing, extra, or
// transposed DTO field changes the canonical JSON and fails the comparison.
//
// If this test fails after adding a field to db.Session, that is the wire
// format asking for a decision: adding the field to manifestSession changes
// every manifest hash fleet-wide (a re-export/re-import of all sessions), so
// extend the DTO only when the field is genuinely part of the session content
// contract; otherwise leave the DTO alone and update populateWireFixture's
// expectations here.
func TestManifestSessionMatchesDBSessionWireFormat(t *testing.T) {
	var sess db.Session
	populateWireFixture(t, reflect.ValueOf(&sess).Elem(), 1)

	// Deliberate parity exemption: quality_signals is hoisted to the
	// manifest-level session_quality_signals field because db.Session's
	// pointer is load-path-transient (see the manifestSession struct
	// comment). The reference for parity is the session without it.
	reference := sess
	reference.QualitySignals = nil

	want, err := canonicalJSON(reference)
	require.NoError(t, err)
	got, err := canonicalJSON(manifestSessionFromDB(sess))
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got),
		"manifestSession must serialize byte-identically to db.Session minus quality_signals")

	withoutPointer, err := canonicalJSON(manifestSessionFromDB(reference))
	require.NoError(t, err)
	assert.Equal(t, string(got), string(withoutPointer),
		"manifest bytes must not depend on the transient quality_signals pointer")

	roundTrip, err := canonicalJSON(manifestSessionFromDB(sess).dbSession())
	require.NoError(t, err)
	assert.Equal(t, string(want), string(roundTrip),
		"converting to the wire DTO and back must preserve every wire-visible field")
}

func TestManifestQualitySignalsMatchesDBWireFormat(t *testing.T) {
	var qs db.QualitySignals
	populateWireFixture(t, reflect.ValueOf(&qs).Elem(), 100)

	want, err := canonicalJSON(qs)
	require.NoError(t, err)
	dto := manifestQualitySignalsFromDB(&qs)
	require.NotNil(t, dto)
	got, err := canonicalJSON(*dto)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))

	roundTrip, err := canonicalJSON(*dto.dbQualitySignals())
	require.NoError(t, err)
	assert.Equal(t, string(want), string(roundTrip))

	assert.Nil(t, manifestQualitySignalsFromDB(nil))
}

// populateWireFixture fills every exported field of a struct with a distinct
// deterministic non-zero value so field transpositions are detectable.
func populateWireFixture(t *testing.T, v reflect.Value, seed int) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		if !field.CanSet() {
			continue
		}
		setWireFixtureValue(t, field, seed+i)
	}
}

func setWireFixtureValue(t *testing.T, field reflect.Value, n int) {
	t.Helper()
	switch field.Kind() {
	case reflect.String:
		field.SetString(fmt.Sprintf("value-%d", n))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		field.SetInt(int64(n))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		field.SetUint(uint64(n))
	case reflect.Bool:
		field.SetBool(true)
	case reflect.Float32, reflect.Float64:
		field.SetFloat(float64(n) + 0.5)
	case reflect.Pointer:
		elem := reflect.New(field.Type().Elem())
		setWireFixtureValue(t, elem.Elem(), n)
		field.Set(elem)
	case reflect.Struct:
		populateWireFixture(t, field, n*10)
	default:
		t.Fatalf("populateWireFixture: unhandled field kind %s; teach the fixture about it", field.Kind())
	}
}
