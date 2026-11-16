package server

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The stored first prompt is a 300-character preview, so a long private key
// block loses its END line and masking cannot match it. The description must
// not carry the key material that is left.
func TestGistDescriptionMasksFirstPrompt(t *testing.T) {
	// Claude previews flatten newlines to spaces before cutting to 300.
	cutKey := "use this key -----BEGIN RSA PRIVATE KEY----- " +
		strings.Repeat("MIIBSECRETKEYMATERIAL0123456789ABCDEF ", 7)[:250]
	awsKey := "AKIA" + "7QHWN2DKR4FYPLJM"
	for name, tc := range map[string]struct {
		first  string
		reveal bool
		want   string
	}{
		"cut key block": {
			first: cutKey,
			want:  "Agent session: my-app - use this key",
		},
		"key block on the first line": {
			first: "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQ",
			want:  "Agent session: my-app - ",
		},
		"other PEM header is kept": {
			first: "check -----BEGIN CERTIFICATE----- parsing",
			want:  "Agent session: my-app - check -----BEGIN CERTIFICATE----- parsing",
		},
		"detected key": {
			first: "deploy with " + awsKey + " please",
			want:  "Agent session: my-app - deploy with AKIA…PLJM please",
		},
		"reveal keeps the prompt": {
			first:  "deploy with " + awsKey + " please",
			reveal: true,
			want:   "Agent session: my-app - deploy with " + awsKey + " please",
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, gistDescription("my-app", &tc.first, tc.reveal))
		})
	}
}
