package db

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const MaxActivityReportTokenLength = 8 * 1024

var (
	ErrInvalidActivityReportToken = errors.New("invalid activity report token")
	ErrActivityReportTokenTooLong = fmt.Errorf(
		"%w: encoded token exceeds maximum length", ErrInvalidActivityReportToken,
	)
)

func EncodeSignedActivityReportToken(secret, payload []byte) (string, error) {
	if len(secret) == 0 {
		return "", fmt.Errorf("%w: signing secret is empty", ErrInvalidActivityReportToken)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	signed := "v1." + encodedPayload
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(signed))
	token := signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(token) > MaxActivityReportTokenLength {
		return "", fmt.Errorf(
			"%w of %d bytes", ErrActivityReportTokenTooLong,
			MaxActivityReportTokenLength,
		)
	}
	return token, nil
}

func DecodeSignedActivityReportToken(secret []byte, token string) ([]byte, error) {
	if len(secret) == 0 {
		return nil, fmt.Errorf("%w: signing secret is empty", ErrInvalidActivityReportToken)
	}
	if len(token) == 0 || len(token) > MaxActivityReportTokenLength {
		return nil, fmt.Errorf("%w: invalid length", ErrInvalidActivityReportToken)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, fmt.Errorf("%w: unsupported format", ErrInvalidActivityReportToken)
	}
	signed := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: signature encoding", ErrInvalidActivityReportToken)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(signed))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return nil, fmt.Errorf("%w: signature mismatch", ErrInvalidActivityReportToken)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: payload encoding", ErrInvalidActivityReportToken)
	}
	return payload, nil
}

func (db *DB) EncodeActivityReportToken(payload []byte) (string, error) {
	db.cursorMu.RLock()
	secret := append([]byte(nil), db.cursorSecret...)
	db.cursorMu.RUnlock()
	return EncodeSignedActivityReportToken(secret, payload)
}

func (db *DB) DecodeActivityReportToken(token string) ([]byte, error) {
	db.cursorMu.RLock()
	secret := append([]byte(nil), db.cursorSecret...)
	db.cursorMu.RUnlock()
	return DecodeSignedActivityReportToken(secret, token)
}
