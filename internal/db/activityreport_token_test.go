package db

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivityReportTokenRoundTripAndSignature(t *testing.T) {
	secret := bytes.Repeat([]byte{1}, 32)
	token, err := EncodeSignedActivityReportToken(secret, []byte(`{"query":"month"}`))
	require.NoError(t, err)
	payload, err := DecodeSignedActivityReportToken(secret, token)
	require.NoError(t, err)
	assert.JSONEq(t, `{"query":"month"}`, string(payload))

	_, err = DecodeSignedActivityReportToken(bytes.Repeat([]byte{2}, 32), token)
	assert.ErrorIs(t, err, ErrInvalidActivityReportToken)
	_, err = DecodeSignedActivityReportToken(secret, "v2.payload.signature")
	assert.ErrorIs(t, err, ErrInvalidActivityReportToken)
}

func TestActivityReportTokenRejectsImpracticalURLLength(t *testing.T) {
	_, err := EncodeSignedActivityReportToken(
		bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte("x"), MaxActivityReportTokenLength),
	)
	assert.ErrorIs(t, err, ErrActivityReportTokenTooLong)
	assert.ErrorIs(t, err, ErrInvalidActivityReportToken)
}

func TestActivityReportTokenRejectsEmptySigningSecret(t *testing.T) {
	_, err := EncodeSignedActivityReportToken(nil, []byte(`{"query":"month"}`))
	assert.ErrorIs(t, err, ErrInvalidActivityReportToken)

	secret := bytes.Repeat([]byte{1}, 32)
	token, err := EncodeSignedActivityReportToken(secret, []byte(`{"query":"month"}`))
	require.NoError(t, err)
	_, err = DecodeSignedActivityReportToken(nil, token)
	assert.ErrorIs(t, err, ErrInvalidActivityReportToken)
}
