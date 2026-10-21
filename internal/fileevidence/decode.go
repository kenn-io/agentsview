package fileevidence

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"strings"
)

func DecodeText(raw []byte) ([]byte, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, errors.New("invalid_unicode")
	}
	if strings.ContainsRune(s, '\ufffd') {
		return nil, errors.New("invalid_unicode")
	}
	if len(s) > MaxAttachmentBytes {
		return nil, errors.New("payload_limit")
	}
	return []byte(s), nil
}

func DecodeBase64(s string) ([]byte, error) {
	if len(s) > base64.StdEncoding.EncodedLen(MaxAttachmentBytes) || strings.ContainsAny(s, "\r\n \t") {
		return nil, errors.New("payload_limit_or_encoding")
	}
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || len(b) > MaxAttachmentBytes {
		return nil, errors.New("invalid_base64")
	}
	return b, nil
}

func Proof(b []byte) Record {
	n := int64(len(b))
	sum := sha256.Sum256(b)
	return Record{Size: &n, SHA256: hex.EncodeToString(sum[:])}
}

// DecodeImageURI is shared with the existing structured tool-image projection.
func DecodeImageURI(uri string) (string, []byte, error) {
	header, payload, ok := strings.Cut(uri, ",")
	if !ok || !strings.HasPrefix(header, "data:image/") || !strings.HasSuffix(header, ";base64") {
		return "", nil, errors.New("unsupported_image")
	}
	media := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
	if strings.ContainsAny(media, "; \r\n") {
		return "", nil, errors.New("unsupported_image")
	}
	b, err := DecodeBase64(payload)
	return media, b, err
}
